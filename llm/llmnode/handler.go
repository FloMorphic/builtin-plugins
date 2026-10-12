package llmnode

import (
	"fmt"
	"strings"

	"github.com/Inflowenger/go-plugin-sdk/jobstop"
	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
	"github.com/bytedance/sonic"
)

// Stops holds the node's runs in flight, filed by jobId as each is accepted, so
// a stop for one reaches it — see runHandler. Its other half, Stops.OnSignal,
// belongs on the plugin's signal port, which the binary owns: main registers it.
var Stops jobstop.Registry

// Register wires the LLM node's `run` action onto the plugin. Call it before
// p.Start(). For a run to stop with its flow, the plugin's signal port must also
// carry Stops.OnSignal — see main.
func Register(p *sdkv1.Plugin) {
	p.AddAction(sdkv1.Action{
		Method:         "run",
		Title:          "Run",
		Description:    "Run an LLM turn against the node's conversation using a settings-profile",
		Middleware:     sdkv1.Use(Stops.Middleware),
		RequestHandler: runHandler,
	})
}

// exceptionTag is the route tag of the node's exception port.
const exceptionTag = "_exception"

// The exception codes, named once so the trait table below, the tests and every
// call site agree on their spelling — they are the machine-readable tag an
// exception branch switches on, so a typo in one of them is a branch that never
// matches.
const (
	codeProviderError      = "provider_error"
	codeNoFunctionSelected = "no_function_selected"
	codeUnboundFunction    = "unbound_function"
)

// nodeDelivered reports whether an exception code means the node DID its job.
//
// The three codes leave through the same port and are not the same thing, and
// the difference decides whether the run records an error:
//
// The node did not deliver — provider_error (the API errored, or the reply was
// unreadable, past the retry budget) and unbound_function (the model called a
// function that was never bound to this node). The second is a wire defect: a
// model inventing a tool it was not offered is broken, the run's ledger should
// say so, and stop_on_error SHOULD stop a run at one rather than carry on from a
// turn nothing can act on.
//
// The node delivered — no_function_selected. The model answered; it just
// answered with prose instead of a tool call. That is an ordinary thing for a
// model to do and the designer drew the exception branch for precisely it, so
// routing out of `_exception` IS this node delivering its answer. Nothing is
// lost: `code`, `error` and the full conversation are in the committed payload,
// which is what the branch reads and what the canvas shows.
//
// An unrecognised code is treated as a failure, which is the safer way round: a
// new exception case over-reports rather than silently passing a defect off as a
// decision.
//
// The ai-decision node sorts its five codes by the same question, for the same
// reason — see its nodeDelivered.
func nodeDelivered(code string) bool {
	switch code {
	case codeNoFunctionSelected:
		return true
	case codeProviderError, codeUnboundFunction:
		return false
	}
	return false
}

// Exception ends a run that left through the node's exception port — the API
// errored, or a function-bound session came back with nothing this node can route
// on.
//
// It narrows the outbound ports to the exception tag with CmdNextFilter, so the
// flow does not die here: only downstream nodes carrying `_exception` fire next
// and the process continues down that branch. How it then CONCLUDES depends on
// whether the node delivered — see nodeDelivered, which is the whole of that
// decision.
//
// For a delivered exception it is Done, and NOT DoneWithErrorData, which is the
// whole reason this comment is long. DoneWithErrorData sets
// CommandPayload.Error, the core's verdict that the node did not deliver — and a
// run started with stop_on_error prunes every outgoing edge of such a node, the
// `_exception` one included, *after* the routing above has already selected it.
// The node then reported an exception and the flow stopped dead at it: the branch
// the author wired for exactly this case could never fire.
//
// For one that did NOT deliver it is DoneWithErrorData, and the cut is the point:
// the failure belongs in the run's ledger, the node renders red, and a run asked
// to stop at errors stops at a model calling a function nobody bound. Only a
// plain-text turn is the node delivering an answer; a broken turn is not, and the
// earlier version of this function treated all three alike, so neither of those
// could stop a run any more.
//
// Both apply only when the node HAS an exception port, and that is what
// `functions` decides. This node's ports are not generated from anything the
// plugin publishes — being a builtin, its ports are hardcoded in the editor,
// which appends the exception port only once functions are bound (nodeCatalog's
// llm `ports`: `if (fns.length === 0) return []`). Unbound, the node keeps one
// untagged handle that already takes every outcome and there is no `_exception`
// to route to — so filtering onto that tag there would prune the only edge the
// node has and end the branch in silence. Such a node therefore keeps reporting a
// real failure whatever the code says: the reason is recorded, and stop_on_error
// cuts the default edge, which is what stopping on an error means when the author
// declared no handler. provider_error is the case that reaches both, since it
// fires before any function is looked at.
//
// Config mistakes (unreadable request body, an incomplete settings-profile, a
// prompt template with nothing to send) are NOT exceptions: they fail with a
// plain DoneWithError and no routing, because there is nothing for a runtime
// branch to recover from.
func Exception(job sdkv1.Job, code, reason string, messages []ChatMessage, functions []BoundFunction, data map[string]any) any {
	payload := exceptionPayload(code, reason, messages, data)
	if !exceptionRoutes(functions) {
		// No exception port exists on this node — see above. Report the failure
		// and route nowhere.
		return job.DoneWithErrorData(reason, payload)
	}
	job.CmdNextFilter([]string{exceptionTag})
	if nodeDelivered(code) {
		return job.Done(payload)
	}
	return job.DoneWithErrorData(reason, payload)
}

// exceptionRoutes reports whether this node has an `_exception` port for an
// exception to leave through, which is true exactly when functions are bound —
// the rule the editor's hardcoded ports follow for a builtin llm node.
func exceptionRoutes(functions []BoundFunction) bool {
	return len(functions) > 0
}

// exceptionPayload is what an exception commits onto the node's scope: the
// machine-readable `code` the exception branch switches on, the `error` reason
// for a reader, the conversation (so a resumed run can still read it back), and
// whatever per-case extras the caller adds.
//
// `error` is written last, so the reason always wins over a caller's key of the
// same name — which is what the core's own withFailureReason did back when the
// reason travelled on CommandPayload.Error instead of in here.
func exceptionPayload(code, reason string, messages []ChatMessage, data map[string]any) map[string]any {
	payload := map[string]any{
		"code":     code,
		"messages": messages,
	}
	for k, v := range data {
		payload[k] = v
	}
	payload["error"] = reason
	return payload
}

// runHandler implements the LLM node's single `run` action.
//
// A run stops with its flow. A model call is paid for, and once the flow is gone
// nothing downstream will read its answer — so the action runs through
// Stops.Middleware, and the job's context is cancelled when the runtime stops this
// job's process (a user stop, a stop command, the workflow's timeout, the idle
// window). It aborts the call in flight and any retry backoff (see callModel).
// Only this job's stop: another flow's run of this node is filed under its own
// jobId.
func runHandler(job sdkv1.Job) {
	ctx := job.Context()
	req, err := sdkv1.CastRequestTo[RunBody](job.Req.Data)
	if err != nil {
		job.DoneWithError(err.Error())
		return
	}
	cfg := req.Body.Settings

	if missing := validateSettings(cfg); len(missing) > 0 {
		job.DoneWithError("missing required settings-profile fields: " + strings.Join(missing, ", "))
		return
	}
	job.Progress(5, sdkv1.Frame{Title: "run", Content: "preparing conversation"})

	// 1. Read the current node scope and pull any existing conversation.
	var scope nodeScope
	if b, ok := job.CmdGetCurrentScope().([]byte); ok && len(b) > 0 {
		_ = sonic.Unmarshal(b, &scope) // messages may legitimately be absent
	}
	messages := scope.Messages
	resumed := len(messages) > 0 // messages present & non-empty ⇒ not the first run

	// clear_history: the drawer asked this node to start every run from its init
	// template, so drop any conversation carried on the node scope and treat this
	// as a first run. A looping/resumed flow then re-seeds the two init messages
	// each pass instead of accumulating history.
	if req.Body.ClearHistory {
		messages = nil
		resumed = false
	}

	// 2. Seed the INIT messages on the first run only. body.Messages is the prompt
	//    template the drawer collects — a system message (index 0) and/or a user
	//    message (index 1). Each content may embed {{$...}} vars resolved against
	//    the live flow context, and empty content is not seated. Once a
	//    conversation exists (resumed run) the template is skipped entirely: the
	//    init messages are seeded once and never re-added, so a looping flow does
	//    not stack them again every pass.
	if !resumed {
		messages = seedMessages(job, req.Body.Messages)
	}

	// A conversation with no message the provider can send as the request turn is
	// unanswerable — and some providers (notably Gemini/googleai) index the last
	// non-system message and panic on an empty history rather than erroring. Guard
	// here: the prompt template producing no content (or a system-only prompt with
	// the user box left blank) is a config mistake, not a crash.
	if !hasSendableTurn(messages) {
		job.DoneWithError("no message to send: the prompt template resolved to no user/assistant content (a system-only or empty prompt)")
		return
	}

	job.Progress(20, sdkv1.Frame{
		Title:   "run",
		Content: fmt.Sprintf("calling %s (%d messages)", cfg.Model, len(messages)),
	})
	// 3. Stream the completion (progress emitted per token inside streamChat). The
	//    result is either plain text or a tool-call message type — or both. A
	//    provider/API failure (bad key, quota, rate limit, network, no choices) is
	//    the first exception case: the flow leaves through `_exception` instead of
	//    stopping at this node.
	result, err := streamChat(ctx, job, cfg, messages, req.Body.Functions)
	if err != nil {
		if ctx.Err() != nil {
			// Stopped, not failed: the runtime has concluded this process and
			// stopped listening, so there is no one to route an exception to —
			// and every command sent now would only wait out the SDK's retries.
			return
		}
		// Nothing came back, so the conversation reported here is the one we sent —
		// which is exactly what the exception branch needs to see (and what the next
		// run must read back) to know what the provider was asked.
		Exception(job, codeProviderError, "provider error: "+err.Error(), messages, req.Body.Functions, map[string]any{
			"resumed":  resumed,
			"provider": cfg.Provider,
			"model":    cfg.Model,
		})
		return
	}

	// 4. Append the assistant turn (text and/or the tool calls it asked for). The
	//    conversation is persisted by carrying the full `messages` array in the Done
	//    payload below: Done commits its data onto this node's scope (node.key), so
	//    whatever `messages` value it reports IS what the next run reads back via
	//    CmdGetCurrentScope. That is exactly why a summary such as len(messages) must
	//    never be reported under the `messages` key — it would overwrite the
	//    persisted conversation with a bare count and break resumption. A separate
	//    CmdSetOnPath here would be clobbered by that same Done commit, so we don't.
	messages = append(messages, ChatMessage{
		Role:      "assistant",
		Content:   result.Content,
		ToolCalls: result.ToolCalls,
	})

	// 5. The second exception case. This run bound functions to the session, so the
	//    only ports it can route on are those function names — yet the model either
	//    picked none of them (a plain-text answer, or an empty turn) or asked for a
	//    function that was never bound. Neither turn has a port to follow, so route
	//    out of `_exception`. The assistant turn is already appended, so the
	//    reported conversation shows exactly what the model said instead.
	if len(req.Body.Functions) > 0 {
		bound := make([]string, len(req.Body.Functions))
		for i, f := range req.Body.Functions {
			bound[i] = f.Name
		}

		if len(result.ToolCalls) == 0 {
			Exception(job, codeNoFunctionSelected, "bound functions were offered but the model selected none", messages, req.Body.Functions, map[string]any{
				"resumed": resumed,
				"bound":   bound,
				"content": result.Content, // what it answered with instead — often empty
			})
			return
		}
		if unknown := unboundCalls(result.ToolCalls, req.Body.Functions); len(unknown) > 0 {
			Exception(job, codeUnboundFunction, "model called function(s) not bound to this node: "+strings.Join(unknown, ", "), messages, req.Body.Functions, map[string]any{
				"resumed":    resumed,
				"bound":      bound,
				"unbound":    unknown,
				"tool_calls": result.ToolCalls,
			})
			return
		}
	}

	// 6. Tool routing. If the model answered with a tool-call message type, route
	//    the flow out of the outbound port(s) named after the called function(s):
	//    each tool call's name IS the port's route tag. CmdNextFilter hands those
	//    tags to the runtime so only the matching ports fire next. No tool call ⇒
	//    skip this and let the flow follow its default route.
	if len(result.ToolCalls) > 0 {
		tags := make([]string, len(result.ToolCalls))
		for i, tc := range result.ToolCalls {
			tags[i] = tc.Name
		}
		job.CmdNextFilter(tags)
		job.Done(map[string]any{
			"resumed":    resumed,
			"messages":   messages,         // full conversation — persisted to node scope
			"tool_calls": result.ToolCalls, // name + raw args per call
			"routed":     tags,             // outbound-port tags fired next
		})
		return
	}

	// 7. Plain-text turn — emit the node output and let the flow follow its default
	//    route. The reply itself is the last entry of `messages` (the assistant turn
	//    appended above), so we don't repeat it under a separate key.
	job.Done(map[string]any{
		"resumed":  resumed,
		"messages": messages,
	})
}

// unboundCalls returns the names the model asked for that are not among the
// bound functions. Each bound function's name is an outbound-port route tag, so a
// call naming anything else (a hallucinated tool, or one left over from a session
// whose bindings changed) has no port to route on.
func unboundCalls(calls []toolCall, functions []BoundFunction) []string {
	bound := make(map[string]struct{}, len(functions))
	for _, f := range functions {
		bound[f.Name] = struct{}{}
	}
	var unknown []string
	for _, c := range calls {
		if _, ok := bound[c.Name]; !ok {
			unknown = append(unknown, c.Name)
		}
	}
	return unknown
}

// seedMessages resolves each init-template message's {{$...}} vars against the
// live flow context and returns the non-empty ones as the initial conversation.
// A message with a blank role defaults to "user". Content that resolves to empty
// (an unset prompt box in the drawer) is dropped, so a system-only or user-only
// template seats just the one message.
func seedMessages(job sdkv1.Job, template []ChatMessage) []ChatMessage {
	out := make([]ChatMessage, 0, len(template))
	for _, m := range template {
		content := resolveVars(job, m.Content)
		if strings.TrimSpace(content) == "" {
			continue
		}
		role := m.Role
		if role == "" {
			role = "user"
		}
		out = append(out, ChatMessage{Role: role, Content: content})
	}
	return out
}
