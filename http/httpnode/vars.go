package httpnode

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
	"github.com/bytedance/sonic"
)

// {{ $.a.b }} — capture the JSON path inside the mustaches. This is the same
// token convention the LLM/MCP/Cast nodes use, so an HTTP field references the
// flow context exactly like a prompt does.
var varRe = regexp.MustCompile(`\{\{\s*(\$[^}]+?)\s*\}\}`)

// resolveVars finds every {{$...}} in text, fetches each distinct path ONCE from
// the runtime via CmdGetScope, and substitutes it back in. Unresolvable tokens
// are left verbatim so nothing is silently dropped. A string with no tokens is
// returned unchanged.
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

// resolveKVs resolves the {{$...}} tokens in every pair's value against the live
// flow context, dropping pairs with a blank key (they have nowhere to land).
func resolveKVs(job sdkv1.Job, pairs []KV) []KV {
	out := make([]KV, 0, len(pairs))
	for _, p := range pairs {
		key := strings.TrimSpace(p.Key)
		if key == "" {
			continue
		}
		out = append(out, KV{Key: key, Value: resolveVars(job, p.Value)})
	}
	return out
}

// fetchScopeString reads a JSON path from the flow context for interpolation
// into surrounding text: a JSON string is unwrapped to its value, anything else
// is inlined as its raw JSON text. An empty reply leaves the token verbatim so
// nothing is silently dropped.
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
