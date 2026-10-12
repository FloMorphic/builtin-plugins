package decisionnode

import "testing"

// The trait table, pinned. These five codes leave through the same port and are
// two different things, and getting one of them on the wrong side has consequences
// nothing else catches: a defect filed as a decision stops being able to stop a
// run, and a decision filed as a defect has its own branch pruned out from under
// it under stop_on_error.
func TestExceptionCodesSplitByWhetherTheNodeDelivered(t *testing.T) {
	for _, tc := range []struct {
		code      string
		delivered bool
		why       string
	}{
		{codeProviderError, false, "the service failed past the retry budget — the node has no decision to offer"},
		{codeNoAnswer, false, "a routed question came back missing — a decider answering outside its contract"},
		{codeUnboundOption, false, "an answer named an undeclared option — a wire defect, and stop_on_error should stop at it"},
		{codeRefused, true, "the Decisions API's own facility for declining one question: the node worked"},
		{codeLowConfidence, true, "the floor the designer set, doing exactly what they set it to do"},
	} {
		if got := nodeDelivered(tc.code); got != tc.delivered {
			t.Errorf("nodeDelivered(%q) = %v, want %v — %s", tc.code, got, tc.delivered, tc.why)
		}
	}
}

// An unrecognised code reports a failure. A new exception case is a new way for
// this node to not deliver until somebody decides otherwise, which is the safer
// way round: it over-reports rather than passing a defect off as a decision.
func TestAnUnknownExceptionCodeIsTreatedAsAFailure(t *testing.T) {
	if nodeDelivered("something_new") {
		t.Error("an unknown code was treated as a delivered decision")
	}
}

// The codes are the machine-readable tag an exception branch switches on, so
// their spelling is contract. A rename here is a branch that silently stops
// matching in every flow already drawn against it.
func TestExceptionCodeSpellings(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{codeProviderError, "provider_error"},
		{codeNoAnswer, "no_answer"},
		{codeUnboundOption, "unbound_option"},
		{codeRefused, "refused"},
		{codeLowConfidence, "low_confidence"},
		{exceptionTag, "_exception"},
	} {
		if tc.got != tc.want {
			t.Errorf("code is %q, want %q — flows already route on this string", tc.got, tc.want)
		}
	}
}

func routed(route bool) Question {
	return Question{ID: "q", Type: typeNoul, Instructions: "well?", Route: &route}
}

// Whether there is a port to leave through. The editor derives a port per option
// of every ROUTED question and appends the exception port only once there is at
// least one, so a node whose every question is data-only keeps a single untagged
// handle — and filtering onto `_exception` there would prune the only edge it has.
func TestExceptionRoutesNeedsOneRoutedQuestion(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name  string
		qs    []Question
		ports bool
	}{
		{"no questions at all", nil, false},
		{"one routed question", []Question{routed(yes)}, true},
		{"route unset is routing on", []Question{{ID: "q", Type: typeNoul, Instructions: "well?"}}, true},
		{"every question data-only", []Question{routed(no), routed(no)}, false},
		{"one routed among data-only", []Question{routed(no), routed(yes), routed(no)}, true},
	} {
		if got := exceptionRoutes(tc.qs); got != tc.ports {
			t.Errorf("%s: exceptionRoutes = %v, want %v", tc.name, got, tc.ports)
		}
	}
}

// The payload is what the exception branch reads. `error` has to be IN it, not
// only on the terminal command's error field: a delivered exception concludes
// with Done, which has no error field, and the branch still has to learn why it
// was taken.
func TestExceptionPayloadCarriesCodeAndReason(t *testing.T) {
	got := exceptionPayload(codeLowConfidence, "confidence below the floor",
		map[string]any{"uncertain": []string{"risk"}, "model": "jev-1.13"})

	if got["code"] != codeLowConfidence {
		t.Errorf("code = %v, want %q", got["code"], codeLowConfidence)
	}
	if got["error"] != "confidence below the floor" {
		t.Errorf("error = %v, want the reason", got["error"])
	}
	if got["model"] != "jev-1.13" {
		t.Errorf("the caller's extras were dropped: %+v", got)
	}
}

// The reason wins over a caller's key of the same name — it is written last for
// exactly this reason, so no per-case payload can shadow why the branch was taken.
func TestExceptionPayloadReasonBeatsACallersErrorKey(t *testing.T) {
	got := exceptionPayload(codeProviderError, "the real reason",
		map[string]any{"error": "something the caller put there"})

	if got["error"] != "the real reason" {
		t.Errorf("error = %v, want the reason to win", got["error"])
	}
}
