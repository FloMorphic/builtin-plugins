package jevnode

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

// resolveState turns the state template into the value sent as the API's
// `state`. A template that is exactly one {{$...}} token whose context value is
// JSON is sent as that JSON value (Jev accepts an object or array state, and a
// structured ticket reads better than its string dump); if the path is unset
// the token is kept as text so the mistake is visible. Any other template is
// resolved token by token into a string (see resolveVars).
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
