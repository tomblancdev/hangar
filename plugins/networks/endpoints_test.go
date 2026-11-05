package networks

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/tomblancdev/hangar/driver"
	"github.com/tomblancdev/hangar/sdk"
	"github.com/tomblancdev/hangar/sdk/pluginpb"
)

func configure(t *testing.T, caps string, settings map[string]any) (*Plugin, *pluginpb.ConfigureResponse, error) {
	t.Helper()
	p := New()
	zone := &pluginpb.ZoneConfig{Name: "z", Driver: "fake"}
	if caps != "" {
		zone.Options = map[string]string{"capabilities": caps}
	}
	resp, err := p.Configure(context.Background(), &pluginpb.ConfigureRequest{Zones: []*pluginpb.ZoneConfig{zone}, Settings: sdk.JSON(settings)})
	return p, resp, err
}

func publishing(services ...map[string]any) map[string]any {
	z := map[string]any{"services": services, "private": []string{"192.0.2.0/24"}}
	return map[string]any{"zones": map[string]any{"z": z}}
}

// What an operator publishes is read when the zone is opened, and refused
// there in words: a service outside the zone's private side (every network
// would reach it), one that is no address and port, a name twice, a zone the
// plugin is not enabled on — and a zone whose driver cannot carry endpoints.
func TestAZonesServicesAreReadWhenItOpens(t *testing.T) {
	logs := map[string]any{"name": "logs", "to": "192.0.2.35:3101"}
	if _, resp, err := configure(t, "", publishing(logs, map[string]any{"name": "syslog", "to": "192.0.2.35:514/udp"})); err != nil || resp.GetZones()[0].GetError() != "" {
		t.Fatalf("%v %v", err, resp)
	}
	for name, c := range map[string]struct {
		settings map[string]any
		want     string
	}{
		"outside its side": {publishing(map[string]any{"name": "far", "to": "203.0.113.9:80"}), "far at 203.0.113.9 is outside the zone's private side: every network would reach it"},
		"outside the private ranges, unsaid": {map[string]any{"zones": map[string]any{"z": map[string]any{"services": []any{map[string]any{"name": "far", "to": "198.51.100.9:80"}}}}},
			"outside the zone's private side"},
		"no port":                       {publishing(map[string]any{"name": "logs", "to": "192.0.2.35"}), "an IPv4 address and a port"},
		"another protocol":              {publishing(map[string]any{"name": "logs", "to": "192.0.2.35:3101/sctp"}), "tcp or udp"},
		"a name twice":                  {publishing(logs, logs), "logs twice"},
		"a name of words":               {publishing(map[string]any{"name": "Log Store", "to": "192.0.2.35:3101"}), "a service's name"},
		"a side of words":               {map[string]any{"zones": map[string]any{"z": map[string]any{"private": []string{"the lab"}}}}, "an IPv4 range"},
		"a zone that cannot carry them": {publishing(logs), ""},
	} {
		caps := ""
		if name == "a zone that cannot carry them" {
			caps = "kind.container,fence.pool,net.private"
			c.want = "it publishes logs (settings), and its driver cannot carry endpoints there (net.endpoints)"
		}
		_, resp, err := configure(t, caps, c.settings)
		if err != nil || len(resp.GetZones()) != 1 || !strings.Contains(resp.GetZones()[0].GetError(), c.want) {
			t.Errorf("%s: %v %v (want %q)", name, err, resp, c.want)
		}
	}
	if _, _, err := configure(t, "", map[string]any{"zones": map[string]any{"elsewhere": map[string]any{}}}); err == nil || !strings.Contains(err.Error(), "elsewhere: the plugin is not enabled there") {
		t.Fatalf("%v", err)
	}
	// a zone that publishes nothing opens on a driver with no endpoints, as before
	if _, resp, err := configure(t, "kind.container,fence.pool,net.private", nil); err != nil || resp.GetZones()[0].GetError() != "" {
		t.Fatalf("%v %v", err, resp)
	}
}

// What a gateway is brought to: its zone's side, and the lines of the
// endpoints it holds — none where the zone publishes nothing; one to a
// service no longer published reaches nothing.
func TestAGatewayCarriesItsNetworksEndpoints(t *testing.T) {
	p, _, err := configure(t, "", publishing(map[string]any{"name": "logs", "to": "192.0.2.35:3101"}, map[string]any{"name": "syslog", "to": "192.0.2.35:514/udp"}))
	if err != nil {
		t.Fatal(err)
	}
	ep := func(id, svc string) *pluginpb.Resource {
		return &pluginpb.Resource{Id: id, Type: "endpoint", Spec: sdk.JSON(Endpoint{Network: "net-a", Service: svc})}
	}
	e := p.endpointsFor("z", []*pluginpb.Resource{ep("ep-2", "syslog"), ep("ep-1", "logs"), ep("ep-3", "logs"), ep("ep-4", "gone")})
	if e == nil || !slices.Equal(e.To, []string{"tcp:192.0.2.35:3101", "udp:192.0.2.35:514"}) || !slices.Equal(e.Private, []string{"192.0.2.0/24"}) {
		t.Fatalf("%+v", e)
	}
	if e := p.endpointsFor("z", nil); e == nil || len(e.To) != 0 || len(e.Private) != 1 {
		t.Fatalf("a network of none still refuses the side: %+v", e)
	}
	quiet, _, _ := configure(t, "", nil)
	if e := quiet.endpointsFor("z", []*pluginpb.Resource{ep("ep-1", "logs")}); e != nil {
		t.Fatalf("a zone that publishes nothing writes nothing: %+v", e)
	}
	// the default side: the private ranges
	def, _, _ := configure(t, "", map[string]any{"zones": map[string]any{"z": map[string]any{"services": []any{map[string]any{"name": "dns", "to": "10.1.2.3:53/udp"}}}}}) // no-environment: ok — inside the default side, which a test must name
	if e := def.endpointsFor("z", nil); !slices.Equal(e.Private, defaultPrivate) {
		t.Fatalf("%v", e.Private)
	}
}

// An endpoint is planned against what its zone publishes and counted under
// its two dimensions; its network and its service are set at its birth.
func TestAnEndpointIsPlanned(t *testing.T) {
	p, _, _ := configure(t, "", publishing(map[string]any{"name": "logs", "to": "192.0.2.35:3101"}))
	ctx := context.Background()
	plan, err := p.Plan(ctx, &pluginpb.PlanRequest{Type: "endpoint", Zone: "z", Spec: sdk.JSON(Endpoint{Network: "net-a", Service: "logs"})})
	if err != nil || len(plan.GetRefusals()) != 0 || plan.GetUsage()["networks.endpoints"] != 1 || plan.GetChoices()["networks.services"] != "logs" || plan.GetRoom() != nil {
		t.Fatalf("%v %v", err, plan)
	}
	plan, _ = p.Plan(ctx, &pluginpb.PlanRequest{Type: "endpoint", Zone: "z", Spec: sdk.JSON(Endpoint{Network: "net-a", Service: "metrics"})})
	if r := plan.GetRefusals(); len(r) != 1 || r[0].GetField() != "/service" || r[0].GetReason() != "zone z publishes no service metrics; it publishes logs" {
		t.Fatalf("%v", r)
	}
	ch, err := p.PlanChange(ctx, &pluginpb.PlanChangeRequest{
		Current: &pluginpb.Resource{Type: "endpoint", Spec: sdk.JSON(Endpoint{Network: "net-a", Service: "logs"})},
		Spec:    sdk.JSON(Endpoint{Network: "net-b", Service: "syslog"})})
	if err != nil || len(ch.GetSteps()) != 0 || len(ch.GetFixed()) != 2 || !strings.Contains(ch.GetFixed()[1].GetReason(), "another service is another endpoint") {
		t.Fatalf("%v %v", err, ch)
	}
	// its type asks for its own flag, and marks its network its owner's own
	d, _ := p.Describe(ctx, nil)
	var et *pluginpb.ResourceType
	for _, rt := range d.GetTypes() {
		if rt.GetName() == "endpoint" {
			et = rt
		}
	}
	var schema struct {
		Properties map[string]map[string]any `json:"properties"`
	}
	if et == nil || !slices.Equal(et.GetRequires(), []string{driver.NetEndpoints}) || json.Unmarshal(et.GetSchema(), &schema) != nil ||
		schema.Properties["network"]["x-hangar-own"] != true || schema.Properties["network"]["x-hangar-member"] != true {
		t.Fatalf("%v", et)
	}
}
