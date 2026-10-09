package decisionnode

import (
	"fmt"
	"strings"

	"github.com/Inflowenger/go-plugin-sdk/jobstop"
	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
)

// Stops holds the node's runs in flight, filed by jobId as each is accepted, so
// a stop for one reaches it — see runHandler. Its other half, Stops.OnSignal,
// belongs on the plugin's signal port, which the binary owns: main registers it.
var Stops jobstop.Registry

// Register wires the node's `run` action onto the plugin. Call it before
// p.Start(). For a run to stop with its flow, the plugin's signal port must also
// carry Stops.OnSignal — see main.
func Register(p *sdkv1.Plugin) {
	p.AddAction(sdkv1.Action{
		Method:         "run",
		Title:          "Run",
		Description:    "Evaluate the state (and any evidence) against typed questions on a decision model \u2014 System One (hosted Jev, local Laya or Ollama nimble) or the Decisions API (gpt-6-luna), chosen by the settings profile \u2014 and route by the answers",
		Middleware:     sdkv1.Use(Stops.Middleware),
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
//
// A run stops with its flow. A decision is paid for, and once the flow is gone
// nothing downstream will route on the answer — not even the `_exception` branch
// — so the action runs through Stops.Middleware, and the job's context is
// cancelled when the runtime stops this job's process (a user stop, a stop
// command, the workflow's timeout, the idle window). It aborts the call in
// flight and any retry wait (see callDecision). Only this job's stop: another
// flow's run of this node is filed under its own jobId.
func runHandler(job sdkv1.Job) {
	ctx := job.Context()
	req, err := sdkv1.CastRequestTo[RunBody](job.Req.Data)
	if err != nil {
		job.DoneWithError(err.Error())
		return
	}
	cfg := req.Body.Settings
	// Which protocol the profile speaks is the first thing to settle: every
	// check below it — the required fields, the score-level ceiling, the wire
	// body, the retryable statuses — depends on the answer.
	d, err := dialectOf(cfg)
	if err != nil {
		job.DoneWithError(err.Error())
		return
	}
	if err := validateSettings(d, cfg); err != nil {
		job.DoneWithError(err.Error())
		return
	}
	questions := req.Body.Questions
	if err := validateQuestions(questions, d.maxScoreLevels(cfg)); err != nil {
		job.DoneWithError(err.Error())
		return
	}
	if err := validateEvidence(req.Body.Evidence); err != nil {
		job.DoneWithError(err.Error())
		return
	}
	job.Progress(5, sdkv1.Frame{Title: "run", Content: "preparing state"})

	// 1. Resolve the templates against the live flow context and fold the
	//    subject and the evidence rows into the one `state` value the API takes
	//    (it has no separate evidence parameter). The model only works on
	//    something to evaluate: nothing to decide ON is a config mistake, not a
	//    decision to route on.
	res := jobResolver(job)
	evidence := resolveEvidence(res, req.Body.Evidence)
	state := assembleState(resolveState(job, req.Body.State), evidence)
	if isBlank(state) {
		job.DoneWithError("empty state: the state template resolved to no content and no evidence was supplied")
		return
	}

	// 2. One call evaluates every question in parallel against the same state.
	//    An API failure (bad key, validation, rate limit past the retry budget,
	//    overload, network) is the first exception case: the flow leaves through
	//    `_exception` instead of stopping at this node.
	model := modelOf(d, cfg)
	job.Progress(20, sdkv1.Frame{
		Title:   "thinking",
		Content: fmt.Sprintf("evaluating %d question(s) on %s", len(questions), model),
	})
	resp, err := callDecision(ctx, d, cfg, d.buildRequest(res, model, state, questions),
		func(msg string) { job.Progress(20, sdkv1.Frame{Title: "waiting", Content: msg}) })
	if err != nil {
		if ctx.Err() != nil {
			// Stopped, not failed: the runtime has concluded this process and
			// stopped listening, so there is no exception branch left to route
			// to — and every command sent now would only wait out the SDK's
			// retries.
			return
		}
		Exception(job, "provider_error", "provider error: "+err.Error(), map[string]any{
			"model":    model,
			"provider": d.name(),
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
	answers := make(map[string]Decision, len(questions))
	var (
		tags      []string // outbound-port tags to fire, in question order
		uncertain []string // routed questions whose confidence fell below their floor
		refused   []string // questions the model declined to answer at all
	)
	for _, q := range questions {
		a, ok := resp.Answers[q.ID]
		if !ok {
			if q.routes() {
				Exception(job, "no_answer", fmt.Sprintf("no answer for routed question %q", q.ID), map[string]any{
					"model":    model,
					"provider": d.name(),
					"question": q.ID,
					"answers":  answers,
				})
				return
			}
			continue
		}
		// A refusal is the Decisions API declining one question while answering
		// the rest. It is not a wire mismatch and not a low-confidence answer —
		// there is simply no answer — so a routed question has no port to take
		// and leaves through `_exception` with its own code, while a data-only
		// one is recorded and skipped.
		if a.Refused {
			refused = append(refused, q.ID)
			if q.routes() {
				Exception(job, "refused", fmt.Sprintf("the model declined to answer routed question %q", q.ID), map[string]any{
					"model":    model,
					"provider": d.name(),
					"question": q.ID,
					"refused":  refused,
					"answers":  answers,
				})
				return
			}
			continue
		}
		dec, err := decide(q, a)
		if err != nil {
			if q.routes() {
				Exception(job, "unbound_option", err.Error(), map[string]any{
					"model":    model,
					"provider": d.name(),
					"question": q.ID,
					"answer":   a,
					"answers":  answers,
				})
				return
			}
			continue
		}
		if q.routes() {
			dec.Routed = true
			tags = append(tags, dec.Tag)
			if q.MinConfidence > 0 && dec.Confidence < q.MinConfidence {
				uncertain = append(uncertain, q.ID)
			}
		}
		answers[q.ID] = dec
	}
	job.Progress(90, sdkv1.Frame{Title: "decided", Content: summarize(answers)})

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
				"model":        model,
				"provider":     d.name(),
				"uncertain":    uncertain,
				"answers":      answers,
				"usage":        resp.Usage,
				"credits_used": resp.CreditsUsed,
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
		"model": model,
		// Which protocol answered. A flow re-pointed between a local decider
		// and a hosted one looks identical on the canvas, so the run is the
		// only place that can record which one actually decided it.
		"provider": d.name(),
		"answers":  answers, // every decision, keyed by question id — what lands on the scope
		"routed":   tags,    // outbound-port tags fired next (empty when nothing routes)
		// Data-only questions the model declined. A routed refusal never gets
		// here — it leaves through `_exception` — so this is the non-routed
		// remainder, reported rather than silently missing from "answers".
		"refused": refused,
		"usage":   resp.Usage,
		// What the call cost and how long the service took. A metered decider is
		// worth accounting for on the canvas, not only in the vendor console.
		"credits_used": resp.CreditsUsed,
		"elapsed_ms":   resp.ElapsedMs,
	})
}
