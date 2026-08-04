# HTTP node

An Inflow plugin node that makes an HTTP / REST request and commits the response
to its scope. Connection config (base URL, auth, default headers) comes from a
settings profile; the per-request method, URL, headers, query and body are set
on the node. Every string field may embed `{{$.a.b}}` tokens, resolved against
the live flow context before the request is sent — the same convention the
LLM / MCP / Cast nodes use.

## What it does

Given a settings profile and the drawer's request fields, the node:

- Resolves the `{{$.a.b}}` tokens in every string field (URL, header/query
  values, body, and the profile's auth credentials).
- Assembles the URL: a URL that already names `http://`/`https://` is used as-is;
  otherwise it is appended to the profile's **base URL**. Query pairs are added
  to whatever query string the URL already carried.
- Applies the profile's **default headers**, then the per-request headers (which
  override a default of the same name), then a default `Content-Type` from the
  body type when a body is present.
- Adds the profile's **auth**:
  - `basic` → `Authorization: Basic base64(user:pass)`
  - `bearer` → `Authorization: Bearer <token>`
  - `api_key` → a single header (`header_name`, default `Authorization`) set to
    the token
  - A header the drawer already set of the same name is never overwritten.
- Sends the request (profile timeout, optional TLS-skip) and commits the
  response.

## Output

The response is committed to the node's scope by being the terminal `Done`
payload — downstream nodes read the fields as `{{$.<node>.status}}`,
`{{$.<node>.body}}`, etc.:

```jsonc
{
  "method":  "GET",
  "url":     "https://api.example.com/users/1",
  "status":  200,
  "ok":      true,                       // true for a 2xx status
  "headers": { "Content-Type": "application/json" },
  "body":    { "id": 1, "name": "Ada" }  // JSON is decoded; anything else is a string
}
```

A **transport-level failure** (bad URL, DNS, connection, timeout) and a missing
URL are hard errors (`DoneWithError`). An **HTTP response — including a
4xx/5xx — is not a failure**: it is committed with its status so a downstream
Rule node can branch on `ok` / `status`.

## Layout

```
main.go        thin bind layer: build the SDK plugin, register, Start, block
httpnode/      all node functionality
  doc.go         package overview + runtime scenario
  types.go       request/response shapes (KV, HTTPSettings, RunBody, httpResult)
  vars.go        {{$.a.b}} token resolution
  request.go     URL assembly, header + auth builders, response-header flatten
  handler.go     the `run` action handler and Register(p)
  request_test.go unit tests for URL/auth/header/body building
```

The root binary only wires things together; everything else lives in package
`httpnode`, which imports [`go-plugin-sdk`](https://github.com/Inflowenger/go-plugin-sdk).

## Action

| Action | Purpose |
| ------ | ------- |
| `run`  | Resolve `{{$...}}` tokens, build and send the request, and commit the response to the node scope. |

### Request body (`body`)

```jsonc
{
  "settings": {
    "base_url":  "https://api.example.com",
    "auth_type": "bearer",
    "token":     "{{$.secrets.apiToken}}",
    "headers":   [{ "key": "X-Env", "value": "prod" }],
    "timeout_seconds": 30
  },
  "method":   "POST",
  "url":      "/users",
  "headers":  [{ "key": "Content-Type", "value": "application/json" }],
  "query":    [{ "key": "verbose", "value": "1" }],
  "body":     "{ \"name\": \"{{$.input.name}}\" }",
  "body_type":"json"
}
```

## Configure & run

Create `.env.inflow` next to the binary (see `.env.inflow.example`):

```
PLUGIN_ID=aaaa-bbbb-cccc-http
INFRA_URL=nats://…
INFRA_CRED=/path/to/infra.creds
```

Then:

```sh
go build -o bin/http .
./bin/http
```

The process serves the node's action over the Inflow infra and blocks until it
is signalled.
