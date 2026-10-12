package llmnode

import "testing"

// The payload is the whole record of an exception, whichever way the job ends.
// It has to be, for a DELIVERED one: that concludes with Done, which has no
// error field — and it concludes that way because a node the core considers
// failed has every outgoing edge pruned under stop_on_error, `_exception`
// included, which is the one edge the exception exists to take. A failed one
// repeats the reason on the terminal command, but the branch still reads it from
// here.
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
	// Routable: functions bound ⇒ the port exists ⇒ the exception may route onto
	// it. Whether it then concludes Done or DoneWithErrorData is a separate
	// question, and the one nodeDelivered answers.
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

// The trait table, pinned. The three codes leave through the same port and are
// not the same thing, and getting one on the wrong side has consequences nothing
// else catches: a defect filed as a decision stops being able to stop a run, and
// a decision filed as a defect has its own branch pruned out from under it under
// stop_on_error — which is the bug the exception routing was fixed for in the
// first place.
func TestExceptionCodesSplitByWhetherTheNodeDelivered(t *testing.T) {
	for _, tc := range []struct {
		code      string
		delivered bool
		why       string
	}{
		{codeProviderError, false, "the API errored past the retry budget — the node has no turn to offer"},
		{codeUnboundFunction, false, "the model invented a tool nobody bound: a defect, and stop_on_error should stop at it"},
		{codeNoFunctionSelected, true, "the model answered with prose instead of a tool call — an ordinary turn, and the branch was drawn for it"},
	} {
		if got := nodeDelivered(tc.code); got != tc.delivered {
			t.Errorf("nodeDelivered(%q) = %v, want %v — %s", tc.code, got, tc.delivered, tc.why)
		}
	}
}

// An unrecognised code reports a failure: a new exception case over-reports
// rather than silently passing a defect off as a decision.
func TestAnUnknownExceptionCodeIsTreatedAsAFailure(t *testing.T) {
	if nodeDelivered("something_new") {
		t.Error("an unknown code was treated as a delivered turn")
	}
}

// The codes are the machine-readable tag an exception branch switches on, so
// their spelling is contract. A rename here is a branch that silently stops
// matching in every flow already drawn against it.
func TestExceptionCodeSpellings(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{codeProviderError, "provider_error"},
		{codeNoFunctionSelected, "no_function_selected"},
		{codeUnboundFunction, "unbound_function"},
		{exceptionTag, "_exception"},
	} {
		if tc.got != tc.want {
			t.Errorf("code is %q, want %q — flows already route on this string", tc.got, tc.want)
		}
	}
}
