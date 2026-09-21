// Package jevnode implements the Inflow "Jev" plugin node: a single `run`
// action that evaluates a block of state against a set of typed questions on
// TypeSafe's System One model (Jev) and routes the flow by the answers.
//
// Jev is a decider, not a reasoner: it takes state plus questions whose valid
// answers are declared up front, and returns a calibrated probability over every
// declared answer — never free text, never an answer that was not listed. That
// is the same contract the LLM node enforces at the runtime layer with bound
// functions (each function is an outbound port; the model can only pick one of
// them), so this node maps onto the canvas the same way — the declared answers
// ARE the ports — with the judgment swapped for a distribution.
//
// The node exposes ONE action, `run`. Every request body arrives as the SDK
// envelope { "_registry": {...}, "body": {...} }, and `body` (see RunBody) has
// three parts:
//
//   - settings  : the *settings-profile* the frontend ships per request — the
//     API key, the model id and an optional base URL. Its shape is JevSettings.
//
//   - state     : the content to evaluate. A text template that may embed
//     {{$.a.b}} variables resolved against the live flow context via
//     job.CmdGetScope (see resolveState). A template that is exactly one
//     {{$...}} token whose value is JSON is sent as that JSON value (object,
//     array, …) rather than as a string, since Jev accepts both.
//
//   - questions : the typed questions, each with the answers the designer
//     declared in the settings drawer (see Question). Every question has an
//     `id`, a `type` (choice / score / noul), the `instructions` Jev evaluates
//     the state against, and its `options` — rows of {name, description}, the
//     same shape as the LLM node's bound functions. What each row means depends
//     on the type:
//
//     choice : one row per selectable option (the API's name→description map);
//     score  : the ordered levels, low to high (2–10 rows);
//     noul   : the two rows "yes" and "no" (descriptions only; optional).
//
// Routing: a question with `route` on (the default) turns its options into the
// node's OUTBOUND PORTS. Each option's port tag is "<question id>.<option name>"
// — prefixed, so two questions that both declare "high" never collide. After the
// call, the top answer of every routed question is translated to its tag and
// the set is handed to the runtime with job.CmdNextFilter, so only the edges
// carrying those tags fire next. Several routed questions ⇒ several tags ⇒ the
// flow continues down one branch per question, exactly like an LLM turn that
// calls several tools. A question with `route` off contributes no ports; its
// answer still lands in the node output. No routed question at all ⇒ no
// CmdNextFilter ⇒ the flow follows its default route.
//
// The top answer is: the `choice` for a choice question; the level with the
// highest probability for a score question; "yes" when p ≥ 0.5 for a noul.
// Unlike an LLM, Jev cannot decline to answer — it always places the state in
// one of the declared options — so a "none of these" case must be drawn by the
// designer as an option (e.g. "other"), which is then just another port.
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
//     (defensive — Jev's output layer should make this impossible);
//   - low_confidence   : a routed question set `min_confidence` and its top
//     answer's confidence fell below it. The threshold is optional (0 = off);
//     the full distribution is always in the output, so a rule node after this
//     one can apply its own policy instead.
//
// Config mistakes (unreadable body, missing API key, a question with no id or a
// bad option count, an empty state) are not exceptions: they fail with a plain
// DoneWithError and no routing.
//
// Runtime scenario for `run`:
//  1. Validate the settings-profile and every question's shape.
//  2. Resolve the state template against the live flow context.
//  3. POST state + questions to the System One endpoint (one call; the
//     questions are evaluated in parallel server-side). 429/529 are retried
//     with a short bounded backoff.
//  4. Turn each answer into a Decision (top answer, its tag, the distribution
//     keyed by option name, the confidence).
//  5. Route the routed questions' tags and report every decision under
//     "answers" in job.Done — that is what lands on the node's scope.
package jevnode
