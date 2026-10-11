package llmnode

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
	`{"index":0,"id":"chatcmpl-tool-7f8fdfb1","type":"function","function":{"name":"dispense_medication","arguments":""}}`,
	`{"index":0,"function":{"arguments":""}}`,
	`{"index":0,"function":{"arguments":"{\"drug_name\": \""}}`,
	`{"index":0,"function":{"arguments":"Amoxicillin"}}`,
	`{"index":0,"function":{"arguments":"\", \"quantity\": \"21\"}"}}`,
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

// generate runs one streamed turn straight through langchaingo, bypassing
// streamChat: the frameBatcher would need a live Job to send progress to, and
// what is under test here is only what the library hands back.
func generate(t *testing.T, srv *httptest.Server) []llms.ToolCall {
	t.Helper()
	llm, err := openai.New(
		openai.WithToken("k"), openai.WithModel("laguna-s-2.1"), openai.WithBaseURL(srv.URL),
	)
	if err != nil {
		t.Fatalf("openai.New: %v", err)
	}
	resp, err := llm.GenerateContent(context.Background(),
		[]llms.MessageContent{{Role: llms.ChatMessageTypeHuman,
			Parts: []llms.ContentPart{llms.TextContent{Text: "R/ Amoxicillin 500mg"}}}},
		llms.WithStreamingFunc(func(context.Context, []byte) error { return nil }),
	)
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	return resp.Choices[0].ToolCalls
}

// Pins the library behaviour the repair exists for. If a langchaingo upgrade
// ever accumulates these deltas by index, this fails and coalesceStreamedToolCalls
// can go.
func TestLangchaingoSplitsOneVLLMToolCallInTwo(t *testing.T) {
	calls := generate(t, streamingToolCallServer(t, vllmToolCallDeltas))

	if len(calls) != 2 {
		t.Fatalf("langchaingo returned %d calls, want the 2 it mis-splits into "+
			"(if this is now 1, the library was fixed)", len(calls))
	}
	if got := calls[0].FunctionCall.Arguments; got != "" {
		t.Errorf("first call's arguments = %q, want empty — the name arrives without them", got)
	}
	if got := calls[1].FunctionCall.Name; got != "" {
		t.Errorf("second call's name = %q, want empty — it is a fragment, not a call", got)
	}
}

// The regression: one function invoked, one routable tool call out.
func TestStreamedToolCallFragmentsRejoinIntoOneCall(t *testing.T) {
	calls := coalesceStreamedToolCalls(generate(t, streamingToolCallServer(t, vllmToolCallDeltas)))

	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want 1: %+v", len(calls), calls)
	}
	if got := calls[0].FunctionCall.Name; got != "dispense_medication" {
		t.Errorf("name = %q, want dispense_medication — an unnamed call routes nowhere", got)
	}
	if got := calls[0].ID; got != "chatcmpl-tool-7f8fdfb1" {
		t.Errorf("id = %q, want the opener's id", got)
	}

	args := calls[0].FunctionCall.Arguments
	if !strings.Contains(args, `"drug_name"`) || !strings.Contains(args, "Amoxicillin") {
		t.Errorf("arguments = %q, want the whole streamed object", args)
	}
	// The rejoined arguments go out to the downstream node and are sent back on
	// the next request, so they have to be complete, valid JSON on their own.
	if _, trimmed := FirstJSONValue(args); trimmed {
		t.Errorf("arguments %q needed trimming, so the rejoin produced malformed JSON", args)
	}

	// The whole point: the node can now route this answer.
	bound := []BoundFunction{{Name: "dispense_medication", Description: "dispense a drug"}}
	out := make([]toolCall, len(calls))
	for i, c := range calls {
		out[i] = toolCall{ID: c.ID, Name: c.FunctionCall.Name, Arguments: args}
	}
	if unknown := unboundCalls(out, bound); len(unknown) > 0 {
		t.Errorf("call still reported unbound: %v — the node would route to _exception", unknown)
	}
}

// Two genuine parallel calls must stay two: each opener names its function, so
// nothing is folded, and each keeps its own arguments.
func TestParallelToolCallsAreNotMerged(t *testing.T) {
	deltas := []string{
		`{"index":0,"id":"call-a","type":"function","function":{"name":"dispense_medication","arguments":""}}`,
		`{"index":0,"function":{"arguments":""}}`,
		`{"index":0,"function":{"arguments":"{\"drug_name\": \"Amoxicillin\"}"}}`,
		`{"index":1,"id":"call-b","type":"function","function":{"name":"order_lab_test","arguments":""}}`,
		`{"index":1,"function":{"arguments":""}}`,
		`{"index":1,"function":{"arguments":"{\"test_name\": \"CBC\"}"}}`,
	}
	calls := coalesceStreamedToolCalls(generate(t, streamingToolCallServer(t, deltas)))

	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2 — parallel calls were folded together: %+v", len(calls), calls)
	}
	for i, want := range []struct{ id, name, arg string }{
		{"call-a", "dispense_medication", "Amoxicillin"},
		{"call-b", "order_lab_test", "CBC"},
	} {
		if calls[i].ID != want.id || calls[i].FunctionCall.Name != want.name {
			t.Errorf("call %d = %s/%s, want %s/%s", i, calls[i].ID, calls[i].FunctionCall.Name, want.id, want.name)
		}
		if !strings.Contains(calls[i].FunctionCall.Arguments, want.arg) {
			t.Errorf("call %d arguments = %q, want to contain %q",
				i, calls[i].FunctionCall.Arguments, want.arg)
		}
	}
}

// OpenAI's own stream does NOT send the empty-argument opener, so langchaingo
// accumulates it correctly and there is nothing to rejoin. This pins that the
// repair is the identity on a well-formed stream: it must not be the case that
// fixing one provider quietly reshapes every other.
//
// The guarantee is structural rather than per-provider — a fragment is
// recognised by having no function name, and a real call always names the
// function it invokes, because the name is what selects the tool. So any
// provider whose calls are well formed passes through unchanged, whether it goes
// through langchaingo's openai, anthropic or googleai path.
func TestWellFormedOpenAIStreamIsUntouched(t *testing.T) {
	deltas := []string{
		`{"index":0,"id":"call_abc123","type":"function","function":{"name":"dispense_medication","arguments":""}}`,
		`{"index":0,"function":{"arguments":"{\"drug_name\":"}}`,
		`{"index":0,"function":{"arguments":" \"Amoxicillin\"}"}}`,
	}
	before := generate(t, streamingToolCallServer(t, deltas))
	if len(before) != 1 {
		t.Fatalf("langchaingo returned %d calls for a well-formed stream, want 1: %+v", len(before), before)
	}

	after := coalesceStreamedToolCalls(before)
	if len(after) != 1 {
		t.Fatalf("coalescing changed a well-formed stream: %d calls, want 1", len(after))
	}
	if after[0].ID != before[0].ID || after[0].FunctionCall.Name != before[0].FunctionCall.Name ||
		after[0].FunctionCall.Arguments != before[0].FunctionCall.Arguments {
		t.Errorf("coalescing altered a well-formed call:\n before %+v\n after  %+v",
			before[0].FunctionCall, after[0].FunctionCall)
	}
	if !strings.Contains(after[0].FunctionCall.Arguments, "Amoxicillin") {
		t.Errorf("arguments = %q", after[0].FunctionCall.Arguments)
	}
}

// Nothing in, nothing out — a plain-text turn carries no tool calls, and the
// repair must not invent one.
func TestCoalesceOnNoToolCalls(t *testing.T) {
	if got := coalesceStreamedToolCalls(nil); len(got) != 0 {
		t.Errorf("got %d calls from nil, want 0", len(got))
	}
}
