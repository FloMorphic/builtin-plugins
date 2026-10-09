package llmnode

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
)

// The run is declared cancelable so a stopped flow does not keep paying for a
// model call nobody will read. These check the half of that the node owns: the
// ctx the SDK hands runHandler actually reaches the provider, both while a call
// is in flight and while a failed one waits to be retried.

// A provider that accepts the request and never answers — the stalled call a
// stop has to cut short.
func TestStopAbortsTheModelCallInFlight(t *testing.T) {
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
	time.AfterFunc(200*time.Millisecond, stop) // the flow is stopped mid-call

	retries := 0
	cfg := LLMSettings{
		Provider: "openai-compatible", URL: server.URL, Model: "m", AccessToken: "k",
		RequestTimeoutS: 60, MaxRetries: &retries,
	}
	start := time.Now()
	_, err := streamChat(ctx, sdkv1.Job{}, cfg, []ChatMessage{{Role: "user", Content: "hi"}}, nil)

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
	// Not asserted: errors.Is(err, context.Canceled). langchaingo reports a
	// cancelled call as "request cancelled" without wrapping it — which is why
	// runHandler asks ctx.Err() whether it was stopped, never the error.
	select {
	case <-aborted:
	case <-time.After(2 * time.Second):
		t.Error("the provider never saw the request abandoned")
	}
}

// A transient failure starts a retry backoff of a second or more; a stop during
// it must end the run now, not after the wait and another attempt.
func TestStopEndsTheRetryBackoff(t *testing.T) {
	model := &flaky{failures: 10, err: io.ErrUnexpectedEOF} // transient: retried

	ctx, stop := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, stop)

	start := time.Now()
	_, err := callModel(ctx, model, nil, nil, time.Minute, 5, nil)

	if err == nil {
		t.Fatal("a stopped run reported success")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("err = %v, want the failure that was being retried", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("the backoff ran %s past the stop", took)
	}
	if calls := model.calls.Load(); calls != 1 {
		t.Errorf("made %d calls, want 1: no attempt after the stop", calls)
	}
}
