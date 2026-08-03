package castnode

import (
	"reflect"
	"testing"

	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
)

// TestResolveValueNoScope covers the branches of resolveValue that never touch
// the flow context: a non-string value passes through untouched, and a string
// with no {{$...}} tokens is returned as-is. A zero Job is safe here precisely
// because these branches return before any CmdGetScope call.
func TestResolveValueNoScope(t *testing.T) {
	var job sdkv1.Job

	cases := []struct {
		name string
		in   any
		want any
	}{
		{"plain string", "hello", "hello"},
		{"number passthrough", float64(42), float64(42)},
		{"bool passthrough", true, true},
		{"nil passthrough", nil, nil},
		{"literal object passthrough", map[string]any{"a": 1}, map[string]any{"a": 1}},
		{"literal array passthrough", []any{1, 2}, []any{1, 2}},
		{"string without tokens", "no tokens here $. plain", "no tokens here $. plain"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveValue(job, tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("resolveValue(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestVarRe checks the token grammar the node shares with the LLM/MCP nodes:
// only $-prefixed paths inside mustaches match, whitespace is tolerated, and the
// captured group is the trimmed path.
func TestVarRe(t *testing.T) {
	cases := []struct {
		in      string
		matches bool
		path    string
	}{
		{"{{$.user.id}}", true, "$.user.id"},
		{"{{ $.a.b }}", true, "$.a.b"},
		{"prefix {{$.x}} suffix", true, "$.x"},
		{"{{notapath}}", false, ""},
		{"no mustaches", false, ""},
	}

	for _, tc := range cases {
		m := varRe.FindStringSubmatch(tc.in)
		if tc.matches {
			if m == nil {
				t.Fatalf("%q: expected a match, got none", tc.in)
			}
			if got := trimPath(m[1]); got != tc.path {
				t.Fatalf("%q: captured %q, want %q", tc.in, got, tc.path)
			}
		} else if m != nil {
			t.Fatalf("%q: expected no match, got %q", tc.in, m[0])
		}
	}
}

// trimPath mirrors the whitespace trimming resolveValue applies to a captured
// path, kept local so the test asserts the exact string the handler would use.
func trimPath(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}
