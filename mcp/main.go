// Command mcp is the Inflow "MCP" plugin node. The binary is a thin bind layer:
// it builds an SDK plugin from the local .env.inflow, registers the node's two
// actions and its meta method (see package mcpnode), starts serving, and blocks.
package main

import (
	"log"

	"github.com/FloMorphic/builtin-plugins/mcp/mcpnode"
	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
)

func main() {
	p, err := sdkv1.NewPlugin(sdkv1.WithDotEnv(".env.morph"))
	if err != nil {
		log.Fatalf("mcp: init plugin: %v", err)
	}

	p.Intro(sdkv1.PluginIntro{
		Name:    "MCP",
		Author:  "inflow Dev. Team",
		Version: "v0.1.0",
	})

	mcpnode.Register(p)

	if err := p.Start(); err != nil {
		log.Fatalf("mcp: start: %v", err)
	}
	select {} // serve until the process is signalled
}
