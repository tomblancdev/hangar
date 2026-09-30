// Package sdk is what a hangar plugin is built with: the handshake, the gRPC
// glue for HashiCorp's go-plugin, Serve, and the few helpers every plugin
// ends up writing (JSON in and out, refusals as the statuses the core reads).
//
// A plugin is a program: its main calls sdk.Serve with a value implementing
// pluginpb.PluginServiceServer. The core starts it as its own process, with an
// empty environment, speaks to it over a local socket under mutual TLS, and
// hands it only its own credential.
package sdk

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/tomblancdev/hangar/sdk/pluginpb"
)

// Protocol is the version of the plugin protocol this SDK speaks. It moves
// only when a change would break a plugin built against the previous one.
const Protocol = 1

// Handshake is shared by the core and every plugin. The cookie is not a
// secret: it only stops a plugin binary from being run by hand by mistake.
var Handshake = plugin.HandshakeConfig{
	ProtocolVersion:  Protocol,
	MagicCookieKey:   "HANGAR_PLUGIN",
	MagicCookieValue: "a-request-a-limit-a-machine",
}

// Dispense is the key a plugin is served and dispensed under.
const Dispense = "plugin"

// GRPCPlugin is the go-plugin glue for the protocol. The core uses it with a
// nil Impl (client side); a plugin serves its Impl.
type GRPCPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	Impl pluginpb.PluginServiceServer
}

func (p *GRPCPlugin) GRPCServer(_ *plugin.GRPCBroker, s *grpc.Server) error {
	pluginpb.RegisterPluginServiceServer(s, p.Impl)
	return nil
}

func (p *GRPCPlugin) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return pluginpb.NewPluginServiceClient(c), nil
}

// Serve runs the plugin until the core lets it go. It never returns while the
// core holds it.
func Serve(impl pluginpb.PluginServiceServer) {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         map[string]plugin.Plugin{Dispense: &GRPCPlugin{Impl: impl}},
		GRPCServer:      plugin.DefaultGRPCServer,
	})
}

// JSON encodes v for a spec, params, observed or result field. A value that
// cannot be encoded is a programming error in the plugin, so it panics.
func JSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("sdk.JSON: %v", err))
	}
	return b
}

// Decode reads a JSON field into v; an empty field leaves v as it was.
func Decode(b []byte, v any) error {
	if len(b) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, v); err != nil {
		return Refuse("not the JSON this plugin expects: %v", err)
	}
	return nil
}

// Refuse: the request can never succeed as written (INVALID_ARGUMENT).
func Refuse(format string, a ...any) error {
	return status.Errorf(codes.InvalidArgument, format, a...)
}

// NotNow: the engine refuses it in its present state (FAILED_PRECONDITION).
func NotNow(format string, a ...any) error {
	return status.Errorf(codes.FailedPrecondition, format, a...)
}

// Unreachable: the engine could not be reached; the core will try again.
func Unreachable(format string, a ...any) error {
	return status.Errorf(codes.Unavailable, format, a...)
}

// Event builds an event for a response.
func Event(name, message string, fields map[string]string) *pluginpb.Event {
	return &pluginpb.Event{Name: name, Message: message, Fields: fields}
}

// Step is one action a PlanChange answers, with its params.
func Step(action string, params any) *pluginpb.Step {
	s := &pluginpb.Step{Action: action}
	if params != nil {
		s.Params = JSON(params)
	}
	return s
}

// Fixed is a field PlanChange finds different and no action changes: set at
// the resource's birth. was is what it is, in words.
func Fixed(field, was, what string) *pluginpb.Refusal {
	return &pluginpb.Refusal{Field: field, Reason: fmt.Sprintf("it is %s, and %s is set at its birth", was, what)}
}
