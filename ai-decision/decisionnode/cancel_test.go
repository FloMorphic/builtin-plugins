package decisionnode

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// The run is declared cancelable so a stopped flow does not keep paying for a
// decision nobody will route on. These check the half of that the node owns: the
// ctx the SDK hands runHandler actually reaches the decider, both while a call is
// in flight and while a failed one waits to be retried.

// A decider that accepts the request and never answers — the stalled call a stop
// has to cut short.
func TestStopAbortsTheDecisionInFlight(t *testing.T) {
	aborted := make(chan struct{})
	finished := make(chan struct{})
	defer close(finished) // runs before srv.Close, so the handler never pins it
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The server only watches for the client hanging up once the request
		// body has been read, so read it first.
		io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done(): // the client hung up
			close(aborted)
		case <-finished:
		}
	}))
	defer srv.Close()

	ctx, stop := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, stop) // the flow is stopped mid-call

	retries := 0
	cfg := DecisionSettings{URL: srv.URL, Model: "m", AccessToken: "k", TimeoutSeconds: 60, MaxRetries: &retries}
	d := systemOne{}
	q := Question{ID: "q", Type: typeNoul, Instructions: "well?"}

	start := time.Now()
	_, err := callDecision(ctx, d, cfg, d.buildRequest(identity, modelOf(d, cfg), "state", []Question{q}), nil)
	took := time.Since(start)

	if err == nil {
		t.Fatal("a stopped call reported success")
	}
	if took < 150*time.Millisecond {
		t.Fatalf("the call failed before the stop, so the stop is not what ended it: %v", err)
	}
	if took > 5*time.Second {
		t.Errorf("the call ran %s past the stop; the 60s request timeout cut it, not the stop", took)
	}
	select {
	case <-aborted:
	case <-time.After(2 * time.Second):
		t.Error("the decider never saw the request abandoned")
	}
}

// A 429 with a Retry-After parks the node for seconds; a stop during that wait
// must end the run now, not after the wait and another attempt.
func TestStopEndsTheRetryBackoff(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Retry-After", "5")
		http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, stop := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, stop)

	retries := 3
	cfg := DecisionSettings{URL: srv.URL, Model: "m", AccessToken: "k", MaxRetries: &retries}
	d := systemOne{}
	q := Question{ID: "q", Type: typeNoul, Instructions: "well?"}

	start := time.Now()
	_, err := callDecision(ctx, d, cfg, d.buildRequest(identity, modelOf(d, cfg), "state", []Question{q}), nil)

	if err == nil {
		t.Fatal("a stopped run reported success")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the backoff ran %s past the stop", took)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("made %d calls, want 1: no attempt after the stop", n)
	}
}
