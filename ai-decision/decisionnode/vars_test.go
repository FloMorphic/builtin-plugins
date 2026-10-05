package decisionnode

import (
	"reflect"
	"strings"
	"testing"
)

// identity is the no-op resolver: the assembly steps are pure functions of a
// resolver, so a test can watch what they do to a template without infra.
func identity(s string) string { return s }

// upper stands in for a resolver with visible effect — every template string it
// touches comes back changed, which is how a test tells "resolved" from
// "passed through verbatim" at any depth.
func upper(s string) string { return strings.ToUpper(s) }

func TestResolveTemplateValue(t *testing.T) {
	t.Run("a string is resolved", func(t *testing.T) {
		if got := resolveTemplateValue(upper, "ask"); got != "ASK" {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("an object has its values resolved, not its keys", func(t *testing.T) {
		// The structured instructions form: the question in one field, the data
		// it cites by backtick in the others. Keys are the backtick handles, so
		// resolving them would rename what the question points at.
		in := map[string]any{
			"question": "does `policy` allow it?",
			"policy":   "thirty days notice",
			"depth":    map[string]any{"note": "strict"},
			"rows":     []any{"a", 2, true},
			"limit":    3.0,
		}
		want := map[string]any{
			"question": "DOES `POLICY` ALLOW IT?",
			"policy":   "THIRTY DAYS NOTICE",
			"depth":    map[string]any{"note": "STRICT"},
			"rows":     []any{"A", 2, true},
			"limit":    3.0,
		}
		if got := resolveTemplateValue(upper, in); !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v want %v", got, want)
		}
	})
	t.Run("the input is not mutated", func(t *testing.T) {
		in := map[string]any{"q": "ask"}
		resolveTemplateValue(upper, in)
		if in["q"] != "ask" {
			t.Fatalf("input mutated: %v", in)
		}
	})
}

func TestResolveEvidence(t *testing.T) {
	rows := []EvidenceItem{{Source: "contract.pdf", Text: "section 12"}, {Text: "section 44"}}
	got := resolveEvidence(upper, rows)
	want := []EvidenceItem{{Source: "CONTRACT.PDF", Text: "SECTION 12"}, {Text: "SECTION 44"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if resolveEvidence(upper, nil) != nil {
		t.Fatal("no rows should stay nil, so assembleState leaves the state alone")
	}
}

func TestAssembleState(t *testing.T) {
	ev := []EvidenceItem{{Source: "contract.pdf", Text: "section 12"}}

	t.Run("no evidence leaves the subject as the state", func(t *testing.T) {
		// The pre-evidence contract: a body written against the single-block
		// shape must reach the wire exactly as it did before.
		if got := assembleState("a ticket", nil); got != "a ticket" {
			t.Fatalf("got %v", got)
		}
		obj := map[string]any{"ticket": "x"}
		if got := assembleState(obj, nil); !reflect.DeepEqual(got, obj) {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("evidence folds the subject under case", func(t *testing.T) {
		got := assembleState(map[string]any{"problem": "early termination"}, ev)
		want := map[string]any{
			"case":     map[string]any{"problem": "early termination"},
			"evidence": ev,
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v want %v", got, want)
		}
	})
	t.Run("a blank subject is left out", func(t *testing.T) {
		got := assembleState("  ", ev)
		want := map[string]any{"evidence": ev}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v want %v", got, want)
		}
	})
	t.Run("evidence keeps its order, so index paths hold", func(t *testing.T) {
		rows := []EvidenceItem{{Text: "first"}, {Text: "second"}, {Text: "third"}}
		got := assembleState("", rows).(map[string]any)["evidence"].([]EvidenceItem)
		if got[0].Text != "first" || got[2].Text != "third" {
			t.Fatalf("order changed: %v", got)
		}
	})
}

func TestIsBlank(t *testing.T) {
	for _, c := range []struct {
		in   any
		want bool
	}{
		{nil, true},
		{"", true},
		{"   ", true},
		{"x", false},
		{map[string]any{}, false}, // an empty object is still a sent state
		{0, false},
	} {
		if got := isBlank(c.in); got != c.want {
			t.Fatalf("isBlank(%#v) = %v want %v", c.in, got, c.want)
		}
	}
}
