package decisionnode

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/bytedance/sonic"
)

// ---- the Decisions dialect: POST /v1/decisions -----------------------------
//
// OpenAI's Decisions API (public beta since DevDay, 2026-09-29), and any
// gateway that implements its request and response shapes — Vercel's AI Gateway
// does, and routes TypeSafe's Jev through it as well, which is why this dialect
// is named after the protocol rather than after OpenAI.
//
// It asks the same questions as System One and answers them with the same kind
// of distribution, so everything downstream of this file is shared. What
// differs is only the encoding:
//
//	           System One                 Decisions
//	subject    "state"                    "input"
//	questions  object keyed by id         array, each with "name"
//	boolean    "noul"  + criteria         "predicate", no criteria at all
//	choice     criteria {name: desc}      "choices" [{value, description}]
//	score      criteria [desc, …]         "levels"  [{label, description}]
//	answers    object keyed by id         array, echoing "name"
//	choice p   {name: p}                  [{value, probability}]
//	score p    {index: p} + legend        [{value, label, probability}]
//	refusal    —                          {"type":"refusal","name":…}

const (
	decisionsBaseURL = "https://api.openai.com"
	decisionsPath    = "/v1/decisions"
	// gpt-6-luna is the ONLY model the Decisions API serves in public beta, so
	// it is the dialect's default rather than something a profile has to supply.
	// It is still only assumed against decisionsBaseURL: a gateway slugs the
	// same model differently (Vercel's is "openai/gpt-6-luna-decisions"), which
	// is exactly why validateSettings requires an explicit model once a profile
	// names its own URL.
	decisionsModel = "gpt-6-luna"

	// The published shape allows 2..10 ordered levels and 1..255 choices. The
	// level ceiling is the one worth enforcing here, since exceeding it is a
	// drawer mistake rather than a service difference.
	decisionsMaxScoreLevels = 10

	// The protocol's name for a boolean question. The node keeps calling it
	// "noul" internally — the vocabulary the drawer and the port tags already
	// use — and translates only on the wire.
	typePredicate = "predicate"
	// The answer type a declined question comes back as, in that question's
	// position, while the others answer normally.
	typeRefusal = "refusal"
)

type decisions struct{}

func (decisions) name() string           { return providerDecisions }
func (decisions) path() string           { return decisionsPath }
func (decisions) defaultBaseURL() string { return decisionsBaseURL }
func (decisions) defaultModel() string   { return decisionsModel }

func (decisions) maxScoreLevels(DecisionSettings) int { return decisionsMaxScoreLevels }

// retryable follows what OpenAI's own SDKs retry: rate limits and any server
// error. A decision call has no side effect — it reads a state and returns a
// distribution — so re-sending one is safe in the sense that matters here, and
// the profile's MaxRetries still bounds how much a flow will wait for it.
func (decisions) retryable(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// ---- wire shapes -----------------------------------------------------------

// decRequest is the whole request body: the model, the shared input every
// question is asked against, and the questions in order.
type decRequest struct {
	Model     string        `json:"model"`
	Input     any           `json:"input"`
	Questions []decQuestion `json:"questions"`
}

// decQuestion is one question on the wire. Name is the question id, which the
// reply echoes back and which this node keys its answers on. Only the field of
// the question's own type is sent: Choices for a choice, Levels for a score,
// and neither for a predicate — which takes no criteria of any kind.
type decQuestion struct {
	Type         string      `json:"type"`
	Name         string      `json:"name"`
	Instructions string      `json:"instructions"`
	Choices      []decChoice `json:"choices,omitempty"`
	Levels       []decLevel  `json:"levels,omitempty"`
}

// decChoice is one declared option. Value is the option name — the identity the
// node routes on — and Description is optional model-facing text. Unlike System
// One's criteria map, a blank description is simply left off: the model already
// sees the name in Value, so there is nothing to fall back to.
type decChoice struct {
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
}

// decLevel is one ordered level of a score question, lowest first (index 0).
type decLevel struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// decResponse is the reply: the model that answered, one answer per question in
// question order, and the token usage. Error carries a failure the service
// described in the body (the OpenAI error envelope), which is checked even on a
// 2xx so a reported failure cannot pass as an answer.
type decResponse struct {
	Model   string         `json:"model"`
	Answers []decAnswer    `json:"answers"`
	Usage   map[string]any `json:"usage"`
	Error   *decError      `json:"error"`
}

// decError is the OpenAI error envelope. Param names the field that failed
// validation, which is the most useful part of a 400 and worth surfacing.
type decError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   any    `json:"param"`
	Code    any    `json:"code"`
}

// decAnswer is the union of the answer shapes; only the fields of the answer's
// Type are set.
//
// Name is a pointer because the gateway allows unnamed questions and returns
// null for them. This node always sends the question id as the name, so an
// answer without one cannot be matched to a question and is dropped — which
// surfaces as a readable no_answer for a routed question rather than as a
// silently misrouted one.
//
// Choice is `any` because the protocol permits boolean choice values as well as
// strings. This node only ever declares string options, so a string is what
// comes back; decoding it loosely means a boolean would be stringified rather
// than fail the whole reply.
type decAnswer struct {
	Type          string    `json:"type"`
	Name          *string   `json:"name"`
	Probability   *float64  `json:"probability"` // predicate
	Choice        any       `json:"choice"`      // choice: string | bool
	Score         *float64  `json:"score"`       // score: probability-weighted level index
	Confidence    *float64  `json:"confidence"`
	Probabilities []decProb `json:"probabilities"`
}

// decProb is one row of a distribution. For a choice, Value is the option name;
// for a score it is the level INDEX, with Label naming that level.
type decProb struct {
	Value       any     `json:"value"`
	Label       string  `json:"label"`
	Probability float64 `json:"probability"`
}

// decode reads a 2xx body into the normalized reply.
func (decisions) decode(body []byte) (reply, error) {
	var out decResponse
	if err := sonic.Unmarshal(body, &out); err != nil {
		return reply{}, fmt.Errorf("decode reply: %w (%s)", err, snippet(body))
	}
	if out.Error != nil && strings.TrimSpace(out.Error.Message) != "" {
		return reply{}, fmt.Errorf("decision api error (%s): %s", out.Error.Type, out.Error.Message)
	}
	r := reply{Model: out.Model, Usage: out.Usage}
	if len(out.Answers) > 0 {
		r.Answers = make(map[string]answer, len(out.Answers))
		for _, a := range out.Answers {
			if a.Name == nil || strings.TrimSpace(*a.Name) == "" {
				continue // unmatchable: this node always sends a name
			}
			r.Answers[*a.Name] = a.answer()
		}
	}
	return r, nil
}

// answer lifts one wire answer into the protocol-independent shape, re-keying
// each distribution the way decide() reads it: by option name for a choice, and
// by level index for a score.
func (a decAnswer) answer() answer {
	switch strings.ToLower(strings.TrimSpace(a.Type)) {
	case typeRefusal:
		return answer{Type: typeRefusal, Refused: true}

	case typePredicate, typeNoul:
		// A predicate reports only the probability, with no confidence field;
		// decide() derives one the same way it does for a System One noul, as
		// the distance from the coin flip.
		return answer{Type: typeNoul, P: a.Probability}

	case typeChoice:
		probs := make(map[string]float64, len(a.Probabilities))
		for _, p := range a.Probabilities {
			if name := choiceText(p.Value); name != "" {
				probs[name] = p.Probability
			}
		}
		return answer{
			Type:          typeChoice,
			Choice:        choiceText(a.Choice),
			Probabilities: probs,
			Confidence:    a.Confidence,
		}

	case typeScore:
		probs := make(map[string]float64, len(a.Probabilities))
		for i, p := range a.Probabilities {
			// Prefer the reported index; fall back to the row's position, since
			// the rows arrive in level order.
			idx, ok := levelIndex(p.Value)
			if !ok {
				idx = strconv.Itoa(i)
			}
			probs[idx] = p.Probability
		}
		return answer{
			Type:          typeScore,
			Score:         a.Score,
			Probabilities: probs,
			Confidence:    a.Confidence,
		}
	}
	// An unknown answer type carries no distribution this node can route on;
	// decide() reports it as an unbound option rather than guessing.
	return answer{Type: normalizeType(a.Type)}
}

// choiceText renders a choice value as the option name the node routes on. The
// protocol allows booleans as well as strings; this node declares only strings,
// so the other cases exist so a surprise is stringified rather than fatal.
func choiceText(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return ""
	default:
		return fmt.Sprint(t)
	}
}

// levelIndex reads a score row's level index as the decimal string decide()
// keys its score distributions by. JSON numbers decode to float64.
func levelIndex(v any) (string, bool) {
	switch t := v.(type) {
	case float64:
		return strconv.Itoa(int(t)), true
	case int:
		return strconv.Itoa(t), true
	case string:
		if _, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
			return strings.TrimSpace(t), true
		}
	}
	return "", false
}

// ---- request assembly ------------------------------------------------------

// buildRequest assembles the wire request. The questions are templates — res
// resolves the instructions and every option description — so a question
// reaches the service with its flow data in it rather than with the token text
// the designer typed.
func (decisions) buildRequest(res resolver, model string, state any, qs []Question) any {
	req := decRequest{Model: model, Input: state, Questions: make([]decQuestion, 0, len(qs))}
	for _, q := range qs {
		wq := decQuestion{
			Type:         decisionsType(q.Type),
			Name:         q.ID,
			Instructions: instructionsText(res, q.Instructions),
		}
		switch q.Type {
		case typeChoice:
			wq.Choices = make([]decChoice, 0, len(q.Options))
			for _, o := range q.Options {
				wq.Choices = append(wq.Choices, decChoice{
					Value:       o.Name,
					Description: res(strings.TrimSpace(o.Description)),
				})
			}
		case typeScore:
			wq.Levels = make([]decLevel, 0, len(q.Options))
			for _, o := range q.Options {
				wq.Levels = append(wq.Levels, decLevel{
					Label:       o.Name,
					Description: res(strings.TrimSpace(o.Description)),
				})
			}
		case typeNoul:
			// A predicate takes no criteria, so a noul's yes/no descriptions
			// have nowhere to go on this protocol. They are folded into the
			// instructions instead (see instructionsText → noulGuidance) rather
			// than dropped, which would quietly change what the question means.
			wq.Instructions = withNoulGuidance(res, wq.Instructions, q.Options)
		}
		req.Questions = append(req.Questions, wq)
	}
	return req
}

// decisionsType maps the node's question vocabulary onto the protocol's names.
// Only the boolean type is spelled differently.
func decisionsType(t string) string {
	if t == typeNoul {
		return typePredicate
	}
	return t
}

// instructionsText renders a question's instructions as the string this
// protocol takes. The structured object/array form — which System One accepts
// natively, and which carries reference data the question cites by backticked
// name — is serialized to JSON text, so the citation idiom keeps working: it is
// a convention about the text the model reads, not about the encoding.
func instructionsText(res resolver, v any) string {
	switch t := resolveTemplateValue(res, v).(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		if raw, err := sonic.Marshal(t); err == nil {
			return string(raw)
		}
		return fmt.Sprint(t)
	}
}

// withNoulGuidance appends a noul question's yes/no descriptions to its
// instructions, which is the only place they can go on a protocol whose
// predicate takes no criteria. A question with no descriptions is left exactly
// as it was.
func withNoulGuidance(res resolver, instructions string, opts []Option) string {
	var lines []string
	for _, o := range opts {
		if d := strings.TrimSpace(o.Description); d != "" {
			lines = append(lines, fmt.Sprintf("%s: %s", o.Name, res(d)))
		}
	}
	if len(lines) == 0 {
		return instructions
	}
	return instructions + "\n\n" + strings.Join(lines, "\n")
}
