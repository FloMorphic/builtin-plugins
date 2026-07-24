package mcpnode

import "github.com/FloMorphic/builtin-plugins/mcp/llm"

// McpConnection is the connection contract the frontend ships (mirrors
// flomorphic-wapp src/types/api.ts → McpConnection). It's set on the node
// (data.url / data.transport / data.auth) and is also the exact POST body of the
// `getToolsList` meta call.
type McpConnection struct {
	URL       string `json:"url"`                 // MCP server endpoint (or command line for stdio)
	Transport string `json:"transport,omitempty"` // "streamable-http" (default) | "sse" | "stdio"
	Auth      string `json:"auth,omitempty"`      // bearer token / auth header value; empty when the server is open
}

// McpTool is one advertised tool (mirrors flomorphic-wapp src/types/api.ts →
// McpTool). Name is the tool id the model calls and the outbound-port route tag;
// InputSchema is the JSON-schema of its arguments, from which the frontend builds
// the "set arguments" dialog for `call_tool`.
type McpTool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema,omitempty"`
}

// McpRunBody is the inner `body` of a `run` request. It is the LLM node's run
// body plus the MCP connection: Settings is the provider profile (reused verbatim
// from the LLM node via the llm package), Connection reaches the MCP server, and
// Functions are the tools loaded on the node — only their names matter here (the
// live input schemas are re-fetched from the server at run time), so an empty
// list means "bind every tool the server advertises".
type McpRunBody struct {
	Settings     llm.LLMSettings     `json:"settings"`
	Connection   McpConnection       `json:"connection"`
	Prompt       string              `json:"prompt"`        // this turn's user prompt (may contain {{$...}})
	SystemPrompt string              `json:"system_prompt"` // system / init prompt (may contain {{$...}})
	Functions    []llm.BoundFunction `json:"functions"`     // MCP tools bound on the node; empty ⇒ bind all
}

// McpCallToolBody is the inner `body` of a `call_tool` request: a connection, the
// tool to invoke, and the arguments the user set in the dialog the frontend built
// from that tool's InputSchema. No LLM is involved.
type McpCallToolBody struct {
	Connection McpConnection  `json:"connection"`
	Tool       string         `json:"tool"`      // tool name to call
	Arguments  map[string]any `json:"arguments"` // arguments matching the tool's inputSchema
}

// mcpMaxToolTurns caps the `run` agentic loop so a model that keeps asking for
// tools can never spin forever. Each turn is one model call plus the tool calls
// it requested.
const mcpMaxToolTurns = 8
