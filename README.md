# PluginStore

Inflow plugin nodes, each an independent Go module that imports the
[`go-plugin-sdk`](https://github.com/Inflowenger/go-plugin-sdk). Every plugin is a
standalone binary: `main.go` is a thin bind layer over the SDK, and the node's
logic lives in its own package(s).

## Plugins

| Plugin | Module | What it does |
| ------ | ------ | ------------ |
| [`llm`](./llm) | `github.com/FloMorphic/builtin-plugins/llm` | One `run` action: a streamed LLM turn over OpenAI / OpenRouter / OpenAI-compatible / Gemini / Anthropic, with tool-call routing to outbound ports. |
| [`mcp`](./mcp) | `github.com/FloMorphic/builtin-plugins/mcp` | An MCP client node: `run` (LLM over MCP tools), `call_tool` (call one tool, no LLM), and a `getToolsList` meta method. Split into an `llm` package (provider glue) and an `mcpnode` package. |

Each plugin pins `go-plugin-sdk` at a released tag in its own `go.mod`, builds
independently, and reads its infra connection from a local `.env.inflow` (see the
`.env.inflow.example` in each). Nothing is shared at build time between plugins:
the MCP node carries its own copy of the LLM provider glue so the two stay
decoupled.

## Build one

```sh
cd llm      # or mcp
go build -o bin/plugin .
./bin/plugin
```
