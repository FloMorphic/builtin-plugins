package decisionnode

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func f(v float64) *float64 { return &v }

func TestValidateQuestions(t *testing.T) {
	ok := func(qs ...Question) []Question { return qs }
	choice := Question{ID: "category", Type: "choice", Instructions: "which team?", Options: []Option{{Name: "billing"}, {Name: "sales"}}}

	cases := []struct {
		name    string
		qs      []Question
		wantErr bool
	}{
		{"valid choice", ok(choice), false},
		{"none", nil, true},
		{"missing id", ok(Question{Type: "choice", Instructions: "x", Options: []Option{{Name: "a"}}}), true},
		{"duplicate id", ok(choice, choice), true},
		{"missing instructions", ok(Question{ID: "q", Type: "choice", Options: []Option{{Name: "a"}}}), true},
		{"choice with no options", ok(Question{ID: "q", Type: "choice", Instructions: "x"}), true},
		{"duplicate option", ok(Question{ID: "q", Type: "choice", Instructions: "x", Options: []Option{{Name: "a"}, {Name: "a"}}}), true},
		{"score with one level", ok(Question{ID: "q", Type: "score", Instructions: "x", Options: []Option{{Name: "low"}}}), true},
		{"score with two levels", ok(Question{ID: "q", Type: "score", Instructions: "x", Options: []Option{{Name: "low"}, {Name: "high"}}}), false},
		{"noul with no rows", ok(Question{ID: "q", Type: "noul", Instructions: "x"}), false},
		{"noul with bad row name", ok(Question{ID: "q", Type: "noul", Instructions: "x", Options: []Option{{Name: "maybe"}}}), true},
		{"noul true/false rows normalize", ok(Question{ID: "q", Type: "noul", Instructions: "x", Options: []Option{{Name: "true"}, {Name: "False"}}}), false},
		{"unknown type", ok(Question{ID: "q", Type: "rank", Instructions: "x", Options: []Option{{Name: "a"}}}), true},
		{"min_confidence out of range", ok(Question{ID: "q", Type: "choice", Instructions: "x", Options: []Option{{Name: "a"}}, MinConfidence: 1.5}), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateQuestions(tc.qs, hostedMaxScoreLevels)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateQuestions err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestBuildCriteria(t *testing.T) {
	t.Run("choice is a name→description map, name as fallback", func(t *testing.T) {
		q := Question{Type: typeChoice, Options: []Option{{Name: "billing", Description: "Payments"}, {Name: "sales"}}}
		got := buildCriteria(identity, q)
		want := map[string]string{"billing": "Payments", "sales": "sales"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v want %v", got, want)
		}
	})
	t.Run("score is the ordered level descriptions", func(t *testing.T) {
		q := Question{Type: typeScore, Options: []Option{{Name: "low", Description: "Calm"}, {Name: "high", Description: "Angry"}}}
		got := buildCriteria(identity, q)
		if !reflect.DeepEqual(got, []string{"Calm", "Angry"}) {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("noul defaults, rows override descriptions", func(t *testing.T) {
		if got := buildCriteria(identity, Question{Type: typeNoul}); !reflect.DeepEqual(got, map[string]string{"true": "Yes", "false": "No"}) {
			t.Fatalf("default got %v", got)
		}
		q := Question{Type: typeNoul, Options: []Option{{Name: noulYes, Description: "Time-sensitive"}}}
		if got := buildCriteria(identity, q); !reflect.DeepEqual(got, map[string]string{"true": "Time-sensitive", "false": "No"}) {
			t.Fatalf("override got %v", got)
		}
	})
}

func TestDecide(t *testing.T) {
	t.Run("choice routes on the API's choice with the calibrated confidence", func(t *testing.T) {
		q := Question{ID: "category", Type: typeChoice, Options: []Option{{Name: "billing"}, {Name: "technical"}, {Name: "sales"}}}
		a := answer{Type: "choice", Choice: "billing", Probabilities: map[string]float64{"billing": 0.88, "technical": 0.12, "sales": 0}, Confidence: f(0.81)}
		d, err := decide(q, a)
		if err != nil {
			t.Fatal(err)
		}
		if d.Answer != "billing" || d.Tag != "category.billing" || d.Confidence != 0.81 {
			t.Fatalf("got %+v", d)
		}
		if d.Probabilities["technical"] != 0.12 {
			t.Fatalf("probabilities not carried: %v", d.Probabilities)
		}
	})
	t.Run("choice naming an undeclared option is an error, not a tag", func(t *testing.T) {
		q := Question{ID: "category", Type: typeChoice, Options: []Option{{Name: "billing"}}}
		if _, err := decide(q, answer{Type: "choice", Choice: "legal"}); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("score picks the most probable level by index and re-keys by name", func(t *testing.T) {
		q := Question{ID: "urgency", Type: typeScore, Options: []Option{{Name: "low"}, {Name: "medium"}, {Name: "high"}}}
		a := answer{Type: "score", Score: f(1.05), Probabilities: map[string]float64{"0": 0, "1": 0.95, "2": 0.05}, Confidence: f(0.92)}
		d, err := decide(q, a)
		if err != nil {
			t.Fatal(err)
		}
		if d.Answer != "medium" || d.Tag != "urgency.medium" || d.Confidence != 0.92 || *d.Score != 1.05 {
			t.Fatalf("got %+v", d)
		}
		if !reflect.DeepEqual(d.Probabilities, map[string]float64{"low": 0, "medium": 0.95, "high": 0.05}) {
			t.Fatalf("probabilities %v", d.Probabilities)
		}
	})
	t.Run("noul splits at 0.5 and reports distance from the coin flip", func(t *testing.T) {
		q := Question{ID: "repeat", Type: typeNoul}
		d, err := decide(q, answer{Type: typeNoul, P: f(0.2)})
		if err != nil {
			t.Fatal(err)
		}
		if d.Answer != "no" || d.Tag != "repeat.no" || d.Confidence != 0.8 || *d.P != 0.2 {
			t.Fatalf("got %+v", d)
		}
		d, _ = decide(q, answer{Type: typeNoul, P: f(0.5)})
		if d.Answer != "yes" {
			t.Fatalf("0.5 should be yes, got %+v", d)
		}
	})
	t.Run("noul without a probability is an error", func(t *testing.T) {
		if _, err := decide(Question{ID: "r", Type: typeNoul}, answer{Type: typeNoul}); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestQuestionRoutesDefault(t *testing.T) {
	off := false
	if !(Question{}).routes() {
		t.Fatal("route unset should route")
	}
	if (Question{Route: &off}).routes() {
		t.Fatal("route=false should not route")
	}
}

func TestValidateInstructions(t *testing.T) {
	for _, c := range []struct {
		name    string
		in      any
		wantErr bool
	}{
		{"plain question", "which team?", false},
		{"blank string", "   ", true},
		{"structured object", map[string]any{"question": "q", "policy": "p"}, false},
		{"empty object", map[string]any{}, true},
		{"array form", []any{"q", "p"}, false},
		{"empty array", []any{}, true},
		{"missing", nil, true},
		{"wrong type", 7, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := validateInstructions("q", c.in); (err != nil) != c.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, c.wantErr)
			}
		})
	}
}

func TestValidateEvidence(t *testing.T) {
	if err := validateEvidence([]EvidenceItem{{Source: "a.pdf", Text: "x"}}); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if err := validateEvidence(nil); err != nil {
		t.Fatalf("no rows is fine: %v", err)
	}
	if err := validateEvidence([]EvidenceItem{{Source: "a.pdf", Text: " "}}); err == nil {
		t.Fatal("a row with no text only spends context budget; want error")
	}
}

func TestBuildRequestResolvesQuestionTemplates(t *testing.T) {
	// The gap this covers: before, instructions and criteria descriptions went
	// to the wire exactly as typed, so a {{$.path}} in either reached the model
	// as token text.
	qs := []Question{{
		ID:           "termination",
		Type:         typeNoul,
		Instructions: map[string]any{"question": "allowed under `policy`?", "policy": "notice"},
		Options:      []Option{{Name: noulYes, Description: "it is"}},
	}}
	req := systemOne{}.buildRequest(upper, "m", "state", qs).(soRequest)
	got := req.Questions["termination"]
	wantInstr := map[string]any{"question": "ALLOWED UNDER `POLICY`?", "policy": "NOTICE"}
	if !reflect.DeepEqual(got.Instructions, wantInstr) {
		t.Fatalf("instructions %v want %v", got.Instructions, wantInstr)
	}
	if crit := got.Criteria.(map[string]string); crit["true"] != "IT IS" {
		t.Fatalf("criteria not resolved: %v", crit)
	}
	if req.State != "state" {
		t.Fatalf("state should arrive already assembled: %v", req.State)
	}
}

// Ceilings on the request's arrays. Every array in the body is assembled
// upstream — the rows come from the drawer, and each evidence text is a template
// resolved from the live flow context — so a loop or a bad path upstream can grow
// one without anything else noticing. These pin that the ceiling is enforced AT
// the boundary rather than one past it, and that the error names the array so the
// mistake is findable.
func TestRequestArrayCeilings(t *testing.T) {
	questions := func(n int) []Question {
		out := make([]Question, n)
		for i := range out {
			out[i] = Question{
				ID: fmt.Sprintf("q%d", i), Type: typeChoice, Instructions: "which?",
				Options: []Option{{Name: "a"}, {Name: "b"}},
			}
		}
		return out
	}
	evidence := func(n int) []EvidenceItem {
		out := make([]EvidenceItem, n)
		for i := range out {
			out[i] = EvidenceItem{Source: "doc.pdf", Text: "a chunk"}
		}
		return out
	}

	t.Run("questions", func(t *testing.T) {
		if err := validateQuestions(questions(maxQuestions), hostedMaxScoreLevels); err != nil {
			t.Fatalf("exactly the ceiling must pass: %v", err)
		}
		err := validateQuestions(questions(maxQuestions+1), hostedMaxScoreLevels)
		if err == nil {
			t.Fatal("one past the ceiling must fail")
		}
		if !strings.Contains(err.Error(), "too many questions") {
			t.Fatalf("the error should name the array: %q", err)
		}
	})

	t.Run("evidence rows", func(t *testing.T) {
		if err := validateEvidence(evidence(maxEvidenceRows)); err != nil {
			t.Fatalf("exactly the ceiling must pass: %v", err)
		}
		err := validateEvidence(evidence(maxEvidenceRows + 1))
		if err == nil {
			t.Fatal("one past the ceiling must fail")
		}
		if !strings.Contains(err.Error(), "too many evidence rows") {
			t.Fatalf("the error should name the array: %q", err)
		}
	})

	// The ceilings are uniform on purpose: a flow that validates against one
	// profile has to validate against the other, or "swap the profile, keep the
	// canvas" stops being true. Only the score-level ceiling is per-endpoint.
	t.Run("the array ceilings do not vary by protocol", func(t *testing.T) {
		for _, d := range []dialect{systemOne{}, decisions{}} {
			for _, cfg := range []DecisionSettings{{}, {URL: "http://127.0.0.1:11434", Model: "nimble"}} {
				if err := validateQuestions(questions(maxQuestions), d.maxScoreLevels(cfg)); err != nil {
					t.Fatalf("%s: the ceiling must hold everywhere: %v", d.name(), err)
				}
				if err := validateQuestions(questions(maxQuestions+1), d.maxScoreLevels(cfg)); err == nil {
					t.Fatalf("%s: one past the ceiling must fail everywhere", d.name())
				}
			}
		}
	})
}
