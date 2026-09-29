package main

import (
	"github.com/tomblancdev/hangar/plugins/toy"
	"github.com/tomblancdev/hangar/sdk/pluginpb"
)

// builtins are the plugins compiled into this binary. Each still runs as its
// own process ("hangar plugin <name>"), with only its own credential: being
// in the same file changes nothing about the walls between them.
var builtins = map[string]func() pluginpb.PluginServiceServer{
	toy.Name: func() pluginpb.PluginServiceServer { return toy.New() },
}
