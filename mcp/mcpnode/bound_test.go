package mcpnode

import (
	"strings"
	"testing"

	"github.com/FloMorphic/builtin-plugins/mcp/llm"
	"github.com/tmc/langchaingo/llms"
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

// allow builds the execution-time whitelist the run loop derives from `bound`.
func allow(ns ...string) map[string]bool {
	m := make(map[string]bool, len(ns))
	for _, n := range ns {
		m[n] = true
	}
	return m
}

func TestUnboundRefusal(t *testing.T) {
	readOnly := allow("list_files", "read_files", "search_files")

	t.Run("a bound tool is not refused", func(t *testing.T) {
		if _, refused := unboundRefusal(readOnly, "call-1", "read_files"); refused {
			t.Fatal("refused a tool that is bound")
		}
	})

	// The whole point of the guard: the model asks for a write/shell tool it was
	// never advertised, and the node must not call it on the server.
	t.Run("an unbound tool is refused", func(t *testing.T) {
		for _, name := range []string{"run_command", "write_file", "edit_file", "hallucinated_tool"} {
			msg, refused := unboundRefusal(readOnly, "call-2", name)
			if !refused {
				t.Fatalf("%s was not refused", name)
			}
			if msg.Role != llms.ChatMessageTypeTool {
				t.Errorf("%s: role = %v, want tool", name, msg.Role)
			}
			if len(msg.Parts) != 1 {
				t.Fatalf("%s: got %d parts, want 1", name, len(msg.Parts))
			}
			resp, ok := msg.Parts[0].(llms.ToolCallResponse)
			if !ok {
				t.Fatalf("%s: part is %T, want ToolCallResponse", name, msg.Parts[0])
			}
			// The id must be echoed back or the next provider request is malformed.
			if resp.ToolCallID != "call-2" {
				t.Errorf("%s: ToolCallID = %q, want call-2", name, resp.ToolCallID)
			}
			if resp.Name != name {
				t.Errorf("%s: Name = %q", name, resp.Name)
			}
			if !strings.Contains(resp.Content, name) {
				t.Errorf("%s: content does not name the tool: %q", name, resp.Content)
			}
			if !strings.Contains(resp.Content, "not bound") {
				t.Errorf("%s: content does not say why: %q", name, resp.Content)
			}
		}
	})

	// selectBoundTools returning nothing already aborts the run, so this is only a
	// belt-and-braces check that an empty map does not silently allow everything.
	t.Run("an empty whitelist refuses everything", func(t *testing.T) {
		if _, refused := unboundRefusal(map[string]bool{}, "call-3", "list_files"); !refused {
			t.Fatal("empty whitelist allowed a call")
		}
	})
}
