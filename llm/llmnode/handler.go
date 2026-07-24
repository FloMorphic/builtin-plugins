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

	// 2. Resolve {{$...}} vars in the system prompt and seat it at index 0 with
	//    the canonical "system" role. ALWAYS replace messages[0]: the system
	//    prompt embeds live context values, and in a loop those change every
	//    iteration — a system message carried over from a prior run is stale, so
	//    we never keep it.
	systemMsg := ChatMessage{
		Role:    "system",
		Content: resolveVars(job, req.Body.SystemPrompt),
	}
	if resumed {
		messages[0] = systemMsg // overwrite the (now-stale) system message in place
	} else {
		messages = []ChatMessage{systemMsg} // seed a fresh conversation
	}

	// 3. Resolve vars in this turn's user prompt and append it.
	messages = append(messages, ChatMessage{
		Role:    "user",
		Content: resolveVars(job, req.Body.Prompt),
	})

	job.Progress(20, sdkv1.Frame{
		Title:   "run",
		Content: fmt.Sprintf("calling %s (%d messages)", cfg.Model, len(messages)),
	})

	// 4. Stream the completion (progress emitted per token inside streamChat). The
	//    result is either plain text or a tool-call message type — or both.
	result, err := streamChat(job, cfg, messages, req.Body.Functions)
	if err != nil {
		job.DoneWithError(err.Error())
		return
	}

	// 5. Append the assistant turn (text and/or the tool calls it asked for) and
	//    commit the conversation to this node's scope. CmdSetOnPath's payload is a
	//    map, so the array is carried under the `messages` key; the empty (no-`$`)
	//    path means "relative to the current scope", so this lands as
	//    <current>.messages = [...].
	messages = append(messages, ChatMessage{
		Role:      "assistant",
		Content:   result.Content,
		ToolCalls: result.ToolCalls,
	})
	job.CmdSetOnPath("", map[string]any{"messages": messages})

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
			"messages":   len(messages),
			"tool_calls": result.ToolCalls, // name + raw args per call
			"routed":     tags,             // outbound-port tags fired next
		})
		return
	}

	// 7. Plain-text turn — emit the node output (distinct from the committed
	//    conversation) and let the flow follow its default route.
	job.Done(map[string]any{
		"reply":    result.Content,
		"resumed":  resumed,
		"messages": len(messages),
	})
}
