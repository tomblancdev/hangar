package server

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

const networksConfig = `
data_dir: %[1]s
identity:
  oidc: {issuer: %[2]s, audience: hangar}
tiers:
  - name: ops
    groups: [ops]
    operator: true
    zones: [z, plain, y]
    limits: {"*": unlimited}
  - name: users
    groups: [users]
    zones: [z, plain, y]
    limits:
      machines.count: 6
      machines.vcpu_hours: 1000
      machines.vcpu: 12
      machines.memory_gb: 16
      machines.disk_gb: 64
      machines.key_pairs: 4
      machines.kind: [vm, container]
      machines.class: [spot]
      networks.count: 2
      networks.visibility: [private, shared]
  - name: guests
    groups: [guests]
    zones: [z]
    limits:
      machines.count: 2
      machines.vcpu_hours: 1000
      machines.vcpu: 4
      machines.memory_gb: 4
      machines.disk_gb: 16
      machines.kind: [container]
      machines.class: [spot]
zones:
  - name: z
    driver: fake
    endpoint: "%[1]s/zone-z.json"
    room:
      memory_gb: 12
      reservations:
        - {name: priority, memory_gb: 6, while_running: "4100"}
  - name: y
    driver: fake
    endpoint: "%[1]s/zone-y.json"
  - name: plain
    driver: fake
    endpoint: "%[1]s/zone-plain.json"
    options:
      capabilities: "kind.vm,kind.container,guest.tags,fence.pool,net.firewall,resize.live.cpu_cap"
plugins:
  - name: machines
    path: %[3]s
    args: [hangar-test-plugin, machines]
    zones: [z, plain, y]
    settings:
      images:
        debian-13: {vm: debian-13, container: "store:vztmpl/debian-13.tar.zst"}
  - name: networks
    path: %[3]s
    args: [hangar-test-plugin, networks]
    zones: [z, plain, y]
reconcile:
  every: 1h
`

const (
	netKeyA = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGXj2Dq6dbWAg2wXCN9pDWc3cS/cJEHWIr0sRd4b3M5V laptop"
	netKeyB = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDeYZ0lS1d0FaLUkBZtD5OzcyHgvYtHhiRl8SLe1kcu8 phone"
)

// networksOf reads the fake engine's networks, by id.
func networksOf(t *testing.T, s *stack) map[string]map[string]any {
	t.Helper()
	var eng struct {
		Networks map[string]map[string]any `json:"networks"`
	}
	if err := json.Unmarshal([]byte(s.engineFile()), &eng); err != nil {
		t.Fatal(err)
	}
	return eng.Networks
}

// editNetworks changes the fake engine's file: someone at the engine, behind
// the brain's back.
func editNetworks(t *testing.T, s *stack, f func(st map[string]any)) {
	t.Helper()
	st := map[string]any{}
	if err := json.Unmarshal([]byte(s.engineFile()), &st); err != nil {
		t.Fatal(err)
	}
	f(st)
	b, _ := json.Marshal(st)
	if err := os.WriteFile(s.engine, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

type netStack struct {
	*stack
	t *testing.T
}

func (n netStack) refused(r reply, code int, kind, words string) {
	n.t.Helper()
	if r.code != code || kind != "" && r.str("kind") != kind || !strings.Contains(r.str("detail"), words) {
		n.t.Fatalf("want %d %s %q: %d %v", code, kind, words, r.code, r.body)
	}
}

func (n netStack) made(r reply) string {
	n.t.Helper()
	if r.code != 202 {
		n.t.Fatalf("%d %v", r.code, r.body)
	}
	return r.str("resource", "id")
}

func (n netStack) machine(who, zone, name string, spec map[string]any) reply {
	n.t.Helper()
	full := map[string]any{"kind": "container", "image": "debian-13", "cores": 1, "memory_gb": 1}
	for k, v := range spec {
		full[k] = v
	}
	return n.create(who, map[string]any{"type": "machine", "zone": zone, "name": name, "spec": full})
}

func (n netStack) get(who, id string) reply {
	n.t.Helper()
	return n.do("GET", "/v1/resources/"+id, who, nil)
}

func (n netStack) act(who, id, action string, params map[string]any) reply {
	n.t.Helper()
	body := map[string]any{}
	if params != nil {
		body["params"] = params
	}
	r := n.do("POST", "/v1/resources/"+id+"/actions/"+action, who, body)
	if r.code == 202 {
		if op := n.done(who, r.str("operation", "id")); op.str("state") != "succeeded" {
			n.t.Fatalf("%s failed: %v", action, op.body)
		}
	}
	return r
}

func (n netStack) gone(who, id string) reply {
	n.t.Helper()
	r := n.do("DELETE", "/v1/resources/"+id, who, nil)
	if r.code == 202 {
		if op := n.done(who, r.str("operation", "id")); op.str("state") != "succeeded" {
			n.t.Fatalf("delete of %s failed: %v", id, op.body)
		}
	}
	return r
}

func (n netStack) look() {
	n.core.ReconcileOnce(context.Background())
	n.core.Wait()
}

// A network through the whole brain, on the fake engine: made with the key
// pairs that may jump into it, a machine put on it by name and given its
// address there, its gateway up while the machine runs and at rest after; not
// deleted while a machine stands on it; its gateway made again when its keys
// change; shared with a group — whose people put machines on it, and never
// change it.
func TestANetworksLife(t *testing.T) {
	s := netStack{newStackWith(t, networksConfig, "machines", "networks"), t}
	alice := s.token("alice", "users", "family")
	bob := s.token("bob", "users", "family")
	carol := s.token("carol", "users")
	s.made(s.create(alice, map[string]any{"type": "keypair", "zone": "z", "name": "laptop", "spec": map[string]any{"public_key": netKeyA}}))
	phone := s.made(s.create(alice, map[string]any{"type": "keypair", "zone": "z", "name": "phone", "spec": map[string]any{"public_key": netKeyB}}))

	net := s.made(s.create(alice, map[string]any{"type": "network", "zone": "z", "name": "lab", "spec": map[string]any{"key_pairs": []string{"laptop"}}}))
	got := s.get(alice, net)
	if got.str("observed", "range") != "192.0.2.0/27" || got.str("observed", "jump") != "jump@203.0.113.200" || got.str("status") != "idle" ||
		got.str("summary") != "192.0.2.0/27 · jump through jump@203.0.113.200 · 1 keys" {
		t.Fatalf("made as %v", got.body)
	}
	// its gateway was born with the key the pair holds — not with its name
	if eng := networksOf(t, s.stack)[net]; len(eng["jump_keys"].([]any)) != 1 || eng["jump_keys"].([]any)[0] != netKeyA || eng["running"] != false {
		t.Fatalf("on the engine: %v", eng)
	}
	// what it books: its gateway's memory, promised for as long as it exists
	if z := s.do("GET", "/v1/zones", alice, nil); z.str("zones", "0", "room", "booked_mb") != "64" {
		t.Fatalf("the zone's room: %v", z.body)
	}

	// a machine names it, by what its owner calls it: on it, at an address of its range
	box := s.made(s.machine(alice, "z", "box", map[string]any{"network": "lab"}))
	m := s.get(alice, box)
	if m.str("spec", "network") != net || !strings.HasPrefix(m.str("observed", "address"), "192.0.2.") || !strings.Contains(m.str("summary"), "· on lab") {
		t.Fatalf("the machine: %v", m.body)
	}
	if eng := networksOf(t, s.stack)[net]; eng["running"] != true {
		t.Fatalf("a machine of it runs, and its gateway does not: %v", eng)
	}
	s.look()
	if got := s.get(alice, net); got.str("status") != "up" {
		t.Fatalf("with a machine running it reads %q", got.str("status"))
	}
	// not deleted while a machine stands on it
	s.refused(s.gone(alice, net), 409, "members", net+" (lab) still holds box: delete it first")
	// its last machine stopped, its gateway rests
	if r := s.act(alice, box, "stop", nil); r.code != 202 {
		t.Fatalf("%d %v", r.code, r.body)
	}
	if eng := networksOf(t, s.stack)[net]; eng["running"] != false {
		t.Fatalf("its last machine stopped, its gateway still runs: %v", eng)
	}

	// its key pairs changed: its gateway is made again with them
	if r := s.act(alice, net, "set_key_pairs", map[string]any{"key_pairs": []string{"laptop", "phone"}}); r.code != 202 {
		t.Fatalf("%d %v", r.code, r.body)
	}
	eng := networksOf(t, s.stack)[net]
	if eng["made"] != float64(2) || len(eng["jump_keys"].([]any)) != 2 || eng["range"] != "192.0.2.0/27" {
		t.Fatalf("after its keys changed: %v", eng)
	}
	if got := s.get(alice, net); got.str("observed", "keys") != "2" || got.str("spec", "key_pairs", "1") != phone {
		t.Fatalf("it reads %v", got.body)
	}
	// a key pair that is not hers is none
	s.refused(s.do("POST", "/v1/resources/"+net+"/actions/set_key_pairs", alice, map[string]any{"params": map[string]any{"key_pairs": []string{"kp-0123456789abcdef0"}}}),
		422, "", "you have no keypair")

	// private: nobody else puts a machine on it
	s.refused(s.machine(bob, "z", "bobs", map[string]any{"network": net}), 422, "", "you have no network "+net)
	// shared with a group she is in: its people do, the others still do not
	if r := s.act(alice, net, "share", map[string]any{"shared_with": []string{"family"}}); r.code != 202 {
		t.Fatalf("%d %v", r.code, r.body)
	}
	if got := s.get(bob, net); got.code != 200 || !strings.HasSuffix(got.str("summary"), "shared with family") {
		t.Fatalf("bob sees %d %v", got.code, got.body)
	}
	bobs := s.made(s.machine(bob, "z", "bobs", map[string]any{"network": net}))
	if m := s.get(bob, bobs); !strings.HasPrefix(m.str("observed", "address"), "192.0.2.") {
		t.Fatalf("bob's machine: %v", m.body)
	}
	s.refused(s.machine(carol, "z", "carols", map[string]any{"network": net}), 422, "", "you have no network "+net)
	// and never change it, nor delete it
	s.refused(s.do("POST", "/v1/resources/"+net+"/actions/share", bob, map[string]any{"params": map[string]any{"shared_with": []string{}}}), 403, "shared", "alice")
	s.refused(s.gone(bob, net), 403, "shared", "alice")
	// she shares only with groups she is in, and with everyone only where her tier says
	s.refused(s.do("POST", "/v1/resources/"+net+"/actions/share", alice, map[string]any{"params": map[string]any{"shared_with": []string{"strangers"}}}), 403, "", "strangers")
	s.refused(s.do("POST", "/v1/resources/"+net+"/actions/share", alice, map[string]any{"params": map[string]any{"shared_with": []string{"*"}}}), 403, "limit", "networks.visibility")

	// a machine's network is set at its birth
	plan := s.do("POST", "/v1/resources/"+box+"/plan", alice, map[string]any{"spec": map[string]any{"kind": "container", "image": "debian-13", "cores": 1, "memory_gb": 1}})
	if plan.code != 200 || plan.str("steps") != "[]" || plan.body["fixed"] != nil {
		t.Fatalf("a spec that names no network leaves a machine where it stands: %d %v", plan.code, plan.body)
	}
	other := s.made(s.create(alice, map[string]any{"type": "network", "zone": "z", "name": "other"}))
	plan = s.do("POST", "/v1/resources/"+box+"/plan", alice, map[string]any{"spec": map[string]any{"kind": "container", "image": "debian-13", "cores": 1, "memory_gb": 1, "network": "other"}})
	if plan.code != 200 || !strings.Contains(plan.str("fixed", "0", "reason"), "the network a machine is on is set at its birth") {
		t.Fatalf("another network asked of a machine that exists: %d %v", plan.code, plan.body)
	}
	// a third network is one too many for her tier, with the numbers
	s.refused(s.create(alice, map[string]any{"type": "network", "zone": "z", "name": "third"}), 403, "limit", "2 of 2 networks.count")
	if r := s.gone(alice, other); r.code != 202 {
		t.Fatalf("%d %v", r.code, r.body)
	}

	// both machines named, then gone, the network goes
	s.refused(s.gone(alice, net), 409, "members", net+" (lab) still holds bobs, box: delete them first")
	for who, id := range map[string]string{alice: box, bob: bobs} {
		if r := s.gone(who, id); r.code != 202 {
			t.Fatalf("%d %v", r.code, r.body)
		}
	}
	if r := s.gone(alice, net); r.code != 202 {
		t.Fatalf("%d %v", r.code, r.body)
	}
	if left := networksOf(t, s.stack); len(left) != 0 {
		t.Fatalf("the engine still holds %v", left)
	}
	if z := s.do("GET", "/v1/zones", alice, nil); z.str("zones", "0", "room", "booked_mb") != "0" {
		t.Fatalf("the zone's room after: %v", z.body)
	}
}

// A machine that names no network is put on its owner's network called
// default — made then, in their name and within their tier, if they have
// none. Each person's is their own. Where the zone makes no networks a
// machine is made as it always was.
func TestAMachineThatNamesNoNetwork(t *testing.T) {
	s := netStack{newStackWith(t, networksConfig, "machines", "networks"), t}
	alice := s.token("alice", "users")
	bob := s.token("bob", "users")
	dora := s.token("dora", "guests")
	nets := func(who string) reply { return s.do("GET", "/v1/resources?type=network", who, nil) }

	one := s.made(s.machine(alice, "z", "one", nil))
	mine := nets(alice)
	net := mine.str("resources", "0", "id")
	if mine.str("resources", "1") != "<nil>" || mine.str("resources", "0", "name") != "default" || mine.str("resources", "0", "tags", "hangar:default") != "machine.network" ||
		mine.str("resources", "0", "owner") != "alice" || mine.str("resources", "0", "state") != "ready" {
		t.Fatalf("her networks: %v", mine.body)
	}
	if m := s.get(alice, one); m.str("spec", "network") != net || !strings.HasPrefix(m.str("observed", "address"), "192.0.2.") || !strings.Contains(m.str("summary"), "on default") {
		t.Fatalf("her machine: %v", m.body)
	}
	// said in the audit, as hers: the network asked for, and what the machine was given
	if logs := s.logs.String(); !strings.Contains(logs, `"action":"default"`) || !strings.Contains(logs, `"resolved":"default=`+net+`"`) {
		t.Fatalf("the audit says nothing of the default:\n%s", logs)
	}
	// her second machine is on the same one: nothing more is made
	two := s.made(s.machine(alice, "z", "two", nil))
	if m := s.get(alice, two); m.str("spec", "network") != net || nets(alice).str("resources", "1") != "<nil>" {
		t.Fatalf("her second machine: %v", m.body)
	}
	// someone else's is their own: another network, another range
	bobs := s.made(s.machine(bob, "z", "bobs", nil))
	his := nets(bob).str("resources", "0", "id")
	if his == net || his == "<nil>" || s.get(bob, bobs).str("spec", "network") != his || s.get(bob, his).str("observed", "range") != "192.0.2.32/27" {
		t.Fatalf("bob's network %s, hers %s: %v", his, net, s.get(bob, his).body)
	}
	if s.get(bob, net).code != 404 {
		t.Fatal("bob sees alice's default network")
	}
	// a machine that names one is on the one it names
	lab := s.made(s.create(alice, map[string]any{"type": "network", "zone": "z", "name": "lab"}))
	if m := s.get(alice, s.made(s.machine(alice, "z", "three", map[string]any{"network": "lab"}))); m.str("spec", "network") != lab {
		t.Fatalf("a machine that names its network: %v", m.body)
	}
	// a tier that allows no network makes no machine where a network is needed — in words
	s.refused(s.machine(dora, "z", "doras", nil), 403, "limit", "it names no network, and your network called default could not be made")
	if r := nets(dora); r.str("resources", "0") != "<nil>" {
		t.Fatalf("something of dora's was made: %v", r.body)
	}
	// where the zone makes no networks, a machine is made as it always was
	plain := s.get(alice, s.made(s.machine(alice, "plain", "plain", nil)))
	if plain.str("spec", "network") != "<nil>" || !strings.HasPrefix(plain.str("observed", "address"), "203.0.113.") {
		t.Fatalf("in a zone with no networks: %v", plain.body)
	}
	s.refused(s.machine(alice, "plain", "nope", map[string]any{"network": "lab"}), 422, "", "is in zone z, not plain")
	// a name is one thing: her default is in one zone, and a machine of
	// another zone that names no network is told so — never put across zones
	s.refused(s.machine(alice, "y", "elsewhere", nil), 422, "", "your network default is in zone z: name a network of zone y")
	if r := s.do("GET", "/v1/resources?type=network&zone=y", alice, nil); r.str("resources", "0") != "<nil>" {
		t.Fatalf("something was made in zone y: %v", r.body)
	}
	// an existing machine is left where it stands by a spec that names nothing
	plan := s.do("POST", "/v1/resources/"+one+"/plan", alice, map[string]any{"spec": map[string]any{"kind": "container", "image": "debian-13", "cores": 1, "memory_gb": 1}})
	if plan.code != 200 || plan.body["fixed"] != nil {
		t.Fatalf("%d %v", plan.code, plan.body)
	}
	// her default deleted once nothing stands on it, the next machine makes it again
	for _, id := range []string{one, two} {
		if r := s.gone(alice, id); r.code != 202 {
			t.Fatalf("%d %v", r.code, r.body)
		}
	}
	if r := s.gone(alice, net); r.code != 202 {
		t.Fatalf("%d %v", r.code, r.body)
	}
	again := s.get(alice, s.made(s.machine(alice, "z", "again", nil))).str("spec", "network")
	if again == net || again == "<nil>" || s.get(alice, again).str("name") != "default" {
		t.Fatalf("after her default was deleted: %s", again)
	}
}

// Two first machines asked at the same moment share one default network.
func TestTwoFirstMachinesAtOnceShareOneDefault(t *testing.T) {
	s := netStack{newStackWith(t, networksConfig, "machines", "networks"), t}
	alice := s.token("alice", "users")
	out := make(chan reply, 3)
	for _, name := range []string{"a", "b", "c"} {
		go func() {
			out <- s.do("POST", "/v1/resources", alice, map[string]any{"type": "machine", "zone": "z", "name": name,
				"spec": map[string]any{"kind": "container", "image": "debian-13", "cores": 1, "memory_gb": 1}})
		}()
	}
	var on []string
	for range 3 {
		r := <-out
		if r.code != 202 {
			t.Fatalf("%d %v", r.code, r.body)
		}
		s.done(alice, r.str("operation", "id"))
		on = append(on, s.get(alice, r.str("resource", "id")).str("spec", "network"))
	}
	if on = slices.Compact(on); len(on) != 1 || on[0] == "<nil>" {
		t.Fatalf("three machines at once stand on %v", on)
	}
	if nets := s.do("GET", "/v1/resources?type=network", alice, nil); nets.str("resources", "1") != "<nil>" {
		t.Fatalf("more than one default was made: %v", nets.body)
	}
}

// A network is kept true: a gateway gone behind the brain's back is made
// again where it was, and one stopped under a running machine is started by
// that machine's own look.
func TestANetworkIsKeptTrue(t *testing.T) {
	s := netStack{newStackWith(t, networksConfig, "machines", "networks"), t}
	alice := s.token("alice", "users")
	net := s.made(s.create(alice, map[string]any{"type": "network", "zone": "z", "name": "lab"}))
	box := s.made(s.machine(alice, "z", "box", map[string]any{"network": "lab"}))
	s.look()
	if strings.Contains(s.logs.String(), "repaired") {
		t.Fatalf("a look at a network that stands repaired something:\n%s", s.logs.String())
	}
	// its gateway stopped under a running machine: the machine's look starts it
	editNetworks(t, s.stack, func(st map[string]any) {
		st["networks"].(map[string]any)[net].(map[string]any)["running"] = false
	})
	s.look()
	if eng := networksOf(t, s.stack)[net]; eng["running"] != true || !strings.Contains(s.logs.String(), "put back: [its network's gateway (started)]") {
		t.Fatalf("a gateway stopped under a running machine: %v\n%s", eng, s.logs.String())
	}
	// its gateway gone: made again at its number — its machine never left it
	editNetworks(t, s.stack, func(st map[string]any) { delete(st["networks"].(map[string]any), net) })
	s.look()
	eng := networksOf(t, s.stack)[net]
	if eng == nil || eng["range"] != "192.0.2.0/27" || eng["running"] != true || !strings.Contains(s.logs.String(), "put back: gateway (it was gone: made again)") {
		t.Fatalf("a gateway that was gone: %v\n%s", eng, s.logs.String())
	}
	if got := s.get(alice, net); got.str("state") != "ready" || got.str("status") != "up" {
		t.Fatalf("the network reads %v", got.body)
	}
	// left running with nothing behind it (a node that stopped its machines
	// alone): the stopped machine's look puts it to rest
	if r := s.act(alice, box, "stop", nil); r.code != 202 {
		t.Fatalf("%d %v", r.code, r.body)
	}
	editNetworks(t, s.stack, func(st map[string]any) {
		st["networks"].(map[string]any)[net].(map[string]any)["running"] = true
	})
	s.look()
	if eng := networksOf(t, s.stack)[net]; eng["running"] != false || !strings.Contains(s.logs.String(), "put back: [its network's gateway (stopped)]") {
		t.Fatalf("a gateway left running with no machine: %v", eng)
	}
}
