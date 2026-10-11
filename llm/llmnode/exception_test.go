package llmnode

import "testing"

// An exception is DELIVERED, not failed: Exception ends the job with Done, so
// the payload built here is the whole record of the failure. Nothing else
// carries it — there is no CommandPayload.Error any more, because a node the
// core considers failed has every outgoing edge pruned under stop_on_error,
// `_exception` included, which is the one edge the exception exists to take.
func TestExceptionPayloadCarriesTheWholeFailure(t *testing.T) {
	msgs := []ChatMessage{{Role: "user", Content: "R/ Amoxicillin"}}
	got := exceptionPayload("unbound_function", "model called function(s) not bound to this node: ",
		msgs, map[string]any{"bound": []string{"dispense_medication"}, "unbound": []string{""}})

	// The branch switches on `code`; a reader needs `error`.
	if got["code"] != "unbound_function" {
		t.Errorf("code = %v, want unbound_function", got["code"])
	}
	if got["error"] != "model called function(s) not bound to this node: " {
		t.Errorf("error = %v, want the reason", got["error"])
	}
	// The conversation must ride along: a terminal payload IS the node's commit,
	// so leaving it out would wipe what a resumed run reads back.
	if len(got["messages"].([]ChatMessage)) != 1 {
		t.Errorf("messages = %v, want the conversation", got["messages"])
	}
	if got["bound"] == nil || got["unbound"] == nil {
		t.Errorf("the caller's extras were dropped: %v", got)
	}
}

// The reason outranks a caller key of the same name, as the core's
// withFailureReason did when the reason still travelled beside the details.
func TestExceptionPayloadReasonWinsOverCallerError(t *testing.T) {
	got := exceptionPayload("provider_error", "the verdict",
		nil, map[string]any{"error": "the job's own field"})

	if got["error"] != "the verdict" {
		t.Errorf("error = %v, want the reason to win", got["error"])
	}
}

// The editor gives an llm node its `_exception` port only once functions are
// bound (nodeCatalog: `if (fns.length === 0) return []`), so that is the test
// for whether an exception has anywhere to go. These pin the plugin to that
// rule — the node's ports are hardcoded in the editor, not published by the
// plugin, so the two can only agree by both following it.
func TestExceptionRoutesOnlyWhenTheNodeHasAnExceptionPort(t *testing.T) {
	bound := []BoundFunction{{Name: "dispense_medication", Description: "d"}}

	if len(bound) == 0 {
		t.Fatal("precondition")
	}
	// Routable: functions bound ⇒ the port exists ⇒ the node must DELIVER, so
	// stop_on_error does not prune the edge the exception is routing onto.
	if !exceptionRoutes(bound) {
		t.Error("functions are bound, so the exception must route out of _exception")
	}
	// Not routable: no functions ⇒ no exception port, only one untagged handle.
	// Filtering onto _exception there would prune the node's only edge and end
	// the branch silently, so the failure must be reported instead.
	if exceptionRoutes(nil) {
		t.Error("no functions bound, so there is no _exception port to route to")
	}
	if exceptionRoutes([]BoundFunction{}) {
		t.Error("an empty function list is the same as none")
	}
}
