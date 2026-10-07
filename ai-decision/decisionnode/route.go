package decisionnode

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Question types as the API names them.
const (
	typeChoice = "choice"
	typeScore  = "score"
	typeNoul   = "noul"
)

// The two answers of a noul question, as option names and port tags. The API
// keys the criteria "true"/"false"; the canvas reads better as yes/no.
const (
	noulYes = "yes"
	noulNo  = "no"
)

// Limits on the declared answers that both protocols share. The score CEILING
// is not shared — it depends on the protocol and on which implementation
// serves it — so it arrives as a parameter from the dialect instead (see
// dialect.maxScoreLevels).
const (
	maxChoiceOptions = 255
	minScoreLevels   = 2
)

// Ceilings on the request's arrays.
//
// These are HARDENING rather than protocol fidelity. Every array in the body is
// assembled upstream — the questions and evidence rows come from the settings
// drawer, and each evidence row's text is a template resolved from the live flow
// context — so a loop or a bad path upstream can grow one of them without
// anything here noticing. Past these counts a request is far likelier to be
// that bug than a decision somebody designed, and the failure should name the
// array rather than arrive as a 400, a 413, or a very large bill.
//
// Both are deliberately UNIFORM across the two protocols, unlike
// dialect.maxScoreLevels. Ollama documents 64 questions per request and the
// other implementations publish no limit at all, so a per-dialect ceiling would
// be mostly invented — and worse, it would mean a flow that validates against
// one profile fails against another, which is the exact portability the dialect
// split exists to protect. One generous shared ceiling keeps "swap the profile,
// keep the canvas" true.
const (
	// The only documented figure (Ollama's). Sixty-four separate questions
	// about one state is already far past what a readable node does.
	maxQuestions = 64
	// Undocumented anywhere, so this is node policy: evidence is meant to be a
	// filtered top-k of retrieved chunks, and the rows share one context budget
	// with the state and every question. A retriever handing over hundreds of
	// rows has not filtered them.
	maxEvidenceRows = 64
)

// tagFor is the outbound-port tag of one declared answer: the question id and
// the option name joined with a dot, so options of different questions that
// share a name ("high") never collide on an edge.
func tagFor(questionID, option string) string {
	return questionID + "." + option
}

// normalizeType lower-cases a question type and accepts the obvious aliases.
func normalizeType(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case typeChoice, "select", "option":
		return typeChoice
	case typeScore, "scale", "level":
		return typeScore
	case typeNoul, "bool", "boolean", "yesno", "yes_no":
		return typeNoul
	}
	return strings.ToLower(strings.TrimSpace(t))
}

// noulName maps a noul option row's name onto the canonical yes/no.
func noulName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case noulYes, "true", "y", "1":
		return noulYes
	case noulNo, "false", "n", "0":
		return noulNo
	}
	return ""
}

// validateInstructions accepts either form the API documents — a plain string
// question, or a structured object/array that carries the question in one field
// and its reference data in the others — and rejects an empty one of either.
// The emptiness check runs on the TEMPLATE, before resolution: an unresolvable
// {{$.path}} is left in place by design, so a question can never resolve to
// nothing, and a blank here is a drawer the designer did not finish.
func validateInstructions(id string, v any) error {
	switch t := v.(type) {
	case string:
		if strings.TrimSpace(t) == "" {
			return fmt.Errorf("question %q has no instructions", id)
		}
	case map[string]any:
		if len(t) == 0 {
			return fmt.Errorf("question %q has empty instructions", id)
		}
	case []any:
		if len(t) == 0 {
			return fmt.Errorf("question %q has empty instructions", id)
		}
	case nil:
		return fmt.Errorf("question %q has no instructions", id)
	default:
		return fmt.Errorf("question %q: instructions must be a string, an object or an array", id)
	}
	return nil
}

// validateEvidence checks the evidence rows the way the API would see them
// once assembleState has folded them into the state: every row has to carry
// text, since a row that contributes nothing only spends context budget.
func validateEvidence(rows []EvidenceItem) error {
	if len(rows) > maxEvidenceRows {
		return fmt.Errorf("too many evidence rows: %d, the most one call takes is %d — evidence is a filtered top-k, and the rows share one context budget with the state and every question", len(rows), maxEvidenceRows)
	}
	for i, r := range rows {
		if strings.TrimSpace(r.Text) == "" {
			return fmt.Errorf("evidence row #%d has no text", i+1)
		}
	}
	return nil
}

// validateQuestions checks every question's shape the way the service will, so
// a config mistake fails here with a readable reason instead of as a 400/422.
// Ids must be non-empty and unique (they key the reply and prefix the tags);
// option names must be non-empty and unique within a question; option counts
// must fit the type's limits. Types are normalized in place.
//
// maxScoreLevels comes from the dialect because it is the one limit that is not
// a property of the node: the System One shape allows 26 ordered levels on a
// local Ollama decider and 10 on TypeSafe's hosted service, and the Decisions
// shape allows 10. Validating against a single hardcoded ceiling would reject
// questions a local decider answers perfectly well.
func validateQuestions(qs []Question, maxScoreLevels int) error {
	if len(qs) == 0 {
		return fmt.Errorf("no questions: declare at least one question with its options")
	}
	if len(qs) > maxQuestions {
		return fmt.Errorf("too many questions: %d, the most one call takes is %d — a count like this is usually a loop or a bad path upstream rather than a decision", len(qs), maxQuestions)
	}
	seen := make(map[string]struct{}, len(qs))
	for i := range qs {
		q := &qs[i]
		q.ID = strings.TrimSpace(q.ID)
		if q.ID == "" {
			return fmt.Errorf("question #%d has no id", i+1)
		}
		if _, dup := seen[q.ID]; dup {
			return fmt.Errorf("question id %q is declared twice", q.ID)
		}
		seen[q.ID] = struct{}{}
		q.Type = normalizeType(q.Type)
		if err := validateInstructions(q.ID, q.Instructions); err != nil {
			return err
		}
		if q.MinConfidence < 0 || q.MinConfidence > 1 {
			return fmt.Errorf("question %q: min_confidence must be within 0..1", q.ID)
		}

		names := make(map[string]struct{}, len(q.Options))
		for j := range q.Options {
			o := &q.Options[j]
			o.Name = strings.TrimSpace(o.Name)
			if q.Type == typeNoul {
				o.Name = noulName(o.Name)
				if o.Name == "" {
					return fmt.Errorf("question %q: a noul option must be named yes or no", q.ID)
				}
			}
			if o.Name == "" {
				return fmt.Errorf("question %q: option #%d has no name", q.ID, j+1)
			}
			if _, dup := names[o.Name]; dup {
				return fmt.Errorf("question %q: option %q is declared twice", q.ID, o.Name)
			}
			names[o.Name] = struct{}{}
		}

		switch q.Type {
		case typeChoice:
			if n := len(q.Options); n == 0 || n > maxChoiceOptions {
				return fmt.Errorf("question %q: a choice needs 1..%d options, got %d", q.ID, maxChoiceOptions, n)
			}
		case typeScore:
			if n := len(q.Options); n < minScoreLevels || n > maxScoreLevels {
				return fmt.Errorf("question %q: a score needs %d..%d levels, got %d", q.ID, minScoreLevels, maxScoreLevels, n)
			}
		case typeNoul:
			// Rows only supply descriptions; none, one or both is fine.
		default:
			return fmt.Errorf("question %q: unknown type %q (choice, score or noul)", q.ID, q.Type)
		}
	}
	return nil
}

func descOrName(o Option) string {
	if d := strings.TrimSpace(o.Description); d != "" {
		return d
	}
	return o.Name
}

// decide reduces one answer to the Decision the flow routes on. For a choice
// the top answer is the API's `choice`; for a score it is the level with the
// highest probability (the legend's index → the option row at that index); for
// a noul it is "yes" when the probability is ≥ 0.5. The distribution is
// re-keyed by option name in every case, and confidence is the service's own
// value where it reports one (choice/score, on both protocols), falling back to
// the top answer's probability where it does not, or — for a noul/predicate,
// which neither protocol scores — how far the probability sits from the
// coin-flip (max(p, 1−p)). It is a measure of decisiveness, not of correctness;
// see Decision.Confidence.
//
// The error is the defensive "unbound option" case: an answer naming something
// the question did not declare — impossible by Jev's contract, but a wire
// mismatch must not turn into a tag no port carries.
func decide(q Question, a answer) (Decision, error) {
	d := Decision{Question: q.ID, Type: q.Type, Probabilities: map[string]float64{}}
	switch q.Type {
	case typeChoice:
		d.Answer = strings.TrimSpace(a.Choice)
		if !hasOption(q, d.Answer) {
			return d, fmt.Errorf("question %q answered %q, which is not a declared option", q.ID, d.Answer)
		}
		for _, o := range q.Options {
			d.Probabilities[o.Name] = a.Probabilities[o.Name]
		}
		if a.Confidence != nil {
			d.Confidence = *a.Confidence
		} else {
			d.Confidence = d.Probabilities[d.Answer]
		}
	case typeScore:
		// Probabilities come keyed by level index ("0", "1", …); map each back
		// onto the declared level at that index and pick the most probable.
		best, bestP := -1, -1.0
		for i, o := range q.Options {
			p := a.Probabilities[strconv.Itoa(i)]
			d.Probabilities[o.Name] = p
			if p > bestP {
				best, bestP = i, p
			}
		}
		if best < 0 {
			return d, fmt.Errorf("question %q: score answer carries no level probabilities", q.ID)
		}
		d.Answer = q.Options[best].Name
		d.Score = a.Score
		if a.Confidence != nil {
			d.Confidence = *a.Confidence
		} else {
			d.Confidence = bestP
		}
	case typeNoul:
		if a.P == nil {
			return d, fmt.Errorf("question %q: noul answer carries no probability", q.ID)
		}
		p := *a.P
		d.P = &p
		d.Probabilities[noulYes] = p
		d.Probabilities[noulNo] = 1 - p
		if p >= 0.5 {
			d.Answer, d.Confidence = noulYes, p
		} else {
			d.Answer, d.Confidence = noulNo, 1-p
		}
	default:
		return d, fmt.Errorf("question %q: unsupported type %q", q.ID, q.Type)
	}
	d.Tag = tagFor(q.ID, d.Answer)
	return d, nil
}

// hasOption reports whether name is one of the question's declared options. A
// noul declares yes/no implicitly whatever rows it carries.
func hasOption(q Question, name string) bool {
	if q.Type == typeNoul {
		return name == noulYes || name == noulNo
	}
	for _, o := range q.Options {
		if o.Name == name {
			return true
		}
	}
	return false
}

// summarize renders the decisions as one line per question for a progress
// frame — "category → billing (0.88)" — in a stable order.
func summarize(decisions map[string]Decision) string {
	ids := make([]string, 0, len(decisions))
	for id := range decisions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	lines := make([]string, 0, len(ids))
	for _, id := range ids {
		d := decisions[id]
		lines = append(lines, fmt.Sprintf("%s → %s (%.2f)", id, d.Answer, d.Confidence))
	}
	return strings.Join(lines, "\n")
}
