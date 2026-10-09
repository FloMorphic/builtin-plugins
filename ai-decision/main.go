// Command ai-decision is the Inflow "AI Decision" plugin node: a decision model
// reached over either decision protocol — POST /v1/systemone (TypeSafe's Jev, Laya,
// Ollama's nimble) or POST /v1/decisions (OpenAI's Decisions API and gateways
// implementing its shape) — which one is a property of the settings profile.
// The binary is a thin bind layer: it builds an SDK plugin from the local
// .env.morph, registers the node's action (see package decisionnode), starts
// serving, and blocks.
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
		Version: "v0.2.0",
	})

	decisionnode.Register(p)
	// The plugin's signal port: one handler, composed — LogSignals prints every
	// signal the runtime publishes for this plugin as it arrives (this job's and,
	// since one subject carries them all, other flows' and other replicas'), then
	// Stops.OnSignal cancels the job a conclusion is about when that conclusion is
	// a cancellation, logging that cancel itself from inside jobstop.
	p.OnSignal(sdkv1.ChainSignals(sdkv1.LogSignals("ai-decision"), decisionnode.Stops.OnSignal))

	if err := p.Start(); err != nil {
		log.Fatalf("ai-decision: start: %v", err)
	}
	select {} // serve until the process is signalled
}
