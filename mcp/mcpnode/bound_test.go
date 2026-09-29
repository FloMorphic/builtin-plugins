package mcpnode

import (
	"testing"

	"github.com/FloMorphic/builtin-plugins/mcp/llm"
)

func names(ts []McpTool) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The server's advertised set, in the order the server returned it.
var server = []McpTool{
	{Name: "list_files"}, {Name: "read_files"}, {Name: "search_files"},
	{Name: "write_file"}, {Name: "edit_file"}, {Name: "run_command"},
}

func bind(ns ...string) []llm.BoundFunction {
	out := make([]llm.BoundFunction, len(ns))
	for i, n := range ns {
		out[i] = llm.BoundFunction{Name: n}
	}
	return out
}

func TestSelectBoundTools(t *testing.T) {
	cases := []struct {
		name  string
		funcs []llm.BoundFunction
		want  []string
	}{{
		// The documented meaning of an empty list, which the drawer's "nothing
		// ticked" warning relies on being true.
		name:  "empty binds every server tool",
		funcs: nil,
		want:  []string{"list_files", "read_files", "search_files", "write_file", "edit_file", "run_command"},
	}, {
		name:  "a subset binds only that subset",
		funcs: bind("list_files", "read_files", "search_files"),
		want:  []string{"list_files", "read_files", "search_files"},
	}, {
		// The write tools must not leak back in just because the server offers them.
		name:  "read-only selection excludes the write tools",
		funcs: bind("read_files"),
		want:  []string{"read_files"},
	}, {
		name:  "bound order wins over server order",
		funcs: bind("run_command", "list_files"),
		want:  []string{"run_command", "list_files"},
	}, {
		// A tool the node remembers but the server dropped: skipped, not invented.
		name:  "a name the server does not advertise is dropped",
		funcs: bind("list_files", "no_such_tool"),
		want:  []string{"list_files"},
	}, {
		name:  "every name unknown yields nothing to bind",
		funcs: bind("ghost"),
		want:  []string{},
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := names(selectBoundTools(server, c.funcs))
			if !eq(got, c.want) {
				t.Fatalf("selectBoundTools() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestResolveMaxToolTurns(t *testing.T) {
	// The floor is deliberate: a caller may raise the cap but not drop below the
	// default, which matches the minimum the drawer enforces.
	cases := []struct{ in, want int }{
		{0, defaultMcpMaxToolTurns},
		{-5, defaultMcpMaxToolTurns},
		{1, defaultMcpMaxToolTurns},
		{defaultMcpMaxToolTurns, defaultMcpMaxToolTurns},
		{defaultMcpMaxToolTurns + 1, defaultMcpMaxToolTurns + 1},
		{24, 24},
	}
	for _, c := range cases {
		if got := resolveMaxToolTurns(c.in); got != c.want {
			t.Errorf("resolveMaxToolTurns(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}
