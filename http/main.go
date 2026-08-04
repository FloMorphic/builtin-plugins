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

	if err := p.Start(); err != nil {
		log.Fatalf("http: start: %v", err)
	}
	select {} // serve until the process is signalled
}
