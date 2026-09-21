# Jev node

An Inflow plugin node that evaluates a block of state against typed questions
on [TypeSafe's Jev](https://docs.typesafe.ai/concepts/system-one) (a "System
One" model) and routes the flow by the answers.

Jev is a decider, not a reasoner. It takes state plus questions whose valid
answers are declared up front and returns a calibrated probability over every
declared answer — never free text, never an answer that was not listed. That is
the same contract the [LLM node](../llm) enforces with bound functions (each
function is an outbound port; the model can only pick one), so this node maps
onto the canvas the same way: **the declared answers are the ports**, with the
judgment swapped for a distribution and the reasoning tax removed (one call,
70–500 ms, all questions in parallel).

## Layout

```
main.go        thin bind layer: build the SDK plugin, register, Start, block
jevnode/       all node functionality
  doc.go         package overview + runtime scenario
  types.go       request shapes (JevSettings, Question, Option, RunBody), Decision, wire shapes
  vars.go        {{$.a.b}} variable resolution; state template → API state value
  client.go      POST /v1/systemone with bounded retry on 429/529
  route.go       question validation, criteria building, answer → Decision → port tag
  handler.go     the `run` action handler, Exception, and Register(p)
```

## Action

| Action | Purpose |
| ------ | ------- |
| `run`  | Resolve the state template, evaluate every question on Jev in one call, translate each routed question's top answer into its outbound-port tag, and report every decision (with its full distribution) to the node scope. |

### Request body (`body`)

```jsonc
{
  "settings": {                       // settings-profile the frontend ships per request
    "access_token": "…",              // required — TypeSafe API key
    "model": "jev-latest",            // optional, defaults to jev-latest
    "url": "",                        // optional custom base URL (default https://api.typesafe.ai)
    "timeout_seconds": 30             // optional
  },
  "state": "Ticket from {{$.ticket.customer}}: {{$.ticket.text}}",
  "questions": [
    {
      "id": "category",               // answer key + tag prefix
      "type": "choice",               // "choice" | "score" | "noul"
      "instructions": "Which team should handle this?",
      "route": true,                  // default true: options become outbound ports
      "min_confidence": 0.8,          // optional floor; 0 = off
      "options": [                    // same rows as LLM bound functions
        { "name": "billing",   "description": "Payments, invoicing, refunds" },
        { "name": "technical", "description": "Bugs, outages, integrations" },
        { "name": "sales",     "description": "Pricing, upgrades, new accounts" },
        { "name": "other",     "description": "None of the above" }
      ]
    },
    {
      "id": "urgency", "type": "score", "instructions": "How urgent is this?", "route": false,
      "options": [ { "name": "low", "description": "Can wait" }, { "name": "high", "description": "Needs action today" } ]
    },
    {
      "id": "repeat", "type": "noul", "instructions": "Is this a repeat contact about the same issue?",
      "options": [ { "name": "yes", "description": "References an earlier ticket" }, { "name": "no" } ]
    }
  ]
}
```

`state` is a text template; `{{$.a.b}}` tokens are resolved once each against
the live flow context via `CmdGetScope`. A template that is *exactly one* token
whose value is JSON (an object, an array) is sent as that JSON value rather than
stringified — Jev accepts both, and a structured ticket reads better than its
dump. Keep the state to what the questions need: accuracy falls as it fills
with unrelated content.

What the option rows mean per type:

| `type` | `options` rows | API criteria | Top answer |
| ------ | -------------- | ------------ | ---------- |
| `choice` | one per selectable option (1–255) | `name → description` map | the API's `choice` |
| `score` | the ordered levels, lowest first (2–10) | `[description, …]` | the level with the highest probability |
| `noul` | `yes` and `no` (descriptions only; optional) | `{true: …, false: …}` | `yes` when p ≥ 0.5 |

An empty description falls back to the name.

### Routing

A question with `route` on turns its options into the node's outbound ports.
Each option's tag is **`<question id>.<option name>`** (`category.billing`,
`repeat.yes`) — prefixed so two questions that both declare `high` never
collide. After the call, the top answer of every routed question is translated
to its tag and the set is handed to the runtime with `job.CmdNextFilter`, so
only the edges carrying those tags fire next. Several routed questions ⇒ several
tags ⇒ one branch per question continues, like an LLM turn that called several
tools. No routed question ⇒ no `CmdNextFilter` ⇒ default route.

Unlike an LLM, Jev **cannot decline to answer**: it always places the state in
one of the declared options, and when none fits the probabilities look *more*
decisive, not less. Draw the no-match case as an option (`other`) — it is then
just another port.

### Exception port

The node also has the implicit `_exception` port. It fires (then the job ends
with `DoneWithErrorData`, so the node is marked failed while the flow continues
down that branch) for, reported as `code`:

| `code` | When |
| ------ | ---- |
| `provider_error` | the API call failed: bad key, validation (422), rate limit / overload past the retry budget, network |
| `no_answer` | the reply has no answer for a routed question |
| `unbound_option` | an answer names an option that was not declared (defensive) |
| `low_confidence` | a routed question set `min_confidence` and its top answer's confidence fell below it |

The threshold is optional. The full distribution is always in the output, so
the policy can instead live in a rule node after this one, where it is visible
and editable without a redeploy.

Config mistakes (no API key, a question without an id, a bad option count, an
empty state) are not exceptions: plain `DoneWithError`, no routing.

### Node output (committed to the node scope)

```jsonc
{
  "model": "jev-1.13.0",
  "routed": ["category.billing", "repeat.no"],
  "answers": {
    "category": { "question": "category", "type": "choice", "answer": "billing", "tag": "category.billing",
                  "confidence": 0.81, "probabilities": { "billing": 0.88, "technical": 0.12, "sales": 0.0, "other": 0.0 }, "routed": true },
    "urgency":  { "question": "urgency", "type": "score", "answer": "high", "tag": "urgency.high",
                  "confidence": 0.92, "score": 1.05, "probabilities": { "low": 0.05, "high": 0.95 }, "routed": false },
    "repeat":   { "question": "repeat", "type": "noul", "answer": "no", "tag": "repeat.no",
                  "confidence": 0.8, "p": 0.2, "probabilities": { "yes": 0.2, "no": 0.8 }, "routed": true }
  },
  "usage": { "input_tokens": 296, "output_tokens": 20 }
}
```

The node is stateless: nothing is read back from the scope on the next run.

## Configure & run

Create `.env.inflow` next to the binary (see `.env.inflow.example`):

```
PLUGIN_ID=jev
INFRA_URL=nats://…
INFRA_CRED=/path/to/infra.creds
```

Then:

```sh
go build -o bin/jev .
./bin/jev
```

The process serves the node's action over the Inflow infra and blocks until it
is signalled.
