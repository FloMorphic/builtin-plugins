// Package decisionnode implements the Inflow "AI Decision" plugin node: a
// single `run` action that evaluates a block of state — plus any evidence the
// designer injects — against a set of typed questions on a System One decision
// model, and routes the flow by the answers.
//
// The node speaks one protocol, POST /v1/systemone, and two models serve it:
// TypeSafe's hosted Jev, and Laya, the open local-first System One model. Their
// request and reply shapes are the same, so which one answers is a property of
// the settings profile (see DecisionSettings and validateSettings), not of the
// node — which is why the node is named for what it does rather than for one
// vendor.
//
// A System One model is a decider, not a reasoner: it takes state plus
// questions whose valid answers are declared up front, and returns a calibrated
// probability over every declared answer — never free text, never an answer
// that was not listed. That is the same contract the LLM node enforces at the
// runtime layer with bound functions (each function is an outbound port; the
// model can only pick one of them), so this node maps onto the canvas the same
// way — the declared answers ARE the ports — with the judgment swapped for a
// distribution.
//
// The node exposes ONE action, `run`. Every request body arrives as the SDK
// envelope { "_registry": {...}, "body": {...} }, and `body` (see RunBody) has
// four parts:
//
//   - settings  : the *settings-profile* the frontend ships per request — the
//     endpoint, the API key, the model id and a timeout. Its shape is
//     DecisionSettings.
//
//   - state     : the subject to decide on — the case, the ticket, the message.
//     A text template that may embed {{$.a.b}} variables resolved against the
//     live flow context via job.CmdGetScope (see resolveState). A template that
//     is exactly one {{$...}} token whose value is JSON is sent as that JSON
//     value (object, array, …) rather than as a string, since the API accepts
//     both.
//
//   - evidence  : the optional reference material the decision rests on — the
//     retrieval side of a RAG decision. Rows of {source, text}, both templates
//     (see EvidenceItem).
//
//   - questions : the typed questions, each with the answers the designer
//     declared in the settings drawer (see Question). Every question has an
//     `id`, a `type` (choice / score / noul), the `instructions` the state is
//     evaluated against, and its `options` — rows of {name, description}, the
//     same shape as the LLM node's bound functions. What each row means depends
//     on the type:
//
//     choice : one row per selectable option (the API's name→description map);
//     score  : the ordered levels, low to high (2–10 rows);
//     noul   : the two rows "yes" and "no" (descriptions only; optional).
//
// How evidence reaches the model: it is NOT a second input to the service. The
// request body is only {model, state, questions} — there is no evidence
// parameter — so assembleState folds the rows into the state itself. With no
// rows, the state is sent exactly as the template resolved, which is what the
// node always did; with rows, the state becomes
//
//	{"case": <state>, "evidence": [{"source": …, "text": …}, …]}
//
// and a question points at either part by backticked dot-and-index path
// (`case.problem`, `evidence[0].text`). Keep both filtered: the state and all
// the questions share one context budget (~64k tokens, state plus the longest
// question inside ~32k) and accuracy FALLS as the state fills with material the
// questions do not need.
//
// Templates: everything the designer authors is resolved before the call — the
// state, each evidence row, each question's instructions (at any depth, for the
// structured object form the API also accepts) and each option's description
// (see resolveTemplateValue). What is NOT re-resolved is a value fetched from
// the scope: templates are authored on the canvas, while a scope value is
// runtime data, and re-resolving it would let that data read the flow context.
//
// Routing: a question with `route` on (the default) turns its options into the
// node's OUTBOUND PORTS. Each option's port tag is "<question id>.<option name>"
// — prefixed, so two questions that both declare "high" never collide. After the
// call, the top answer of every routed question is translated to its tag and
// the set is handed to the runtime with job.CmdNextFilter, so only the edges
// carrying those tags fire next. Several routed questions ⇒ several tags ⇒ the
// flow continues down one branch per question, exactly like an LLM turn that
// calls several tools. A question with `route` off contributes no ports; its
// answer still lands in the node output. Evidence rows derive no ports at all —
// they are input, not an outcome. No routed question ⇒ no CmdNextFilter ⇒ the
// flow follows its default route.
//
// The top answer is: the `choice` for a choice question; the level with the
// highest probability for a score question; "yes" when p ≥ 0.5 for a noul.
// Unlike an LLM, the model cannot decline to answer — it always places the state
// in one of the declared options — so a "none of these" case must be drawn by
// the designer as an option (e.g. "other"), which is then just another port.
//
// Exception routing: the node also has an implicit `_exception` port, fired
// (CmdNextFilter) before ending the job with DoneWithErrorData, so the node is
// marked failed while the flow carries on through downstream nodes tagged
// `_exception`. The cases, each reported as `code`:
//
//   - provider_error   : the API call failed (bad key, validation, rate limit,
//     overload after retries, network, unreadable reply);
//   - no_answer        : the reply carries no answer for a routed question;
//   - unbound_option   : an answer names an option that was not declared
//     (defensive — the model's output layer should make this impossible);
//   - low_confidence   : a routed question set `min_confidence` and its top
//     answer's confidence fell below it. The threshold is optional (0 = off);
//     the full distribution is always in the output, so a rule node after this
//     one can apply its own policy instead.
//
// Config mistakes (unreadable body, a missing key on the hosted endpoint, a
// missing model on a custom one, a question with no id or a bad option count, an
// evidence row with no text, nothing to decide on at all) are not exceptions:
// they fail with a plain DoneWithError and no routing.
//
// Runtime scenario for `run`:
//  1. Validate the settings-profile, every question's shape and every evidence
//     row.
//  2. Resolve the templates against the live flow context and assemble the one
//     `state` value the API takes.
//  3. POST state + questions to the System One endpoint (one call; the
//     questions are evaluated in parallel server-side). 429/529 are retried
//     with a short bounded backoff.
//  4. Turn each answer into a Decision (top answer, its tag, the distribution
//     keyed by option name, the confidence).
//  5. Route the routed questions' tags and report every decision under
//     "answers" in job.Done — that is what lands on the node's scope.
package decisionnode
