// Package mcpnode is the MCP-relevant half of the MCP plugin node: an MCP
// *client* exposed as an Inflow node. It owns the connection to an MCP server,
// the tool listing/calling, and the agentic loop; the LLM provider glue it reuses
// lives in the sibling llm package.
//
// The node has two modes on the canvas (data.mcpMode) that map to two actions:
//
//   - `run`       ("MCP With LLM"): like the LLM node, but the tools bound to the
//     model are the MCP server's tools rather than hand-declared outbound ports.
//     THIS node runs the agentic loop — when the model asks for a tool it calls
//     it on the MCP server, feeds the result back, and re-prompts, until the
//     model answers with text. Each called tool is also routed as an outbound
//     port (CmdNextFilter), mirroring the LLM node's tool routing.
//   - `call_tool` ("MCP Tool Only"): no LLM. Connect, call ONE named tool with
//     the user-supplied arguments, and return its result.
//
// Plus one meta method (outside the job lifecycle):
//
//   - `getToolsList`: given only the connection fields, connect and return the
//     server's tools (name/title/description/inputSchema) so the frontend can
//     render one output port / argument dialog per tool. The POST body is the
//     McpConnection directly (not the {_registry,body} action envelope), and the
//     response is a bare []McpTool.
//
// Library: github.com/mark3labs/mcp-go — the client speaks the MCP protocol over
// streamable-http / SSE / stdio; this package owns only the connection + loop.
package mcpnode
