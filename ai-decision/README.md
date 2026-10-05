# AI Decision node

An Inflow plugin node that evaluates a block of state — plus any evidence you
inject — against typed questions on a **System One decision model**, and routes
the flow by the answers.

The node speaks one protocol, `POST /v1/systemone`, and two models serve it:

| Model | Where | In the profile |
| ----- | ----- | -------------- |
| [Jev](https://docs.typesafe.ai/concepts/system-one) (TypeSafe) | hosted | `access_token`; `url` and `model` may stay empty |
| [Laya](https://github.com/receptron/laya) (Convai, Apache-2.0) | local / self-hosted | `url` + `model`; usually no key at all |

Which one answers is a property of the settings profile, not of the node — the
request and reply shapes are the same. That is why this node is named for what
it does rather than for one vendor.

A System One model is a **decider, not a reasoner**. It takes state plus
questions whose valid answers are declared up front and returns a calibrated
probability over every declared answer — never free text, never an answer that
was not listed. That is the same contract the [LLM node](../llm) enforces with
bound functions (each function is an outbound port; the model can only pick
one), so this node maps onto the canvas the same way: **the declared answers are
the ports**, with the judgment swapped for a distribution and the reasoning tax
removed (one call, 70–500 ms, all questions in parallel).

## Layout

```
main.go        thin bind layer: build the SDK plugin, register, Start, block
decisionnode/  all node functionality
  doc.go         package overview + runtime scenario
  types.go       request shapes (DecisionSettings, Question, Option, EvidenceItem, RunBody), Decision, wire shapes
  vars.go        {{$.a.b}} resolution; template walking; state + evidence assembly
  client.go      profile validation, POST /v1/systemone with bounded retry on 429/529
  route.go       question validation, criteria building, answer → Decision → port tag
  handler.go     the `run` action handler, Exception, and Register(p)
```

## Action

| Action | Purpose |
| ------ | ------- |
| `run`  | Resolve the state and evidence templates, evaluate every question in one call, translate each routed question's top answer into its outbound-port tag, and report every decision (with its full distribution) to the node scope. |

### Request body (`body`)

```jsonc
{
  "settings": {                       // settings-profile the frontend ships per request
    "access_token": "…",              // required for the hosted endpoint; empty for a local model
    "model": "jev-latest",            // defaults to jev-latest on the hosted endpoint; REQUIRED for any other url
    "url": "",                        // optional base URL — a local Laya, a proxy, a private deployment
    "timeout_seconds": 30,            // optional
    "max_retries": 2                  // optional; omit for the default (2), 0 to never retry
  },
  "state": "Ticket from {{$.ticket.customer}}: {{$.ticket.text}}",
  "evidence": [                       // optional — the retrieval side of the decision
    { "source": "contract.pdf",   "text": "{{$.kb.termination_clause}}" },
    { "source": "regulation.pdf", "text": "{{$.kb.regulation}}" }
  ],
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
        { "name": "other",     "description": "None of the above" }
      ]
    },
    {
      "id": "termination_valid",
      "type": "noul",
      // The structured form: the question in one field, the data it cites in
      // the others, referenced by BACKTICKED name.
      "instructions": {
        "question": "Does `case.problem` qualify for termination under `policy`?",
        "policy": "{{$.kb.termination_policy}}"
      },
      "options": [{ "name": "yes" }, { "name": "no" }]
    }
  ]
}
```

#### State, evidence, and how they reach the model

The service's request body is **only** `{model, state, questions}` — there is no
evidence parameter. Evidence therefore rides *inside* `state`, which the API
accepts as a string, a JSON object or a JSON array:

- **No evidence rows** ⇒ `state` is sent exactly as the template resolved, which
  is what this node always did. A template that is *exactly one* `{{$.path}}`
  token whose value is JSON is sent as that JSON value rather than stringified.
- **Any evidence row** ⇒ the node sends

  ```jsonc
  { "case": <the resolved state>, "evidence": [ { "source": …, "text": … }, … ] }
  ```

  so a question can point at either part by **backticked dot-and-index path**:
  `` `case.problem` ``, `` `evidence[0].text` ``. A state that resolved to
  nothing is left out, since evidence alone is content enough to decide on.

Keep both filtered. The state and all the questions share one context budget
(~64k tokens, with state plus the longest single question inside ~32k), and
accuracy *falls* as the state fills with material the questions do not need —
retrieve, filter, then inject. Text only: no images, audio or video.

#### What is a template, and what is not

Everything the designer authors is a template whose `{{$.a.b}}` tokens are
resolved against the live flow context via `CmdGetScope` before the call:

| Authored field | Resolved |
| -------------- | -------- |
| `state` | yes |
| `evidence[].text`, `evidence[].source` | yes |
| `questions[].instructions` | yes — at any depth, for the structured form |
| `questions[].options[].description` (the criteria) | yes |

What is **not** re-resolved is the value that came back from the scope. If the
state template is one `{{$.chunks}}` token, the fetched JSON is sent as it
stands: its own strings are not walked for tokens. That line is deliberate —
templates are authored on the canvas, while a scope value is runtime data (a
retrieved chunk, a customer's own words), and re-resolving it would let that
data read the flow context.

#### What the option rows mean per type

| `type` | `options` rows | API criteria | Top answer |
| ------ | -------------- | ------------ | ---------- |
| `choice` | one per selectable option (1–255) | `name → description` map | the API's `choice` |
| `score` | the ordered levels, lowest first (2–10) | `[description, …]` | the level with the highest probability |
| `noul` | `yes` and `no` (descriptions only; optional) | `{true: …, false: …}` | `yes` when p ≥ 0.5 |

An empty description falls back to the name.

### Retrying

Only the two statuses the service asks callers to back off on are retried — 429
(rate limit) and 529 (overloaded). A 401, a 422 or a malformed reply is returned
at once, because a second identical request cannot fix any of them.

How reliable a particular endpoint is — a shared key against a rate-limited
hosted service, a local Laya with nothing in front of it — is a property of the
connection rather than of the decision, so the budget lives in the profile, as
it does on the [LLM](../llm) and [HTTP](../http) nodes. `max_retries` is how
many *further* attempts a failed call gets:

| `max_retries` | Behaviour |
| ------------- | --------- |
| absent | the default, 2 further attempts |
| `0` | decide once; a transient failure routes `_exception` |
| `n` | n further attempts |

Between attempts the node waits for whatever `Retry-After` asked for (both
documented forms — a delay in seconds, or an HTTP date), and otherwise backs off
exponentially from 500 ms. Either way the wait is capped at 8 s: a decision node
sits on the hot path, and one that parks a flow for minutes is not helping it.
Each wait is reported as a progress frame, so a retrying node reads as waiting
rather than as hung.

### Endpoint & reply shape

The node posts to `<base>/v1/systemone` with `Authorization: Bearer
<access_token>` when a key is set, where `<base>` is the profile's `url` or
`https://thejevai.com`. Four notes, all learned against the live service rather
than from the reference:

- The default host is **not** the documented one, and the difference is real.
  `docs.typesafe.ai` documents `https://api.typesafe.ai/v1/systemone`, which is
  live and whose documented reply is **flat** (`{model, answers, usage}`).
  `thejevai.com` — the gateway these keys are issued for, and the one every run
  behind this node went to — answers with an **envelope**
  (`{code, message, data:{result, creditsUsed}}`) that the reference never
  mentions, and that is where `credits_used` and `elapsed_ms` come from. Two
  gateways in front of the same model: the default stays on the one the keys
  belong to, `reply()` decodes both, and a profile can point `url` at either.
  Both sit behind a CDN that screens unfamiliar clients, so the node sends an
  explicit `User-Agent`.
- The live reply is **wrapped**: `{"code":0,"message":"ok","data":{"result":{…},"creditsUsed":1}}`,
  while the reference shows the answer document flat at the top level. The node
  decodes both (see `apiResponse.reply`), treats a non-zero `code` on an HTTP 200
  as a failure, and reports `credits_used` / `elapsed_ms` on the node output so a
  metered decider is accounted for on the canvas.
- An empty bearer header is worse than none: the node omits `Authorization`
  entirely when no key is set, rather than sending `Bearer ` to a local model.
- The profile is validated by endpoint (`validateSettings`): the hosted default
  requires a key and may take the default model alias, while a profile naming
  its own `url` requires an explicit `model` — `jev-latest` means nothing to a
  Laya server, and a readable error here beats a 422 from the far side.

### Routing

A question with `route` on turns its options into the node's outbound ports.
Each option's tag is **`<question id>.<option name>`** (`category.billing`,
`repeat.yes`) — prefixed so two questions that both declare `high` never
collide. After the call, the top answer of every routed question is translated
to its tag and the set is handed to the runtime with `job.CmdNextFilter`, so
only the edges carrying those tags fire next. Several routed questions ⇒ several
tags ⇒ one branch per question continues, like an LLM turn that called several
tools. No routed question ⇒ no `CmdNextFilter` ⇒ default route.

Evidence rows derive no ports: they are input, not an outcome.

Unlike an LLM, a System One model **cannot decline to answer**: it always places
the state in one of the declared options, and when none fits the probabilities
look *more* decisive, not less. Draw the no-match case as an option (`other`) —
it is then just another port.

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

Config mistakes (a missing key on the hosted endpoint, a missing model on a
custom one, a question without an id, a bad option count, an evidence row with
no text, nothing to decide on at all) are not exceptions: plain `DoneWithError`,
no routing.

### Node output (committed to the node scope)

```jsonc
{
  "model": "jev-1.13.0",
  "routed": ["category.billing", "repeat.no"],
  "credits_used": 1,
  "elapsed_ms": 1801,
  "answers": {
    "category": { "question": "category", "type": "choice", "answer": "billing", "tag": "category.billing",
                  "confidence": 0.81, "probabilities": { "billing": 0.88, "technical": 0.12, "other": 0.0 }, "routed": true },
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
PLUGIN_ID=aaaa-bbbb-cccc-jev0
INFRA_URL=nats://…
INFRA_CRED=/path/to/infra.creds
```

`PLUGIN_ID` is the plugin's registered identity, not a label: it must match the
`pluginId` of the backing extension row (see the api's
`repository/sqlite/seed/builtins.json`), so it kept its original value through
the rename rather than forcing a redeploy and a row migration.

Then:

```sh
go build -o bin/ai-decision .
./bin/ai-decision
```

The process serves the node's action over the Inflow infra and blocks until it
is signalled.
