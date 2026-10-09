package decisionnode

import (
	"fmt"
	"strings"
)

// Protocol names, as a settings-profile spells them in `provider`.
//
// These name the WIRE PROTOCOL, not a vendor, and deliberately so: each shape
// already carries several vendors (System One is served by TypeSafe's Jev,
// Laya, OpenJev and Ollama's local deciders; the Decisions shape by OpenAI and
// by gateways that implement it, Vercel's included — which routes Jev through
// it as well). A `provider: "openai"` enum would have aged badly on the day a
// gateway started answering the same body, so the vendor names below are
// accepted only as aliases.
const (
	providerSystemOne = "systemone"
	providerDecisions = "decisions"
)

// dialect is the whole of the protocol-dependent surface. Everything else in
// the package — validateQuestions, decide, the port tags, the confidence
// floor, the `_exception` branch — is written once against the neutral `answer`
// and `reply` types and does not know which service replied.
//
// Adding a third protocol is another implementation of this interface plus a
// case in dialectOf, which is the same shape the LLM node's provider.go has for
// the same reason.
type dialect interface {
	// name is the canonical protocol name, for error messages and the Done
	// payload (so a run records which protocol answered it).
	name() string
	// path is the endpoint path appended to the base URL.
	path() string
	// defaultBaseURL is the protocol's first-party host, used when the profile
	// names no URL.
	defaultBaseURL() string
	// defaultModel is the model id assumed against defaultBaseURL only. It
	// means nothing at another endpoint, which is why validateSettings requires
	// an explicit model there.
	defaultModel() string

	// maxScoreLevels is the most ordered levels a score question may declare on
	// this protocol at this endpoint. It is endpoint-dependent, not just
	// protocol-dependent: the System One shape allows far more levels on a
	// local Ollama decider than TypeSafe's hosted service accepts.
	maxScoreLevels(cfg DecisionSettings) int

	// buildRequest assembles the wire body from the already-assembled state and
	// the validated questions, resolving every template through res.
	buildRequest(res resolver, model string, state any, qs []Question) any

	// decode turns a 2xx body into the normalized reply, or reports the failure
	// the service described in a body it nonetheless returned with 200.
	decode(body []byte) (reply, error)

	// retryable reports whether a non-2xx status is one this protocol asks
	// callers to back off on and try again.
	retryable(status int) bool
}

// Both protocols implement the interface, asserted here so a half-finished
// dialect fails to compile at its definition rather than at the one call site
// that happens to need the missing method.
var (
	_ dialect = systemOne{}
	_ dialect = decisions{}
)

// dialectOf resolves the profile's `provider` onto a dialect.
//
// Empty means System One, and must keep meaning it: settings profiles are
// snapshotted onto nodes when applied, so every flow deployed before this field
// existed carries provider:"" and has to go on behaving exactly as it did.
//
// Vendor and deployment names are accepted as aliases because that is what
// someone filling in a settings drawer will reach for — "openai", "nimble" —
// but the canonical values are the two protocol names.
func dialectOf(cfg DecisionSettings) (dialect, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "", providerSystemOne, "system-one", "system_one", "system one",
		"jev", "typesafe", "laya", "openjev", "nimble", "tev", "ollama",
		// Microsoft Foundry serves Microsoft's own decision models over System
		// One verbatim — Microsoft-Decision-1 is a post-trained Qwen3.5-9B, the
		// same base family as Ollama's nimble, announced 2026-10-09 at
		// commandline.microsoft.com/microsoft-decision-1-model-foundry. Its
		// route sits at the resource root, not under the `/openai/v1` base its
		// chat models use, so a Foundry profile is an ordinary System One
		// profile whose `url` carries that prefix (see the README). Bare
		// "azure" is deliberately NOT an alias: the same resource also fronts
		// OpenAI models, which answer the other protocol, so the name alone
		// does not pick a shape.
		"microsoft", "foundry", "azure-foundry", "mai", "decision-1":
		return systemOne{}, nil
	case providerDecisions, "decision", "openai-decisions", "openai_decisions",
		"openai", "gpt", "luna", "vercel", "ai-gateway", "gateway":
		return decisions{}, nil
	}
	return nil, fmt.Errorf("unsupported settings.provider %q (expected %q — TypeSafe Jev, Laya, OpenJev, Ollama nimble, Microsoft Foundry — or %q — OpenAI's Decisions API and gateways implementing it)",
		cfg.Provider, providerSystemOne, providerDecisions)
}
