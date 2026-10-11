package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/llms/openai"
)

// The delta sequence a vLLM-backed OpenAI-compatible endpoint sends for ONE tool
// call, captured from a live one. The second chunk is the troublemaker: an
// opening chunk with the arguments still empty, which OpenAI itself never sends.
var vllmToolCallDeltas = []string{
	`{"index":0,"id":"chatcmpl-tool-7f8fdfb1","type":"function","function":{"name":"read_file","arguments":""}}`,
	`{"index":0,"function":{"arguments":""}}`,
	`{"index":0,"function":{"arguments":"{\"path\": \""}}`,
	`{"index":0,"function":{"arguments":"README.md"}}`,
	`{"index":0,"function":{"arguments":"\"}"}}`,
}

func streamingToolCallServer(t *testing.T, deltas []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, d := range deltas {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":null,"+
				"\"role\":\"assistant\",\"tool_calls\":[%s]},\"index\":0}],"+
				"\"object\":\"chat.completion.chunk\"}\n\n", d)
			if flusher != nil {
				flusher.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// generate runs one streamed turn straight through langchaingo, which is what
// mcpnode does — WithStreamingFunc set only so the provider streams.
func generate(t *testing.T, srv *httptest.Server) []llms.ToolCall {
	t.Helper()
	model, err := openai.New(
		openai.WithToken("k"), openai.WithModel("m"), openai.WithBaseURL(srv.URL),
	)
	if err != nil {
		t.Fatalf("openai.New: %v", err)
	}
	resp, err := model.GenerateContent(context.Background(),
		[]llms.MessageContent{{Role: llms.ChatMessageTypeHuman,
			Parts: []llms.ContentPart{llms.TextContent{Text: "read the readme"}}}},
		llms.WithStreamingFunc(func(context.Context, []byte) error { return nil }),
	)
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	return resp.Choices[0].ToolCalls
}

// Pins the library behaviour the repair exists for. If a langchaingo upgrade
// ever accumulates these deltas by index, this fails and
// CoalesceStreamedToolCalls can go — from both plugins.
func TestLangchaingoSplitsOneVLLMToolCallInTwo(t *testing.T) {
	calls := generate(t, streamingToolCallServer(t, vllmToolCallDeltas))

	if len(calls) != 2 {
		t.Fatalf("langchaingo returned %d calls, want the 2 it mis-splits into "+
			"(if this is now 1, the library was fixed)", len(calls))
	}
	if got := calls[0].FunctionCall.Arguments; got != "" {
		t.Errorf("first call's arguments = %q, want empty", got)
	}
	if got := calls[1].FunctionCall.Name; got != "" {
		t.Errorf("second call's name = %q, want empty — it is a fragment", got)
	}
}

// The regression. Untouched, mcpnode would call the named half with no arguments
// and refuse the unnamed half as unbound, costing a turn twice over.
func TestStreamedToolCallFragmentsRejoinIntoOneCall(t *testing.T) {
	calls := CoalesceStreamedToolCalls(generate(t, streamingToolCallServer(t, vllmToolCallDeltas)))

	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want 1: %+v", len(calls), calls)
	}
	if got := calls[0].FunctionCall.Name; got != "read_file" {
		t.Errorf("name = %q, want read_file — an unnamed call is refused as unbound", got)
	}
	if got := calls[0].ID; got != "chatcmpl-tool-7f8fdfb1" {
		t.Errorf("id = %q, want the opener's id — the tool result is keyed by it", got)
	}
	args := calls[0].FunctionCall.Arguments
	if !strings.Contains(args, `"path"`) || !strings.Contains(args, "README.md") {
		t.Errorf("arguments = %q, want the whole streamed object", args)
	}
	// The rejoined arguments go to the MCP server and ride back on the next
	// request, so they must be complete, valid JSON on their own.
	if _, trimmed := FirstJSONValue(args); trimmed {
		t.Errorf("arguments %q needed trimming, so the rejoin produced malformed JSON", args)
	}
}

// Two genuine parallel calls stay two — an agent loop calling several tools in
// one turn is the normal case here, so this matters more than in the llm node.
func TestParallelToolCallsAreNotMerged(t *testing.T) {
	deltas := []string{
		`{"index":0,"id":"call-a","type":"function","function":{"name":"read_file","arguments":""}}`,
		`{"index":0,"function":{"arguments":""}}`,
		`{"index":0,"function":{"arguments":"{\"path\": \"README.md\"}"}}`,
		`{"index":1,"id":"call-b","type":"function","function":{"name":"list_dir","arguments":""}}`,
		`{"index":1,"function":{"arguments":""}}`,
		`{"index":1,"function":{"arguments":"{\"path\": \"src\"}"}}`,
	}
	calls := CoalesceStreamedToolCalls(generate(t, streamingToolCallServer(t, deltas)))

	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2 — parallel calls were folded: %+v", len(calls), calls)
	}
	for i, want := range []struct{ id, name, arg string }{
		{"call-a", "read_file", "README.md"},
		{"call-b", "list_dir", "src"},
	} {
		if calls[i].ID != want.id || calls[i].FunctionCall.Name != want.name {
			t.Errorf("call %d = %s/%s, want %s/%s", i, calls[i].ID, calls[i].FunctionCall.Name, want.id, want.name)
		}
		if !strings.Contains(calls[i].FunctionCall.Arguments, want.arg) {
			t.Errorf("call %d arguments = %q, want to contain %q", i, calls[i].FunctionCall.Arguments, want.arg)
		}
	}
}

// OpenAI's own stream sends no empty-argument opener, so there is nothing to
// rejoin and the repair must be the identity — the same guarantee that makes it
// safe on the anthropic and googleai paths, where a call always has a name.
func TestWellFormedOpenAIStreamIsUntouched(t *testing.T) {
	deltas := []string{
		`{"index":0,"id":"call_abc123","type":"function","function":{"name":"read_file","arguments":""}}`,
		`{"index":0,"function":{"arguments":"{\"path\":"}}`,
		`{"index":0,"function":{"arguments":" \"README.md\"}"}}`,
	}
	before := generate(t, streamingToolCallServer(t, deltas))
	if len(before) != 1 {
		t.Fatalf("langchaingo returned %d calls for a well-formed stream, want 1", len(before))
	}
	after := CoalesceStreamedToolCalls(before)
	if len(after) != 1 {
		t.Fatalf("coalescing changed a well-formed stream: %d calls, want 1", len(after))
	}
	if after[0].ID != before[0].ID || after[0].FunctionCall.Name != before[0].FunctionCall.Name ||
		after[0].FunctionCall.Arguments != before[0].FunctionCall.Arguments {
		t.Errorf("coalescing altered a well-formed call:\n before %+v\n after  %+v",
			before[0].FunctionCall, after[0].FunctionCall)
	}
}

func TestCoalesceOnNoToolCalls(t *testing.T) {
	if got := CoalesceStreamedToolCalls(nil); len(got) != 0 {
		t.Errorf("got %d calls from nil, want 0", len(got))
	}
}
