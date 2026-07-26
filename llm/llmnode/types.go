package llmnode

// LLMSettings is the shape the frontend settings-profile must produce. Collect
// these into a profile (e.g. "gemini-config") and ship them in body.settings.
// Provider is the one field that picks the backend; once it's known langchaingo
// owns the message body and role mapping, so the profile no longer names the
// system/user/assistant roles.
type LLMSettings struct {
	Provider    string  `json:"provider"`     // "openai", "openai-compatible", "openrouter", "gemini", "anthropic"
	URL         string  `json:"url"`          // optional custom *base* URL (e.g. ".../v1"); leave empty for the provider default
	Model       string  `json:"model"`        // model id, e.g. "gemini-2.0-flash" / "gpt-4o"
	AccessToken string  `json:"access_token"` // bearer token / api key
	Temperature float64 `json:"temperature"`  // sampling temperature
	MaxTokens   int     `json:"max_tokens"`   // optional; omitted when 0
}

// BoundFunction is one function bound to the LLM session in the node's settings
// drawer. Each bound function is an OUTBOUND PORT: Name is the port's route tag
// used by job.CmdNextFilter when the model calls it, and Description is the
// model-facing text the LLM uses to decide *when* to call it — both are supplied
// by the user in the settings drawer, and the description is required (the model
// picks tools by their description). Parameters is the JSON schema of the call
// arguments the drawer collects per function — an object schema whose properties
// are the arguments the model has to fill in (the values come back on the tool
// call's Arguments). A function declared with no arguments omits it, and
// streamChat then sends an empty-object schema so the provider request stays
// well-formed.
type BoundFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// RunBody is body of the `run` action request (the inner `body` of the envelope).
//
// Messages is the INIT prompt template: an ordered list of role-tagged messages
// the settings drawer collects — a "system" message (index 0) and/or a "user"
// message (index 1). Each message's Content may embed {{$...}} vars. It seeds the
// conversation on the first run only; once messages exist on the node's scope,
// this template is ignored (see the handler), so the init messages are never
// re-added on resumed/looping runs.
type RunBody struct {
	Settings  LLMSettings     `json:"settings"`  // fed by the settings-profile
	Messages  []ChatMessage   `json:"messages"`  // init template: system (0) and/or user (1)
	Functions []BoundFunction `json:"functions"` // bound functions == outbound ports
}

// ChatMessage is one entry of the conversation held on the node's scope. Roles
// are the canonical set ("system"/"user"/"assistant"/"tool"); buildMessages maps
// them to langchaingo's typed roles. When the assistant answers with tool calls,
// they ride along in ToolCalls (omitted on plain-text turns) so the committed
// conversation records what was requested.
type ChatMessage struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
}

// toolCall is one function call the model asked for. Name is the bound function's
// name — i.e. the outbound-port route tag fed to job.CmdNextFilter. ID is the
// provider's call id, kept so a resumed conversation can round-trip the call.
type toolCall struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // raw JSON args string, as the provider streams it
}

// completion is the result of one streamed turn: either plain Content, or one or
// more ToolCalls (a tool-call message type), or both.
type completion struct {
	Content   string
	ToolCalls []toolCall
}

// nodeScope is the slice of the current scope we care about.
type nodeScope struct {
	Messages []ChatMessage `json:"messages"`
}
