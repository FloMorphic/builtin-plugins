// Package llmnode implements the Inflow "LLM" plugin node: a single `run`
// action that drives one turn of an LLM conversation held on the node's scope.
//
// The node exposes ONE action, `run`. Every request body arrives as the SDK
// envelope { "_registry": {...}, "body": {...} }, and `body` (see RunBody) has
// three parts:
//
//   - settings : the *settings-profile* the frontend ships per request (e.g. a
//     "gemini-config" profile) — the per-provider config needed to talk to an
//     LLM. Its required shape is LLMSettings.
//   - messages : the INIT prompt template — an ordered array of role-tagged
//     messages ({role, content}), typically a "system" message and a "user"
//     turn. Content may embed {{$.a.b}} variables resolved against the live flow
//     context via job.CmdGetScope before use (see resolveVars). It seeds the
//     conversation on the first run only (see seedMessages); on a resumed run the
//     persisted conversation is used as-is and this template is ignored.
//   - functions : the *bound functions* declared in the node's settings drawer.
//     Each bound function is an OUTBOUND PORT of the node — its name is the
//     port's route tag. They are forwarded to the provider as tools, and let the
//     model answer with a tool/function call. A function carries the arguments
//     it takes as `parameters`, the JSON schema the drawer builds per function;
//     what the model fills in comes back as the tool call's raw `arguments`
//     JSON, reported under "tool_calls" on the node output.
//
// Message body & roles come from langchaingo (github.com/tmc/langchaingo/llms):
// the conversation is expressed as []llms.MessageContent — a typed role plus a
// list of content parts — so the node stores canonical role strings on the scope
// ("system"/"user"/"assistant"/"tool") and langchaingo normalizes them per
// provider. Streaming and tool-call accumulation are handled by the library.
//
// Tool routing: when the model replies with a tool-call message type instead of
// plain text, the flow must leave this node through the port matching the called
// function. The SDK does that at runtime with job.CmdNextFilter([]string{name}):
// the called function name IS the route tag. No tool call and no bound function
// ⇒ no CmdNextFilter ⇒ the flow follows its default route.
//
// Exception routing: the node also has an implicit `_exception` port, used for
// the two failures that happen at the provider boundary —
//
//   - the provider/API errors (bad key, quota, rate limit, network, no choices);
//   - functions were bound to the session but the turn carries no port to route
//     on: the model selected none of them, or called a function that was never
//     bound.
//
// Both go through Exception, which routes out of `_exception` (CmdNextFilter) and
// then ends the job with DoneWithErrorData — so the node is marked failed while
// the flow carries on through downstream nodes tagged `_exception`. The failure
// still reports its state: "error" (the reason), "code" (provider_error /
// no_function_selected / unbound_function — what the branch switches on) and the
// full "messages" conversation, plus per-case extras. Reporting the conversation
// is what keeps it on the node's scope, since a terminal payload IS the commit.
//
// Config mistakes (unreadable body, incomplete settings-profile, nothing
// sendable) are not exceptions: they fail with a plain DoneWithError and no
// routing.
//
// Runtime scenario for `run`:
//  1. Read this node's current scope (job.CmdGetCurrentScope).
//  2. If it already has a non-empty `messages` array → this is a *resumed* run:
//     the persisted conversation is used as-is and the body's init template is
//     ignored. Otherwise it's the first run: seed the conversation from the init
//     template (resolve {{$...}} vars in each message; drop empties).
//  3. Stream the completion from the provider named in settings via langchaingo;
//     each streamed token is surfaced through job.Progress, capped below 100
//     (progress==100 marks the job done).
//  4. Append the assistant reply and commit the whole array back to the current
//     scope by reporting it under the "messages" key of job.Done.
package llmnode
