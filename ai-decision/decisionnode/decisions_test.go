package decisionnode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bytedance/sonic"
)

// TestDialectOf guards the compatibility promise that matters most on this
// change: settings profiles are snapshotted onto nodes when applied, so every
// flow deployed before `provider` existed carries provider:"" forever. If empty
// ever stopped meaning System One, those flows would start talking to the wrong
// service with the wrong key.
func TestDialectOf(t *testing.T) {
	for _, c := range []struct {
		in   string
		want string
	}{
		{"", providerSystemOne}, // the promise
		{"systemone", providerSystemOne},
		{"system_one", providerSystemOne},
		{"  SystemOne  ", providerSystemOne},
		{"jev", providerSystemOne},
		{"nimble", providerSystemOne},
		{"laya", providerSystemOne},
		{"decisions", providerDecisions},
		{"openai", providerDecisions},
		{"OpenAI-Decisions", providerDecisions},
		{"vercel", providerDecisions},
	} {
		t.Run("provider="+c.in, func(t *testing.T) {
			d, err := dialectOf(DecisionSettings{Provider: c.in})
			if err != nil {
				t.Fatalf("dialectOf(%q): %v", c.in, err)
			}
			if d.name() != c.want {
				t.Fatalf("dialectOf(%q) = %s, want %s", c.in, d.name(), c.want)
			}
		})
	}

	t.Run("an unknown provider names both protocols", func(t *testing.T) {
		_, err := dialectOf(DecisionSettings{Provider: "anthropic"})
		if err == nil {
			t.Fatal("expected an error")
		}
		// A profile that named a protocol this node does not speak should be
		// told which two it does, rather than left to guess.
		if !strings.Contains(err.Error(), providerSystemOne) || !strings.Contains(err.Error(), providerDecisions) {
			t.Fatalf("error should name both protocols: %q", err)
		}
	})
}

// TestDecisionsWireShape pins the body this node puts on POST /v1/decisions
// against the published request model:
//
//	model      (string)                — required
//	input      (string | user messages) — required, the shared subject
//	questions  (array)                  — required, each with:
//	  type         ("predicate"|"choice"|"score")
//	  name         (string)             — echoed back on the answer
//	  instructions (string)
//	  choices      ([{value, description?}]) — choice only
//	  levels       ([{label, description?}])  — score only
//
// Three differences from System One are load-bearing and are all asserted
// here: the questions are an ARRAY rather than an object keyed by id, a boolean
// question is a "predicate" carrying NO criteria of any kind, and the node's
// own "noul" vocabulary never reaches the wire.
func TestDecisionsWireShape(t *testing.T) {
	qs := []Question{
		{
			ID:           "department",
			Type:         typeChoice,
			Instructions: "Which team should handle this ticket?",
			Options:      []Option{{Name: "billing", Description: "Refunds and payments"}, {Name: "shipping"}},
		},
		{
			ID:           "urgency",
			Type:         typeScore,
			Instructions: "How urgent is this ticket?",
			Options:      []Option{{Name: "low", Description: "Can wait"}, {Name: "high", Description: "Now"}},
		},
		{
			ID:           "damaged",
			Type:         typeNoul,
			Instructions: "Does the customer report a damaged item?",
			Options:      []Option{{Name: noulYes, Description: "visible physical damage"}},
		},
	}
	if err := validateQuestions(qs, decisionsMaxScoreLevels); err != nil {
		t.Fatalf("validate: %v", err)
	}

	raw, err := sonic.Marshal(decisions{}.buildRequest(identity, "gpt-6-luna", "The screen arrived broken.", qs))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := sonic.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	want := map[string]any{
		"model": "gpt-6-luna",
		"input": "The screen arrived broken.",
		"questions": []any{
			map[string]any{
				"type":         "choice",
				"name":         "department",
				"instructions": "Which team should handle this ticket?",
				"choices": []any{
					map[string]any{"value": "billing", "description": "Refunds and payments"},
					// No description to send: the model already sees the name
					// in `value`, so there is nothing to fall back to.
					map[string]any{"value": "shipping"},
				},
			},
			map[string]any{
				"type":         "score",
				"name":         "urgency",
				"instructions": "How urgent is this ticket?",
				"levels": []any{
					map[string]any{"label": "low", "description": "Can wait"},
					map[string]any{"label": "high", "description": "Now"},
				},
			},
			map[string]any{
				"type": "predicate",
				"name": "damaged",
				// A predicate takes no criteria, so the yes/no descriptions a
				// noul question carries are folded into the instructions rather
				// than dropped — dropping them would quietly change what the
				// question asks.
				"instructions": "Does the customer report a damaged item?\n\nyes: visible physical damage",
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wire shape drifted\n got: %s\nwant: %#v", raw, want)
	}

	// Exactly the three documented top-level fields. An extra key is the
	// mistake that looks like it works — ignored by the service — while
	// whatever it carried never reaches the model.
	if len(got) != 3 {
		t.Fatalf("body has %d top-level fields, want exactly model/input/questions: %s", len(got), raw)
	}
	// "noul" is this node's word, not the protocol's.
	if strings.Contains(string(raw), "noul") || strings.Contains(string(raw), "criteria") {
		t.Fatalf("System One vocabulary leaked onto the Decisions wire: %s", raw)
	}
}

// A structured `instructions` — the object form that carries reference data the
// question cites by backticked name — has no place in a protocol that
// documents instructions as a string, so it is serialized to JSON text. The
// citation idiom is a convention about the text the model reads, so it survives
// the change of encoding; silently dropping the reference data would not.
func TestDecisionsFlattensStructuredInstructions(t *testing.T) {
	qs := []Question{{
		ID:           "termination",
		Type:         typeNoul,
		Instructions: map[string]any{"question": "Allowed under `policy`?", "policy": "30 days notice"},
	}}
	req := decisions{}.buildRequest(identity, "gpt-6-luna", "a case", qs).(decRequest)
	instr := req.Questions[0].Instructions
	var back map[string]any
	if err := sonic.Unmarshal([]byte(instr), &back); err != nil {
		t.Fatalf("instructions should be JSON text, got %q: %v", instr, err)
	}
	if back["question"] != "Allowed under `policy`?" || back["policy"] != "30 days notice" {
		t.Fatalf("reference data lost in the flattening: %v", back)
	}
}

// The templates in a question reach the service resolved, on this protocol too
// — the gap being that instructions and option descriptions are authored on the
// canvas, so a {{$.path}} left as token text would reach the model verbatim.
func TestDecisionsResolvesTemplates(t *testing.T) {
	qs := []Question{{
		ID:           "department",
		Type:         typeChoice,
		Instructions: "route it",
		Options:      []Option{{Name: "billing", Description: "payments"}},
	}}
	req := decisions{}.buildRequest(upper, "m", "s", qs).(decRequest)
	if req.Questions[0].Instructions != "ROUTE IT" {
		t.Fatalf("instructions not resolved: %q", req.Questions[0].Instructions)
	}
	if got := req.Questions[0].Choices[0].Description; got != "PAYMENTS" {
		t.Fatalf("choice description not resolved: %q", got)
	}
	// The option NAME is the port tag the runtime routes on, so it must travel
	// as the designer declared it, unresolved.
	if got := req.Questions[0].Choices[0].Value; got != "billing" {
		t.Fatalf("option name must not be templated: %q", got)
	}
}

// TestDecisionsDecode feeds back the response documented for this protocol,
// verbatim, and checks it lands on the same normalized reply a System One
// answer does — which is the whole claim of the dialect split, since decide()
// and the routing below it are shared code.
//
// The two re-keyings are the substance: a choice distribution arrives as
// [{value, probability}] and has to become name→p, and a score distribution
// arrives as [{value, label, probability}] and has to become index→p, because
// that is how decide() reads each of them.
func TestDecisionsDecode(t *testing.T) {
	const body = `{
      "model": "openai/gpt-6-luna-decisions",
      "answers": [
        { "type": "predicate", "name": "damaged", "probability": 0.95 },
        {
          "type": "choice",
          "name": "queue",
          "choice": "shipping",
          "confidence": 0.81,
          "probabilities": [
            { "value": "billing", "probability": 0.19 },
            { "value": "shipping", "probability": 0.81 }
          ]
        },
        {
          "type": "score",
          "name": "urgency",
          "score": 1.3,
          "confidence": 0.62,
          "probabilities": [
            { "value": 0, "label": "low", "probability": 0.08 },
            { "value": 1, "label": "medium", "probability": 0.54 },
            { "value": 2, "label": "high", "probability": 0.38 }
          ]
        }
      ],
      "usage": { "input_tokens": 96, "output_tokens": 0, "total_tokens": 96 }
    }`

	r, err := decisions{}.decode([]byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r.Model != "openai/gpt-6-luna-decisions" {
		t.Fatalf("model = %q", r.Model)
	}
	if r.Usage["input_tokens"] != float64(96) {
		t.Fatalf("usage = %v", r.Usage)
	}

	// A predicate becomes a noul: same question, same ports, so decide() needs
	// no case of its own for it.
	if a := r.Answers["damaged"]; a.Type != typeNoul || a.P == nil || *a.P != 0.95 {
		t.Fatalf("predicate = %+v", a)
	}
	if a := r.Answers["queue"]; a.Type != typeChoice || a.Choice != "shipping" ||
		!reflect.DeepEqual(a.Probabilities, map[string]float64{"billing": 0.19, "shipping": 0.81}) {
		t.Fatalf("choice = %+v", a)
	}
	if a := r.Answers["urgency"]; a.Type != typeScore ||
		!reflect.DeepEqual(a.Probabilities, map[string]float64{"0": 0.08, "1": 0.54, "2": 0.38}) {
		t.Fatalf("score = %+v", a)
	}

	// …and the shared routing layer turns each of them into the same port tag
	// it would have produced from a System One reply.
	for _, c := range []struct {
		q       Question
		id      string
		answer  string
		tag     string
		confTop float64
	}{
		{Question{ID: "damaged", Type: typeNoul}, "damaged", "yes", "damaged.yes", 0.95},
		{Question{ID: "queue", Type: typeChoice, Options: []Option{{Name: "billing"}, {Name: "shipping"}}},
			"queue", "shipping", "queue.shipping", 0.81},
		{Question{ID: "urgency", Type: typeScore, Options: []Option{{Name: "low"}, {Name: "medium"}, {Name: "high"}}},
			"urgency", "medium", "urgency.medium", 0.62},
	} {
		dec, err := decide(c.q, r.Answers[c.id])
		if err != nil {
			t.Fatalf("decide(%s): %v", c.id, err)
		}
		if dec.Answer != c.answer || dec.Tag != c.tag || dec.Confidence != c.confTop {
			t.Fatalf("decide(%s) = %+v, want answer %q tag %q conf %v", c.id, dec, c.answer, c.tag, c.confTop)
		}
	}
}

// A refusal is this protocol's own case: the model declines ONE question and
// answers the rest, keeping the refused question in its position. It is neither
// a wire mismatch nor a low-confidence answer — there is no answer — so it has
// to be distinguishable from both by the time the handler sees it.
func TestDecisionsRefusal(t *testing.T) {
	r, err := decisions{}.decode([]byte(
		`{"model":"gpt-6-luna","answers":[{"type":"refusal","name":"sensitive"},{"type":"predicate","name":"ok","probability":0.7}]}`))
	if err != nil {
		t.Fatalf("a refusal is not a transport error: %v", err)
	}
	if a := r.Answers["sensitive"]; !a.Refused {
		t.Fatalf("refusal not marked: %+v", a)
	}
	// The other questions still answer normally.
	if a := r.Answers["ok"]; a.Refused || a.P == nil || *a.P != 0.7 {
		t.Fatalf("sibling answer disturbed by the refusal: %+v", a)
	}
}

// The OpenAI error envelope, which may arrive on a 2xx as well as on a 4xx, must
// not pass as an answer — the same rule the System One envelope's non-zero
// `code` gets. `param` names the field that failed, which is the useful half of
// a validation failure.
func TestDecisionsErrorEnvelopeIsNotAnAnswer(t *testing.T) {
	_, err := decisions{}.decode([]byte(
		`{"error":{"message":"Image input isn't supported yet. Send text input.","type":"invalid_request_error","param":"input[0].content[1]","code":"invalid_request_error"}}`))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "Image input isn't supported") {
		t.Fatalf("error should quote the service's message: %q", err)
	}
}

// The retryable set is a property of the protocol, not of the node: OpenAI's own
// SDKs retry rate limits and any server error, while System One documents only
// 429 and 529 and nothing else.
func TestRetryableIsPerProtocol(t *testing.T) {
	for _, c := range []struct {
		status  int
		so, dec bool
	}{
		{http.StatusTooManyRequests, true, true},
		{529, true, true},
		{http.StatusInternalServerError, false, true},
		{http.StatusServiceUnavailable, false, true},
		{http.StatusBadRequest, false, false},
		{http.StatusUnauthorized, false, false},
		{http.StatusUnprocessableEntity, false, false},
	} {
		if got := (systemOne{}).retryable(c.status); got != c.so {
			t.Errorf("systemone retryable(%d) = %v, want %v", c.status, got, c.so)
		}
		if got := (decisions{}).retryable(c.status); got != c.dec {
			t.Errorf("decisions retryable(%d) = %v, want %v", c.status, got, c.dec)
		}
	}
}

// The profile rule is the same on both protocols — it follows the URL — but the
// reason the second half matters is protocol-specific and worth pinning: the
// Decisions API's own model id means nothing to a gateway serving the same
// model under its own slug, so a profile that names a URL must name a model.
func TestDecisionsValidateSettings(t *testing.T) {
	t.Run("the hosted endpoint needs a key and assumes the model", func(t *testing.T) {
		if err := validateSettings(decisions{}, DecisionSettings{Provider: "openai"}); err == nil {
			t.Fatal("want an error naming access_token")
		}
		if err := validateSettings(decisions{}, DecisionSettings{Provider: "openai", AccessToken: "sk-x"}); err != nil {
			t.Fatalf("a key is all the hosted endpoint needs: %v", err)
		}
		// gpt-6-luna is the only model the API serves in beta, so a profile
		// should not have to supply it.
		if got := modelOf(decisions{}, DecisionSettings{AccessToken: "sk-x"}); got != decisionsModel {
			t.Fatalf("default model = %q, want %q", got, decisionsModel)
		}
	})
	t.Run("a gateway needs its own slug", func(t *testing.T) {
		gw := DecisionSettings{Provider: "decisions", URL: "https://ai-gateway.vercel.sh/v1", AccessToken: "k"}
		if err := validateSettings(decisions{}, gw); err == nil {
			t.Fatal("want an error naming model: the gateway slugs the model differently")
		}
		gw.Model = "openai/gpt-6-luna-decisions"
		if err := validateSettings(decisions{}, gw); err != nil {
			t.Fatalf("a gateway profile with a slug should be valid: %v", err)
		}
	})
	t.Run("the two protocols have different default hosts", func(t *testing.T) {
		if baseURL(decisions{}, DecisionSettings{}) == baseURL(systemOne{}, DecisionSettings{}) {
			t.Fatal("the dialects must not share a default host")
		}
		if got := baseURL(decisions{}, DecisionSettings{}); got != "https://api.openai.com" {
			t.Fatalf("decisions default host = %q", got)
		}
	})
}

// The score ceiling is the one declared-answer limit that is NOT a property of
// the node, and the old hardcoded 10 was wrong for a local decider: Ollama
// documents 2..26 ordered levels, so an eleven-level question was being
// rejected here for a service that answers it perfectly well.
func TestScoreCeilingFollowsTheEndpoint(t *testing.T) {
	levels := func(n int) []Question {
		opts := make([]Option, n)
		for i := range opts {
			opts[i] = Option{Name: string(rune('a' + i))}
		}
		return []Question{{ID: "q", Type: typeScore, Instructions: "how much?", Options: opts}}
	}

	hosted := DecisionSettings{}                                              // TypeSafe's own host
	local := DecisionSettings{URL: "http://127.0.0.1:11434", Model: "nimble"} // Ollama
	openai := DecisionSettings{Provider: "decisions", AccessToken: "k"}

	for _, c := range []struct {
		name    string
		d       dialect
		cfg     DecisionSettings
		n       int
		wantErr bool
	}{
		{"hosted jev holds to its documented ten", systemOne{}, hosted, 12, true},
		{"hosted jev accepts ten", systemOne{}, hosted, 10, false},
		{"a local decider takes more", systemOne{}, local, 12, false},
		{"a local decider still has a ceiling", systemOne{}, local, 27, true},
		{"decisions allows ten", decisions{}, openai, 10, false},
		{"decisions rejects eleven", decisions{}, openai, 11, true},
		{"two is the floor everywhere", systemOne{}, local, 1, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := validateQuestions(levels(c.n), c.d.maxScoreLevels(c.cfg))
			if (err != nil) != c.wantErr {
				t.Fatalf("%d levels: err = %v, wantErr %v", c.n, err, c.wantErr)
			}
		})
	}
}

// End to end over HTTP: the dialect has to put the request on the protocol's own
// PATH, carry the bearer, and come back through the shared retry machinery.
func TestCallDecisionOverTheDecisionsProtocol(t *testing.T) {
	t.Run("posts to /v1/decisions and decodes", func(t *testing.T) {
		var gotPath, gotAuth string
		var gotBody decRequest
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
			_ = sonic.ConfigDefault.NewDecoder(r.Body).Decode(&gotBody)
			_, _ = w.Write([]byte(`{"model":"gpt-6-luna","answers":[{"type":"choice","name":"q","choice":"a","confidence":0.9,"probabilities":[{"value":"a","probability":0.9},{"value":"b","probability":0.1}]}],"usage":{"input_tokens":12}}`))
		}))
		defer srv.Close()

		cfg := DecisionSettings{Provider: "openai", URL: srv.URL, Model: "gpt-6-luna", AccessToken: "sk-x"}
		d, err := dialectOf(cfg)
		if err != nil {
			t.Fatal(err)
		}
		qs := []Question{{ID: "q", Type: typeChoice, Instructions: "which?", Options: []Option{{Name: "a"}, {Name: "b"}}}}
		r, err := callDecision(context.Background(), d, cfg,
			d.buildRequest(identity, modelOf(d, cfg), "subject", qs), nil)
		if err != nil {
			t.Fatal(err)
		}
		if gotPath != decisionsPath {
			t.Errorf("path = %q, want %q", gotPath, decisionsPath)
		}
		if gotAuth != "Bearer sk-x" {
			t.Errorf("auth = %q", gotAuth)
		}
		if gotBody.Input != "subject" || len(gotBody.Questions) != 1 || gotBody.Questions[0].Name != "q" {
			t.Errorf("body = %+v", gotBody)
		}
		if r.Answers["q"].Choice != "a" {
			t.Errorf("reply = %+v", r.Answers)
		}
	})

	t.Run("a 500 is retried on this protocol", func(t *testing.T) {
		var calls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if atomic.AddInt32(&calls, 1) == 1 {
				http.Error(w, `{"error":{"message":"server error","type":"server_error"}}`, http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(`{"model":"gpt-6-luna","answers":[{"type":"predicate","name":"q","probability":0.5}]}`))
		}))
		defer srv.Close()
		cfg := DecisionSettings{Provider: "openai", URL: srv.URL, Model: "gpt-6-luna", AccessToken: "k"}
		if _, err := callDecision(context.Background(), decisions{}, cfg, decRequest{}, nil); err != nil {
			t.Fatal(err)
		}
		if calls != 2 {
			t.Fatalf("calls = %d, want 2", calls)
		}
	})
}
