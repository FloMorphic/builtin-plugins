package decisionnode

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/bytedance/sonic"
)

// ---- the System One dialect: POST /v1/systemone ----------------------------
//
// Served by TypeSafe's hosted Jev, by the local-first Laya, by OpenJev, and by
// Ollama's local deciders (nimble, tev1) — one protocol, which is why this node
// was never bound to a single vendor.

const (
	// The protocol's first-party host: TypeSafe's own API, as the vendor
	// reference documents it.
	//
	// It is deliberately NOT thejevai.com, which an earlier version of this
	// node defaulted to. That host is a third-party AGGREGATOR — its catalogue
	// spans several vendors (typesafe/jev, convaiinnovations/laya,
	// cloudflare/clef, perplexity/pplx-decider, …), it bills in credits where
	// TypeSafe bills per input token, and it answers with an envelope
	// ({code, message, data:{result, creditsUsed}}) that appears nowhere in the
	// reference. Useful, and the only way to reach several of those models
	// behind one key — but a workflow product should not route a customer's
	// state through an unaffiliated third party unless someone chose to, so it
	// is a `url` a profile opts into rather than the default.
	//
	// Keys are not interchangeable between the two: an aggregator key will not
	// authenticate here, and a TypeSafe key (console.typesafe.ai) will not
	// authenticate there. systemOne.decode handles both reply shapes, so the
	// only thing a profile has to get right is the pair of url and key.
	systemOneBaseURL = "https://api.typesafe.ai"
	// The hosted service's documented alias, used only when the profile talks
	// to systemOneBaseURL (validateSettings requires an explicit model for any
	// other endpoint, since this alias means nothing there). The hosted service
	// also accepts a vendor-prefixed, pinned id ("typesafe/jev-1.13"), which is
	// the safer thing to put in a profile once a flow is in production — a
	// decision node changing model under a flow is a change someone should
	// choose.
	systemOneModel = "jev-latest"
	// The one endpoint of the protocol. Every implementation serves it, which is
	// why this dialect is not bound to any of them.
	systemOnePath = "/v1/systemone"
)

// Score-level ceilings, which differ by WHO serves the protocol rather than by
// the protocol itself:
//
//   - TypeSafe's hosted service documents 2..10 ordered levels.
//   - Ollama documents 2..26 for its local deciders, with scores running 0..25.
//
// The node cannot know which implementation sits behind a custom URL, so it
// validates against the permissive ceiling there and lets the far side reject
// anything it will not take; against the hosted default, where it does know, it
// holds to the documented limit so the mistake is caught here with a readable
// reason instead of arriving as a 422.
const (
	hostedMaxScoreLevels = 10
	localMaxScoreLevels  = 26
)

type systemOne struct{}

func (systemOne) name() string           { return providerSystemOne }
func (systemOne) path() string           { return systemOnePath }
func (systemOne) defaultBaseURL() string { return systemOneBaseURL }
func (systemOne) defaultModel() string   { return systemOneModel }

// maxScoreLevels holds the hosted service to its documented ten levels and
// allows any other implementation the protocol's wider ceiling.
func (d systemOne) maxScoreLevels(cfg DecisionSettings) int {
	if isDefaultEndpoint(d, cfg) {
		return hostedMaxScoreLevels
	}
	return localMaxScoreLevels
}

// The API asks callers to back off on 429 (rate limit) and 529 (overloaded),
// and documents no other retryable status.
func (systemOne) retryable(status int) bool {
	return status == http.StatusTooManyRequests || status == 529
}

// ---- wire shapes -----------------------------------------------------------

// soRequest is the System One request body: the state, the model, and the
// questions keyed by id. Questions run in parallel against the same state.
type soRequest struct {
	State     any                   `json:"state"`
	Model     string                `json:"model"`
	Questions map[string]soQuestion `json:"questions"`
}

// soQuestion is one question on the wire. Criteria's shape depends on Type:
// a name→description map for choice, an ordered []string of level descriptions
// for score, and a {"true": …, "false": …} map for noul (see buildCriteria).
type soQuestion struct {
	Type string `json:"type"`
	// string | object | array, as the API documents it and as the node received
	// it, with every template resolved (see resolveTemplateValue).
	Instructions any `json:"instructions"`
	Criteria     any `json:"criteria,omitempty"`
}

// soResponse is the reply, decoded permissively because the service and the
// published reference disagree on where the answers sit. The live endpoint
// wraps them — {"code":0,"message":"ok","data":{"result":{…},"creditsUsed":1}}
// — while docs.typesafe.ai's API reference shows the same document flat at the
// top level. Both are decoded here and reconciled by normalize(): Data wins
// when present, the flat fields are the fallback, so neither shape breaks the
// node.
type soResponse struct {
	// enveloped form (what the aggregator returns today)
	Code    int     `json:"code"`
	Message string  `json:"message"`
	Data    *soData `json:"data"`

	// flat form (the published reference)
	Model   string              `json:"model"`
	Answers map[string]soAnswer `json:"answers"`
	Usage   map[string]any      `json:"usage"`
}

// soData is the envelope's payload: the answer document plus what the call
// cost. CreditsUsed is worth carrying to the canvas — a decision node runs on a
// metered budget, and the run that spent the credit is the only place that can
// report it.
type soData struct {
	Result      soResult `json:"result"`
	CreditsUsed int      `json:"creditsUsed"`
}

// soResult is the answer document itself: one answer per question id, the exact
// model version that answered, the token usage and the service-side latency.
type soResult struct {
	Model     string              `json:"model"`
	Answers   map[string]soAnswer `json:"answers"`
	Usage     map[string]any      `json:"usage"`
	ElapsedMs int                 `json:"elapsedMs"`
}

// soAnswer is the union of the three answer shapes; only the fields of the
// answer's Type are set. Choice/score carry Probabilities (keyed by option name,
// or by level index for score, with Legend mapping index → description) and a
// Confidence; noul carries only Noul, the probability the statement is true.
type soAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Legend        map[string]string  `json:"legend,omitempty"`
	Noul          *float64           `json:"noul,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
}

// answer lifts one wire answer into the protocol-independent shape. The
// probability keys already match what decide() expects — option names for a
// choice, level indices for a score — so this is a straight transcription.
func (a soAnswer) answer() answer {
	return answer{
		Type:          normalizeType(a.Type),
		Choice:        a.Choice,
		Score:         a.Score,
		P:             a.Noul,
		Probabilities: a.Probabilities,
		Confidence:    a.Confidence,
	}
}

// normalize reconciles the two reply shapes: the envelope when the service sent
// one, otherwise the flat document.
func (r soResponse) normalize() reply {
	src := struct {
		model     string
		answers   map[string]soAnswer
		usage     map[string]any
		elapsedMs int
		credits   int
	}{r.Model, r.Answers, r.Usage, 0, 0}
	if r.Data != nil {
		src.model, src.answers, src.usage = r.Data.Result.Model, r.Data.Result.Answers, r.Data.Result.Usage
		src.elapsedMs, src.credits = r.Data.Result.ElapsedMs, r.Data.CreditsUsed
	}
	out := reply{Model: src.model, Usage: src.usage, ElapsedMs: src.elapsedMs, CreditsUsed: src.credits}
	if len(src.answers) > 0 {
		out.Answers = make(map[string]answer, len(src.answers))
		for id, a := range src.answers {
			out.Answers[id] = a.answer()
		}
	}
	return out
}

// decode reads a 2xx body. The envelope carries its own status: a non-zero
// `code` is a failure the service reported with HTTP 200, so it must not pass
// as an answer.
func (systemOne) decode(body []byte) (reply, error) {
	var out soResponse
	if err := sonic.Unmarshal(body, &out); err != nil {
		return reply{}, fmt.Errorf("decode reply: %w (%s)", err, snippet(body))
	}
	if out.Code != 0 {
		return reply{}, fmt.Errorf("decision api code %d: %s", out.Code, out.Message)
	}
	return out.normalize(), nil
}

// ---- request assembly ------------------------------------------------------

// buildCriteria shapes a question's declared answers into the API's criteria
// for its type. Every description is model-facing text, so it is a template
// like the instructions are: res resolves its {{$.a.b}} tokens, which is how a
// criterion can quote a threshold or a policy held in the flow context. An
// empty description falls back to the option's name so the model always has
// something to score against — the criteria are a map whose keys are the option
// names, and a key with no value would leave the model nothing to match.
func buildCriteria(res resolver, q Question) any {
	switch q.Type {
	case typeChoice:
		m := make(map[string]string, len(q.Options))
		for _, o := range q.Options {
			m[o.Name] = res(descOrName(o))
		}
		return m
	case typeScore:
		levels := make([]string, len(q.Options))
		for i, o := range q.Options {
			levels[i] = res(descOrName(o))
		}
		return levels
	case typeNoul:
		m := map[string]string{"true": "Yes", "false": "No"}
		for _, o := range q.Options {
			if d := strings.TrimSpace(o.Description); d != "" {
				m[map[string]string{noulYes: "true", noulNo: "false"}[o.Name]] = res(d)
			}
		}
		return m
	}
	return nil
}

// buildRequest assembles the wire request from the already-assembled state and
// the validated questions. The questions are templates too — res resolves the
// instructions (at any depth, for the structured form) and every criteria
// description — so a question reaches the service with its flow data in it
// rather than with the token text the designer typed.
func (systemOne) buildRequest(res resolver, model string, state any, qs []Question) any {
	req := soRequest{State: state, Model: model, Questions: make(map[string]soQuestion, len(qs))}
	for _, q := range qs {
		req.Questions[q.ID] = soQuestion{
			Type:         q.Type,
			Instructions: resolveTemplateValue(res, q.Instructions),
			Criteria:     buildCriteria(res, q),
		}
	}
	return req
}
