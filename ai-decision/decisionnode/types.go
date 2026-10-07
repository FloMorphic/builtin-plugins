package decisionnode

// DecisionSettings is the shape the frontend settings-profile must produce.
// Collect these into a profile (e.g. "jev-config", "laya-local", "luna") and
// ship them in body.settings.
//
// The node speaks TWO decision protocols, picked by Provider, because the
// decision-model space settled into two wire shapes rather than one:
//
//   - "systemone" — POST /v1/systemone: TypeSafe's hosted Jev, the local-first
//     Laya, OpenJev, and Ollama's local deciders (nimble, tev1).
//   - "decisions" — POST /v1/decisions: OpenAI's Decisions API, and any gateway
//     that implements its shape (Vercel's AI Gateway does, and routes Jev
//     through it too).
//
// Which SERVICE answers is a property of the profile, not of the node, and so
// is which PROTOCOL it speaks. Everything downstream of the dialect — the typed
// questions, the declared options, the "<question>.<option>" port tags, the
// confidence floor, the `_exception` branch — is identical either way, so a
// flow can be re-pointed from a local Nimble to hosted Jev to gpt-6-luna by
// swapping the profile, with the canvas wiring untouched.
//
// Within a protocol the deployment still decides what the profile must carry:
//
//   - default endpoint : AccessToken only; URL and Model may stay empty (the
//     defaults are that protocol's first-party host and model).
//   - another endpoint : URL pointing at the server and the Model it serves;
//     AccessToken may be empty, since a local endpoint usually has no key.
//
// Because a model id is only guessable for the default host, Model is REQUIRED
// whenever URL names another endpoint (see validateSettings): "jev-latest"
// means nothing to a Laya server, and "gpt-6-luna" means nothing to a gateway
// that slugs the same model "openai/gpt-6-luna-decisions".
type DecisionSettings struct {
	// Provider selects the wire protocol: "systemone" (the default when empty)
	// or "decisions". Vendor names are accepted as aliases — see dialectOf —
	// but the canonical values name the PROTOCOL rather than a vendor, because
	// each shape already carries several vendors and will carry more.
	//
	// Empty MUST keep meaning "systemone": settings profiles are snapshotted
	// onto nodes when applied, so every flow deployed before this field existed
	// carries provider:"" forever and has to go on behaving as it did.
	Provider string `json:"provider"`

	AccessToken    string `json:"access_token"`    // bearer API key; omitted from the request when empty (local endpoints)
	Model          string `json:"model"`           // model id, e.g. "typesafe/jev-1.13", "gpt-6-luna"; empty ⇒ the dialect's default (default host only)
	URL            string `json:"url"`             // optional custom *base* URL; empty ⇒ the dialect's default host
	TimeoutSeconds int    `json:"timeout_seconds"` // optional per-call timeout; ≤0 ⇒ defaultTimeout

	// MaxRetries is how many FURTHER attempts a failed call gets, and only for
	// the statuses the protocol asks callers to back off on (see
	// dialect.retryable — 429/529 for System One, 429 and any 5xx for the
	// Decisions API, which is what OpenAI's own SDKs retry).
	//
	// How reliable a particular endpoint is — a shared key against a
	// rate-limited hosted service, a local Nimble with nothing in front of it —
	// is a property of the connection rather than of the decision, which is
	// why it belongs to the profile, exactly as it does on the LLM node.
	//
	// It is a POINTER so that "never retry" and "unset" are different things:
	// nil (absent from the profile) takes DefaultMaxRetries, and an explicit 0
	// turns retrying off. An int could not express both, which would leave a
	// settings form with no way to say "decide once, or not at all" — a real
	// choice for a flow where a late decision is worse than no decision.
	MaxRetries *int `json:"max_retries"`
}

// Option is one declared answer of a question, as collected by the settings
// drawer — the same {name, description} row an LLM bound function has, and for
// the same reason: Name is the identity the runtime routes on (it becomes the
// option's outbound-port tag, prefixed by the question id), Description is the
// model-facing text the service uses to decide whether the state matches it.
//
// For a `score` question the rows are the ordered levels (index 0 is the lowest)
// and Description is what the API scores against. For a `noul` question the rows
// are named "yes" and "no" (also accepted: "true"/"false") and carry only the
// descriptions; both rows are optional.
//
// How a blank Description is treated is the one place the two protocols differ
// visibly. System One names its options as criteria KEYS and wants a value for
// each, so an empty description falls back to the name; the Decisions API
// carries the name as the choice's `value` and treats `description` as
// optional, so a blank one is simply left off the wire. The model sees the
// option name either way.
type Option struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Question is one typed question the node asks of the state. ID is the key the
// answer comes back under and the prefix of every port tag this question
// derives ("<id>.<option>"). Type selects the question type in the node's own
// vocabulary — "choice", "score" or "noul" — which each dialect maps onto its
// protocol's name ("noul" is the Decisions API's "predicate").
//
// Instructions is what to decide about the state, and System One accepts it in
// either of two forms, so this field is typed `any`:
//
//   - a string — the plain question ("Which team should handle this?");
//
//   - a JSON object (or array) — the question in one field and the reference
//     data it needs in the others, which the question then cites by BACKTICKED
//     name, the same notation that points at a part of the state:
//
//     { "question": "Does this qualify for termination under `policy`?",
//     "policy":   "{{$.kb.termination_policy}}" }
//
// The Decisions API documents `instructions` as a string only, so the decisions
// dialect flattens the structured form to JSON text (see instructionsText). The
// backticked-citation idiom survives that, since it is a convention about the
// text rather than about the encoding.
//
// Either form is a TEMPLATE: every string in it (at any depth, for the object
// form) has its {{$.a.b}} tokens resolved against the live flow context before
// the call — see resolveTemplateValue. The same is true of each Option's
// Description, which is the model-facing criteria text.
//
// Route decides whether this question's options are outbound ports of the node
// (nil/true) or the answer is data only (false). MinConfidence, when > 0, is a
// floor on the top answer's confidence below which the node routes `_exception`
// (code low_confidence) instead of the answer's port; it only applies to routed
// questions.
type Question struct {
	ID            string   `json:"id"`
	Type          string   `json:"type"`
	Instructions  any      `json:"instructions"` // string | object | array — see above
	Options       []Option `json:"options"`
	Route         *bool    `json:"route,omitempty"`
	MinConfidence float64  `json:"min_confidence,omitempty"`
}

// routes reports whether the question's options are outbound ports: on unless
// the drawer explicitly turned it off, so a hand-written body without the flag
// routes like the drawer default.
func (q Question) routes() bool {
	return q.Route == nil || *q.Route
}

// EvidenceItem is one chunk of reference material injected into the state —
// the retrieval side of a RAG decision. Text is the chunk itself, a template
// whose {{$.a.b}} tokens are resolved per run (so a retriever node upstream can
// hand each chunk over by path), and Source is where it came from: a file name,
// a URL, a row id. Source is optional but worth filling in, because it travels
// with the chunk into the state and a question can then cite the chunk it means
// by backticked path (`evidence[0].text`) and report which source decided it.
//
// Neither protocol has a separate evidence parameter — System One's body is
// {model, state, questions} and the Decisions API's is {model, input,
// questions} — so evidence is not a second input to the service: assembleState
// folds these rows INTO the subject. Keep them few and filtered: the subject
// and all the questions share one context budget, and accuracy falls as the
// state fills with material the questions do not need.
type EvidenceItem struct {
	Source string `json:"source,omitempty"` // provenance: file, URL, row id — a template
	Text   string `json:"text"`             // the chunk — a template
}

// RunBody is the body of the `run` action request (the inner `body` of the
// envelope).
//
// State is the text template of the subject to evaluate — the case, the ticket,
// the message — and may embed {{$...}} vars (see resolveState). Evidence is the
// optional reference material that supports the decision (see EvidenceItem);
// when any row is present the two are assembled into a single state object,
// {"case": <state>, "evidence": [...]}, which is what the service receives.
// With no evidence rows the state is sent exactly as before, so a body written
// against the single-block shape keeps its behaviour.
//
// Questions are the typed questions with their declared answers.
type RunBody struct {
	Settings  DecisionSettings `json:"settings"`           // fed by the settings-profile
	State     string           `json:"state"`              // the subject; {{$.a.b}} tokens resolved per run
	Evidence  []EvidenceItem   `json:"evidence,omitempty"` // optional RAG chunks folded into the state
	Questions []Question       `json:"questions"`          // typed questions; routed ones derive outbound ports
}

// Decision is one question's answer, reduced to what the flow needs: the top
// answer, the tag it routes on, the full distribution re-keyed by option name,
// and the confidence the threshold and downstream rule nodes read. Score and P
// carry the type-specific raw values (the continuous score of a score question;
// the yes-probability of a noul/predicate) for readers who want them. Every
// Decision is reported under "answers" in the run's Done payload, keyed by
// question id.
type Decision struct {
	Question string `json:"question"`
	Type     string `json:"type"`
	Answer   string `json:"answer"` // top option name
	Tag      string `json:"tag"`    // "<question>.<answer>" — the port tag
	// How sure the service is of Answer — and NOT a calibrated probability of
	// being correct, on either protocol: System One derives it from the spread
	// of the distribution (1 - H(p)/ln(N), so 0 means uniform), and the
	// Decisions API has the model report it, which OpenAI's own guidance says
	// to calibrate against your own labelled examples before trusting. A
	// min_confidence floor tuned on one service does not transfer to the other.
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`   // option name → probability
	Score         *float64           `json:"score,omitempty"` // score question: continuous score
	P             *float64           `json:"p,omitempty"`     // noul question: probability of "yes"
	Routed        bool               `json:"routed"`          // whether Tag was fired as a next-filter
}

// ---- the protocol-independent middle ---------------------------------------

// answer is one question's answer with the wire shape taken off it. Each
// dialect decodes its own protocol into this, and decide() reduces it to a
// Decision without knowing which service replied — that split is what lets the
// routing semantics be written once.
//
// Probabilities is keyed the way decide() reads it: by OPTION NAME for a
// choice, and by LEVEL INDEX ("0", "1", …) for a score, which is how System One
// reports a score natively and what the decisions dialect converts its
// [{value, label, probability}] rows into.
type answer struct {
	Type          string             // the node's vocabulary: choice | score | noul
	Choice        string             // choice: the chosen option name
	Score         *float64           // score: the probability-weighted level
	P             *float64           // noul/predicate: probability of yes
	Probabilities map[string]float64 // choice: name → p; score: level index → p
	Confidence    *float64           // where the service reports one
	Refused       bool               // the model declined this question (Decisions API only)
}

// reply is the normalized result the handler works with, whichever protocol —
// and, within System One, whichever of its two reply shapes — answered.
//
// CreditsUsed and ElapsedMs are only reported by some endpoints (the aggregator
// envelope carries both); they stay zero elsewhere rather than being faked, so
// a canvas reading them shows a real number or nothing.
type reply struct {
	Model       string
	Answers     map[string]answer
	Usage       map[string]any
	ElapsedMs   int
	CreditsUsed int
}
