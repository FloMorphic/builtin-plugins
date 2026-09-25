package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tmc/langchaingo/llms"
)

// A model call is the one step of a node that can fail for reasons nothing here
// controls, and it fails in a way that is easy to mistake for a hang.
//
// Two things go wrong in practice. A provider stalls: the request is accepted
// and then nothing comes back, with no status and no error, until something
// upstream gives up — a real endpoint was measured taking 1m52s to its first
// token, and another request to the same endpoint hung past fifteen minutes.
// And a provider has a bad minute: a 502 or a 429 that would have succeeded a
// second later. Without a deadline the first wedges the node indefinitely;
// without a retry the second throws away every turn of work before it.
const (
	// DefaultRequestTimeout bounds ONE model call.
	//
	// Both bounds are measured. Below: a first token has been seen at 1m52s, so
	// two minutes cuts off calls that were about to succeed. Above: the idle
	// timeout in front of a model is commonly around 220 seconds, and a
	// deadline longer than that never fires — the intermediary answers first
	// with a 502, which is neither a clean timeout nor retried the same way.
	DefaultRequestTimeout = 3 * time.Minute

	// DefaultMaxRetries is how many further attempts a failed call gets.
	DefaultMaxRetries = 3

	// maxBackoff caps the wait between attempts. A provider having a bad minute
	// is worth waiting out; one having a bad hour is not, and the node should
	// fail and say so while the flow can still react.
	maxBackoff = 30 * time.Second
)

// CallModel makes one model call, bounded by a deadline and tried again when
// the failure is the kind a second attempt can fix.
//
// The conversation is not touched between attempts: retrying the TURN costs one
// call, while failing the node costs every turn before it. `notify` is called
// before each retry so the canvas shows the wait rather than going quiet.
func CallModel(
	ctx context.Context,
	model llms.Model,
	messages []llms.MessageContent,
	options []llms.CallOption,
	timeout time.Duration,
	retries int,
	notify func(string),
) (*llms.ContentResponse, error) {
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}
	if retries < 0 {
		retries = 0
	}

	var (
		last     error
		attempts int
	)

	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			wait := backoff(attempt)
			if notify != nil {
				notify(fmt.Sprintf("retrying in %s (attempt %d of %d) — %s",
					wait.Round(time.Millisecond), attempt+1, retries+1, oneLine(last.Error())))
			}
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return nil, fmt.Errorf("the model call failed and the node was cancelled before it could be retried: %w", last)
			}
		}

		// Each attempt gets its own deadline. A stalled provider is cut off
		// here rather than holding the node until something upstream decides to
		// hang up, which is what turns a bad minute into a lost run.
		attempts++
		call, cancel := context.WithTimeout(ctx, timeout)
		response, err := model.GenerateContent(call, messages, options...)
		stalled := call.Err() != nil && ctx.Err() == nil
		cancel()

		switch {
		case err == nil && len(response.Choices) > 0:
			return response, nil
		case err == nil:
			// A provider that answers with no choices has failed, it has just
			// not said so. Treated as retryable: it is the shape of a truncated
			// response, not of a rejected request.
			err = fmt.Errorf("the provider returned no choices")
		case stalled:
			err = fmt.Errorf("the model did not answer within %s", timeout)
		}
		last = err

		if ctx.Err() != nil {
			return nil, err
		}

		budget := retries
		switch classify(err, stalled) {
		case fatal:
			return nil, err
		case unclassified:
			// One more attempt, not the whole budget: enough to ride out
			// something momentary, little enough that a deterministic failure
			// nobody has classified costs one wasted call and stays visible as
			// itself rather than as a retry count.
			if budget > 1 {
				budget = 1
			}
		}
		if attempt >= budget {
			break
		}
	}

	if attempts > 1 {
		return nil, fmt.Errorf("the model call failed %d times, last: %w", attempts, last)
	}
	return nil, last
}

// verdict is what a failed model call earns.
type verdict int

const (
	fatal        verdict = iota // a refusal: the same bytes get the same answer
	transient                   // the provider or the network having a moment
	unclassified                // everything else
)

// classify reads structure rather than wording. Error text belongs to the
// provider and differs between them — langchaingo's OpenAI and Anthropic paths
// phrase a bad status one way, Google's SDK another, and a transport failure is
// worded by the kernel. Matching on literals gives a classifier that is right
// for whichever provider it was written against and quietly wrong for the next.
func classify(err error, stalled bool) verdict {
	if stalled {
		return transient
	}
	if errors.Is(err, context.Canceled) {
		return fatal
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return transient
	}

	// The connection failed or died mid-flight. None of these say anything
	// about whether the request was acceptable.
	switch {
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF),
		errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.ECONNREFUSED),
		errors.Is(err, syscall.ECONNABORTED), errors.Is(err, syscall.EPIPE),
		errors.Is(err, syscall.ETIMEDOUT), errors.Is(err, syscall.EHOSTUNREACH),
		errors.Is(err, syscall.ENETUNREACH):
		return transient
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return transient
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		// A name that does not resolve is usually a typo in the profile's URL;
		// a resolver that is merely unreachable is not.
		if dnsErr.IsTemporary || dnsErr.IsTimeout {
			return transient
		}
		return fatal
	}

	// The provider answered with a status. Classify by CLASS: 5xx is the server
	// failing and worth another go; 4xx is a verdict on the request, except the
	// few that explicitly mean "later".
	if status, ok := statusOf(err); ok {
		switch {
		case status >= 500:
			return transient
		case status == http.StatusRequestTimeout, // 408
			status == http.StatusConflict,        // 409
			status == http.StatusTooEarly,        // 425
			status == http.StatusTooManyRequests: // 429
			return transient
		case status >= 400:
			return fatal
		}
	}

	// Built wrong on this side: the request never left the process, and the
	// same bytes will be rejected identically every time.
	if strings.Contains(strings.ToLower(err.Error()), "expected part of type") {
		return fatal
	}
	return unclassified
}

// statusPatterns are how the providers these nodes drive report an HTTP status
// inside an error message. Each is anchored on its own wording so a stray
// three-digit number elsewhere cannot be mistaken for a status.
var statusPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)status code:\s*(\d{3})`), // langchaingo: OpenAI-compatible and Anthropic
	regexp.MustCompile(`(?i)googleapi: Error (\d{3})`),
	regexp.MustCompile(`(?i)\bHTTP (\d{3})\b`),
}

func statusOf(err error) (int, bool) {
	text := err.Error()
	for _, pattern := range statusPatterns {
		if match := pattern.FindStringSubmatch(text); match != nil {
			if status, convErr := strconv.Atoi(match[1]); convErr == nil && status >= 100 && status < 600 {
				return status, true
			}
		}
	}
	return 0, false
}

// backoff doubles the pause per attempt, with jitter. The jitter is not
// decoration: a provider outage fails every node addressing it at once, and
// without it they would all come back in the same instant and fail together.
func backoff(attempt int) time.Duration {
	wait := time.Second << uint(attempt-1)
	if wait > maxBackoff || wait <= 0 {
		wait = maxBackoff
	}
	spread := wait / 5
	if spread <= 0 {
		return wait
	}
	return wait - spread/2 + time.Duration(rand.Int64N(int64(spread)))
}

// FirstJSONValue trims a tool call's arguments back to the first complete JSON
// value in them, reporting whether anything had to go.
//
// Streamed tool calls can arrive corrupted, and the corruption has a consistent
// shape: a valid object with more welded onto the end, such as
//
//	{"depth": 2, "path": "legate-work"}-work"}
//
// which is what a streamed answer looks like when two PARALLEL tool calls have
// their fragments appended to the same call. langchaingo's streamed ToolCall
// type has no Index field, so the index saying which call a fragment belongs to
// is discarded at decode time and every fragment lands on the last call seen.
//
// The damage is not confined to the call that carries it. The raw text is
// recorded in the assistant turn and sent back on the NEXT request, where the
// provider fails to parse the conversation and rejects the whole thing with a
// 400 — one malformed tool call costing the entire run. Trimming keeps the cost
// at one call: the truncated arguments are usually the ones the model meant, so
// the call tends to work, and when it does not the tool refuses it and the
// model is told, which is an ordinary turn.
func FirstJSONValue(arguments string) (string, bool) {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return arguments, false
	}
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	var value json.RawMessage
	if err := decoder.Decode(&value); err != nil {
		// Not JSON at all. An empty object is refused cleanly by the tool,
		// which the model can read and act on; the original text would not
		// survive the next request.
		return "{}", true
	}
	if end := decoder.InputOffset(); strings.TrimSpace(trimmed[end:]) != "" {
		return trimmed[:end], true
	}
	return arguments, false
}

func oneLine(text string) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
	const limit = 120
	if len(text) > limit {
		return text[:limit] + "…"
	}
	return text
}

// TimeoutOf and RetriesOf read the two knobs off a settings profile, applying
// the defaults when a profile does not mention them — which most will not.
func TimeoutOf(cfg LLMSettings) time.Duration {
	if cfg.RequestTimeoutS > 0 {
		return time.Duration(cfg.RequestTimeoutS) * time.Second
	}
	return DefaultRequestTimeout
}

func RetriesOf(cfg LLMSettings) int {
	if cfg.MaxRetries == nil {
		return DefaultMaxRetries
	}
	if *cfg.MaxRetries < 0 {
		return 0
	}
	return *cfg.MaxRetries
}
