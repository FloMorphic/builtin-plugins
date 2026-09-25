package httpnode

import (
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Retrying an HTTP call is not like retrying a model call, and the difference
// is the whole design of this file.
//
// A model call has no side effect: sending it twice costs tokens and nothing
// else. An HTTP request may create an order, charge a card or send a message,
// and a node that quietly sends it again has done that twice. So the question
// is never merely "did this fail" but "could it have succeeded on the server
// before it failed here".
//
// That splits by method. GET, HEAD, OPTIONS, TRACE, PUT and DELETE are
// idempotent by definition — the same request applied twice leaves the same
// state — so they are retried on any transient failure. POST and PATCH are not,
// and are retried ONLY when the request provably never reached the server: a
// dial that was refused, a name that did not resolve. A 502 to a POST is not
// retried, because the gateway may be reporting a backend that already acted.
//
// A server asking for later is always honoured, whatever the method: 429 and
// 503 are the server's own statement that it did not process the request.
const (
	// DefaultMaxRetries is how many further attempts a failed request gets.
	DefaultMaxRetries = 2

	maxRetryBackoff = 20 * time.Second

	// maxRetryAfter caps what a server can make this node wait. A Retry-After
	// of an hour is not something a flow should sit through.
	maxRetryAfter = 60 * time.Second
)

// send performs the request, retrying when it is both safe and worthwhile.
func send(client *http.Client, request *http.Request, retries int, notify func(string)) (*http.Response, error) {
	if retries < 0 {
		retries = 0
	}
	idempotent := isIdempotent(request.Method)

	var last error
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			// A retried request needs its body back; without GetBody it has
			// already been consumed and resending would send nothing.
			if request.Body != nil {
				if request.GetBody == nil {
					return nil, last
				}
				body, err := request.GetBody()
				if err != nil {
					return nil, last
				}
				request.Body = body
			}
		}

		response, err := client.Do(request)

		switch {
		case err == nil && !retryableStatus(response.StatusCode, idempotent):
			return response, nil

		case err == nil:
			// Out of budget. The status is still the SERVER'S ANSWER, so it is
			// handed back to be routed like any other: the node reports
			// `status` and `ok` and the flow branches on them. A 503 that ran
			// out of attempts must arrive the same way a 404 does, and the same
			// way the 502 that was never retried at all does — retrying is a
			// thing that happens on the way to the answer, not a new failure
			// mode that hides the answer and the body behind a node error.
			if attempt >= retries {
				return response, nil
			}

			// Retrying, so this response is thrown away: drain and close it
			// first, or the connection cannot be reused for the next attempt.
			wait := retryAfter(response)
			io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
			response.Body.Close()
			last = fmt.Errorf("%s", response.Status)
			if wait <= 0 {
				wait = backoff(attempt + 1)
			}
			if notify != nil {
				notify(fmt.Sprintf("%s — retrying in %s (attempt %d of %d)",
					response.Status, wait.Round(time.Millisecond), attempt+2, retries+1))
			}
			time.Sleep(wait)

		default:
			last = err
			if !retryableError(err, idempotent) || attempt >= retries {
				return nil, err
			}
			wait := backoff(attempt + 1)
			if notify != nil {
				notify(fmt.Sprintf("%s — retrying in %s (attempt %d of %d)",
					err, wait.Round(time.Millisecond), attempt+2, retries+1))
			}
			time.Sleep(wait)
		}
	}
}

// isIdempotent reports whether sending the request twice is defined to leave
// the same state as sending it once (RFC 9110 §9.2.2).
func isIdempotent(method string) bool {
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead, http.MethodOptions,
		http.MethodTrace, http.MethodPut, http.MethodDelete:
		return true
	default:
		return false
	}
}

// retryableStatus decides whether a status is worth another attempt.
//
// 429 and 503 are the server saying it did not process this and to come back —
// safe for any method. A 5xx is retried only for idempotent methods: a gateway
// error does not tell you whether the backend behind it already acted.
func retryableStatus(status int, idempotent bool) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return true
	}
	return idempotent && status >= 500
}

// retryableError decides whether a transport failure is worth another attempt.
//
// For an idempotent method, any of them. For POST or PATCH, only the failures
// that prove the request never arrived — a refused connection, a name that did
// not resolve. A timeout or a reset mid-flight is deliberately NOT retried
// there: the server may have received and acted on it, and sending it again
// would do the thing twice.
func retryableError(err error, idempotent bool) bool {
	var dnsErr *net.DNSError
	neverArrived := errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) ||
		errors.As(err, &dnsErr)

	if !idempotent {
		return neverArrived
	}
	if neverArrived {
		return true
	}
	switch {
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF),
		errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE),
		errors.Is(err, syscall.ETIMEDOUT):
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// retryAfter reads the server's own instruction, in either of its forms, capped
// so a server cannot park a flow indefinitely.
func retryAfter(response *http.Response) time.Duration {
	header := strings.TrimSpace(response.Header.Get("Retry-After"))
	if header == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(header); err == nil && seconds >= 0 {
		return capWait(time.Duration(seconds) * time.Second)
	}
	if when, err := http.ParseTime(header); err == nil {
		if wait := time.Until(when); wait > 0 {
			return capWait(wait)
		}
	}
	return 0
}

func capWait(wait time.Duration) time.Duration {
	if wait > maxRetryAfter {
		return maxRetryAfter
	}
	return wait
}

// backoff doubles the pause per attempt, with jitter so that every node calling
// one failing service does not return in the same instant.
func backoff(attempt int) time.Duration {
	wait := 500 * time.Millisecond << uint(attempt-1)
	if wait > maxRetryBackoff || wait <= 0 {
		wait = maxRetryBackoff
	}
	spread := wait / 5
	if spread <= 0 {
		return wait
	}
	return wait - spread/2 + time.Duration(rand.Int64N(int64(spread)))
}

// retriesOf reads the knob off the settings profile. Absent takes the default;
// an explicit zero turns retrying off.
func retriesOf(s HTTPSettings) int {
	if s.MaxRetries == nil {
		return DefaultMaxRetries
	}
	if *s.MaxRetries < 0 {
		return 0
	}
	return *s.MaxRetries
}
