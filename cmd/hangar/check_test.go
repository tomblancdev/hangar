package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/tomblancdev/hangar/internal/config"
)

// check holds each zone's options against its driver's own reading of them,
// with no engine to ask: a file whose zone could never open — an address past
// its range, a group that is no name, a word mistyped — is refused before it
// lands, and before a brain starts on it. (Two such files once came back
// « sound »: the driver refused them only when the zone was opened.)
func TestCheckReadsAZonesOptions(t *testing.T) {
	file := func(options string) *config.Config {
		t.Helper()
		cfg, err := config.Parse(fmt.Appendf(nil, `
data_dir: /tmp/hangar-check-test
tiers:
  - {name: users, groups: [users], zones: [z, f], limits: {machines.count: 1}}
zones:
  - name: z
    driver: proxmox
    endpoint: https://192.0.2.1:8006   # nothing answers there: it is never asked
    options: {node: n, pool: hangar, storage: s, seed_storage: seeds, bridge: vmbr0, vmids: 11000-11099%s}
  - {name: f, driver: fake}
plugins:
  - {name: machines, builtin: machines, zones: [z, f]}
`, options))
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	walled := `, subnet: 203.0.113.0/24, first_address: 203.0.113.100, gateway: 203.0.113.1, firewall: "on", firewall_groups: floor`
	nets := walled + `, net_bridge: hgnets, net_tag: "100", net_block: 198.51.100.0/24, net_size: "25", net_vmids: 11200-11201, net_pool: hangar-nets, net_address: 203.0.113.200, net_archive: "local:vztmpl/gw.tar.zst"`
	for name, options := range map[string]string{"a plain zone": "", "a walled zone": walled, "a zone that cuts networks": nets} {
		if err := checkZones(file(options)); err != nil {
			t.Errorf("%s, sound, its engine out of reach: %v", name, err)
		}
	}
	for name, c := range map[string][2]string{
		"an address past its range":    {`, subnet: 203.0.113.0/24, first_address: 203.0.113.200`, "outside what 203.0.113.0/24 gives"},
		"a group that is no name":      {`, subnet: 203.0.113.0/24, first_address: 203.0.113.100, firewall: "on", firewall_groups: "floor -j ACCEPT"`, "no security group's name"},
		"a word mistyped":              {`, firewal: "on"`, "no option firewal"},
		"networks with no wall":        {`, net_bridge: hgnets, net_tag: "100", net_block: 198.51.100.0/24, net_vmids: 11200-11201, net_pool: p, net_address: 203.0.113.200, net_archive: a`, "needs subnet, gateway and firewall: on"},
		"networks too small":           {strings.Replace(nets, `net_size: "25"`, `net_size: "28"`, 1), "a smaller net_size, or fewer ids"},
		"gateways among the machines'": {strings.Replace(nets, "203.0.113.200", "203.0.113.150", 1), "run into the guests'"},
	} {
		if err := checkZones(file(c[0])); err == nil || !strings.Contains(err.Error(), c[1]) || !strings.Contains(err.Error(), "zone z") {
			t.Errorf("%s: %v (want %q, naming the zone)", name, err, c[1])
		}
	}
	// and it is what check and a start hold a file against, before any plugin
	// is started: neither goes on with a zone that could never open
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	if _, err := startPlugins(context.Background(), file(`, firewal: "on"`), log, io.Discard); err == nil || !strings.Contains(err.Error(), "config: proxmox zone z: no option firewal") {
		t.Fatalf("a start on a file with a word mistyped: %v", err)
	}
}
