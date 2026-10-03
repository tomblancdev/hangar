package server

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Three tiers, most to least: one that opens a VM's host processor and VMs
// inside it, one that opens the processor alone, and one written before
// either existed — it names neither, and goes on making machines.
const processorConfig = `
data_dir: %[1]s
identity:
  oidc: {issuer: %[2]s, audience: hangar}
tiers:
  - name: benches
    groups: [benches]
    zones: [m]
    limits:
      machines.count: 4
      machines.vcpu_hours: 1000
      machines.vcpu: 16
      machines.memory_gb: 32
      machines.disk_gb: 64
      machines.kind: [vm, container]
      machines.class: [spot]
      machines.cpu: [host]
      machines.virtualization: [nested]
  - name: fast
    groups: [fast]
    zones: [m]
    limits:
      machines.count: 4
      machines.vcpu_hours: 1000
      machines.vcpu: 16
      machines.memory_gb: 32
      machines.disk_gb: 64
      machines.kind: [vm, container]
      machines.class: [spot]
      machines.cpu: [host]
  - name: users
    groups: [users]
    zones: [m]
    limits:
      machines.count: 4
      machines.vcpu_hours: 1000
      machines.vcpu: 16
      machines.memory_gb: 32
      machines.disk_gb: 64
      machines.kind: [vm, container]
      machines.class: [spot]
  - name: everything
    groups: [everything]
    zones: [m]
    limits:
      "*": unlimited
zones:
  - {name: m, driver: fake, endpoint: "%[1]s/zone-m.json"}
plugins:
  - name: machines
    path: %[3]s
    args: [hangar-test-plugin, machines]
    zones: [m]
    settings:
      images:
        debian-13: {vm: debian-13, container: "store:vztmpl/debian-13.tar.zst"}
reconcile:
  every: 1h
`

// A VM's host processor and the VMs inside it are each a tier's to open: a
// tier that names neither gives neither, in words — and still makes every
// machine that asks for neither. A weight only yields: no tier is asked.
func TestAProcessorIsATiersToOpen(t *testing.T) {
	s := newStackWith(t, processorConfig, "machines")
	user, fast, bench, all := s.token("alice", "users"), s.token("bob", "fast"), s.token("carol", "benches"), s.token("dave", "everything")
	machine := func(who string, more map[string]any) reply {
		spec := map[string]any{"image": "debian-13"}
		for k, v := range more {
			spec[k] = v
		}
		return s.do("POST", "/v1/resources", who, map[string]any{"type": "machine", "zone": "m", "spec": spec})
	}
	made := func(r reply, who string) string {
		t.Helper()
		if r.code != 202 || s.done(who, r.str("operation", "id")).str("state") != "succeeded" {
			t.Fatalf("%d %v", r.code, r.body)
		}
		return r.str("resource", "id")
	}
	host := map[string]any{"cpu": "host"}
	nested := map[string]any{"cpu": "host", "virtualization": true}

	// the tier written before the levers: nothing of them, everything else
	plain := made(machine(user, map[string]any{"cpu_weight": 25}), user)
	if r := machine(user, host); r.code != 403 || !strings.Contains(r.str("detail"), "tier users allows no machines.cpu") {
		t.Fatalf("a host processor in a tier that names none: %d %v", r.code, r.body)
	}
	if r := machine(user, nested); r.code != 403 || r.str("refusals", "0", "message") != "tier users allows no machines.cpu" ||
		r.str("refusals", "1", "message") != "tier users allows no machines.virtualization" {
		t.Fatalf("VMs inside in a tier that names none: %d %v", r.code, r.body)
	}
	// the processor alone: speed, and no VM inside
	quick := made(machine(fast, host), fast)
	if r := machine(fast, nested); r.code != 403 || r.str("detail") != "tier fast allows no machines.virtualization" ||
		len(asList(r.body["refusals"])) != 1 {
		t.Fatalf("VMs inside in a tier that opens the processor alone: %d %v", r.code, r.body)
	}
	// both, and the engine is handed both
	box := made(machine(bench, map[string]any{"cpu": "host", "virtualization": true, "cpu_weight": 50}), bench)
	// "*": unlimited is the operator saying everything
	made(machine(all, nested), all)

	var engine struct {
		Specs map[string]struct {
			CPU            string
			Virtualization bool
			CPUWeight      int
		} `json:"specs"`
	}
	b, _ := os.ReadFile(s.engine[:strings.LastIndex(s.engine, "/")] + "/zone-m.json")
	_ = json.Unmarshal(b, &engine)
	if got := engine.Specs[plain]; got.CPU != "" || got.Virtualization || got.CPUWeight != 25 {
		t.Fatalf("the plain machine's engine was handed %+v", got)
	}
	if got := engine.Specs[quick]; got.CPU != "host" || got.Virtualization {
		t.Fatalf("the fast machine's engine was handed %+v", got)
	}
	if got := engine.Specs[box]; got.CPU != "host" || !got.Virtualization || got.CPUWeight != 50 {
		t.Fatalf("the bench's engine was handed %+v", got)
	}

	// what the schema refuses before any plugin is asked
	if r := machine(bench, map[string]any{"cpu": "EPYC"}); r.code != 422 || r.str("kind") != "schema" {
		t.Fatalf("a processor by another name: %d %v", r.code, r.body)
	}
	if r := machine(bench, map[string]any{"cpu_weight": 500}); r.code != 422 || r.str("kind") != "schema" {
		t.Fatalf("more than a full share: %d %v", r.code, r.body)
	}
	if r := machine(bench, map[string]any{"virtualization": true}); r.code != 422 || !strings.Contains(r.str("detail"), "say cpu: host with it") {
		t.Fatalf("VMs inside on the zone's own model: %d %v", r.code, r.body)
	}

	// a resource as it is served: its sentence says its processor
	if got := s.do("GET", "/v1/resources/"+box, bench, nil); !strings.Contains(got.str("summary"), "host CPU · runs VMs · CPU weight 50") {
		t.Fatalf("the bench reads %q", got.str("summary"))
	}
	if got := s.do("GET", "/v1/resources/"+quick, fast, nil); !strings.Contains(got.str("summary"), "host CPU") ||
		strings.Contains(got.str("summary"), "runs VMs") || strings.Contains(got.str("summary"), "weight") {
		t.Fatalf("the fast machine reads %q", got.str("summary"))
	}

	// the weight, while it runs: its own action, and what apply would ask
	r := s.do("POST", "/v1/resources/"+box+"/actions/set_cpu_weight", bench, map[string]any{"params": map[string]any{"cpu_weight": 10}})
	if r.code != 202 || s.done(bench, r.str("operation", "id")).str("state") != "succeeded" {
		t.Fatalf("set_cpu_weight: %d %v", r.code, r.body)
	}
	if got := s.do("GET", "/v1/resources/"+box, bench, nil); got.str("spec", "cpu_weight") != "10" || got.str("observed", "cpu_weight") != "10" {
		t.Fatalf("after the action: %v", got.body)
	}
	p := s.do("POST", "/v1/resources/"+box+"/plan", bench, map[string]any{"spec": map[string]any{"image": "debian-13", "cpu": "host", "cpu_weight": 30}})
	if p.code != 200 || p.str("steps", "0", "action") != "set_cpu_weight" || p.str("fixed", "0", "field") != "/virtualization" {
		t.Fatalf("a change's plan: %d %v", p.code, p.body)
	}
	// a start or a stop asks the tier nothing anew
	if r := s.do("POST", "/v1/resources/"+box+"/actions/stop", bench, nil); r.code != 202 || s.done(bench, r.str("operation", "id")).str("state") != "succeeded" {
		t.Fatalf("stop: %d %v", r.code, r.body)
	}
	if r := s.do("POST", "/v1/resources/"+box+"/actions/start", bench, nil); r.code != 202 || s.done(bench, r.str("operation", "id")).str("state") != "succeeded" {
		t.Fatalf("start: %d %v", r.code, r.body)
	}
}
