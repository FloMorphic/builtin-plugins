package mcpnode

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/FloMorphic/builtin-plugins/mcp/llm"
	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
	"github.com/bytedance/sonic"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/tmc/langchaingo/llms"
)

// Register wires the MCP node's two actions and its meta method onto the plugin.
// Call it before p.Start().
func Register(p *sdkv1.Plugin) {
	p.AddAction(sdkv1.Action{
		Method:         "run",
		Title:          "Run",
		Description:    "Drive an LLM (from a settings-profile) over an MCP server's tools; the node executes the tool calls",
		RequestHandler: mcpRunHandler,
	})
	p.AddAction(sdkv1.Action{
		Method:         "call_tool",
		Title:          "Call Tool",
		Description:    "Call a single MCP tool with the given arguments; no LLM involved",
		RequestHandler: mcpCallToolHandler,
	})
	p.AddMeta(sdkv1.Meta{
		Method:         "getToolsList",
		RequestHandler: mcpGetToolsList,
	})
}

// ---- run action -----------------------------------------------------------

// mcpRunHandler implements the `run` (MCP-with-LLM) action: an agentic loop where
// the model is bound to the MCP server's tools and THIS node executes the calls.
func mcpRunHandler(job sdkv1.Job) {
	req, err := sdkv1.CastRequestTo[McpRunBody](job.Req.Data)
	if err != nil {
		job.DoneWithError(err.Error())
		return
	}
	body := req.Body
	cfg := body.Settings

	var missing []string
	missing = append(missing, llm.ValidateSettings(cfg)...)
	if strings.TrimSpace(body.Connection.URL) == "" {
		missing = append(missing, "connection.url")
	}
	if len(missing) > 0 {
		job.DoneWithError("missing required fields: " + strings.Join(missing, ", "))
		return
	}

	ctx := context.Background()
	job.Progress(5, sdkv1.Frame{Title: "run", Content: "connecting to MCP server"})

	cli, err := newMCPClient(ctx, body.Connection)
	if err != nil {
		job.DoneWithError(err.Error())
		return
	}
	defer cli.Close()

	// Fetch the live tools and pick the ones the node bound (by name). An empty
	// Functions list means "bind everything the server offers".
	serverTools, err := listMCPTools(ctx, cli)
	if err != nil {
		job.DoneWithError("list tools failed: " + err.Error())
		return
	}
	bound := selectBoundTools(serverTools, body.Functions)
	if len(bound) == 0 {
		job.DoneWithError("no MCP tools available to bind")
		return
	}
	tools := make([]llms.Tool, len(bound))
	for i, t := range bound {
		params := any(t.InputSchema)
		if t.InputSchema == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		tools[i] = llms.Tool{
			Type:     "function",
			Function: &llms.FunctionDefinition{Name: t.Name, Description: t.Description, Parameters: params},
		}
	}

	model, err := llm.NewLLM(ctx, cfg)
	if err != nil {
		job.DoneWithError(err.Error())
		return
	}

	// Seed the conversation on the FIRST run only. body.Messages is the init
	// template — a system message (index 0) and/or a user message (index 1); each
	// content resolves {{$...}} against the live flow context and empty content is
	// dropped, exactly like the LLM node. Once a prior run committed a conversation
	// to this node's scope, that conversation is reused as-is and the template is
	// ignored, so the init messages are never re-seeded on a resumed/looping run.
	var scope nodeScope
	if b, ok := job.CmdGetCurrentScope().([]byte); ok && len(b) > 0 {
		_ = sonic.Unmarshal(b, &scope) // messages may legitimately be absent
	}
	seed := scope.Messages
	if len(seed) == 0 {
		for _, m := range body.Messages {
			content := strings.TrimSpace(llm.ResolveVars(job, m.Content))
			if content == "" {
				continue
			}
			role := m.Role
			if role == "" {
				role = "user"
			}
			seed = append(seed, llm.ChatMessage{Role: role, Content: content})
		}
	}
	messages := buildMCPMessages(seed)

	// A system-only or empty conversation has no request turn for the provider to
	// answer; Gemini/googleai in particular indexes the last non-system message and
	// panics on an empty history. Fail cleanly instead of crashing.
	if !hasSendableTurn(seed) {
		job.DoneWithError("no message to send: the prompt template resolved to no user/assistant content (a system-only or empty prompt)")
		return
	}

	callOpts := []llms.CallOption{llms.WithTemperature(cfg.Temperature), llms.WithTools(tools)}
	if cfg.MaxTokens > 0 {
		callOpts = append(callOpts, llms.WithMaxTokens(cfg.MaxTokens))
	}

	calledTools := map[string]bool{} // tool names that fired ⇒ outbound-port tags
	percent := 20

	for turn := 0; turn < mcpMaxToolTurns; turn++ {
		resp, err := model.GenerateContent(ctx, messages, callOpts...)
		if err != nil {
			job.DoneWithError(err.Error())
			return
		}
		if len(resp.Choices) == 0 {
			job.DoneWithError("provider returned no choices")
			return
		}
		choice := resp.Choices[0]

		// No tool call ⇒ this is the final answer.
		if len(choice.ToolCalls) == 0 {
			routeCalledTools(job, calledTools)
			job.Done(map[string]any{
				"reply":      choice.Content,
				"turns":      turn + 1,
				"tools_used": keysOf(calledTools),
				// Persist the whole exchange on the node's scope so a resumed run
				// reads it back via CmdGetCurrentScope (Done commits its data onto
				// node.key). Seeding above skips the init template when this is
				// non-empty, so init messages are never re-added.
				"messages": flattenConversation(messages, choice.Content),
			})
			return
		}

		// Record the assistant turn (its tool calls) so the next model call sees
		// what it asked for.
		aiParts := make([]llms.ContentPart, 0, len(choice.ToolCalls)+1)
		if strings.TrimSpace(choice.Content) != "" {
			aiParts = append(aiParts, llms.TextContent{Text: choice.Content})
		}
		for _, tc := range choice.ToolCalls {
			aiParts = append(aiParts, tc)
		}
		messages = append(messages, llms.MessageContent{Role: llms.ChatMessageTypeAI, Parts: aiParts})

		// Execute each requested tool on the MCP server and feed results back.
		for _, tc := range choice.ToolCalls {
			if tc.FunctionCall == nil {
				continue
			}
			name := tc.FunctionCall.Name
			calledTools[name] = true
			if percent < 90 {
				percent += 5
			}
			job.Progress(percent, sdkv1.Frame{Title: "calling tool", Content: name})

			result, callErr := callMCPTool(ctx, cli, name, tc.FunctionCall.Arguments)
			messages = append(messages, llms.MessageContent{
				Role: llms.ChatMessageTypeTool,
				Parts: []llms.ContentPart{llms.ToolCallResponse{
					ToolCallID: tc.ID,
					Name:       name,
					Content:    result,
				}},
			})
			if callErr != nil {
				// Surface the failure to the model as the tool's content (already
				// prefixed) so it can recover; don't abort the whole run.
				_ = callErr
			}
		}
	}

	// Hit the turn cap without the model settling on a text answer.
	routeCalledTools(job, calledTools)
	job.DoneWithError(fmt.Sprintf("reached max tool turns (%d) without a final answer", mcpMaxToolTurns))
}

// selectBoundTools keeps the server tools whose names appear in the node's bound
// functions, preserving the bound order. When no functions are bound, every
// server tool is used.
func selectBoundTools(serverTools []McpTool, functions []llm.BoundFunction) []McpTool {
	if len(functions) == 0 {
		return serverTools
	}
	index := make(map[string]McpTool, len(serverTools))
	for _, t := range serverTools {
		index[t.Name] = t
	}
	out := make([]McpTool, 0, len(functions))
	for _, f := range functions {
		if t, ok := index[f.Name]; ok {
			out = append(out, t)
		}
	}
	return out
}

// hasSendableTurn reports whether the seeded conversation carries at least one
// non-system message with content — something the provider can send as the
// request turn. A system message is instruction-only (Gemini lifts it into
// SystemInstruction, leaving an empty request history), so a system-only or empty
// conversation has nothing to answer.
func hasSendableTurn(msgs []llm.ChatMessage) bool {
	for _, m := range msgs {
		if toLCRole(m.Role) == llms.ChatMessageTypeSystem {
			continue
		}
		if strings.TrimSpace(m.Content) != "" {
			return true
		}
	}
	return false
}

// buildMCPMessages turns the persisted (or freshly seeded) conversation into
// langchaingo message content the model call consumes. Only role + text is
// carried — the committed conversation is text-only (tool-call plumbing is a
// within-run detail), so this is the exact inverse of flattenConversation.
func buildMCPMessages(msgs []llm.ChatMessage) []llms.MessageContent {
	out := make([]llms.MessageContent, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, llms.MessageContent{
			Role:  toLCRole(m.Role),
			Parts: []llms.ContentPart{llms.TextContent{Text: m.Content}},
		})
	}
	return out
}

// toLCRole maps a canonical stored role onto langchaingo's typed role. Unknown
// roles fall back to the human/user role so a stray value never drops a turn.
func toLCRole(role string) llms.ChatMessageType {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "system":
		return llms.ChatMessageTypeSystem
	case "assistant", "ai", "model":
		return llms.ChatMessageTypeAI
	case "tool", "function":
		return llms.ChatMessageTypeTool
	default:
		return llms.ChatMessageTypeHuman
	}
}

// flattenConversation folds the in-memory langchaingo messages plus the final
// assistant reply into a plain `[]llm.ChatMessage`, so the run's Done payload can
// persist the exchange on the node's scope for a resumed run (and downstream
// nodes) to read. Only text is kept (tool-call plumbing is a within-run detail).
func flattenConversation(messages []llms.MessageContent, finalReply string) []llm.ChatMessage {
	out := make([]llm.ChatMessage, 0, len(messages)+1)
	for _, m := range messages {
		text := textOfParts(m.Parts)
		if text == "" {
			continue
		}
		out = append(out, llm.ChatMessage{Role: roleString(m.Role), Content: text})
	}
	out = append(out, llm.ChatMessage{Role: "assistant", Content: finalReply})
	return out
}

// routeCalledTools fires an outbound port per tool that was invoked, so the flow
// can branch on which MCP tools ran — the same convention as the LLM node.
func routeCalledTools(job sdkv1.Job, called map[string]bool) {
	tags := keysOf(called)
	if len(tags) > 0 {
		job.CmdNextFilter(tags)
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func textOfParts(parts []llms.ContentPart) string {
	var b strings.Builder
	for _, p := range parts {
		if tc, ok := p.(llms.TextContent); ok && tc.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func roleString(r llms.ChatMessageType) string {
	switch r {
	case llms.ChatMessageTypeSystem:
		return "system"
	case llms.ChatMessageTypeAI:
		return "assistant"
	case llms.ChatMessageTypeTool:
		return "tool"
	default:
		return "user"
	}
}

// ---- call_tool action ------------------------------------------------------

// mcpCallToolHandler implements the `call_tool` (MCP-tool-only) action: connect,
// invoke ONE tool with the user's arguments, return its result. No LLM.
func mcpCallToolHandler(job sdkv1.Job) {
	req, err := sdkv1.CastRequestTo[McpCallToolBody](job.Req.Data)
	if err != nil {
		job.DoneWithError(err.Error())
		return
	}
	body := req.Body

	var missing []string
	if strings.TrimSpace(body.Connection.URL) == "" {
		missing = append(missing, "connection.url")
	}
	if strings.TrimSpace(body.Tool) == "" {
		missing = append(missing, "tool")
	}
	if len(missing) > 0 {
		job.DoneWithError("missing required fields: " + strings.Join(missing, ", "))
		return
	}

	ctx := context.Background()
	job.Progress(10, sdkv1.Frame{Title: "call_tool", Content: "connecting to MCP server"})

	cli, err := newMCPClient(ctx, body.Connection)
	if err != nil {
		job.DoneWithError(err.Error())
		return
	}
	defer cli.Close()

	job.Progress(50, sdkv1.Frame{Title: "call_tool", Content: "calling " + body.Tool})
	callReq := mcp.CallToolRequest{}
	callReq.Params.Name = body.Tool
	callReq.Params.Arguments = body.Arguments
	res, err := cli.CallTool(ctx, callReq)
	if err != nil {
		job.DoneWithError(err.Error())
		return
	}

	out := map[string]any{
		"tool":     body.Tool,
		"content":  callResultText(res),
		"is_error": res.IsError,
	}
	if res.StructuredContent != nil {
		out["structured"] = res.StructuredContent
	}
	job.Done(out)
}

// ---- getToolsList meta -----------------------------------------------------

// mcpGetToolsList is the `getToolsList` meta method: given only the connection
// fields, it connects and returns the server's tools so the frontend can build a
// dialog (one output port / arg form per tool). The POST body is the McpConnection
// directly (not the {_registry, body} action envelope), and the response is a bare
// []McpTool. On failure it returns a small error object the caller can surface.
func mcpGetToolsList(req sdkv1.Request) any {
	var conn McpConnection
	if err := sonic.Unmarshal(req.Data, &conn); err != nil {
		return map[string]any{"error": "bad connection body: " + err.Error()}
	}
	if strings.TrimSpace(conn.URL) == "" {
		return map[string]any{"error": "connection.url is required"}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cli, err := newMCPClient(ctx, conn)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	defer cli.Close()

	tools, err := listMCPTools(ctx, cli)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	return tools // bare []McpTool — matches the frontend McpTool[] contract
}
