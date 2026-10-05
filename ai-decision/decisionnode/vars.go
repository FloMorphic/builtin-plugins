package decisionnode

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
	"github.com/bytedance/sonic"
)

// {{ $.a.b }} — capture the JSON path inside the mustaches.
var varRe = regexp.MustCompile(`\{\{\s*(\$[^}]+?)\s*\}\}`)

// singleTokenRe matches a template that is nothing but one {{$...}} token —
// the case where the designer points the state at one context value and wants
// it sent as it is (a JSON object, an array) instead of stringified.
var singleTokenRe = regexp.MustCompile(`^\s*\{\{\s*(\$[^}]+?)\s*\}\}\s*$`)

// resolver resolves the {{$.a.b}} tokens of ONE designer-authored template
// against the live flow context. Passing it around instead of the job keeps
// every assembly step below a pure function of its input, which is what makes
// them testable without infra.
type resolver func(string) string

// jobResolver binds a job as the resolver for this run.
func jobResolver(job sdkv1.Job) resolver {
	return func(text string) string { return resolveVars(job, text) }
}

// resolveTemplateValue resolves every string inside a template value, at any
// depth: a string is resolved, a map has its VALUES resolved (keys are left
// alone — for a structured `instructions` they are the field names a question
// cites by backtick, not content), a slice has its elements resolved, and
// anything else (a number, a bool, nil) is already a literal.
//
// This is what makes a structured `instructions` object carry flow data: the
// question sits in one field and `{{$.kb.policy}}` in another, and both arrive
// at the service resolved.
func resolveTemplateValue(res resolver, v any) any {
	switch t := v.(type) {
	case string:
		return res(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = resolveTemplateValue(res, val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = resolveTemplateValue(res, val)
		}
		return out
	default:
		return v
	}
}

// resolveEvidence resolves each evidence row's templates. Rows keep their order
// and their shape, so a question can cite one by index (`evidence[0].text`).
func resolveEvidence(res resolver, rows []EvidenceItem) []EvidenceItem {
	if len(rows) == 0 {
		return nil
	}
	out := make([]EvidenceItem, len(rows))
	for i, r := range rows {
		out[i] = EvidenceItem{Source: res(r.Source), Text: res(r.Text)}
	}
	return out
}

// assembleState folds the subject and the evidence rows into the one `state`
// value the API takes — it has no separate evidence parameter.
//
// With no evidence the subject IS the state, unchanged, so a body written
// before evidence existed behaves exactly as it did. With evidence the state
// becomes an object: the subject under "case" and the rows under "evidence", so
// a question can point at either by backticked path (`case.problem`,
// `evidence[1].text`). A subject that resolved to nothing is left out rather
// than sent as an empty field, since evidence alone is content enough to decide
// on.
func assembleState(subject any, evidence []EvidenceItem) any {
	if len(evidence) == 0 {
		return subject
	}
	out := map[string]any{"evidence": evidence}
	if !isBlank(subject) {
		out["case"] = subject
	}
	return out
}

// isBlank reports whether a resolved subject carries no content.
func isBlank(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// resolveState turns the state template into the value sent as the API's
// `state`. A template that is exactly one {{$...}} token whose context value is
// JSON is sent as that JSON value (the API accepts an object or array state,
// and a structured ticket reads better than its string dump); if the path is
// unset the token is kept as text so the mistake is visible. Any other template
// is resolved token by token into a string (see resolveVars).
//
// Note what is NOT resolved: the tokens of the designer's template are, but the
// VALUE that came back from the scope is passed through as it stands, without a
// second pass over the strings inside it. That line is deliberate — templates
// are authored on the canvas, while a scope value is runtime data (a retrieved
// chunk, a customer's own words), and re-resolving it would let that data read
// the flow context.
func resolveState(job sdkv1.Job, template string) any {
	if m := singleTokenRe.FindStringSubmatch(template); m != nil {
		path := strings.TrimSpace(m[1])
		if raw, ok := job.CmdGetScope(path).([]byte); ok && len(raw) > 0 {
			var v any
			if err := sonic.Unmarshal(raw, &v); err == nil && v != nil {
				return v
			}
			return string(raw)
		}
		return fmt.Sprintf("{{%s}}", path)
	}
	return resolveVars(job, template)
}

// resolveVars finds every {{$...}} in text, fetches each distinct path ONCE from
// the runtime via CmdGetScope, and substitutes it back in. Unresolvable tokens
// are left verbatim so nothing is silently dropped.
func resolveVars(job sdkv1.Job, text string) string {
	matches := varRe.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return text
	}
	// Collect all variables first, then resolve each unique path a single time.
	cache := make(map[string]string, len(matches))
	for _, m := range matches {
		path := strings.TrimSpace(m[1])
		if _, done := cache[path]; done {
			continue
		}
		cache[path] = fetchScopeString(job, path)
	}
	return varRe.ReplaceAllStringFunc(text, func(tok string) string {
		path := strings.TrimSpace(varRe.FindStringSubmatch(tok)[1])
		if v, ok := cache[path]; ok {
			return v
		}
		return tok
	})
}

// fetchScopeString reads a JSON path from the flow context. The reply is JSON:
// a JSON string is unwrapped to its value, anything else is returned raw so it
// can be inlined into the state text.
func fetchScopeString(job sdkv1.Job, jsonPath string) string {
	raw, ok := job.CmdGetScope(jsonPath).([]byte)
	if !ok || len(raw) == 0 {
		return fmt.Sprintf("{{%s}}", jsonPath) // leave the token in place
	}
	var s string
	if err := sonic.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}
