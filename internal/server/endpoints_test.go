package server

import (
	"slices"
	"strings"
	"testing"
	"time"
)

const endpointsConfig = `
data_dir: %[1]s
identity:
  oidc: {issuer: %[2]s, audience: hangar}
tiers:
  - name: users
    groups: [users]
    zones: [z]
    limits:
      machines.count: 4
      machines.vcpu_hours: 1000
      machines.vcpu: 8
      machines.memory_gb: 8
      machines.disk_gb: 64
      machines.key_pairs: 4
      machines.kind: [container]
      machines.class: [spot]
      networks.count: 2
      networks.visibility: [private, shared]
      networks.endpoints: 3
      networks.services: [logs, syslog]
  - name: before
    groups: [before]
    zones: [z]
    limits:
      networks.count: 1
      networks.visibility: [private]
zones:
  - name: z
    driver: fake
    endpoint: "%[1]s/zone-z.json"
plugins:
  - name: machines
    path: %[3]s
    args: [hangar-test-plugin, machines]
    zones: [z]
    settings:
      images:
        debian-13: {container: "store:vztmpl/debian-13.tar.zst"}
  - name: networks
    path: %[3]s
    args: [hangar-test-plugin, networks]
    zones: [z]
    settings:
      zones:
        z:
          services:
            - {name: logs, description: "the log store's push door", to: "192.0.2.35:3101"}
            - {name: syslog, to: "192.0.2.35:514/udp"}
            - {name: base, to: "192.0.2.36:5433"}
          private: [192.0.2.0/24]
reconcile:
  every: 1h
`

const (
	lineLogs   = "tcp:192.0.2.35:3101"
	lineSyslog = "udp:192.0.2.35:514"
)

// envOf reads what a network's gateway carries of its endpoints, on the fake
// engine: what is written, and those it runs with.
func envOf(t *testing.T, s *stack, net string) (to []string, private []string, runs []string, restarted int) {
	t.Helper()
	n := networksOf(t, s)[net]
	strs := func(v any) []string {
		var out []string
		l, _ := v.([]any)
		for _, x := range l {
			out = append(out, x.(string))
		}
		return out
	}
	if env, ok := n["env"].(map[string]any); ok {
		to, private = strs(env["to"]), strs(env["private"])
	}
	if r, ok := n["restarted"].(float64); ok {
		restarted = int(r)
	}
	return to, private, strs(n["endpoints"]), restarted
}

// An endpoint through the whole brain, on the fake engine: a network of a
// zone that publishes services is born refusing the operator's side; an
// endpoint of its owner's lets one service through, its gateway written and
// — running — started again; its page says what it reaches and as whom; a
// service the zone does not publish, or the tier does not open, is refused;
// a network shared with someone holds their machines, never their
// endpoints; a network that holds one is not deleted; a hand's edit of the
// gateway's record is written again at the next look; a delete takes its
// line away.
func TestAnEndpointsLife(t *testing.T) {
	s := netStack{newStackWith(t, endpointsConfig, "machines", "networks"), t}
	alice := s.token("alice", "users", "family")
	bob := s.token("bob", "users", "family")

	net := s.made(s.create(alice, map[string]any{"type": "network", "zone": "z", "name": "lab"}))
	if to, private, runs, _ := envOf(t, s.stack, net); len(to) != 0 || !slices.Equal(private, []string{"192.0.2.0/24"}) || len(runs) != 0 {
		t.Fatalf("born carrying %v %v %v: a zone that publishes refuses its side to a network of none", to, private, runs)
	}

	// her endpoint, by what she calls her network
	ep := s.made(s.create(alice, map[string]any{"type": "endpoint", "zone": "z", "name": "lab-logs", "spec": map[string]any{"network": "lab", "service": "logs"}}))
	got := s.get(alice, ep)
	if got.str("spec", "network") != net || got.str("observed", "to") != "192.0.2.35:3101/tcp" || got.str("observed", "from") != "203.0.113.200" ||
		got.str("status") != "applied" {
		t.Fatalf("made as %v", got.body)
	}
	if !strings.HasPrefix(got.str("summary"), "logs from lab · to 192.0.2.35:3101/tcp · as 203.0.113.200") {
		t.Fatalf("it reads %q", got.str("summary"))
	}
	if to, _, runs, _ := envOf(t, s.stack, net); !slices.Equal(to, []string{lineLogs}) || !slices.Equal(runs, []string{lineLogs}) {
		t.Fatalf("its gateway carries %v, runs with %v", to, runs)
	}

	// what the zone does not publish, and what her tier does not open
	s.refused(s.create(alice, map[string]any{"type": "endpoint", "zone": "z", "spec": map[string]any{"network": net, "service": "nope"}}),
		422, "", "zone z publishes no service nope; it publishes base, logs, syslog")
	s.refused(s.create(alice, map[string]any{"type": "endpoint", "zone": "z", "spec": map[string]any{"network": net, "service": "base"}}),
		403, "limit", "networks.services")
	// a tier written before endpoints makes none, and needs no change
	old := s.token("olga", "before")
	olgas := s.made(s.create(old, map[string]any{"type": "network", "zone": "z", "name": "mine"}))
	s.refused(s.create(old, map[string]any{"type": "endpoint", "zone": "z", "spec": map[string]any{"network": olgas, "service": "logs"}}),
		403, "limit", "networks.")

	// shared with her family: bob puts machines on it, never an endpoint
	if r := s.act(alice, net, "share", map[string]any{"shared_with": []string{"family"}}); r.code != 202 {
		t.Fatalf("%d %v", r.code, r.body)
	}
	s.refused(s.create(bob, map[string]any{"type": "endpoint", "zone": "z", "spec": map[string]any{"network": net, "service": "logs"}}),
		422, "", "you have no network "+net)

	// not deleted while it holds one
	s.refused(s.gone(alice, net), 409, "members", net+" (lab) still holds lab-logs: delete it first")

	// a machine of it runs: a second endpoint is written, and its running
	// gateway started again — a change's cut
	box := s.made(s.machine(alice, "z", "box", map[string]any{"network": "lab"}))
	_, _, _, before := envOf(t, s.stack, net)
	ep2 := s.made(s.create(alice, map[string]any{"type": "endpoint", "zone": "z", "spec": map[string]any{"network": net, "service": "syslog"}}))
	to, _, runs, restarted := envOf(t, s.stack, net)
	if !slices.Equal(to, []string{lineLogs, lineSyslog}) || !slices.Equal(runs, to) || restarted != before+1 {
		t.Fatalf("after a second: %v, runs with %v, started again %d times (was %d)", to, runs, restarted, before)
	}
	if g := s.get(alice, ep2); g.str("observed", "to") != "192.0.2.35:514/udp" || g.str("status") != "applied" {
		t.Fatalf("%v", g.body)
	}

	// a hand writes another line on the engine: written again at the next look
	editNetworks(t, s.stack, func(st map[string]any) {
		n := st["networks"].(map[string]any)[net].(map[string]any)
		n["env"] = map[string]any{"to": []string{"tcp:192.0.2.35:22"}, "private": []string{"192.0.2.0/24"}}
	})
	s.look()
	if to, _, _, _ := envOf(t, s.stack, net); !slices.Equal(to, []string{lineLogs, lineSyslog}) {
		t.Fatalf("after a look: %v", to)
	}
	if !strings.Contains(s.logs.String(), "network.repaired") || !strings.Contains(s.logs.String(), "put back: endpoints") {
		t.Fatalf("no repair said:\n%s", s.logs.String())
	}

	// deleted: its line goes, the rest stays
	if r := s.gone(alice, ep); r.code != 202 {
		t.Fatalf("%d %v", r.code, r.body)
	}
	if to, _, runs, _ := envOf(t, s.stack, net); !slices.Equal(to, []string{lineSyslog}) || !slices.Equal(runs, to) {
		t.Fatalf("after its delete: %v, runs with %v", to, runs)
	}
	s.gone(alice, ep2)
	s.gone(alice, box)
	if r := s.gone(alice, net); r.code != 202 {
		t.Fatalf("with none left it goes: %d %v", r.code, r.body)
	}
}

// Two endpoints of one network asked while the first is still being
// written (its write slow, on the engine): each create holds its network
// while it writes, so what the gateway carries is both of them — never the
// first one alone, written last from what it read first.
func TestEndpointsAskedTogetherAreAllCarried(t *testing.T) {
	s := netStack{newStackWith(t, endpointsConfig, "machines", "networks"), t}
	alice := s.token("alice", "users")
	net := s.made(s.create(alice, map[string]any{"type": "network", "zone": "z", "name": "lab"}))
	editNetworks(t, s.stack, func(st map[string]any) { st["network_writes"] = []string{"slow:800"} })
	var ops []string
	for _, svc := range []string{"logs", "syslog"} {
		r := s.do("POST", "/v1/resources", alice, map[string]any{"type": "endpoint", "zone": "z", "spec": map[string]any{"network": net, "service": svc}})
		if r.code != 202 {
			t.Fatalf("%d %v", r.code, r.body)
		}
		ops = append(ops, r.str("operation", "id"))
		time.Sleep(200 * time.Millisecond) // the first is in its write
	}
	for _, op := range ops {
		if r := s.done(alice, op); r.str("state") != "succeeded" {
			t.Fatalf("%v", r.body)
		}
	}
	if to, _, _, _ := envOf(t, s.stack, net); !slices.Equal(to, []string{lineLogs, lineSyslog}) {
		t.Fatalf("its gateway carries %v", to)
	}
}

// A create the engine wrote and then refused: the brain clears what it left
// — the endpoint's line taken away, the endpoint itself counted out of what
// its network's gateway carries though it is still being made.
func TestAFailedEndpointLeavesNoLine(t *testing.T) {
	s := netStack{newStackWith(t, endpointsConfig, "machines", "networks"), t}
	alice := s.token("alice", "users")
	net := s.made(s.create(alice, map[string]any{"type": "network", "zone": "z", "name": "lab"}))
	editNetworks(t, s.stack, func(st map[string]any) { st["network_writes"] = []string{"refused-after"} })
	r := s.do("POST", "/v1/resources", alice, map[string]any{"type": "endpoint", "zone": "z", "spec": map[string]any{"network": net, "service": "logs"}})
	if r.code != 202 {
		t.Fatalf("%d %v", r.code, r.body)
	}
	if op := s.done(alice, r.str("operation", "id")); op.str("state") != "failed" {
		t.Fatalf("%v", op.body)
	}
	if to, _, _, _ := envOf(t, s.stack, net); len(to) != 0 {
		t.Fatalf("a failed endpoint left %v on its gateway", to)
	}
}
