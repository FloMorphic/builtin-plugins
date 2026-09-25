package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/tmc/langchaingo/llms"
)

// flaky is a provider that fails a set number of times and then answers, or
// stalls until its context expires — the shape of the failure this exists for:
// nothing is wrong with the request, the endpoint is having a moment.
type flaky struct {
	failures int32
	err      error
	stallFor time.Duration
	calls    atomic.Int32
}

func (f *flaky) GenerateContent(ctx context.Context, _ []llms.MessageContent, _ ...llms.CallOption) (*llms.ContentResponse, error) {
	if f.calls.Add(1) <= f.failures {
		if f.stallFor > 0 {
			select {
			case <-time.After(f.stallFor):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if f.err != nil {
			return nil, f.err
		}
	}
	return &llms.ContentResponse{Choices: []*llms.ContentChoice{{Content: "done"}}}, nil
}

func (f *flaky) Call(context.Context, string, ...llms.CallOption) (string, error) { return "", nil }

func try(model llms.Model, retries int, timeout time.Duration) (*llms.ContentResponse, error) {
	return CallModel(context.Background(), model, nil, nil, timeout, retries, nil)
}

func TestATransientFailureIsRetried(t *testing.T) {
	model := &flaky{failures: 2, err: errors.New("API returned unexpected status code: 502")}

	if _, err := try(model, 3, time.Second); err != nil {
		t.Fatalf("the call failed despite retries being available: %v", err)
	}
	if got := model.calls.Load(); got != 3 {
		t.Errorf("provider was called %d times, want 3", got)
	}
}

// The failure this plugin is most exposed to: the endpoint accepts the request
// and then never answers. Without a deadline it holds the node indefinitely.
func TestAStalledProviderIsCutOffAndRetried(t *testing.T) {
	model := &flaky{failures: 1, stallFor: time.Hour}

	start := time.Now()
	if _, err := try(model, 3, 50*time.Millisecond); err != nil {
		t.Fatalf("a stalled call was not recovered: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("the stalled call was not cut off promptly: took %s", elapsed)
	}
}

func TestRetriesAreBounded(t *testing.T) {
	model := &flaky{failures: 99, err: errors.New("API returned unexpected status code: 503")}

	if _, err := try(model, 2, time.Second); err == nil {
		t.Fatal("a permanently failing provider was reported as success")
	}
	if got := model.calls.Load(); got != 3 {
		t.Errorf("provider was called %d times, want 3 (the first plus two retries)", got)
	}
}

// A refusal is a verdict, not a hiccup: retrying wastes calls and buries the
// real problem under a retry count.
func TestRefusalsAreNotRetried(t *testing.T) {
	for _, refusal := range []string{
		"API returned unexpected status code: 400: invalid model",
		"API returned unexpected status code: 401",
		"googleapi: Error 403: permission denied",
		"API returned unexpected status code: 404",
	} {
		model := &flaky{failures: 99, err: errors.New(refusal)}
		if _, err := try(model, 3, time.Second); err == nil {
			t.Fatalf("%s: expected failure", refusal)
		}
		if got := model.calls.Load(); got != 1 {
			t.Errorf("%s was attempted %d times, want 1", refusal, got)
		}
	}
}

// Classification reads structure, not wording — error text belongs to the
// provider and differs between them.
func TestClassifyReadsStructureNotWording(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want verdict
	}{
		{"our own deadline", context.DeadlineExceeded, transient},
		{"cancelled", context.Canceled, fatal},
		{"connection reset", syscall.ECONNRESET, transient},
		{"stream ended", io.ErrUnexpectedEOF, transient},
		{"wrapped deep", fmt.Errorf("call: %w", fmt.Errorf("dial: %w", syscall.ECONNREFUSED)), transient},
		{"openai-compatible 502", errors.New("API returned unexpected status code: 502"), transient},
		{"anthropic 529", errors.New("API returned unexpected status code: 529"), transient},
		{"google 503", errors.New("googleapi: Error 503: backend unavailable"), transient},
		{"rate limited", errors.New("API returned unexpected status code: 429"), transient},
		{"bad request", errors.New("API returned unexpected status code: 400"), fatal},
		{"google bad key", errors.New("googleapi: Error 403: denied"), fatal},
		{"built wrong here", errors.New("expected part of type ToolCallResponse for role tool"), fatal},
		{"something new", errors.New("the flux capacitor is misaligned"), unclassified},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := classify(test.err, false); got != test.want {
				t.Errorf("classify(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
	if classify(errors.New("anything"), true) != transient {
		t.Error("a stalled call must be transient")
	}
	var dnsErr *net.DNSError
	_ = dnsErr
}

// A three-digit number that is not a status must not be read as one.
func TestStatusIsNotGuessedFromStrayNumbers(t *testing.T) {
	for _, text := range []string{
		"model gpt-404 is deprecated",
		"read 500 bytes before the stream ended",
	} {
		if status, ok := statusOf(errors.New(text)); ok {
			t.Errorf("read a status of %d out of %q", status, text)
		}
	}
	if status, ok := statusOf(errors.New("API returned unexpected status code: 502")); !ok || status != 502 {
		t.Errorf("got %d, %v", status, ok)
	}
}

// The corruption seen in the field, and why it matters: unparseable arguments
// do not merely fail their own call — they are recorded in the conversation and
// rejected by the provider on the NEXT request, taking the whole run with them.
func TestFirstJSONValueTrimsWeldedArguments(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		changed bool
	}{
		{`{"depth": 2, "path": "legate-work"}-work"}`, `{"depth": 2, "path": "legate-work"}`, true},
		{`{"path": "."}{"path": "go.mod"}`, `{"path": "."}`, true},
		{`{"command": "cd /tmp && ls"}`, `{"command": "cd /tmp && ls"}`, false},
		{"", "", false},
		{`cd /tmp && ls`, `{}`, true},
		{`{"path": "unter`, `{}`, true},
	}
	for _, test := range cases {
		got, changed := FirstJSONValue(test.in)
		if got != test.want || changed != test.changed {
			t.Errorf("FirstJSONValue(%q) = %q,%v — want %q,%v", test.in, got, changed, test.want, test.changed)
		}
	}
}

// The defaults are measured, not guessed, and bounded on both sides.
func TestDefaultsSitWhereTheEvidencePutThem(t *testing.T) {
	// A first token has been seen at 1m52s, so the deadline must clear two
	// minutes; and an upstream idle timeout is commonly ~220s, past which our
	// deadline never fires because the intermediary answers first.
	if DefaultRequestTimeout <= 2*time.Minute || DefaultRequestTimeout >= 220*time.Second {
		t.Errorf("DefaultRequestTimeout = %s is outside the measured window", DefaultRequestTimeout)
	}
	if DefaultMaxRetries < 1 {
		t.Errorf("DefaultMaxRetries = %d", DefaultMaxRetries)
	}
}
