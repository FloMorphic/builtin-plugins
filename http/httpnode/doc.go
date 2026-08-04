// Package httpnode implements the Inflow "HTTP" plugin node: a single `run`
// action that makes an HTTP / REST request and commits the response to the
// node's scope.
//
// The node exposes ONE action, `run`. Every request body arrives as the SDK
// envelope { "_registry": {...}, "body": {...} }, and `body` (see RunBody) has
// two halves:
//
//   - settings : the connection-level config from the node's settings profile
//     (HTTPSettings) — a base URL, an auth method (none / basic / bearer /
//     api_key), default headers applied to every request, a timeout, and an
//     optional TLS-skip. The frontend resolves the selected profile onto the
//     node and the compiler ships it here as body.settings.
//   - the per-request fields the drawer collects: method, url, headers, query,
//     body and body_type.
//
// Variable resolution: every string field — url, header/query values, body, and
// the settings' auth credentials (base URL, username, password, token) — may
// embed {{$.a.b}} tokens, resolved against the live flow context via
// job.CmdGetScope before the request is sent (see resolveVars). This is the same
// token convention the LLM / MCP / Cast nodes use.
//
// URL assembly (buildURL): a url that already names http:// or https:// is
// absolute and used as-is; otherwise it is appended to settings.base_url with a
// single slash between them. Query pairs are added to whatever query string the
// url already carried.
//
// Auth (applyAuth): basic → Authorization: Basic base64(user:pass); bearer →
// Authorization: Bearer <token>; api_key → a single header (header_name,
// default Authorization) set to the token. A header the drawer already set of
// the same name is never overwritten, so a hand-set Authorization always wins.
//
// Result: the response is committed to the node's scope by being the terminal
// Done payload — method, url, status, ok (a 2xx), headers (flattened), and body
// (decoded to its natural type for JSON responses, else the raw string). A
// downstream node reads them as {{$.<node>.status}}, {{$.<node>.body}}, etc., so
// a Rule node can branch on the status without this node needing an error port.
//
// Failure cases: a transport-level failure (bad URL, DNS, connection, timeout)
// and a missing url are hard errors reported with DoneWithError and no result.
// An HTTP response — including a 4xx/5xx — is NOT a failure here: it is committed
// with its status so the flow can decide what a non-2xx means.
//
// Runtime scenario for `run`:
//  1. Decode the request body into RunBody.
//  2. Resolve the {{$...}} tokens in every string field.
//  3. Assemble the URL (base + path + query) and build the request, applying
//     default + per-request headers and the profile's auth.
//  4. Send it (profile timeout, optional TLS-skip) and read the response.
//  5. Commit {method, url, status, ok, headers, body} as the node's scope.
package httpnode
