// Command ai-decision is the Inflow "AI Decision" plugin node: a System One
// decision model (TypeSafe's hosted Jev, or a local Laya — both serve the same
// POST /v1/systemone protocol, and which one answers is a property of the
// settings profile). The binary is a thin bind layer: it builds an SDK plugin
// from the local .env.morph, registers the node's action (see package
// decisionnode), starts serving, and blocks.
package main

import (
	"log"

	"github.com/FloMorphic/builtin-plugins/ai-decision/decisionnode"
	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
)

func main() {
	p, err := sdkv1.NewPlugin(sdkv1.WithDotEnv(".env.morph"))
	if err != nil {
		log.Fatalf("ai-decision: init plugin: %v", err)
	}

	p.Intro(sdkv1.PluginIntro{
		Name:    "AI Decision",
		Author:  "inflow Dev. Team",
		Version: "v0.1.0",
	})

	decisionnode.Register(p)

	if err := p.Start(); err != nil {
		log.Fatalf("ai-decision: start: %v", err)
	}
	select {} // serve until the process is signalled
}
