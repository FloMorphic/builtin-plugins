package decisionnode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bytedance/sonic"
)

func TestCallDecision(t *testing.T) {
	t.Run("posts the request shape and decodes the answers", func(t *testing.T) {
		var got apiRequest
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != endpointPath || r.Method != http.MethodPost {
				t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			}
			if r.Header.Get("Authorization") != "Bearer k3y" {
				t.Errorf("auth header = %q", r.Header.Get("Authorization"))
			}
			if err := sonic.ConfigDefault.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"category":{"type":"choice","choice":"billing","probabilities":{"billing":0.88,"sales":0.12},"confidence":0.81}},"usage":{"input_tokens":10,"output_tokens":2}}`))
		}))
		defer srv.Close()

		cfg := DecisionSettings{AccessToken: "k3y", URL: srv.URL + "/"}
		q := Question{ID: "category", Type: typeChoice, Instructions: "which team?", Options: []Option{{Name: "billing", Description: "Payments"}, {Name: "sales"}}}
		resp, err := callDecision(context.Background(), cfg, buildRequest(identity, cfg, "duplicate charge", []Question{q}), nil)
		if err != nil {
			t.Fatal(err)
		}
		if got.Model != defaultModel || got.State != "duplicate charge" {
			t.Fatalf("request = %+v", got)
		}
		if got.Questions["category"].Type != "choice" {
			t.Fatalf("question not sent: %+v", got.Questions)
		}
		if resp.Model != "jev-1.13.0" || resp.Answers["category"].Choice != "billing" || *resp.Answers["category"].Confidence != 0.81 {
			t.Fatalf("response = %+v", resp)
		}
	})

	t.Run("retries 429 then succeeds", func(t *testing.T) {
		var calls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if atomic.AddInt32(&calls, 1) == 1 {
				http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
				return
			}
			_, _ = w.Write([]byte(`{"model":"jev","answers":{"q":{"type":"noul","noul":0.6}}}`))
		}))
		defer srv.Close()
		_, err := callDecision(context.Background(), DecisionSettings{AccessToken: "k", URL: srv.URL}, apiRequest{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if calls != 2 {
			t.Fatalf("calls = %d, want 2", calls)
		}
	})

	t.Run("does not retry 401 and quotes the body", func(t *testing.T) {
		var calls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			http.Error(w, `{"error":"Missing or invalid API key"}`, http.StatusUnauthorized)
		}))
		defer srv.Close()
		_, err := callDecision(context.Background(), DecisionSettings{AccessToken: "bad", URL: srv.URL}, apiRequest{}, nil)
		if err == nil {
			t.Fatal("expected error")
		}
		if calls != 1 {
			t.Fatalf("calls = %d, want 1", calls)
		}
		if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "invalid API key") {
			t.Fatalf("error = %q", err)
		}
	})

	t.Run("gives up after the retry budget on 529", func(t *testing.T) {
		var calls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			http.Error(w, "overloaded", 529)
		}))
		defer srv.Close()
		_, err := callDecision(context.Background(), DecisionSettings{AccessToken: "k", URL: srv.URL}, apiRequest{}, nil)
		if err == nil {
			t.Fatal("expected error")
		}
		if want := int32(DefaultMaxRetries + 1); calls != want {
			t.Fatalf("calls = %d, want %d (one call plus %d retries)", calls, want, DefaultMaxRetries)
		}
	})
}

// The live service wraps the answer document in {code, message, data:{result,
// creditsUsed}} while the published reference shows it flat. Both have to land
// on the same normalized reply, and a non-zero `code` — a failure the service
// reports with HTTP 200 — must not pass as an answer.
func TestCallDecisionEnvelope(t *testing.T) {
	serve := func(body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ua := r.Header.Get("User-Agent"); ua != userAgent {
				t.Errorf("User-Agent = %q, want %q", ua, userAgent)
			}
			_, _ = w.Write([]byte(body))
		}))
	}

	t.Run("enveloped reply is unwrapped, credits carried", func(t *testing.T) {
		srv := serve(`{"code":0,"message":"ok","data":{"result":{"answers":{"urgency":{"type":"noul","noul":0.61}},"usage":{"input_tokens":311,"output_tokens":21},"elapsedMs":1801},"creditsUsed":1}}`)
		defer srv.Close()
		got, err := callDecision(context.Background(), DecisionSettings{AccessToken: "k", URL: srv.URL}, apiRequest{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if *got.Answers["urgency"].Noul != 0.61 {
			t.Errorf("answers = %+v", got.Answers)
		}
		if got.CreditsUsed != 1 || got.ElapsedMs != 1801 {
			t.Errorf("credits = %d, elapsed = %d", got.CreditsUsed, got.ElapsedMs)
		}
		if got.Usage["input_tokens"] != float64(311) {
			t.Errorf("usage = %v", got.Usage)
		}
	})

	t.Run("flat reply still works", func(t *testing.T) {
		srv := serve(`{"model":"jev-1.13.0","answers":{"urgency":{"type":"noul","noul":0.2}},"usage":{"input_tokens":10}}`)
		defer srv.Close()
		got, err := callDecision(context.Background(), DecisionSettings{AccessToken: "k", URL: srv.URL}, apiRequest{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got.Model != "jev-1.13.0" || *got.Answers["urgency"].Noul != 0.2 || got.CreditsUsed != 0 {
			t.Errorf("reply = %+v", got)
		}
	})

	t.Run("non-zero code on HTTP 200 is an error", func(t *testing.T) {
		srv := serve(`{"code":4001,"message":"insufficient credits","data":null}`)
		defer srv.Close()
		_, err := callDecision(context.Background(), DecisionSettings{AccessToken: "k", URL: srv.URL}, apiRequest{}, nil)
		if err == nil || !strings.Contains(err.Error(), "insufficient credits") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("an empty answer set is an error, not a silent pass", func(t *testing.T) {
		srv := serve(`{"code":0,"message":"ok","data":{"result":{"answers":{}},"creditsUsed":0}}`)
		defer srv.Close()
		if _, err := callDecision(context.Background(), DecisionSettings{AccessToken: "k", URL: srv.URL}, apiRequest{}, nil); err == nil {
			t.Fatal("expected error")
		}
	})
}

// The default endpoint is the host that actually serves the API, and a profile
// URL overrides it.
func TestBaseURL(t *testing.T) {
	if got := baseURL(DecisionSettings{}); got != "https://api.typesafe.ai" {
		t.Errorf("default baseURL = %q", got)
	}
	if got := baseURL(DecisionSettings{URL: "https://proxy.internal/"}); got != "https://proxy.internal" {
		t.Errorf("override baseURL = %q", got)
	}
}

func TestValidateSettings(t *testing.T) {
	// The rule follows the URL, because that is what tells the two deployments
	// apart: a key is required against the hosted service and pointless against
	// a local one, while "jev-latest" means nothing to anything but the host.
	t.Run("hosted needs a key", func(t *testing.T) {
		if err := validateSettings(DecisionSettings{}); err == nil {
			t.Fatal("want an error naming access_token")
		}
		if err := validateSettings(DecisionSettings{AccessToken: "k"}); err != nil {
			t.Fatalf("hosted with a key: %v", err)
		}
	})
	t.Run("hosted by explicit url still needs a key", func(t *testing.T) {
		if err := validateSettings(DecisionSettings{URL: defaultBaseURL + "/"}); err == nil {
			t.Fatal("want an error: this is still the hosted endpoint")
		}
	})
	t.Run("a local endpoint needs a model, not a key", func(t *testing.T) {
		local := DecisionSettings{URL: "http://127.0.0.1:8080"}
		if err := validateSettings(local); err == nil {
			t.Fatal("want an error naming model")
		}
		local.Model = "laya-base"
		if err := validateSettings(local); err != nil {
			t.Fatalf("local Laya profile should be valid: %v", err)
		}
	})
}

func TestAuthorizationOmittedWithoutAKey(t *testing.T) {
	// An empty bearer is worse than no header: some servers reject the
	// malformed credential instead of treating the call as unauthenticated.
	var gotAuth string
	var had bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, had = r.Header.Get("Authorization"), len(r.Header.Values("Authorization")) > 0
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"laya","answers":{"q":{"type":"noul","noul":0.9}}}`))
	}))
	defer srv.Close()

	cfg := DecisionSettings{URL: srv.URL, Model: "laya-base"}
	if _, err := callDecision(context.Background(), cfg, apiRequest{State: "x", Model: "laya-base"}, nil); err != nil {
		t.Fatalf("call: %v", err)
	}
	if had || gotAuth != "" {
		t.Fatalf("Authorization sent with no key: %q", gotAuth)
	}
}

// Retrying is a property of the endpoint, so it lives in the profile — the
// pointer is what lets a profile say "never retry" (0) as distinct from
// "unset" (absent), which an int could not express.
func TestRetriesOf(t *testing.T) {
	zero, three, negative := 0, 3, -1
	for _, c := range []struct {
		name string
		cfg  DecisionSettings
		want int
	}{
		{"unset takes the default", DecisionSettings{}, DefaultMaxRetries},
		{"explicit zero never retries", DecisionSettings{MaxRetries: &zero}, 0},
		{"explicit count is honoured", DecisionSettings{MaxRetries: &three}, 3},
		{"a negative is treated as none", DecisionSettings{MaxRetries: &negative}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := retriesOf(c.cfg); got != c.want {
				t.Fatalf("retriesOf = %d, want %d", got, c.want)
			}
		})
	}
}

func TestRetryBudgetFromTheProfile(t *testing.T) {
	serve := func(calls *int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(calls, 1)
			http.Error(w, "overloaded", 529)
		}))
	}
	t.Run("zero retries means exactly one call", func(t *testing.T) {
		var calls int32
		srv := serve(&calls)
		defer srv.Close()
		none := 0
		_, err := callDecision(context.Background(), DecisionSettings{AccessToken: "k", URL: srv.URL, MaxRetries: &none}, apiRequest{}, nil)
		if err == nil {
			t.Fatal("expected error")
		}
		if calls != 1 {
			t.Fatalf("calls = %d, want 1 — a profile that turned retrying off must not retry", calls)
		}
	})
	t.Run("a raised budget is used", func(t *testing.T) {
		var calls int32
		srv := serve(&calls)
		defer srv.Close()
		four := 4
		_, _ = callDecision(context.Background(), DecisionSettings{AccessToken: "k", URL: srv.URL, MaxRetries: &four}, apiRequest{}, nil)
		if calls != 5 {
			t.Fatalf("calls = %d, want 5 (one call plus 4 retries)", calls)
		}
	})
}

// A 429 may carry Retry-After, in either documented form. The service's ask
// wins over the backoff, but only up to maxBackoff — a decision node that
// parks a flow for minutes is not helping it.
func TestRetryAfterAndWait(t *testing.T) {
	hdr := func(v string) http.Header { return http.Header{"Retry-After": []string{v}} }
	if got := retryAfter(hdr("3")); got != 3*time.Second {
		t.Fatalf("seconds form = %v", got)
	}
	if got := retryAfter(hdr("")); got != 0 {
		t.Fatalf("absent = %v", got)
	}
	if got := retryAfter(hdr("-5")); got != 0 {
		t.Fatalf("negative = %v", got)
	}
	if got := retryAfter(hdr("not-a-date")); got != 0 {
		t.Fatalf("garbage = %v", got)
	}
	if got := retryAfter(hdr(time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat))); got != 0 {
		t.Fatalf("a date in the past = %v", got)
	}
	if got := retryAfter(hdr(time.Now().Add(4 * time.Second).UTC().Format(http.TimeFormat))); got <= 0 || got > 5*time.Second {
		t.Fatalf("date form = %v", got)
	}

	if got := waitFor(1, 0); got != retryBackoff {
		t.Fatalf("no hint, first retry = %v", got)
	}
	if got := waitFor(3, 0); got != 4*retryBackoff {
		t.Fatalf("no hint, third retry = %v", got)
	}
	if got := waitFor(1, 2*time.Second); got != 2*time.Second {
		t.Fatalf("hint should win = %v", got)
	}
	if got := waitFor(1, time.Hour); got != maxBackoff {
		t.Fatalf("hint must be capped = %v", got)
	}
	if got := waitFor(99, 0); got != maxBackoff {
		t.Fatalf("backoff must be capped = %v", got)
	}
}
