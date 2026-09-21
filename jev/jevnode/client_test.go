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
			_, _ = w.Write([]byte(`{"model":"jev","answers":{}}`))
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
