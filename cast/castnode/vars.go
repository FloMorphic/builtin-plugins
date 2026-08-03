package castnode

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
	"github.com/bytedance/sonic"
)

// {{ $.a.b }} — capture the JSON path inside the mustaches. This is the same
// token convention the LLM/MCP nodes use, so a mapping value references the flow
// context exactly like a prompt does.
var varRe = regexp.MustCompile(`\{\{\s*(\$[^}]+?)\s*\}\}`)

// resolveValue resolves the {{$...}} tokens in one mapping value against the live
// flow context. A non-string value (a number, bool, nested object/array supplied
// literally in the drawer) has no tokens and passes through untouched.
//
// The string case has two modes:
//
//   - Whole-value token: when the trimmed value is exactly one {{$.path}} and
//     nothing else, the raw JSON value at that path is returned with its TYPE
//     intact — an object stays an object, a number a number, an array an array.
//     This is what lets a mapping lift a nested structure out of the scope, not
//     just its string form.
//   - Embedded token(s): when one or more tokens sit inside surrounding text,
//     string interpolation is used and the result is always a string, each path
//     resolved once.
//
// A token that resolves to nothing (an unset path) is left verbatim so a mapping
// never silently drops to empty.
func resolveValue(job sdkv1.Job, value any) any {
	s, ok := value.(string)
	if !ok {
		return value
	}
	matches := varRe.FindAllStringSubmatch(s, -1)
	if len(matches) == 0 {
		return s
	}

	// Whole-value token → return the typed JSON value at that path.
	trimmed := strings.TrimSpace(s)
	if whole := varRe.FindStringSubmatch(trimmed); whole != nil && whole[0] == trimmed {
		path := strings.TrimSpace(whole[1])
		if v, ok := fetchScopeValue(job, path); ok {
			return v
		}
		return s // unresolvable → keep the value as written
	}

	// Embedded token(s) → string interpolation. Resolve each distinct path once.
	cache := make(map[string]string, len(matches))
	for _, m := range matches {
		path := strings.TrimSpace(m[1])
		if _, done := cache[path]; done {
			continue
		}
		cache[path] = fetchScopeString(job, path)
	}
	return varRe.ReplaceAllStringFunc(s, func(tok string) string {
		path := strings.TrimSpace(varRe.FindStringSubmatch(tok)[1])
		if v, ok := cache[path]; ok {
			return v
		}
		return tok
	})
}

// fetchScopeValue reads a JSON path from the flow context and unmarshals it to
// its natural Go type (map, slice, string, float64, bool, nil). ok is false when
// the path returned nothing, so the caller can leave the token in place. Raw JSON
// that fails to unmarshal is handed back as its string form rather than dropped.
func fetchScopeValue(job sdkv1.Job, jsonPath string) (any, bool) {
	raw, ok := job.CmdGetScope(jsonPath).([]byte)
	if !ok || len(raw) == 0 {
		return nil, false
	}
	var v any
	if err := sonic.Unmarshal(raw, &v); err != nil {
		return string(raw), true
	}
	return v, true
}

// fetchScopeString reads a JSON path for interpolation into surrounding text: a
// JSON string is unwrapped to its value, anything else is inlined as its raw JSON
// text. An empty reply leaves the token verbatim so nothing is silently dropped.
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
