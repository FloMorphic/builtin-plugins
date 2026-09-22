package jevnode

// JevSettings is the shape the frontend settings-profile must produce. Collect
// these into a profile (e.g. "jev-config") and ship them in body.settings. Only
// the access token is required; the model defaults to defaultModel and the base
// URL to the service's own endpoint.
type JevSettings struct {
	AccessToken    string `json:"access_token"`    // bearer API key
	Model          string `json:"model"`           // model id, e.g. "typesafe/jev-1.13"; empty ⇒ defaultModel
	URL            string `json:"url"`             // optional custom *base* URL; empty ⇒ defaultBaseURL
	TimeoutSeconds int    `json:"timeout_seconds"` // optional per-call timeout; ≤0 ⇒ defaultTimeout
}

// Option is one declared answer of a question, as collected by the settings
// drawer — the same {name, description} row an LLM bound function has, and for
// the same reason: Name is the identity the runtime routes on (it becomes the
// option's outbound-port tag, prefixed by the question id), Description is the
// model-facing text Jev uses to decide whether the state matches it.
//
// For a `score` question the rows are the ordered levels (index 0 is the lowest)
// and Description is what the API scores against — an empty description falls
// back to the name. For a `noul` question the rows are named "yes" and "no"
// (also accepted: "true"/"false") and carry only the descriptions; both rows are
// optional.
type Option struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Question is one typed question the node asks of the state. ID is the key the
// answer comes back under and the prefix of every port tag this question
// derives ("<id>.<option>"). Type selects the API question type: "choice",
// "score" or "noul". Instructions is the evaluation prompt. Options are the
// declared answers (see Option).
//
// Route decides whether this question's options are outbound ports of the node
// (nil/true) or the answer is data only (false). MinConfidence, when > 0, is a
// floor on the top answer's confidence below which the node routes `_exception`
// (code low_confidence) instead of the answer's port; it only applies to routed
// questions.
type Question struct {
	ID            string   `json:"id"`
	Type          string   `json:"type"`
	Instructions  string   `json:"instructions"`
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

// RunBody is the body of the `run` action request (the inner `body` of the
// envelope). State is the text template of the content to evaluate — it may
// embed {{$...}} vars (see resolveState). Questions are the typed questions with
// their declared answers.
type RunBody struct {
	Settings  JevSettings `json:"settings"`  // fed by the settings-profile
	State     string      `json:"state"`     // content to evaluate; {{$.a.b}} tokens resolved per run
	Questions []Question  `json:"questions"` // typed questions; routed ones derive outbound ports
}

// Decision is one question's answer, reduced to what the flow needs: the top
// answer, the tag it routes on, the full distribution re-keyed by option name,
// and the confidence the threshold and downstream rule nodes read. Score and P
// carry the type-specific raw values (the continuous score of a score question;
// the yes-probability of a noul) for readers who want them. Every Decision is
// reported under "answers" in the run's Done payload, keyed by question id.
type Decision struct {
	Question      string             `json:"question"`
	Type          string             `json:"type"`
	Answer        string             `json:"answer"`          // top option name
	Tag           string             `json:"tag"`             // "<question>.<answer>" — the port tag
	Confidence    float64            `json:"confidence"`      // calibrated confidence of Answer
	Probabilities map[string]float64 `json:"probabilities"`   // option name → probability
	Score         *float64           `json:"score,omitempty"` // score question: continuous score
	P             *float64           `json:"p,omitempty"`     // noul question: probability of "yes"
	Routed        bool               `json:"routed"`          // whether Tag was fired as a next-filter
}

// ---- wire shapes of POST /v1/systemone ------------------------------------

// apiRequest is the System One request body: the state, the model, and the
// questions keyed by id. Questions run in parallel against the same state.
type apiRequest struct {
	State     any                    `json:"state"`
	Model     string                 `json:"model"`
	Questions map[string]apiQuestion `json:"questions"`
}

// apiQuestion is one question on the wire. Criteria's shape depends on Type:
// a name→description map for choice, an ordered []string of level descriptions
// for score, and a {"true": …, "false": …} map for noul (see buildCriteria).
type apiQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

// apiResponse is the reply, decoded permissively because the service and the
// published reference disagree on where the answers sit. The live endpoint
// wraps them — {"code":0,"message":"ok","data":{"result":{…},"creditsUsed":1}}
// — while docs.typesafe.ai's API reference shows the same document flat at the
// top level. Both are decoded here and reconciled by reply(): Data wins when
// present, the flat fields are the fallback, so neither shape breaks the node.
type apiResponse struct {
	// enveloped form (what api returns today)
	Code    int      `json:"code"`
	Message string   `json:"message"`
	Data    *apiData `json:"data"`

	// flat form (the published reference)
	Model   string               `json:"model"`
	Answers map[string]apiAnswer `json:"answers"`
	Usage   map[string]any       `json:"usage"`
}

// apiData is the envelope's payload: the answer document plus what the call
// cost. CreditsUsed is worth carrying to the canvas — a decision node runs on a
// metered budget, and the run that spent the credit is the only place that can
// report it.
type apiData struct {
	Result      apiResult `json:"result"`
	CreditsUsed int       `json:"creditsUsed"`
}

// apiResult is the answer document itself: one answer per question id, the
// exact model version that answered, the token usage and the service-side
// latency.
type apiResult struct {
	Model     string               `json:"model"`
	Answers   map[string]apiAnswer `json:"answers"`
	Usage     map[string]any       `json:"usage"`
	ElapsedMs int                  `json:"elapsedMs"`
}

// reply is the normalized result the handler works with, whichever shape the
// service answered in.
type reply struct {
	Model       string
	Answers     map[string]apiAnswer
	Usage       map[string]any
	ElapsedMs   int
	CreditsUsed int
}

// reply reconciles the two shapes: the envelope when the service sent one,
// otherwise the flat document.
func (r apiResponse) reply() reply {
	if r.Data != nil {
		return reply{
			Model:       r.Data.Result.Model,
			Answers:     r.Data.Result.Answers,
			Usage:       r.Data.Result.Usage,
			ElapsedMs:   r.Data.Result.ElapsedMs,
			CreditsUsed: r.Data.CreditsUsed,
		}
	}
	return reply{Model: r.Model, Answers: r.Answers, Usage: r.Usage}
}

// apiAnswer is the union of the three answer shapes; only the fields of the
// answer's Type are set. Choice/score carry Probabilities (keyed by option name,
// or by level index for score, with Legend mapping index → description) and a
// Confidence; noul carries only Noul, the probability the statement is true.
type apiAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Legend        map[string]string  `json:"legend,omitempty"`
	Noul          *float64           `json:"noul,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
}
