// Command llm is the Inflow "LLM" plugin node. The binary is a thin bind layer:
// it builds an SDK plugin from the local .env.inflow, registers the node's
// action (see package llmnode), starts serving, and blocks.
package main

import (
	"log"

	"github.com/FloMorphic/builtin-plugins/llm/llmnode"
	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
)

func main() {
	p, err := sdkv1.NewPlugin(sdkv1.WithDotEnv(".env.morph"))
	if err != nil {
		log.Fatalf("llm: init plugin: %v", err)
	}

	p.Intro(sdkv1.PluginIntro{
		Name:    "LLM",
		Author:  "inflow Dev. Team",
		Version: "v0.1.0",
	})

	llmnode.Register(p)

	if err := p.Start(); err != nil {
		log.Fatalf("llm: start: %v", err)
	}
	select {} // serve until the process is signalled
}
