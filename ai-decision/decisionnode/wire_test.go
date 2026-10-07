package decisionnode

import (
	"reflect"
	"testing"

	"github.com/bytedance/sonic"
)

// TestWireShapeMatchesTheDocumentedModel pins the request this node puts on
// POST /v1/systemone against the published request model, which is the whole
// reason one node serves both the hosted Jev and a local Laya:
//
//	model        (string)                   — required
//	state        (string | object | array)  — required. "The content to
//	                                          evaluate: a string, a JSON object,
//	                                          or an array of texts. Text only."
//	questions    (object)                   — required. key → question:
//	  type         ("noul"|"choice"|"score")
//	  instructions (string | object | array) — "What to decide about the state.
//	                                           A plain question, or structured
//	                                           guidance as a JSON object/array."
//	  criteria     (object | array)          — required for choice and score
//
// There is NO top-level evidence parameter: the three fields above are the
// whole body. Evidence therefore has to ride inside `state`, which is what
// assembleState does — and the nested shape it builds is the one the vendor
// docs address in their own examples (a question may cite a nested value by
// backticked dot-and-index path, e.g. `support.tickets[0].message`), so an
// object holding an array of {source, text} rows is addressable as
// `evidence[0].text`.
//
// If a future version of the service grows a real evidence parameter, this test
// is where that shows up as a decision rather than as a surprise.
func TestWireShapeMatchesTheDocumentedModel(t *testing.T) {
	cfg := DecisionSettings{AccessToken: "k", Model: "jev-latest"}
	questions := []Question{{
		ID:           "termination_valid",
		Type:         typeNoul,
		Instructions: map[string]any{"question": "Does `case.problem` qualify under `evidence[0].text`?"},
		Options:      []Option{{Name: noulYes, Description: "it qualifies"}},
	}}
	if err := validateQuestions(questions, hostedMaxScoreLevels); err != nil {
		t.Fatalf("validate: %v", err)
	}

	evidence := []EvidenceItem{
		{Source: "contract.pdf", Text: "Section 12 allows termination with 30 days notice..."},
		{Source: "regulation.pdf", Text: "Section 44 requires..."},
	}
	state := assembleState(map[string]any{"customer": "ABC Ltd", "problem": "Early contract termination"}, evidence)
	req := systemOne{}.buildRequest(identity, modelOf(systemOne{}, cfg), state, questions)

	raw, err := sonic.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := sonic.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	want := map[string]any{
		"model": "jev-latest",
		"state": map[string]any{
			"case": map[string]any{"customer": "ABC Ltd", "problem": "Early contract termination"},
			"evidence": []any{
				map[string]any{"source": "contract.pdf", "text": "Section 12 allows termination with 30 days notice..."},
				map[string]any{"source": "regulation.pdf", "text": "Section 44 requires..."},
			},
		},
		"questions": map[string]any{
			"termination_valid": map[string]any{
				"type":         "noul",
				"instructions": map[string]any{"question": "Does `case.problem` qualify under `evidence[0].text`?"},
				"criteria":     map[string]any{"true": "it qualifies", "false": "No"},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wire shape drifted\n got: %s\nwant: %#v", raw, want)
	}

	// The body carries nothing beyond the three documented fields — an unknown
	// top-level key is exactly the mistake that looks like it works (ignored by
	// the service) while the evidence never reaches the model.
	if len(got) != 3 {
		t.Fatalf("body has %d top-level fields, want exactly model/state/questions: %s", len(got), raw)
	}

	// A source left empty must drop out rather than travel as "": the key is a
	// provenance label a question may cite, not a placeholder.
	raw2, _ := sonic.Marshal(assembleState("", []EvidenceItem{{Text: "bare chunk"}}))
	var got2 map[string]any
	_ = sonic.Unmarshal(raw2, &got2)
	row := got2["evidence"].([]any)[0].(map[string]any)
	if _, present := row["source"]; present {
		t.Fatalf("empty source should be omitted: %s", raw2)
	}
	if len(got2) != 1 {
		t.Fatalf("a blank subject should not send a case field: %s", raw2)
	}
}

// TestWireShapeWithoutEvidenceIsUnchanged guards the compatibility promise: a
// body written before evidence existed must reach the wire byte-for-byte as it
// did, so no deployed flow changes behaviour on this upgrade.
func TestWireShapeWithoutEvidenceIsUnchanged(t *testing.T) {
	q := []Question{{ID: "category", Type: typeChoice, Instructions: "Which team?", Options: []Option{{Name: "billing", Description: "Payments"}}}}
	req := systemOne{}.buildRequest(identity, "jev-latest", assembleState("a ticket", nil), q)
	raw, _ := sonic.Marshal(req)
	want := `{"state":"a ticket","model":"jev-latest","questions":{"category":{"type":"choice","instructions":"Which team?","criteria":{"billing":"Payments"}}}}`
	if string(raw) != want {
		t.Fatalf("\n got: %s\nwant: %s", raw, want)
	}
}
