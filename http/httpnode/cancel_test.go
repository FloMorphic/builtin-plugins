package httpnode

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The request is declared cancelable so a stopped flow does not hold the node on
// a call nobody will read. These check the half of that the node owns: the ctx
// the SDK hands runHandler reaches the transport through the request, and ends
// the retry backoff as well as the attempt in flight.

// An endpoint that accepts the request and never answers — the stalled call a
// stop has to cut short.
func TestStopAbortsTheRequestInFlight(t *testing.T) {
	aborted := make(chan struct{})
	finished := make(chan struct{})
	defer close(finished) // runs before server.Close, so the handler never pins it
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The server only watches for the client hanging up once the request
		// body has been read, so read it first.
		io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done(): // the client hung up
			close(aborted)
		case <-finished:
		}
	}))
	defer server.Close()

	ctx, stop := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, stop) // the flow is stopped mid-request

	req := request(t, http.MethodGet, server.URL, "")
	start := time.Now()
	_, err := send(server.Client(), req.WithContext(ctx), 2, nil)
	took := time.Since(start)

	if err == nil {
		t.Fatal("a stopped request reported success")
	}
	if took < 150*time.Millisecond {
		t.Fatalf("the request failed before the stop, so the stop is not what ended it: %v", err)
	}
	if took > 5*time.Second {
		t.Errorf("the request ran %s past the stop", took)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want a cancellation: that is what tells the handler not to report", err)
	}
	select {
	case <-aborted:
	case <-time.After(2 * time.Second):
		t.Error("the endpoint never saw the request abandoned")
	}
}

// A 503 with a Retry-After parks the node for seconds; a stop during that wait
// must end the run now, and must not send the request again.
func TestStopEndsTheRetryBackoff(t *testing.T) {
	var seen int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		seen++
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	ctx, stop := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, stop)

	req := request(t, http.MethodGet, server.URL, "")
	start := time.Now()
	response, err := send(server.Client(), req.WithContext(ctx), 3, nil)
	if response != nil {
		response.Body.Close()
	}

	if err == nil {
		t.Fatal("a stopped run reported success")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want a cancellation", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the backoff ran %s past the stop", took)
	}
	if seen != 1 {
		t.Errorf("made %d requests, want 1: no attempt after the stop", seen)
	}
}
