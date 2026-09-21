package jevnode

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
	defaultBaseURL = "https://api.typesafe.ai"
	defaultModel   = "jev-latest"
	defaultTimeout = 30 * time.Second
	endpointPath   = "/v1/systemone"

	// The API asks callers to back off on 429 (rate limit) and 529 (overloaded).
	// A decision node is on the hot path of a flow, so the retry budget is kept
	// short: three attempts, ~0.5s → 1s → 2s apart, then the failure routes
	// `_exception` and the flow decides.
	maxAttempts  = 3
	retryBackoff = 500 * time.Millisecond

	// How much of an error reply body is quoted back in the failure reason.
	errBodyLimit = 300
)

// baseURL returns the profile's base URL or the public default, without a
// trailing slash so the endpoint path joins cleanly.
func baseURL(cfg JevSettings) string {
	u := strings.TrimSpace(cfg.URL)
	if u == "" {
		u = defaultBaseURL
	}
	return strings.TrimRight(u, "/")
}

// modelOf returns the profile's model id or the default alias.
func modelOf(cfg JevSettings) string {
	if m := strings.TrimSpace(cfg.Model); m != "" {
		return m
	}
	return defaultModel
}

// timeoutOf returns the per-call timeout the profile set, or the default.
func timeoutOf(cfg JevSettings) time.Duration {
	if cfg.TimeoutSeconds > 0 {
		return time.Duration(cfg.TimeoutSeconds) * time.Second
	}
	return defaultTimeout
}

// callJev POSTs one System One request and decodes the reply. Transient
// statuses (429, 529) are retried with a bounded exponential backoff; any other
// non-2xx status is returned at once with the status and a slice of the body so
// the exception branch can see what the API objected to (a 422 names the
// question that failed validation).
func callJev(ctx context.Context, cfg JevSettings, req apiRequest) (apiResponse, error) {
	payload, err := sonic.Marshal(req)
	if err != nil {
		return apiResponse{}, fmt.Errorf("encode request: %w", err)
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
				return apiResponse{}, ctx.Err()
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
	return apiResponse{}, lastErr
}

// postOnce performs a single attempt. The retry flag tells the caller whether
// the failure is one the API asks to be retried (rate limit / overload).
func postOnce(ctx context.Context, client *http.Client, url, token string, payload []byte) (apiResponse, bool, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return apiResponse{}, false, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))

	res, err := client.Do(httpReq)
	if err != nil {
		return apiResponse{}, false, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return apiResponse{}, false, fmt.Errorf("read reply: %w", err)
	}

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		retry := res.StatusCode == http.StatusTooManyRequests || res.StatusCode == 529
		return apiResponse{}, retry, fmt.Errorf("jev api %s: %s", res.Status, snippet(body))
	}

	var out apiResponse
	if err := sonic.Unmarshal(body, &out); err != nil {
		return apiResponse{}, false, fmt.Errorf("decode reply: %w (%s)", err, snippet(body))
	}
	return out, false, nil
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
