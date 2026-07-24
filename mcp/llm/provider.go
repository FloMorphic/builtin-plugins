package llm

import (
	"context"
	"fmt"
	"strings"

	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/llms/anthropic"
	"github.com/tmc/langchaingo/llms/googleai"
	"github.com/tmc/langchaingo/llms/openai"
)

// NewLLM builds a langchaingo model for the configured provider. URL is an
// optional custom base URL — empty means the provider's default endpoint.
func NewLLM(ctx context.Context, cfg LLMSettings) (llms.Model, error) {
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
