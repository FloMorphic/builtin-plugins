// Package castnode implements the Inflow "Cast / Mapping" plugin node: a single
// `run` action that assembles a JSON object out of a list of key/value mappings.
//
// The node exposes ONE action, `run`. Every request body arrives as the SDK
// envelope { "_registry": {...}, "body": {...} }, and `body` (see RunBody) has a
// single part:
//
//   - mappings : the ordered list of key/value pairs the node's drawer collects.
//     Each pair is a `key` (the field name it lands on in the output object) and
//     a `value` — any JSON value. When the value is a string it may embed
//     {{$.a.b}} tokens, resolved against the live flow context via
//     job.CmdGetScope before the object is built (see resolveValue). This is the
//     same token convention the LLM and MCP nodes use for prompts.
//
// Value resolution (resolveValue) has two modes:
//
//   - A value that is exactly one whole {{$.path}} token resolves to the TYPED
//     JSON value at that path — an object stays an object, a number stays a
//     number — so a mapping can lift a nested structure out of the scope.
//   - A value with {{$.path}} tokens embedded in surrounding text is string
//     interpolated, and the result is always a string.
//
// Non-string values (numbers, bools, literal objects/arrays entered in the
// drawer) carry no tokens and pass through unchanged. A token that resolves to
// nothing is left verbatim rather than dropped.
//
// The assembled object is committed to the node's scope by being the terminal
// Done payload: job.Done's details ARE the commit, so each mapping key lands at
// the top level of the node's scope and is readable downstream as
// {{$.<node>.<key>}}. Pairs with a blank key are skipped; a later duplicate key
// overwrites an earlier one.
//
// Failure cases are plain config mistakes, reported with DoneWithError and no
// routing: an unreadable request body, an empty mappings list, or a mappings list
// in which every pair has a blank key.
//
// Runtime scenario for `run`:
//  1. Decode the request body into RunBody.
//  2. For each mapping with a non-blank key, resolve its value's {{$...}} tokens
//     against the live flow context and set out[key] = resolved.
//  3. Commit the assembled object as the node's scope via job.Done.
package castnode
