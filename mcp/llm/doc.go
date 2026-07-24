// Package llm is the LLM-relevant half of the MCP node: the provider glue the
// node shares with the standalone LLM plugin (settings contract, variable
// resolution, provider construction).
//
// The MCP node is deliberately shaped after the LLM node and reuses its LLM
// helpers. In the SDK both nodes lived in one package and shared these symbols
// directly; as independent PluginStore modules this package carries the MCP
// node's own copy so the two plugins stay fully decoupled (no cross-module
// import). Keep it in sync with the standalone llm plugin
// (github.com/FloMorphic/builtin-plugins/llm) if the LLM contract changes.
//
// Only the subset the MCP node uses lives here — LLMSettings, BoundFunction,
// ChatMessage, ResolveVars, ValidateSettings, NewLLM. The full LLM node also has
// streaming, message building and role mapping; those stay in the llm plugin.
package llm
