package llmnode

import (
	"context"
	"fmt"
	"strings"

	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/llms/anthropic"
	"github.com/tmc/langchaingo/llms/googleai"
	"github.com/tmc/langchaingo/llms/openai"
)

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

// buildMessages turns the persisted conversation into langchaingo message
// content. A stored assistant turn that carried tool calls is rebuilt with
// ToolCall parts so a resumed run re-sends what the model previously asked for.
func buildMessages(msgs []ChatMessage) []llms.MessageContent {
	out := make([]llms.MessageContent, 0, len(msgs))
	for _, m := range msgs {
		var parts []llms.ContentPart
		if strings.TrimSpace(m.Content) != "" {
			parts = append(parts, llms.TextContent{Text: m.Content})
		}
		for _, tc := range m.ToolCalls {
			parts = append(parts, llms.ToolCall{
				ID:           tc.ID,
				Type:         "function",
				FunctionCall: &llms.FunctionCall{Name: tc.Name, Arguments: tc.Arguments},
			})
		}
		if len(parts) == 0 {
			parts = append(parts, llms.TextContent{Text: ""})
		}
		out = append(out, llms.MessageContent{Role: toLCRole(m.Role), Parts: parts})
	}
	return out
}

// newLLM builds the langchaingo model for the configured provider. This is the
// only provider-aware code left: everything downstream speaks langchaingo's
// typed messages, so adding a provider is just another case here. URL is an
// optional custom base URL — empty means the provider's default endpoint.
func newLLM(ctx context.Context, cfg LLMSettings) (llms.Model, error) {
	url := strings.TrimSpace(cfg.URL)
	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	switch provider {
	case "openai", "openai-compatible", "compatible", "openrouter":
		// OpenRouter is an OpenAI-compatible gateway; default its base URL so the
		// profile only needs the key + a "vendor/model" id. Any other compatible
		// endpoint (Ollama, vLLM, Groq, Together, …) supplies its own URL.
		if url == "" && provider == "openrouter" {
			url = "https://openrouter.ai/api/v1"
		}
		opts := []openai.Option{openai.WithToken(cfg.AccessToken), openai.WithModel(cfg.Model)}
		if url != "" {
			opts = append(opts, openai.WithBaseURL(url))
		}
		return openai.New(opts...)
	case "gemini", "googleai", "google":
		opts := []googleai.Option{googleai.WithAPIKey(cfg.AccessToken), googleai.WithDefaultModel(cfg.Model)}
		return googleai.New(ctx, opts...)
	case "anthropic", "claude":
		opts := []anthropic.Option{anthropic.WithToken(cfg.AccessToken), anthropic.WithModel(cfg.Model)}
		if url != "" {
			opts = append(opts, anthropic.WithBaseURL(url))
		}
		return anthropic.New(opts...)
	default:
		return nil, fmt.Errorf("unsupported provider %q", cfg.Provider)
	}
}
