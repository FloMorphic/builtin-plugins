package castnode

import (
	"fmt"
	"strings"

	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
)

// Register wires the Cast / Mapping node's `run` action onto the plugin. Call it
// before p.Start().
func Register(p *sdkv1.Plugin) {
	p.AddAction(sdkv1.Action{
		Method:         "run",
		Title:          "Run",
		Description:    "Assemble a JSON object from key/value mappings, resolving any {{$.a.b}} tokens in the values against the live flow context",
		RequestHandler: runHandler,
	})
}

// runHandler implements the Cast / Mapping node's single `run` action. It reads
// the ordered key/value mappings, resolves any {{$...}} tokens in each value
// against the live flow context (see resolveValue), and commits the assembled
// object to the node's scope.
//
// The resolved object IS the terminal Done payload, so it becomes this node's
// scope verbatim: each mapping key lands at the top level, readable downstream as
// {{$.<node>.<key>}}. A later key with the same name as an earlier one wins, and
// a pair with a blank key is skipped (it has nowhere to land).
func runHandler(job sdkv1.Job) {
	req, err := sdkv1.CastRequestTo[RunBody](job.Req.Data)
	if err != nil {
		job.DoneWithError(err.Error())
		return
	}
	mappings := req.Body.Mappings
	if len(mappings) == 0 {
		job.DoneWithError("no mappings: the node has no key/value pairs to cast")
		return
	}

	job.Progress(20, sdkv1.Frame{
		Title:   "cast",
		Content: fmt.Sprintf("resolving %d field(s)", len(mappings)),
	})

	out := make(map[string]any, len(mappings))
	for _, m := range mappings {
		key := strings.TrimSpace(m.Key)
		if key == "" {
			continue // a pair with no key has nowhere to land in the object
		}
		out[key] = resolveValue(job, m.Value)
	}

	if len(out) == 0 {
		job.DoneWithError("no mappings with a key: every pair had a blank key")
		return
	}

	// Commit the assembled object as the node's scope: Done's details ARE the
	// committed payload, so reporting the keys at the top level makes the node's
	// output the mapped object itself.
	job.Done(out)
}
