// Command cast is the Inflow "Cast / Mapping" plugin node. The binary is a thin
// bind layer: it builds an SDK plugin from the local .env.morph, registers the
// node's action (see package castnode), starts serving, and blocks.
package main

import (
	"log"

	"github.com/FloMorphic/builtin-plugins/cast/castnode"
	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
)

func main() {
	p, err := sdkv1.NewPlugin(sdkv1.WithDotEnv(".env.morph"))
	if err != nil {
		log.Fatalf("cast: init plugin: %v", err)
	}

	p.Intro(sdkv1.PluginIntro{
		Name:    "Cast / Mapping",
		Author:  "inflow Dev. Team",
		Version: "v0.1.0",
	})

	castnode.Register(p)

	if err := p.Start(); err != nil {
		log.Fatalf("cast: start: %v", err)
	}
	select {} // serve until the process is signalled
}
