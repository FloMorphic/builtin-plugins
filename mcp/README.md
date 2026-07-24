# MCP node

An Inflow plugin node that is an [MCP](https://modelcontextprotocol.io) *client*.
It connects to an MCP server over streamable-http / SSE / stdio and exposes its
tools to a flow, either driven by an LLM (the node runs the agentic loop) or
called directly with no LLM at all.

The node is deliberately shaped after the [LLM node](../llm) and reuses its
provider glue. Because the two plugins are independent modules, that shared LLM
code is carried here as its own package rather than imported across repos.

In "With LLM" mode it drives any provider the LLM node supports — OpenAI,
OpenRouter, any OpenAI-compatible endpoint (Ollama, vLLM, Groq, Together, …),
Gemini, or Anthropic. See the [LLM node's provider table](../llm#providers); the
`settings` contract here is identical.

## Layout — two concerns

```
main.go        thin bind layer: build the SDK plugin, register, Start, block
llm/           LLM-relevant code (shared with the LLM plugin)
  types.go       LLMSettings, BoundFunction, ChatMessage
  vars.go        {{$.a.b}} variable resolution (ResolveVars)
  validate.go    ValidateSettings
  provider.go    NewLLM — langchaingo provider construction
mcpnode/       MCP-relevant code (imports ./llm)
  types.go       McpConnection, McpTool, McpRunBody, McpCallToolBody
  client.go      connect / list tools / call tool (mark3labs/mcp-go)
  handler.go     the two actions, the meta method, and Register(p)
```

The root binary only wires things together. The `llm` package is the LLM half
(provider, settings, variable resolution) with **exported** symbols; `mcpnode`
is the MCP half and imports it. Keep `llm` in sync with the standalone
[`PluginStore/llm`](../llm) plugin if the LLM contract changes.

## Actions & meta

| Kind | Name | Purpose |
| ---- | ---- | ------- |
| action | `run` | "MCP With LLM": bind the model to the server's tools and run the agentic loop — the node executes each tool call, feeds the result back, and re-prompts until the model answers with text. Every called tool is also routed as an outbound port. |
| action | `call_tool` | "MCP Tool Only": no LLM. Connect and call one named tool with the given arguments, return its result. |
| meta | `getToolsList` | Given only the connection, connect and return the server's tools (`[]McpTool`) so the frontend can render one output port / argument dialog per tool. |

### Connection

Shared by every entry point:

```jsonc
{
  "url": "https://…/mcp",      // server endpoint, or a command line for stdio
  "transport": "streamable-http", // "streamable-http" (default) | "sse" | "stdio"
  "auth": ""                    // bearer token or full "Scheme value" header
}
```

### `run` body

The LLM node's run body plus a `connection`. `functions` binds specific tools by
name; an empty list binds every tool the server advertises. Like the LLM node,
`messages` is the INIT prompt template — it seeds the agentic loop's conversation
on the first run only; once the node's scope carries a conversation, the template
is ignored and the persisted exchange is reused.

```jsonc
{
  "settings": { "provider": "openrouter", "model": "anthropic/claude-3.5-sonnet", "access_token": "…" },
  "connection": { "url": "https://…/mcp" },
  "messages": [
    { "role": "system", "content": "…" },
    { "role": "user",   "content": "…" }
  ],
  "functions": [ { "name": "search" } ]
}
```

### `call_tool` body

```jsonc
{
  "connection": { "url": "https://…/mcp" },
  "tool": "search",
  "arguments": { "query": "…" }   // matches the tool's inputSchema
}
```

### `getToolsList` meta

The POST body is the connection object **directly** (not the `{_registry, body}`
action envelope); the response is a bare `[]McpTool`. On failure it returns a
small `{ "error": "…" }` object.

## Configure & run

Create `.env.inflow` next to the binary (see `.env.inflow.example`):

```
PLUGIN_ID=mcp
INFRA_URL=nats://…
INFRA_CRED=/path/to/infra.creds
```

Then:

```sh
go build -o bin/mcp .
./bin/mcp
```
