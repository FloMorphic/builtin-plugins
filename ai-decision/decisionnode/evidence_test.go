package decisionnode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/bytedance/sonic"
)

// TestEvidenceReachesTheServiceEndToEnd walks one RAG-shaped decision through
// every link the node owns — resolve the evidence templates, fold them into the
// state, build the request, POST it, decode the wrapped reply, reduce each
// answer to a Decision and a port tag — against a server standing in for the
// System One endpoint.
//
// What it proves: the evidence a designer injects arrives at the endpoint
// resolved, in the documented `state` object, addressable by the backticked
// paths the questions use. What it cannot prove is the vendor's acceptance of
// that body: only a live call does that (see verify-api.sh).
func TestEvidenceReachesTheServiceEndToEnd(t *testing.T) {
	// Stands in for the flow context: the retriever node upstream put the
	// clauses on the scope, and the drawer points at them by path.
	scope := map[string]string{
		"{{$.kb.termination_clause}}": "Section 12 allows termination with 30 days notice...",
		"{{$.kb.regulation}}":         "Section 44 requires a written statement of cause.",
		"{{$.kb.policy}}":             "Terminations require legal sign-off above £50k.",
		"{{$.ticket.customer}}":       "ABC Ltd",
	}
	res := func(s string) string {
		for tok, val := range scope {
			if s == tok {
				return val
			}
		}
		return s
	}

	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := sonic.ConfigDefault.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("service could not decode the request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		// The live reply shape: enveloped, with creditsUsed alongside.
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{"result":{
			"model":"jev-1.13.0",
			"answers":{
				"termination_valid":{"type":"noul","noul":0.88},
				"risk":{"type":"score","score":1.4,"probabilities":{"0":0.12,"1":0.88},"confidence":0.9}
			},
			"usage":{"input_tokens":412,"output_tokens":24},
			"elapsedMs":194
		},"creditsUsed":1}}`))
	}))
	defer srv.Close()

	// ---- what the designer configured in the drawer -------------------------
	cfg := DecisionSettings{URL: srv.URL, Model: "jev-latest"}
	if err := validateSettings(cfg); err != nil {
		t.Fatalf("profile: %v", err)
	}
	evidenceRows := []EvidenceItem{
		{Source: "contract.pdf", Text: "{{$.kb.termination_clause}}"},
		{Source: "regulation.pdf", Text: "{{$.kb.regulation}}"},
	}
	if err := validateEvidence(evidenceRows); err != nil {
		t.Fatalf("evidence: %v", err)
	}
	questions := []Question{
		{
			ID:   "termination_valid",
			Type: typeNoul,
			// The structured form, as the compiler assembles it from the
			// question text plus a named reference row.
			Instructions: map[string]any{
				"question": "Does `case.problem` qualify for termination under `evidence[0].text`, given `policy`?",
				"policy":   "{{$.kb.policy}}",
			},
			Options: []Option{{Name: noulYes, Description: "it qualifies"}, {Name: noulNo, Description: "it does not"}},
		},
		{
			ID:           "risk",
			Type:         typeScore,
			Instructions: "How much legal risk does proceeding carry, per `evidence[1].text`?",
			Options:      []Option{{Name: "low", Description: "routine"}, {Name: "high", Description: "contested"}},
		},
	}
	if err := validateQuestions(questions); err != nil {
		t.Fatalf("questions: %v", err)
	}

	// ---- the node's own pipeline -------------------------------------------
	evidence := resolveEvidence(res, evidenceRows)
	state := assembleState(map[string]any{
		"customer": res("{{$.ticket.customer}}"),
		"problem":  "Early contract termination",
	}, evidence)
	resp, err := callDecision(context.Background(), cfg, buildRequest(res, cfg, state, questions), nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}

	// ---- what the service actually received --------------------------------
	wantState := map[string]any{
		"case": map[string]any{"customer": "ABC Ltd", "problem": "Early contract termination"},
		"evidence": []any{
			map[string]any{"source": "contract.pdf", "text": "Section 12 allows termination with 30 days notice..."},
			map[string]any{"source": "regulation.pdf", "text": "Section 44 requires a written statement of cause."},
		},
	}
	if !reflect.DeepEqual(got["state"], wantState) {
		raw, _ := sonic.Marshal(got["state"])
		t.Fatalf("state as received:\n%s\nwant %#v", raw, wantState)
	}
	// The reference value inside the structured instructions was resolved too —
	// the gap this whole change closed.
	instr := got["questions"].(map[string]any)["termination_valid"].(map[string]any)["instructions"].(map[string]any)
	if instr["policy"] != "Terminations require legal sign-off above £50k." {
		t.Fatalf("instructions reference not resolved: %#v", instr)
	}
	if instr["question"] == nil {
		t.Fatalf("the question itself went missing: %#v", instr)
	}
	// Nothing beyond the three documented top-level fields.
	if len(got) != 3 {
		t.Fatalf("body has %d top-level fields, want model/state/questions: %#v", len(got), got)
	}

	// ---- and what the flow routes on ---------------------------------------
	if resp.CreditsUsed != 1 || resp.ElapsedMs != 194 || resp.Model != "jev-1.13.0" {
		t.Fatalf("accounting lost: %+v", resp)
	}
	for _, q := range questions {
		d, err := decide(q, resp.Answers[q.ID])
		if err != nil {
			t.Fatalf("decide %s: %v", q.ID, err)
		}
		switch q.ID {
		case "termination_valid":
			if d.Answer != noulYes || d.Tag != "termination_valid.yes" || d.Confidence != 0.88 {
				t.Fatalf("noul decision: %+v", d)
			}
		case "risk":
			if d.Answer != "high" || d.Tag != "risk.high" || d.Probabilities["low"] != 0.12 {
				t.Fatalf("score decision: %+v", d)
			}
		}
	}
}
