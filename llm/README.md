# LLM node

An Inflow plugin node that runs one turn of an LLM conversation held on the
node's scope. It talks to OpenAI, OpenRouter, any OpenAI-compatible endpoint
(Ollama, vLLM, Groq, Together, …), Google Gemini, or Anthropic through
[langchaingo](https://github.com/tmc/langchaingo), streams the completion back
to the canvas, and can route the flow through outbound ports when the model
answers with a tool call.

## Providers

| `provider` | Backend | `url` (base) | Notes |
| ---------- | ------- | ------------ | ----- |
| `openai` | OpenAI | optional (defaults to OpenAI) | |
| `openrouter` | OpenRouter (OpenAI-compatible) | auto-defaults to `https://openrouter.ai/api/v1` | One key → 300+ models; set `model` as `vendor/model`, e.g. `anthropic/claude-3.5-sonnet`. |
| `openai-compatible` (aliases: `compatible`) | any OpenAI-compatible server | **required** (`…/v1`) | Ollama, vLLM, Groq, Together, DeepSeek, … |
| `gemini` (aliases: `google`, `googleai`) | Google Gemini | n/a | |
| `anthropic` (alias: `claude`) | Anthropic | optional | |

Every provider runs through langchaingo, which owns the message body and role
mapping — so `provider` is the only field that picks the backend. Tool routing
depends on the upstream model honoring the OpenAI tools schema (OpenRouter and
the major gateways do; some local models don't).

## Layout

```
main.go        thin bind layer: build the SDK plugin, register, Start, block
llmnode/       all node functionality
  doc.go         package overview + runtime scenario
  types.go       request/scope shapes (LLMSettings, RunBody, ChatMessage, …)
  vars.go        {{$.a.b}} variable resolution against the live flow context
  provider.go    langchaingo provider construction + role/message mapping
  chat.go        settings validation + the streaming turn
  handler.go     the `run` action handler and Register(p)
```

The root binary only wires things together; everything else lives in package
`llmnode`, which imports [`go-plugin-sdk`](https://github.com/Inflowenger/go-plugin-sdk).

## Action

| Action | Purpose |
| ------ | ------- |
| `run`  | Resolve prompts against the flow context, stream one completion from the configured provider, commit the conversation to the node scope, and — if the model called a tool — route the flow out of the matching outbound port(s). |

### Request body (`body`)

```jsonc
{
  "settings": {                 // settings-profile the frontend ships per request
    "provider": "gemini",       // "openai" | "openrouter" | "openai-compatible" | "gemini" | "anthropic" (+ aliases)
    "model": "gemini-2.0-flash",
    "access_token": "…",
    "url": "",                  // optional custom base URL (auto-set for openrouter)
    "temperature": 0.7,
    "max_tokens": 0             // omitted when 0
  },
  "prompt": "…",                // this turn's user prompt (may embed {{$.a.b}})
  "system_prompt": "…",         // system/init prompt (may embed {{$.a.b}})
  "functions": [                // bound functions == outbound ports
    { "name": "approve", "description": "call when the request is approved" }
  ]
}
```

`{{$.some.path}}` tokens in either prompt are resolved once each against the live
flow context via `CmdGetScope` before the call.

### Tool routing

Each bound function is an outbound port whose route tag is its `name`. When the
model replies with a tool call, the handler calls
`job.CmdNextFilter([]string{name})` so the runtime fires only the matching
port(s) next. No tool call ⇒ the flow follows its default route.

## Configure & run

Create `.env.inflow` next to the binary (see `.env.inflow.example`):

```
PLUGIN_ID=llm
INFRA_URL=nats://…
INFRA_CRED=/path/to/infra.creds
```

Then:

```sh
go build -o bin/llm .
./bin/llm
```

The process serves the node's action over the Inflow infra and blocks until it
is signalled.
