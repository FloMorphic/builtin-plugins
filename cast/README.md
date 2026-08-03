# Cast / Mapping node

An Inflow plugin node that assembles a JSON object from a list of key/value
mappings. Each value may embed `{{$.a.b}}` tokens; the node resolves them against
the live flow context and emits the finished object as its scope — a small,
dependency-light way to shape data between nodes.

## What it does

Given an ordered list of `{ key, value }` pairs, the node builds an object where
each `key` holds its resolved `value`:

- A **string value with `{{$.a.b}}` tokens** has each token resolved against the
  flow context (the same convention the LLM/MCP nodes use in prompts).
  - When the whole value is a single token (`"{{$.user}}"`), the **typed** JSON
    value at that path is lifted in — an object stays an object, a number a
    number, an array an array.
  - When tokens are embedded in surrounding text (`"Hi {{$.user.name}}!"`), the
    value is string-interpolated and the result is a string.
- A **string value with no tokens** is a constant and passes through as-is.
- A **non-string value** (number, bool, literal object/array entered in the
  drawer) has no tokens and passes through unchanged.

An unresolvable token is left verbatim rather than dropped. Pairs with a blank
key are skipped; a later duplicate key overwrites an earlier one.

## Layout

```
main.go        thin bind layer: build the SDK plugin, register, Start, block
castnode/      all node functionality
  doc.go         package overview + runtime scenario
  types.go       request/scope shapes (Mapping, RunBody)
  vars.go        {{$.a.b}} token resolution (typed + interpolated)
  handler.go     the `run` action handler and Register(p)
  vars_test.go   unit tests for value/token resolution
```

The root binary only wires things together; everything else lives in package
`castnode`, which imports [`go-plugin-sdk`](https://github.com/Inflowenger/go-plugin-sdk).

## Action

| Action | Purpose |
| ------ | ------- |
| `run`  | Resolve every mapping value's `{{$...}}` tokens against the live flow context and commit the assembled object to the node scope. |

### Request body (`body`)

```jsonc
{
  "mappings": [
    { "key": "userId",   "value": "{{$.user.id}}" },      // typed lift: id keeps its type
    { "key": "greeting", "value": "Hi {{$.user.name}}!" }, // interpolated → string
    { "key": "profile",  "value": "{{$.user.profile}}" },  // lifts a nested object as-is
    { "key": "source",   "value": "cast-node" },           // constant, no tokens
    { "key": "retries",  "value": 3 }                       // non-string, passes through
  ]
}
```

### Output

The assembled object is committed to the node's scope by being the terminal
`Done` payload — each key lands at the top level, so downstream nodes read them as
`{{$.<node>.<key>}}`:

```jsonc
{
  "userId":   42,
  "greeting": "Hi Ada!",
  "profile":  { "tier": "pro", "since": 2021 },
  "source":   "cast-node",
  "retries":  3
}
```

Config mistakes fail cleanly with an error and no routing: an unreadable request
body, an empty `mappings` list, or a list in which every pair has a blank key.

## Configure & run

Create `.env.inflow` next to the binary (see `.env.inflow.example`):

```
PLUGIN_ID=cast
INFRA_URL=nats://…
INFRA_CRED=/path/to/infra.creds
```

Then:

```sh
go build -o bin/cast .
./bin/cast
```

The process serves the node's action over the Inflow infra and blocks until it
is signalled.
