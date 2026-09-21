package jevnode

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

// API limits on the declared answers.
const (
	maxChoiceOptions = 255
	minScoreLevels   = 2
	maxScoreLevels   = 10
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

// validateQuestions checks every question's shape the way the API will, so a
// config mistake fails here with a readable reason instead of as a 422. Ids
// must be non-empty and unique (they key the reply and prefix the tags); option
// names must be non-empty and unique within a question; option counts must fit
// the type's limits. Types are normalized in place.
func validateQuestions(qs []Question) error {
	if len(qs) == 0 {
		return fmt.Errorf("no questions: declare at least one question with its options")
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
		if strings.TrimSpace(q.Instructions) == "" {
			return fmt.Errorf("question %q has no instructions", q.ID)
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

// buildCriteria shapes a question's declared answers into the API's criteria
// for its type. An empty description falls back to the option's name so the
// model always has something to score against.
func buildCriteria(q Question) any {
	switch q.Type {
	case typeChoice:
		m := make(map[string]string, len(q.Options))
		for _, o := range q.Options {
			m[o.Name] = descOrName(o)
		}
		return m
	case typeScore:
		levels := make([]string, len(q.Options))
		for i, o := range q.Options {
			levels[i] = descOrName(o)
		}
		return levels
	case typeNoul:
		m := map[string]string{"true": "Yes", "false": "No"}
		for _, o := range q.Options {
			if d := strings.TrimSpace(o.Description); d != "" {
				m[map[string]string{noulYes: "true", noulNo: "false"}[o.Name]] = d
			}
		}
		return m
	}
	return nil
}

func descOrName(o Option) string {
	if d := strings.TrimSpace(o.Description); d != "" {
		return d
	}
	return o.Name
}

// buildRequest assembles the wire request from the resolved state and the
// validated questions.
func buildRequest(cfg JevSettings, state any, qs []Question) apiRequest {
	req := apiRequest{State: state, Model: modelOf(cfg), Questions: make(map[string]apiQuestion, len(qs))}
	for _, q := range qs {
		req.Questions[q.ID] = apiQuestion{Type: q.Type, Instructions: q.Instructions, Criteria: buildCriteria(q)}
	}
	return req
}

// decide reduces one answer to the Decision the flow routes on. For a choice
// the top answer is the API's `choice`; for a score it is the level with the
// highest probability (the legend's index → the option row at that index); for
// a noul it is "yes" when the probability is ≥ 0.5. The distribution is
// re-keyed by option name in every case, and confidence is the API's calibrated
// value where it gives one (choice/score) or, for a noul, how far the
// probability sits from the coin-flip (max(p, 1−p)).
//
// The error is the defensive "unbound option" case: an answer naming something
// the question did not declare — impossible by Jev's contract, but a wire
// mismatch must not turn into a tag no port carries.
func decide(q Question, a apiAnswer) (Decision, error) {
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
		if a.Noul == nil {
			return d, fmt.Errorf("question %q: noul answer carries no probability", q.ID)
		}
		p := *a.Noul
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
