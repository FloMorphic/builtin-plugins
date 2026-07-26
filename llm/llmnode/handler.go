package llmnode

import (
	"fmt"
	"strings"

	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
	"github.com/bytedance/sonic"
)

// Register wires the LLM node's `run` action onto the plugin. Call it before
// p.Start().
func Register(p *sdkv1.Plugin) {
	p.AddAction(sdkv1.Action{
		Method:         "run",
		Title:          "Run",
		Description:    "Run an LLM turn against the node's conversation using a settings-profile",
		RequestHandler: runHandler,
	})
}

// exceptionTag is the route tag of the node's exception port.
const exceptionTag = "_exception"

// Exception ends a run that failed *at the provider boundary* — the API errored,
// or a function-bound session came back with nothing this node can route on. It
// does two things, in order:
//
//  1. CmdNextFilter([_exception]) — narrow the outbound ports to the exception
//     tag, so the flow does not die here: only downstream nodes carrying
//     `_exception` fire next and the process continues down that branch.
//  2. DoneWithErrorData(reason, data) — conclude the job as failed, with the
//     reason under "error" plus the run's state: `code` (a machine-readable tag
//     the exception branch can switch on) and the full conversation, so the
//     canvas always shows what happened and what the messages were, and the next
//     run still reads the conversation back off the node scope (a terminal
//     payload IS the node's committed scope — reporting only "error" would drop
//     it).
//
// Config mistakes (unreadable request body, an incomplete settings-profile, a
// prompt template with nothing to send) are NOT exceptions: they fail with a
// plain DoneWithError and no routing, because there is nothing for a runtime
// branch to recover from.
func Exception(job sdkv1.Job, code, reason string, messages []ChatMessage, data map[string]any) any {
	job.CmdNextFilter([]string{exceptionTag})
	payload := map[string]any{
		"code":     code,
		"messages": messages,
	}
	for k, v := range data {
		payload[k] = v
	}
	return job.DoneWithErrorData(reason, payload)
}

// runHandler implements the LLM node's single `run` action.
func runHandler(job sdkv1.Job) {
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
	result, err := streamChat(job, cfg, messages, req.Body.Functions)
	if err != nil {
		// Nothing came back, so the conversation reported here is the one we sent —
		// which is exactly what the exception branch needs to see (and what the next
		// run must read back) to know what the provider was asked.
		Exception(job, "provider_error", "provider error: "+err.Error(), messages, map[string]any{
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
			Exception(job, "no_function_selected", "bound functions were offered but the model selected none", messages, map[string]any{
				"resumed": resumed,
				"bound":   bound,
				"content": result.Content, // what it answered with instead — often empty
			})
			return
		}
		if unknown := unboundCalls(result.ToolCalls, req.Body.Functions); len(unknown) > 0 {
			Exception(job, "unbound_function", "model called function(s) not bound to this node: "+strings.Join(unknown, ", "), messages, map[string]any{
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
