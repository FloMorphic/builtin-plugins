package decisionnode

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bytedance/sonic"
)

const (
	defaultTimeout = 30 * time.Second

	// A decision node sits on the hot path of a flow, so the default retry
	// budget is deliberately small — two further attempts, ~0.5s then 1s apart
	// — after which the failure routes `_exception` and the flow decides what
	// to do about it. Which statuses are retried at all is the protocol's
	// business: see dialect.retryable.
	//
	// DefaultMaxRetries is what a profile that never mentions retrying gets. A
	// profile can raise it for a rate-limited key, or set it to 0 for a flow
	// where a late decision is worse than no decision.
	DefaultMaxRetries = 2
	retryBackoff      = 500 * time.Millisecond

	// maxBackoff caps one wait. The service may ask for longer through
	// Retry-After; a decision node that parks a flow for minutes is not doing
	// it a favour, so the ask is honoured only up to this.
	maxBackoff = 8 * time.Second

	// How much of an error reply body is quoted back in the failure reason.
	errBodyLimit = 300

	// Both hosts sit behind a CDN that screens unfamiliar clients, so identify
	// the node rather than leaving Go's default agent on the request.
	userAgent = "flomorphic-ai-decision-node/0.2"
)

// validateSettings checks the profile the way the deployments differ, which is
// the same rule on either protocol: it follows the URL.
//
// A key is required against a first-party hosted service and pointless against
// a local one, and a model id is only guessable for the default host — so the
// default endpoint needs an access token and may take the dialect's default
// model, while a profile that names its own endpoint (a local Laya or Nimble, a
// gateway, a proxy, a private deployment) needs no token but must say which
// model that endpoint serves.
//
// That second half is not pedantry on either protocol: "jev-latest" means
// nothing to a Nimble server, and "gpt-6-luna" means nothing to a gateway that
// slugs the same model "openai/gpt-6-luna-decisions". Either would come back as
// a validation error from the far side instead of a readable one from here.
func validateSettings(d dialect, cfg DecisionSettings) error {
	if isDefaultEndpoint(d, cfg) {
		if strings.TrimSpace(cfg.AccessToken) == "" {
			return fmt.Errorf("missing required settings-profile fields: settings.access_token (required for the hosted %s endpoint %s)",
				d.name(), d.defaultBaseURL())
		}
		return nil
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return fmt.Errorf("missing required settings-profile fields: settings.model (required when settings.url names its own endpoint — a local server, a gateway or a proxy — because %q is only meaningful at %s)",
			d.defaultModel(), d.defaultBaseURL())
	}
	return nil
}

// isDefaultEndpoint reports whether the profile talks to the protocol's
// first-party host.
func isDefaultEndpoint(d dialect, cfg DecisionSettings) bool {
	return baseURL(d, cfg) == d.defaultBaseURL()
}

// baseURL returns the profile's base URL or the dialect's default, without a
// trailing slash so the endpoint path joins cleanly.
func baseURL(d dialect, cfg DecisionSettings) string {
	u := strings.TrimSpace(cfg.URL)
	if u == "" {
		u = d.defaultBaseURL()
	}
	return strings.TrimRight(u, "/")
}

// modelOf returns the profile's model id or the dialect's default.
func modelOf(d dialect, cfg DecisionSettings) string {
	if m := strings.TrimSpace(cfg.Model); m != "" {
		return m
	}
	return d.defaultModel()
}

// timeoutOf returns the per-call timeout the profile set, or the default.
func timeoutOf(cfg DecisionSettings) time.Duration {
	if cfg.TimeoutSeconds > 0 {
		return time.Duration(cfg.TimeoutSeconds) * time.Second
	}
	return defaultTimeout
}

// retriesOf returns how many FURTHER attempts a failed call gets. The field is
// a pointer so that "never retry" and "unset" are different answers: nil takes
// the default, an explicit 0 turns retrying off.
func retriesOf(cfg DecisionSettings) int {
	if cfg.MaxRetries == nil {
		return DefaultMaxRetries
	}
	if *cfg.MaxRetries < 0 {
		return 0
	}
	return *cfg.MaxRetries
}

// waitFor is how long to hold before the next attempt: what the service asked
// for through Retry-After when it said anything, otherwise an exponential
// backoff, and never longer than maxBackoff.
func waitFor(attempt int, hint time.Duration) time.Duration {
	wait := hint
	if wait <= 0 {
		// Bound the shift before shifting: a profile may raise the retry count,
		// and `retryBackoff << 98` overflows to zero — which would turn the
		// backoff into a hot loop against a service that just asked for room.
		shift := attempt - 1
		if shift > 10 {
			shift = 10 // 500ms << 10 = 512s, already far past maxBackoff
		}
		if shift < 0 {
			shift = 0
		}
		wait = retryBackoff << shift
	}
	if wait > maxBackoff {
		wait = maxBackoff
	}
	return wait
}

// retryAfter reads the Retry-After header, which either service may send with a
// 429. Both documented forms are accepted: a delay in seconds, or an HTTP date.
func retryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(v); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}

// callDecision POSTs one request and decodes the reply through the dialect.
// Transient statuses — whichever ones the protocol asks callers to back off on
// — are retried as many times as the profile allows, waiting for whatever the
// service asked for or an exponential backoff; any other non-2xx status is
// returned at once with the status and a slice of the body so the exception
// branch can see what the API objected to (a 400/422 names the question or the
// field that failed validation).
//
// `notify` is called before each wait so the canvas shows the backoff rather
// than the node going quiet — with retries raised on a rate-limited key, a
// silent node is easy to mistake for a hang.
func callDecision(ctx context.Context, d dialect, cfg DecisionSettings, req any, notify func(string)) (reply, error) {
	payload, err := sonic.Marshal(req)
	if err != nil {
		return reply{}, fmt.Errorf("encode request: %w", err)
	}
	client := &http.Client{Timeout: timeoutOf(cfg)}
	url := baseURL(d, cfg) + d.path()
	retries := retriesOf(cfg)

	var (
		lastErr  error
		lastHint time.Duration // what the service asked for, if it said anything
	)
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			wait := waitFor(attempt, lastHint)
			if notify != nil {
				notify(fmt.Sprintf("provider busy, retrying in %s (attempt %d of %d)",
					wait.Round(100*time.Millisecond), attempt+1, retries+1))
			}
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return reply{}, ctx.Err()
			}
		}
		resp, hint, retry, err := postOnce(ctx, client, d, url, cfg.AccessToken, payload)
		if err == nil {
			return resp, nil
		}
		lastErr, lastHint = err, hint
		if !retry {
			break
		}
	}
	return reply{}, lastErr
}

// postOnce performs a single attempt. The retry flag tells the caller whether
// the failure is one the protocol asks to be retried.
func postOnce(ctx context.Context, client *http.Client, d dialect, url, token string, payload []byte) (reply, time.Duration, bool, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return reply{}, 0, false, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", userAgent)
	// A local endpoint usually has no key, and an empty bearer is worse than no
	// header at all: some servers reject the malformed credential outright.
	if t := strings.TrimSpace(token); t != "" {
		httpReq.Header.Set("Authorization", "Bearer "+t)
	}

	res, err := client.Do(httpReq)
	if err != nil {
		return reply{}, 0, false, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return reply{}, 0, false, fmt.Errorf("read reply: %w", err)
	}

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return reply{}, retryAfter(res.Header), d.retryable(res.StatusCode),
			fmt.Errorf("decision api %s: %s", res.Status, snippet(body))
	}

	r, err := d.decode(body)
	if err != nil {
		return reply{}, 0, false, err
	}
	if len(r.Answers) == 0 {
		return reply{}, 0, false, fmt.Errorf("decision api returned no answers (%s)", snippet(body))
	}
	return r, 0, false, nil
}

// snippet trims a reply body to a single-line excerpt for an error message.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > errBodyLimit {
		s = s[:errBodyLimit] + "…"
	}
	return s
}
