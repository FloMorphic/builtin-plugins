package jevnode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bytedance/sonic"
)

func TestCallJev(t *testing.T) {
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

		cfg := JevSettings{AccessToken: "k3y", URL: srv.URL + "/"}
		q := Question{ID: "category", Type: typeChoice, Instructions: "which team?", Options: []Option{{Name: "billing", Description: "Payments"}, {Name: "sales"}}}
		resp, err := callJev(context.Background(), cfg, buildRequest(cfg, "duplicate charge", []Question{q}))
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
		_, err := callJev(context.Background(), JevSettings{AccessToken: "k", URL: srv.URL}, apiRequest{})
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
		_, err := callJev(context.Background(), JevSettings{AccessToken: "bad", URL: srv.URL}, apiRequest{})
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
		_, err := callJev(context.Background(), JevSettings{AccessToken: "k", URL: srv.URL}, apiRequest{})
		if err == nil {
			t.Fatal("expected error")
		}
		if calls != maxAttempts {
			t.Fatalf("calls = %d, want %d", calls, maxAttempts)
		}
	})
}

// The live service wraps the answer document in {code, message, data:{result,
// creditsUsed}} while the published reference shows it flat. Both have to land
// on the same normalized reply, and a non-zero `code` — a failure the service
// reports with HTTP 200 — must not pass as an answer.
func TestCallJevEnvelope(t *testing.T) {
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
		got, err := callJev(context.Background(), JevSettings{AccessToken: "k", URL: srv.URL}, apiRequest{})
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
		got, err := callJev(context.Background(), JevSettings{AccessToken: "k", URL: srv.URL}, apiRequest{})
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
		_, err := callJev(context.Background(), JevSettings{AccessToken: "k", URL: srv.URL}, apiRequest{})
		if err == nil || !strings.Contains(err.Error(), "insufficient credits") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("an empty answer set is an error, not a silent pass", func(t *testing.T) {
		srv := serve(`{"code":0,"message":"ok","data":{"result":{"answers":{}},"creditsUsed":0}}`)
		defer srv.Close()
		if _, err := callJev(context.Background(), JevSettings{AccessToken: "k", URL: srv.URL}, apiRequest{}); err == nil {
			t.Fatal("expected error")
		}
	})
}

// The default endpoint is the host that actually serves the API, and a profile
// URL overrides it.
func TestBaseURL(t *testing.T) {
	if got := baseURL(JevSettings{}); got != "https://thejevai.com" {
		t.Errorf("default baseURL = %q", got)
	}
	if got := baseURL(JevSettings{URL: "https://proxy.internal/"}); got != "https://proxy.internal" {
		t.Errorf("override baseURL = %q", got)
	}
}
