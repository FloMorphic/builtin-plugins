package llmnode

import (
	"context"
	"fmt"
	"strings"

	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
	"github.com/tmc/langchaingo/llms"
)

// validateSettings returns the list of required settings-profile fields missing
// from the request. The frontend profile must supply all of them.
func validateSettings(cfg LLMSettings) []string {
	var missing []string
	if strings.TrimSpace(cfg.Provider) == "" {
		missing = append(missing, "settings.provider")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		missing = append(missing, "settings.model")
	}
	if strings.TrimSpace(cfg.AccessToken) == "" {
		missing = append(missing, "settings.access_token")
	}
	return missing
}

// hasSendableTurn reports whether the conversation carries at least one message a
// provider can send as the request turn — i.e. a non-system message with content
// or tool calls. Providers treat the system message as instruction-only (Gemini
// lifts it into SystemInstruction and leaves the request history empty), so a
// system-only or empty conversation has nothing to answer.
func hasSendableTurn(msgs []ChatMessage) bool {
	for _, m := range msgs {
		if toLCRole(m.Role) == llms.ChatMessageTypeSystem {
			continue
		}
		if strings.TrimSpace(m.Content) != "" || len(m.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// streamChat drives one turn through the configured langchaingo provider. Bound
// functions are forwarded as llms.Tool, so the model may answer with a tool-call
// message type instead of (or alongside) text. Every streamed token is pushed to
// the canvas via job.Progress, ramping the percentage but NEVER reaching 100 —
// the runtime treats 100 as "done". The library accumulates the tool-call deltas
// for us; we read the finished calls off the first choice.
func streamChat(job sdkv1.Job, cfg LLMSettings, messages []ChatMessage, functions []BoundFunction) (completion, error) {
	ctx := context.Background()
	llm, err := newLLM(ctx, cfg)
	if err != nil {
		return completion{}, err
	}

	callOpts := []llms.CallOption{llms.WithTemperature(cfg.Temperature)}
	if cfg.MaxTokens > 0 {
		callOpts = append(callOpts, llms.WithMaxTokens(cfg.MaxTokens))
	}
	// Advertise each bound function as a tool. The provider may then reply with a
	// tool call naming one of these — that name is our outbound-port tag.
	if len(functions) > 0 {
		tools := make([]llms.Tool, len(functions))
		for i, f := range functions {
			params := any(f.Parameters)
			if f.Parameters == nil {
				// A function declared with no arguments in the settings drawer: send a
				// valid empty-object schema so the request stays well-formed.
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools[i] = llms.Tool{
				Type: "function",
				Function: &llms.FunctionDefinition{
					Name:        f.Name,
					Description: f.Description,
					Parameters:  params,
				},
			}
		}
		callOpts = append(callOpts, llms.WithTools(tools))
	}

	// Stream tokens straight to the canvas as they arrive.
	var full strings.Builder
	percent := 25 // start of the streaming window; grows toward (but never hits) 100
	callOpts = append(callOpts, llms.WithStreamingFunc(func(ctx context.Context, chunk []byte) error {
		full.Write(chunk)
		if percent < 95 {
			percent++
		}
		job.Progress(percent, sdkv1.Frame{
			Title:   "generating",
			Content: full.String(), // the live, accumulating completion
		})
		return nil
	}))

	resp, err := llm.GenerateContent(ctx, buildMessages(messages), callOpts...)
	if err != nil {
		return completion{}, err
	}
	if len(resp.Choices) == 0 {
		return completion{}, fmt.Errorf("provider returned no choices")
	}
	choice := resp.Choices[0]

	out := completion{Content: choice.Content}
	for _, tc := range choice.ToolCalls {
		call := toolCall{ID: tc.ID}
		if tc.FunctionCall != nil {
			call.Name = tc.FunctionCall.Name
			call.Arguments = tc.FunctionCall.Arguments
		}
		out.ToolCalls = append(out.ToolCalls, call)
	}
	return out, nil
}
