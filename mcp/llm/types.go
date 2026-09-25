package llm

// LLMSettings is the per-provider config the node needs to talk to an LLM. It is
// the contract the frontend settings-profile must satisfy — identical to the LLM
// plugin's LLMSettings so the two nodes stay in lockstep.
type LLMSettings struct {
	Provider    string  `json:"provider"`     // "openai", "openai-compatible", "openrouter", "gemini", "anthropic"
	URL         string  `json:"url"`          // optional custom *base* URL (e.g. ".../v1"); leave empty for the provider default
	Model       string  `json:"model"`        // model id, e.g. "gemini-2.0-flash" / "gpt-4o"
	AccessToken string  `json:"access_token"` // bearer token / api key
	Temperature float64 `json:"temperature"`  // sampling temperature
	MaxTokens   int     `json:"max_tokens"`   // optional; omitted when 0

	// RequestTimeoutS bounds ONE model call, and MaxRetries is how many further
	// attempts a failed one gets. They describe how reliable a particular
	// endpoint is, which is a property of the endpoint rather than of the work,
	// so they belong to the connection profile.
	//
	// RequestTimeoutS of zero takes the default. MaxRetries is a POINTER so
	// that "do not retry" and "unset" are different things: nil (absent from
	// the profile) takes the default, and an explicit 0 turns retrying off.
	// An int could not express both, which would leave a settings form with no
	// way to say "never retry this endpoint".
	RequestTimeoutS int  `json:"request_timeout_s"`
	MaxRetries      *int `json:"max_retries"`
}

// BoundFunction is one function bound to the node in its settings drawer. For the
// MCP node, Name matches an MCP tool the server advertises; the live input schema
// is re-fetched from the server at run time, so only the name matters here.
type BoundFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// ChatMessage is one entry of the conversation persisted on the node's scope.
// Roles are the canonical set ("system"/"user"/"assistant"/"tool").
type ChatMessage struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
}

// toolCall is one function call the model asked for. Kept so a committed
// conversation can record what was requested.
type toolCall struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
