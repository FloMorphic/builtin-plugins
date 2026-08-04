package httpnode

// KV is one key/value pair the drawer collects — used for request headers,
// default (profile) headers, and query-string parameters. Value may embed
// {{$.a.b}} tokens resolved against the live flow context before the request is
// built (see resolveVars).
type KV struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// HTTPSettings is the shape the frontend settings-profile must produce (shipped
// in body.settings). It carries the connection-level config every request off
// this node shares: a base URL, an auth method, and default headers. Keep this
// in lockstep with the frontend `http` settings schema (settingsSchemas.ts).
//
// AuthType selects the credential the request carries:
//
//   - ""/"none" : no auth header is added.
//   - "basic"   : Authorization: Basic base64(Username:Password).
//   - "bearer"  : Authorization: Bearer <Token>.
//   - "api_key" : a single header (HeaderName, default Authorization) set to Token.
type HTTPSettings struct {
	BaseURL            string `json:"base_url"`             // prepended to a relative request URL
	AuthType           string `json:"auth_type"`           // "", none, basic, bearer, api_key
	Username           string `json:"username"`            // basic auth user
	Password           string `json:"password"`            // basic auth password
	Token              string `json:"token"`               // bearer / api_key value
	HeaderName         string `json:"header_name"`         // api_key header name (default Authorization)
	Headers            []KV   `json:"headers"`             // default headers on every request
	TimeoutSeconds     int    `json:"timeout_seconds"`     // request timeout; 0 → default
	InsecureSkipVerify bool   `json:"insecure_skip_verify"` // skip TLS cert verification
}

// RunBody is the inner `body` of the run action envelope
// ({ "_registry": {...}, "body": {...} }). Settings is fed by the settings
// profile; the rest is the per-request config the node's drawer collects.
//
// Every string field (URL, header/query values, Body, and the settings' auth
// credentials) may embed {{$.a.b}} tokens resolved against the live flow context
// before the request is sent.
type RunBody struct {
	Settings HTTPSettings `json:"settings"`
	Method   string       `json:"method"`    // GET/POST/PUT/PATCH/DELETE… (default GET)
	URL      string       `json:"url"`       // full URL, or a path appended to Settings.BaseURL
	Headers  []KV         `json:"headers"`   // per-request headers (override profile defaults)
	Query    []KV         `json:"query"`     // query-string parameters
	Body     string       `json:"body"`      // raw request body
	BodyType string       `json:"body_type"` // json|text|form → default Content-Type
}

// httpResult is the terminal payload committed to the node's scope: the request
// as sent (method + resolved URL), the response status, its headers and parsed
// body, and `ok` (a 2xx status). Downstream nodes read them as {{$.<node>.status}},
// {{$.<node>.body}}, etc., so a Rule node can branch on the status without this
// node needing its own error port.
type httpResult struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Status  int               `json:"status"`
	OK      bool              `json:"ok"`
	Headers map[string]string `json:"headers"`
	Body    any               `json:"body"`
}
