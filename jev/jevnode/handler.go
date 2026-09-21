package jevnode

import (
	"context"
	"fmt"
	"strings"

	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
)

// Register wires the Jev node's `run` action onto the plugin. Call it before
// p.Start().
func Register(p *sdkv1.Plugin) {
	p.AddAction(sdkv1.Action{
		Method:         "run",
		Title:          "Run",
		Description:    "Evaluate the state against typed questions on Jev (TypeSafe System One) and route by the answers",
		RequestHandler: runHandler,
	})
}

// exceptionTag is the route tag of the node's exception port.
const exceptionTag = "_exception"

// Exception ends a run that failed *at the decision boundary* — the API errored,
// a routed question came back unanswered or with an undeclared option, or its
// confidence fell below the floor the designer set. It does two things, in
// order:
//
//  1. CmdNextFilter([_exception]) — narrow the outbound ports to the exception
//     tag, so the flow does not die here: only downstream nodes carrying
//     `_exception` fire next and the process continues down that branch.
//  2. DoneWithErrorData(reason, data) — conclude the job as failed, with the
//     reason under "error" plus the run's state: `code` (a machine-readable tag
//     the exception branch can switch on) and whatever answers were decided, so
//     the canvas shows what Jev said even when the node did not route on it.
//
// Config mistakes (unreadable request body, a missing API key, malformed
// questions, an empty state) are NOT exceptions: they fail with a plain
// DoneWithError and no routing, because there is nothing for a runtime branch
// to recover from.
func Exception(job sdkv1.Job, code, reason string, data map[string]any) any {
	job.CmdNextFilter([]string{exceptionTag})
	payload := map[string]any{"code": code}
	for k, v := range data {
		payload[k] = v
	}
	return job.DoneWithErrorData(reason, payload)
}

// runHandler implements the Jev node's single `run` action.
func runHandler(job sdkv1.Job) {
	req, err := sdkv1.CastRequestTo[RunBody](job.Req.Data)
	if err != nil {
		job.DoneWithError(err.Error())
		return
	}
	cfg := req.Body.Settings
	if strings.TrimSpace(cfg.AccessToken) == "" {
		job.DoneWithError("missing required settings-profile fields: settings.access_token")
		return
	}
	questions := req.Body.Questions
	if err := validateQuestions(questions); err != nil {
		job.DoneWithError(err.Error())
		return
	}
	job.Progress(5, sdkv1.Frame{Title: "run", Content: "preparing state"})

	// 1. Resolve the state template against the live flow context. Jev only
	//    works on something to evaluate: a template that resolves to nothing is a
	//    config mistake, not a decision to route on.
	state := resolveState(job, req.Body.State)
	if s, ok := state.(string); ok && strings.TrimSpace(s) == "" {
		job.DoneWithError("empty state: the state template resolved to no content")
		return
	}

	// 2. One call evaluates every question in parallel against the same state.
	//    An API failure (bad key, validation, rate limit past the retry budget,
	//    overload, network) is the first exception case: the flow leaves through
	//    `_exception` instead of stopping at this node.
	model := modelOf(cfg)
	job.Progress(20, sdkv1.Frame{
		Title:   "thinking",
		Content: fmt.Sprintf("evaluating %d question(s) on %s", len(questions), model),
	})
	resp, err := callJev(context.Background(), cfg, buildRequest(cfg, state, questions))
	if err != nil {
		Exception(job, "provider_error", "provider error: "+err.Error(), map[string]any{
			"model": model,
		})
		return
	}
	if resp.Model != "" {
		model = resp.Model // the exact version that answered
	}

	// 3. Reduce every answer to a Decision. A routed question that has no answer
	//    or answers with an undeclared option has no port to follow, so it routes
	//    `_exception`; a data-only question with the same problem is just left
	//    out of the answers, since nothing downstream was drawn on it.
	decisions := make(map[string]Decision, len(questions))
	var (
		tags      []string // outbound-port tags to fire, in question order
		uncertain []string // routed questions whose confidence fell below their floor
	)
	for _, q := range questions {
		a, ok := resp.Answers[q.ID]
		if !ok {
			if q.routes() {
				Exception(job, "no_answer", fmt.Sprintf("no answer for routed question %q", q.ID), map[string]any{
					"model":    model,
					"question": q.ID,
					"answers":  decisions,
				})
				return
			}
			continue
		}
		d, err := decide(q, a)
		if err != nil {
			if q.routes() {
				Exception(job, "unbound_option", err.Error(), map[string]any{
					"model":    model,
					"question": q.ID,
					"answer":   a,
					"answers":  decisions,
				})
				return
			}
			continue
		}
		if q.routes() {
			d.Routed = true
			tags = append(tags, d.Tag)
			if q.MinConfidence > 0 && d.Confidence < q.MinConfidence {
				uncertain = append(uncertain, q.ID)
			}
		}
		decisions[q.ID] = d
	}
	job.Progress(90, sdkv1.Frame{Title: "decided", Content: summarize(decisions)})

	// 4. The confidence floor. The designer asked this node not to act on a
	//    routed answer it is not sure enough about: instead of that answer's port,
	//    the flow leaves through `_exception` with the full distribution in hand,
	//    so the exception branch (a human, a reasoning model) can take it from
	//    there. One uncertain question is enough — the alternative, firing the
	//    confident questions' ports and silently dropping the rest, would hide a
	//    branch the designer explicitly drew.
	if len(uncertain) > 0 {
		Exception(job, "low_confidence",
			"confidence below the floor for question(s): "+strings.Join(uncertain, ", "),
			map[string]any{
				"model":     model,
				"uncertain": uncertain,
				"answers":   decisions,
				"usage":     resp.Usage,
			})
		return
	}

	// 5. Route. Each routed question's top answer is one outbound-port tag —
	//    "<question>.<option>" — and CmdNextFilter hands the set to the runtime so
	//    only the edges carrying those tags fire next. Several routed questions ⇒
	//    several tags ⇒ one branch per question continues, like an LLM turn that
	//    called several tools. No routed question ⇒ skip this and let the flow
	//    follow its default route.
	if len(tags) > 0 {
		job.CmdNextFilter(tags)
	}
	job.Done(map[string]any{
		"model":   model,
		"answers": decisions, // every decision, keyed by question id — what lands on the scope
		"routed":  tags,      // outbound-port tags fired next (empty when nothing routes)
		"usage":   resp.Usage,
	})
}
