# AI Decision node

An Inflow plugin node that evaluates a block of state — plus any evidence you
inject — against typed questions on a **decision model**, and routes the flow by
the answers.

The decision-model space settled into **two wire protocols**, and the node
speaks both. Which one it uses is `settings.provider`:

| `provider` | Endpoint | Serves |
| ---------- | -------- | ------ |
| `systemone` *(default)* | `POST /v1/systemone` | [Jev](https://docs.typesafe.ai/concepts/system-one) (TypeSafe, hosted), **Microsoft-Decision-1** (Microsoft Foundry — see below), [Laya](https://github.com/receptron/laya) (Convai, local), [OpenJev](https://github.com/razorback16/openjev), and Ollama's local deciders — [`nimble`](https://ollama.com/library/nimble:9b-q8_0) (Qwen3.5-9B), `tev1` |
| `decisions` | `POST /v1/decisions` | [OpenAI's Decisions API](https://developers.openai.com/api/docs/guides/decisions) (`gpt-6-luna`), and gateways implementing its shape — [Vercel's AI Gateway](https://vercel.com/docs/ai-gateway/sdks-and-apis/openai-decisions) does, and routes Jev through it too |

`provider` names the **protocol, not the vendor**, deliberately: each shape
already carries several vendors, and a gateway can serve one vendor's model over
the other vendor's protocol. Vendor names (`jev`, `nimble`, `openai`, `vercel`)
are accepted as aliases. An empty `provider` means `systemone` and always will —
settings profiles are snapshotted onto nodes when applied, so flows built before
this field existed carry `provider:""` forever.

Within a protocol, the deployment decides what the profile must carry:

| Deployment | In the profile |
| ---------- | -------------- |
| hosted, first-party — the default | `access_token`; `url` and `model` may stay empty |
| local / self-hosted (Laya, Nimble) | `url` + `model`; usually no key at all |
| a gateway or aggregator (`thejevai.com`, Vercel) | its own `access_token` **and** its `url`; `model` is that gateway's slug (`typesafe/jev-1.13`, `openai/gpt-6-luna-decisions`) |

### Microsoft Foundry (Microsoft-Decision-1)

Microsoft's own decision model is a **System One** service, not a third protocol.
Per [Microsoft's announcement](https://commandline.microsoft.com/microsoft-decision-1-model-foundry/)
(2026-10-09), `Microsoft-Decision-1` is a post-trained **Qwen3.5-9B** — the same
base family as Ollama's `nimble` — scoring a fixed set of declared options with
a calibrated probability each, over a 32,768-token input. Foundry serves it with
System One's request and reply bodies verbatim (`state`, a `questions` map of
`noul`/`choice`/`score`, `answers` keyed by question id), so it needs no
`provider` of its own:

```jsonc
{
  "provider": "systemone",                                             // or "microsoft" / "foundry"
  "url": "https://<resource>.services.ai.azure.com/providers/microsoft",
  "model": "decision-1",                                               // the DEPLOYMENT name
  "access_token": "<foundry resource key>"
}
```

Two things are easy to get wrong, and both live in the profile:

- **The route is not under `/openai/v1`.** Foundry's chat models sit under
  `/openai/v1` (and project-scoped bases want an `api-version` this route does
  not document); the decision route hangs off the **resource root** at
  `/providers/microsoft/v1/systemone`. Because the node joins `url` +
  `/v1/systemone`, the profile's `url` carries the `/providers/microsoft`
  prefix and nothing else — no `/openai/v1`, no `/api/projects/<name>`.
- **`model` is the deployment name, not the catalog id.** Azure reserves
  `Microsoft-Decision-1`, so a deployment is named something else
  (`decision-1`); it answers only to that name and reports the catalog id
  `microsoft-decision-1` back in the reply.

Foundry enforces the same 2–10 score levels and 2–255 choice options System One
declares, and bills one token meter ($0.042/Mtok in, output free) — so a flow
can be pointed at it from hosted Jev by swapping the profile alone.

*Caveat:* the announcement states the protocol family and the model, but **not
the wire format** — no endpoint path, request body or auth header appears in it,
and Microsoft has published none elsewhere. The route and body above are what
the Foundry portal's own Decision playground sends, corroborated by two
independent client implementations and a live deployment. The node sends the key
as `Authorization: Bearer`, which that capture shows is accepted; Foundry also
takes an `api-key` header on other routes, and if a tenant ever rejects the
bearer form that is the one thing here that would need adding.

Microsoft also says it intends to **rebase the model** on others, MAI and OpenAI
among them. That changes what answers, not how it is asked: the endpoint is the
contract here, not the base model, so a rebase is expected to leave this profile
working. Worth re-checking the reply's `model` field after one, since that is
where the catalog id would move.

A gateway is a legitimate third option — one key reaching several deciders — but
it is not the default: a workflow product should not route a customer's state
through an unaffiliated third party unless someone chose to. Keys are not
interchangeable between hosts, and the reply shapes differ. The node decodes all
of them, so a profile only has to pair the right `url` with the right key.

**Nothing downstream of the protocol differs.** The typed questions, the declared
options, the `<question>.<option>` port tags, the confidence floor and the
`_exception` branch are one implementation. That is the point of the split: a
flow can be re-pointed from a local Nimble (development) to hosted Jev
(production) to `gpt-6-luna` (an A/B) by swapping the settings profile, **with
the canvas wiring untouched**.

A decision model is a **decider, not a reasoner**. It takes state plus questions
whose valid answers are declared up front and returns a probability over every
declared answer — never free text, never an answer that was not listed. That is
the same contract the [LLM node](../llm) enforces with bound functions (each
function is an outbound port; the model can only pick one), so this node maps
onto the canvas the same way: **the declared answers are the ports**, with the
judgment swapped for a distribution and the reasoning tax removed (one call, all
questions in parallel).

## Layout

```
main.go        thin bind layer: build the SDK plugin, register, Start, block
decisionnode/  all node functionality
  doc.go         package overview + runtime scenario
  types.go       request shapes (DecisionSettings, Question, Option, EvidenceItem, RunBody),
                 Decision, and the protocol-independent `answer` / `reply`
  dialect.go     the dialect interface — the whole protocol-dependent surface — and provider resolution
  systemone.go   the /v1/systemone dialect: wire shapes, criteria building, both reply shapes
  decisions.go   the /v1/decisions dialect: wire shapes, predicate mapping, refusals
  vars.go        {{$.a.b}} resolution; template walking; state + evidence assembly
  client.go      profile validation, the POST, bounded retry on the protocol's own statuses
  route.go       question validation, answer → Decision → port tag (shared by both protocols)
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
    "provider": "systemone",          // "systemone" (default when empty) | "decisions"
    "access_token": "…",              // required for the hosted endpoint; empty for a local model
    "model": "jev-latest",            // defaults per protocol on its own host; REQUIRED for any other url
    "url": "",                        // optional base URL — a local Nimble, a gateway, a proxy
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

| `type` | `options` rows | Top answer |
| ------ | -------------- | ---------- |
| `choice` | one per selectable option (1–255) | the service's `choice` |
| `score` | the ordered levels, lowest first | the level with the highest probability |
| `noul` | `yes` and `no` (descriptions only; optional) | `yes` when p ≥ 0.5 |

The rows are the same either way; only their encoding differs:

| `type` | `systemone` | `decisions` |
| ------ | ----------- | ----------- |
| `choice` | `criteria`: `{name → description}` | `choices`: `[{value, description?}]` |
| `score` | `criteria`: `[description, …]` | `levels`: `[{label, description?}]` |
| `noul` | `criteria`: `{true: …, false: …}` | `predicate` — **no criteria field exists** |

Two consequences worth knowing:

- **An empty description** falls back to the option name on `systemone`, where
  the names are criteria *keys* and a key with no value leaves the model nothing
  to match. On `decisions` it is simply omitted — the name already travels as
  `value`, so there is nothing to fall back to. The model sees the name either
  way.
- **A `noul`'s yes/no descriptions** have nowhere to go on `decisions`, whose
  `predicate` takes no criteria at all. Rather than drop them — which would
  quietly change what the question asks — the node appends them to the
  instructions.

**Score levels** are capped by whoever serves the protocol, not by the node:
2–10 on hosted Jev and on `decisions`, 2–26 on a local Ollama decider. The node
validates against the ceiling for the endpoint the profile actually names.

#### Request ceilings

| Array | Ceiling | Why |
| ----- | ------- | --- |
| `questions` | 64 | The only documented figure (Ollama's); the other implementations publish none |
| `evidence` | 64 | Node policy — evidence is a filtered top-k, not a document dump |
| `options` (choice) | 255 | Protocol limit |
| `options` (score) | 10 or 26 | Per endpoint, see above |

The first two are **hardening, not protocol fidelity**. Every array in the body
is assembled upstream — the rows come from the settings drawer, and each
evidence row's `text` is a template resolved from the live flow context — so a
loop or a bad path upstream can grow one without anything else noticing. Past
these counts a request is far likelier to be that bug than a decision somebody
designed, and it should fail here naming the array rather than arrive as a 400,
a 413, or a surprising bill.

They are deliberately **uniform across both protocols**, unlike the score
ceiling: a per-dialect count would mean a flow that validates against one
profile fails against another, which is the exact portability the dialect split
exists to protect.

Not enforced, and worth knowing: Ollama rejects a body over **64 KiB** (413),
and every implementation shares one context budget between the state and the
questions. A byte ceiling is not applied here because the documented figure is
Ollama's alone, and imposing it everywhere would reject requests the hosted
services answer fine.

### Retrying

Which statuses are retried is the protocol's business, not the node's:

| `provider` | Retried |
| ---------- | ------- |
| `systemone` | 429 (rate limit) and 529 (overloaded) — the two the service documents, and no others |
| `decisions` | 429 and any 5xx, which is what OpenAI's own SDKs retry |

A 401, a 400/422 or a malformed reply is returned at once on either, because a
second identical request cannot fix any of them.

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

The node posts to `<base><path>` with `Authorization: Bearer <access_token>`
when a key is set, where `<base>` is the profile's `url` or the protocol's own
host, and `<path>` is the protocol's endpoint:

| `provider` | Default host | Path | Default model |
| ---------- | ------------ | ---- | ------------- |
| `systemone` | `https://api.typesafe.ai` | `/v1/systemone` | `jev-latest` |
| `decisions` | `https://api.openai.com` | `/v1/decisions` | `gpt-6-luna` |

`gpt-6-luna` is the **only** model the Decisions API serves in public beta, which
is why it is the dialect's default rather than something a profile must supply.
It is still only assumed against `api.openai.com`: a gateway slugs the same model
differently (Vercel's is `openai/gpt-6-luna-decisions`), which is exactly why a
profile naming its own `url` must name a `model`.

Notes, all learned against the live services rather than from the references:

- **`systemone` has two reply shapes, both decoded.** TypeSafe's own API returns
  the answer document **flat** (`{model, answers, usage}`), as the reference
  documents. An aggregator such as `thejevai.com` wraps it in an **envelope**
  (`{code, message, data:{result, creditsUsed}}`) — which is where
  `credits_used` and `elapsed_ms` come from, so a metered gateway is still
  accounted for on the canvas. `soResponse.normalize()` reconciles the two, and
  a non-zero `code` on an HTTP 200 is treated as a failure, not an answer.
- **`decisions` answers as an array, not a map.** Answers come back in question
  order, each echoing the `name` the node sent (the question id). The
  distributions are re-keyed on the way in — `[{value, probability}]` becomes
  `name → p` for a choice, `[{value, label, probability}]` becomes `index → p`
  for a score — because that is how the shared routing layer reads them.
- **The OpenAI error envelope** (`{"error":{message, type, param, code}}`) can
  arrive on a 2xx as well as a 4xx, and is treated as a failure rather than an
  answer, the same way a non-zero `systemone` `code` is. `param` names the field
  that failed validation.
- **Both hosts sit behind CDNs** that screen unfamiliar clients, so the node
  sends an explicit `User-Agent`.
- **An empty bearer header is worse than none:** the node omits `Authorization`
  entirely when no key is set, rather than sending `Bearer ` to a local model.
- **The profile is validated by endpoint** (`validateSettings`) on both
  protocols, by the same rule: the default host requires a key and may take the
  default model, while a profile naming its own `url` requires an explicit
  `model`. A readable error here beats a validation failure from the far side.
- **A structured `instructions`** — the object form carrying reference data the
  question cites by backticked name — is native to `systemone`. The Decisions
  API documents instructions as a string, so the node serializes the object to
  JSON text; the citation idiom is a convention about the text the model reads,
  so it survives.
- **Images** are supported by neither dialect yet. Both protocols accept them,
  but differently (a shared top-level `images` array vs. inline base64 data URLs
  in `input` content parts), and Vercel's gateway rejects them outright for now.

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

A **`systemone`** model cannot decline to answer: it always places the state in
one of the declared options, and when none fits the probabilities look *more*
decisive, not less. Draw the no-match case as an option (`other`) — it is then
just another port.

The **`decisions`** protocol does allow a refusal: the model may decline one
question — returning `{"type":"refusal","name":…}` in that question's position —
while answering the rest normally. A refused question has no answer and
therefore no port, so a *routed* one leaves through `_exception` with code
`refused`; a data-only one is listed under `refused` in the output and skipped.
This is the one semantic that exists on one protocol and not the other, so a
flow that must survive it should draw the `_exception` branch before switching a
node to `decisions`.

### Exception port

The node also has the implicit `_exception` port. It fires (then the job ends
with `DoneWithErrorData`, so the node is marked failed while the flow continues
down that branch) for, reported as `code`:

| `code` | When |
| ------ | ---- |
| `provider_error` | the API call failed: bad key, validation (422), rate limit / overload past the retry budget, network |
| `no_answer` | the reply has no answer for a routed question |
| `refused` | the model declined a routed question (`decisions` only — see Routing) |
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
  "provider": "systemone",            // which protocol answered this run
  "routed": ["category.billing", "repeat.no"],
  "refused": [],                      // data-only questions the model declined (`decisions` only)
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
