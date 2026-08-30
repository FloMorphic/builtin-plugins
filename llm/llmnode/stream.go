package llmnode

import (
	"strings"
	"time"

	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
)

// Streaming providers differ wildly in how finely they chop a response. Gemini
// hands us coarse text spans; OpenAI (and other token-stream providers) emit one
// delta per token, so a single long turn can arrive as 1000–2000+ chunks. Mapping
// one chunk to one job.Progress would fire that many NATS sends and blow past the
// runtime's per-job send threshold.
//
// frameBatcher decouples our send rate from the provider's chunk granularity. It
// keeps accumulating chunks into the live completion and only emits a progress
// frame when a batch is "ready": enough new text has piled up AND it ends on a
// word boundary, or a hard character cap is hit (so a boundary-less token run
// still flushes), or a minimum time gap has elapsed (so a slow stream stays live).
// Whatever the token rate, sends are bounded by both the character cap and the
// time floor.
//
// The langchaingo StreamingFunc is invoked inline and sequentially per request,
// so no locking is needed: Add runs one call at a time, and Flush only after the
// stream returns.
type frameBatcher struct {
	job   sdkv1.Job
	title string

	// tuning knobs (see newFrameBatcher for the defaults)
	softChars int           // flush once this many new chars buffered AND on a word boundary
	hardChars int           // flush unconditionally once this many new chars buffered
	minGap    time.Duration // never send two frames closer together than this

	full     strings.Builder // the whole completion so far (what each frame shows)
	pending  int             // chars accumulated since the last emitted frame
	lastSend time.Time       // when the last frame went out
	percent  int             // ramps toward, but never reaches, 100
}

func newFrameBatcher(job sdkv1.Job, title string) *frameBatcher {
	return &frameBatcher{
		job:       job,
		title:     title,
		softChars: 150,
		hardChars: 450,
		minGap:    80 * time.Millisecond,
		percent:   25, // start of the streaming window
	}
}

// Add appends a streamed chunk and emits a frame only when a batch is ready.
func (b *frameBatcher) Add(chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	b.full.Write(chunk)
	b.pending += len(chunk)

	if b.ready(chunk) {
		b.emit()
	}
}

// ready reports whether the buffered text should be sent now.
func (b *frameBatcher) ready(lastChunk []byte) bool {
	if b.pending == 0 {
		return false
	}
	// Hard cap: flush a long boundary-less run regardless of everything else.
	if b.pending >= b.hardChars {
		return true
	}
	// Time floor: keep the last frame from being too recent. A slow token stream
	// (few chars, but seconds apart) still gets a frame this way.
	if !b.lastSend.IsZero() && time.Since(b.lastSend) < b.minGap {
		return false
	}
	if b.lastSend.IsZero() {
		// First token: let it show immediately so the node lights up.
		return true
	}
	// Enough new text and we're at a word boundary — send a whole-word batch.
	return b.pending >= b.softChars && endsOnBoundary(lastChunk)
}

// Flush emits any remainder so the live view shows the complete text before the
// caller finalizes the job. Safe to call when nothing is pending.
func (b *frameBatcher) Flush() {
	if b.pending > 0 {
		b.emit()
	}
}

func (b *frameBatcher) emit() {
	if b.percent < 95 {
		b.percent++
	}
	b.job.Progress(b.percent, sdkv1.Frame{
		Title:   b.title,
		Content: b.full.String(),
	})
	b.pending = 0
	b.lastSend = time.Now()
}

// endsOnBoundary reports whether a chunk ends at a natural word break, so batches
// don't get cut mid-word.
func endsOnBoundary(chunk []byte) bool {
	r := chunk[len(chunk)-1]
	switch r {
	case ' ', '\t', '\n', '\r', '.', ',', ';', ':', '!', '?', ')', ']', '}', '"', '\'':
		return true
	}
	return false
}
