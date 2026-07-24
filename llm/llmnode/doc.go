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
//   - prompt / system_prompt : this turn's user prompt and the system/init
//     prompt. Both may embed {{$.a.b}} variables resolved against the live flow
//     context via job.CmdGetScope before use (see resolveVars).
//   - functions : the *bound functions* declared in the node's settings drawer.
//     Each bound function is an OUTBOUND PORT of the node — its name is the
//     port's route tag. They are forwarded to the provider as tools, and let the
//     model answer with a tool/function call.
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
// the called function name IS the route tag. No tool call ⇒ no CmdNextFilter ⇒
// the flow follows its default route.
//
// Runtime scenario for `run`:
//  1. Read this node's current scope (job.CmdGetCurrentScope).
//  2. If it already has a non-empty `messages` array → this is a *resumed* run;
//     otherwise it's the first run and messages are seeded.
//  3. Resolve {{$...}} vars in the system prompt and place it at messages[0]
//     with the canonical "system" role.
//  4. Resolve vars in the user prompt and append it as a "user" message.
//  5. Stream the completion from the provider named in settings via langchaingo;
//     each streamed token is surfaced through job.Progress, capped below 100
//     (progress==100 marks the job done).
//  6. Append the assistant reply and commit the whole array back to the current
//     scope with job.CmdSetOnPath("messages", ...).
package llmnode
