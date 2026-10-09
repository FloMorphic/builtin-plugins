// Command http is the Inflow "HTTP" plugin node. The binary is a thin bind
// layer: it builds an SDK plugin from the local .env.morph, registers the node's
// action (see package httpnode), starts serving, and blocks.
package main

import (
	"log"

	"github.com/FloMorphic/builtin-plugins/http/httpnode"
	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
)

func main() {
	p, err := sdkv1.NewPlugin(sdkv1.WithDotEnv(".env.morph"))
	if err != nil {
		log.Fatalf("http: init plugin: %v", err)
	}

	p.Intro(sdkv1.PluginIntro{
		Name:    "HTTP",
		Author:  "inflow Dev. Team",
		Version: "v0.1.0",
	})

	httpnode.Register(p)

	// The plugin's signal port: one handler, composed — LogSignals prints every
	// signal the runtime publishes for this plugin as it arrives (this job's and,
	// since one subject carries them all, other flows' and other replicas'), then
	// Stops.OnSignal cancels the request a conclusion is about when that conclusion
	// is a cancellation, logging that cancel itself from inside jobstop.
	p.OnSignal(sdkv1.ChainSignals(sdkv1.LogSignals("http"), httpnode.Stops.OnSignal))

	if err := p.Start(); err != nil {
		log.Fatalf("http: start: %v", err)
	}
	select {} // serve until the process is signalled
}
