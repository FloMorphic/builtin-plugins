package decisionnode

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bytedance/sonic"
)

const (
	// The hosted service's own host, and the node's default. docs.typesafe.ai
	// documents api.typesafe.ai, which answers 401 to every key — thejevai.com
	// is the endpoint that serves the System One API. A profile points `url`
	// elsewhere to reach any other server speaking the same protocol: a local
	// Laya (the open, local-first System One model, which serves this same
	// endpoint with Jev-shaped choice/score/noul answers), a proxy, a private
	// deployment.
	defaultBaseURL = "https://thejevai.com"
	// The hosted service's documented alias, used only when the profile talks
	// to defaultBaseURL (validateSettings requires an explicit model for any
	// other endpoint, since this alias means nothing there). The hosted service
	// also accepts a vendor-prefixed, pinned id ("typesafe/jev-1.13"), which is
	// the safer thing to put in a profile once a flow is in production — a
	// decision node changing model under a flow is a change someone should
	// choose.
	defaultModel   = "jev-latest"
	defaultTimeout = 30 * time.Second
	// The one endpoint of the protocol. Both the hosted service and Laya serve
	// it, which is why this node is not bound to either.
	endpointPath = "/v1/systemone"

	// The API asks callers to back off on 429 (rate limit) and 529 (overloaded).
	// A decision node is on the hot path of a flow, so the retry budget is kept
	// short: three attempts, ~0.5s → 1s → 2s apart, then the failure routes
	// `_exception` and the flow decides.
	maxAttempts  = 3
	retryBackoff = 500 * time.Millisecond

	// How much of an error reply body is quoted back in the failure reason.
	errBodyLimit = 300

	// The host sits behind a CDN that screens unfamiliar clients, so identify
	// the node rather than leaving Go's default agent on the request.
	userAgent = "flomorphic-ai-decision-node/0.1"
)

// validateSettings checks the profile the way the two deployments differ.
//
// A key is required against the hosted service and pointless against a local
// one, and a model id is only guessable for the hosted default — so the rule
// follows the URL: the default endpoint needs an access token and may take the
// default model, while a profile that names its own endpoint (a local Laya, a
// proxy, a private deployment) needs no token but must say which model that
// endpoint serves. Sending "jev-latest" to a Laya server would otherwise come
// back as a 422 from the far side instead of a readable error from here.
func validateSettings(cfg DecisionSettings) error {
	if isDefaultEndpoint(cfg) {
		if strings.TrimSpace(cfg.AccessToken) == "" {
			return fmt.Errorf("missing required settings-profile fields: settings.access_token (required for the hosted endpoint %s)", defaultBaseURL)
		}
		return nil
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return fmt.Errorf("missing required settings-profile fields: settings.model (required when settings.url names its own endpoint, e.g. a local Laya server)")
	}
	return nil
}

// isDefaultEndpoint reports whether the profile talks to the hosted service.
func isDefaultEndpoint(cfg DecisionSettings) bool {
	return baseURL(cfg) == defaultBaseURL
}

// baseURL returns the profile's base URL or the public default, without a
// trailing slash so the endpoint path joins cleanly.
func baseURL(cfg DecisionSettings) string {
	u := strings.TrimSpace(cfg.URL)
	if u == "" {
		u = defaultBaseURL
	}
	return strings.TrimRight(u, "/")
}

// modelOf returns the profile's model id or the default alias.
func modelOf(cfg DecisionSettings) string {
	if m := strings.TrimSpace(cfg.Model); m != "" {
		return m
	}
	return defaultModel
}

// timeoutOf returns the per-call timeout the profile set, or the default.
func timeoutOf(cfg DecisionSettings) time.Duration {
	if cfg.TimeoutSeconds > 0 {
		return time.Duration(cfg.TimeoutSeconds) * time.Second
	}
	return defaultTimeout
}

// callDecision POSTs one System One request and decodes the reply. Transient
// statuses (429, 529) are retried with a bounded exponential backoff; any other
// non-2xx status is returned at once with the status and a slice of the body so
// the exception branch can see what the API objected to (a 422 names the
// question that failed validation).
func callDecision(ctx context.Context, cfg DecisionSettings, req apiRequest) (reply, error) {
	payload, err := sonic.Marshal(req)
	if err != nil {
		return reply{}, fmt.Errorf("encode request: %w", err)
	}
	client := &http.Client{Timeout: timeoutOf(cfg)}
	url := baseURL(cfg) + endpointPath

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			wait := retryBackoff << (attempt - 2)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return reply{}, ctx.Err()
			}
		}
		resp, retry, err := postOnce(ctx, client, url, cfg.AccessToken, payload)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !retry {
			break
		}
	}
	return reply{}, lastErr
}

// postOnce performs a single attempt. The retry flag tells the caller whether
// the failure is one the API asks to be retried (rate limit / overload).
func postOnce(ctx context.Context, client *http.Client, url, token string, payload []byte) (reply, bool, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return reply{}, false, err
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
		return reply{}, false, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return reply{}, false, fmt.Errorf("read reply: %w", err)
	}

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		retry := res.StatusCode == http.StatusTooManyRequests || res.StatusCode == 529
		return reply{}, retry, fmt.Errorf("decision api %s: %s", res.Status, snippet(body))
	}

	var out apiResponse
	if err := sonic.Unmarshal(body, &out); err != nil {
		return reply{}, false, fmt.Errorf("decode reply: %w (%s)", err, snippet(body))
	}
	// The envelope carries its own status: a non-zero `code` is a failure the
	// service reported with HTTP 200, so it must not pass as an answer.
	if out.Code != 0 {
		return reply{}, false, fmt.Errorf("decision api code %d: %s", out.Code, out.Message)
	}
	r := out.reply()
	if len(r.Answers) == 0 {
		return reply{}, false, fmt.Errorf("decision api returned no answers (%s)", snippet(body))
	}
	return r, false, nil
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
