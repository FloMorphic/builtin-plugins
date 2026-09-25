package httpnode

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// counting serves a scripted sequence of statuses and records how many requests
// it actually received — which is the number that matters when the question is
// whether a side effect happened twice.
func counting(t *testing.T, statuses ...int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var seen atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		n := int(seen.Add(1))
		status := statuses[len(statuses)-1]
		if n <= len(statuses) {
			status = statuses[n-1]
		}
		w.WriteHeader(status)
		w.Write([]byte("body"))
	}))
	t.Cleanup(server.Close)
	return server, &seen
}

func request(t *testing.T, method, url, body string) *http.Request {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// An idempotent method is safe to send again, so a 502 is worth another go.
func TestIdempotentRequestsRetryOn5xx(t *testing.T) {
	server, seen := counting(t, 502, 502, 200)

	response, err := send(server.Client(), request(t, http.MethodGet, server.URL, ""), 2, nil)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != 200 {
		t.Errorf("status = %d, want 200", response.StatusCode)
	}
	if got := seen.Load(); got != 3 {
		t.Errorf("server saw %d requests, want 3", got)
	}
}

// THE ONE THAT MATTERS. A POST may have created something before the gateway
// answered 502, so sending it again could create it twice. It must not be
// retried on a status — only the server's own "I did not process this".
func TestPostIsNotRetriedOnAServerError(t *testing.T) {
	server, seen := counting(t, 502, 200)

	// The 502 comes back as a RESPONSE, not an error — the flow branches on the
	// status. What must not happen is a second POST.
	response, err := send(server.Client(), request(t, http.MethodPost, server.URL, `{"charge":100}`), 3, nil)
	if err != nil {
		t.Fatalf("the status should be returned for the flow to read: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != 502 {
		t.Errorf("status = %d, want the 502 passed through", response.StatusCode)
	}
	if got := seen.Load(); got != 1 {
		t.Errorf("the server saw %d POSTs — a side effect may have happened twice", got)
	}
}

// 429 and 503 are the server stating it did NOT process the request, so they
// are safe to retry whatever the method.
func TestPostIsRetriedWhenTheServerSaysItDidNotProcessIt(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		server, seen := counting(t, status, 200)

		response, err := send(server.Client(), request(t, http.MethodPost, server.URL, `{"a":1}`), 2, nil)
		if err != nil {
			t.Fatalf("status %d: %v", status, err)
		}
		response.Body.Close()
		if got := seen.Load(); got != 2 {
			t.Errorf("status %d: server saw %d requests, want 2", status, got)
		}
	}
}

// A retried request must carry its body again, or the second attempt sends an
// empty one and the server sees a different request.
func TestTheBodyIsResentOnRetry(t *testing.T) {
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		if len(bodies) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	response, err := send(server.Client(), request(t, http.MethodPut, server.URL, `{"name":"x"}`), 2, nil)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	response.Body.Close()

	if len(bodies) != 2 {
		t.Fatalf("server saw %d requests", len(bodies))
	}
	if bodies[0] != bodies[1] {
		t.Errorf("the retry sent a different body: %q then %q", bodies[0], bodies[1])
	}
}

// A refusal is a verdict on the request. Retrying it wastes calls and buries
// the real problem under a retry count.
func TestClientErrorsAreNotRetried(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 422} {
		server, seen := counting(t, status)
		response, err := send(server.Client(), request(t, http.MethodGet, server.URL, ""), 3, nil)
		if err != nil {
			t.Fatalf("status %d should be returned, not raised: %v", status, err)
		}
		response.Body.Close()
		if got := seen.Load(); got != 1 {
			t.Errorf("status %d was attempted %d times, want 1", status, got)
		}
	}
}

// Transport failures split the same way: a POST is retried only when the
// request provably never arrived.
func TestTransportFailuresRespectIdempotence(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		idempotent bool
		want       bool
	}{
		{"GET, connection refused", syscall.ECONNREFUSED, true, true},
		{"GET, reset mid-flight", syscall.ECONNRESET, true, true},
		{"POST, connection refused — never arrived", syscall.ECONNREFUSED, false, true},
		{"POST, reset mid-flight — may have been processed", syscall.ECONNRESET, false, false},
		{"POST, timed out — may have been processed", syscall.ETIMEDOUT, false, false},
		{"POST, name did not resolve", &net.DNSError{Err: "no such host"}, false, true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := retryableError(test.err, test.idempotent); got != test.want {
				t.Errorf("retryableError(%v, idempotent=%v) = %v, want %v",
					test.err, test.idempotent, got, test.want)
			}
		})
	}
}

// A server that says how long to wait is obeyed, but cannot park a flow forever.
func TestRetryAfterIsHonouredAndCapped(t *testing.T) {
	response := &http.Response{Header: http.Header{}}

	response.Header.Set("Retry-After", "2")
	if got := retryAfter(response); got != 2*time.Second {
		t.Errorf("got %s, want 2s", got)
	}

	response.Header.Set("Retry-After", "86400")
	if got := retryAfter(response); got != maxRetryAfter {
		t.Errorf("an hour-long Retry-After was not capped: %s", got)
	}

	response.Header.Set("Retry-After", "not a number")
	if got := retryAfter(response); got != 0 {
		t.Errorf("garbage Retry-After produced %s", got)
	}
}

// Running out of attempts is not a new failure mode: the last status is the
// server's answer and the node routes it, exactly as it would a 404 or a 502 on
// a POST. A flow branching on `status` must not see a 503 disappear into a node
// error just because retrying was tried on the way.
func TestAnExhaustedRetryStillReturnsTheServersAnswer(t *testing.T) {
	server, seen := counting(t, 503, 503, 503, 503)

	response, err := send(server.Client(), request(t, http.MethodGet, server.URL, ""), 2, nil)
	if err != nil {
		t.Fatalf("the final status should be returned for the flow to read: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want the 503 passed through", response.StatusCode)
	}
	// The body has to survive too — it is what the node reports as `body`.
	if raw, _ := io.ReadAll(response.Body); string(raw) != "body" {
		t.Errorf("body = %q, want it intact", raw)
	}
	if got := seen.Load(); got != 3 {
		t.Errorf("server saw %d requests, want 3 (the first plus 2 retries)", got)
	}
}

func TestRetriesCanBeTurnedOff(t *testing.T) {
	server, seen := counting(t, 503, 200)
	response, err := send(server.Client(), request(t, http.MethodGet, server.URL, ""), 0, nil)
	if err != nil {
		t.Fatalf("the status should be returned, not raised: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want the 503 passed through untried", response.StatusCode)
	}
	if got := seen.Load(); got != 1 {
		t.Errorf("server saw %d requests with retries off, want 1", got)
	}
	none := 0
	if retriesOf(HTTPSettings{MaxRetries: &none}) != 0 {
		t.Error("an explicit 0 did not disable retrying")
	}
	if retriesOf(HTTPSettings{}) != DefaultMaxRetries {
		t.Error("an absent MaxRetries did not take the default")
	}
	five := 5
	if retriesOf(HTTPSettings{MaxRetries: &five}) != 5 {
		t.Error("an explicit count was not honoured")
	}
}

var _ = errors.Is
