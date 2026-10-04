package proxmox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/tomblancdev/hangar/driver"
)

// The zone the networks' tests open: ten machine ids, four networks of /27
// cut from one documentation range, their gateways on the lane from .200.
var netZone = map[string]string{
	"subnet": "198.51.100.0/24", "first_address": "198.51.100.20", "gateway": "198.51.100.1",
	"firewall": "on", "firewall_groups": "floor", "firewall_log": "info",
	"net_bridge": "hgnets", "net_tag": "100", "net_block": "203.0.113.0/24", "net_size": "27",
	"net_vmids": "11200-11203", "net_pool": "hangar-nets", "net_address": "198.51.100.200",
	"net_archive": "local:vztmpl/hangar-gateway-1.tar.zst",
}

func with(base map[string]string, more ...string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for i := 0; i+1 < len(more); i += 2 {
		if more[i+1] == "" {
			delete(out, more[i])
		} else {
			out[more[i]] = more[i+1]
		}
	}
	return out
}

// Everything of a network is derived from its gateway's own number — and a
// zone's file that cannot hold what it says is refused, in words.
func TestAZonesNetworks(t *testing.T) {
	d, err := parse(driver.Params{Zone: "z", Options: with(netZone, "node", "n", "pool", "hangar", "storage", "s", "seed_storage", "seeds", "bridge", "vmbr0", "vmids", "11000-11009")})
	if err != nil {
		t.Fatal(err)
	}
	n := d.nets
	for i, want := range map[int][4]string{
		0: {"203.0.113.0/27", "203.0.113.1", "198.51.100.200", "203.0.113.10/27"},
		3: {"203.0.113.96/27", "203.0.113.97", "198.51.100.203", "203.0.113.106/27"},
	} {
		got := [4]string{n.rangeOf(i).String(), n.gateway(i).String(), n.lane(i).String(), n.place(i, 0, nil).addr.String()}
		if got != want {
			t.Errorf("network %d: %v, want %v", i, got, want)
		}
	}
	// a guest's address: its network's tenth, plus its own number less the first
	if pl := n.place(2, 9, d.lane.resolvers); pl.addr.String() != "203.0.113.83/27" || pl.gateway.String() != "203.0.113.65" || pl.tag != 102 ||
		pl.bridge != "hgnets" || resolverList(pl.resolvers) != "198.51.100.1" {
		t.Errorf("the last guest of the zone on network 2 stands at %+v", pl)
	}
	// a card says which network it stands on; the lane's says none
	for card, want := range map[string]int{
		"name=eth0,bridge=hgnets,tag=102,firewall=1,ip=203.0.113.74/27": 2,
		"virtio=00:00:5E:00:53:FA,bridge=hgnets,tag=100,firewall=1":     0,
		"name=eth0,bridge=vmbr0,firewall=1,ip=198.51.100.23/24":         -1,
		"name=eth0,bridge=hgnets,tag=104":                               -1, // past the last network
		"name=eth0,bridge=hgnets":                                       -1,
	} {
		got, on := n.of(card)
		if !on {
			got = -1
		}
		if got != want {
			t.Errorf("%s stands on network %d, want %d", card, got, want)
		}
	}
	base := with(netZone, "node", "n", "pool", "hangar", "storage", "s", "seed_storage", "seeds", "bridge", "vmbr0", "vmids", "11000-11009")
	for name, c := range map[string]struct {
		o    map[string]string
		want string
	}{
		"a tag with no bridge":             {with(base, "net_bridge", ""), "said with net_bridge"},
		"a bridge alone":                   {with(base, "net_pool", "", "net_archive", ""), "net_bridge needs net_pool, net_archive"},
		"no wall":                          {with(base, "firewall", "", "firewall_groups", "", "firewall_log", ""), "needs subnet, gateway and firewall: on"},
		"no way out of the lane":           {with(base, "gateway", ""), "needs subnet, gateway and firewall: on"},
		"the lane's own bridge":            {with(base, "net_bridge", "vmbr0"), "not the lane's"},
		"the machines' pool":               {with(base, "net_pool", "hangar"), "the gateways' own pool"},
		"ids that are machines'":           {with(base, "net_vmids", "11005-11020"), "a gateway's id is no machine's"},
		"tags past the last":               {with(base, "net_tag", "4093"), "the first network's tag"},
		"a block that is no range":         {with(base, "net_block", "203.0.113.7/24"), "a range of IPv4 addresses"},
		"a block on the lane":              {with(base, "net_block", "198.51.100.0/25"), "holds addresses of the lane"},
		"more networks than the block has": {with(base, "net_size", "28", "net_block", "203.0.113.0/27"), "holds 2 networks of /28, and net_vmids names 4"},
		"networks too small for the ids":   {with(base, "net_size", "28"), "a smaller net_size, or fewer ids"},
		"a size wider than the block":      {with(base, "net_size", "16"), "no wider than the block"},
		"gateways off the lane":            {with(base, "net_address", "192.0.2.200"), "outside what 198.51.100.0/24 gives"},
		"gateways past the broadcast":      {with(base, "net_address", "198.51.100.253"), "run to 198.51.101.0"}, // no-environment: ok
		"gateways among the guests":        {with(base, "net_address", "198.51.100.27"), "run into the guests'"},
		"gateways on the lane's own":       {with(base, "gateway", "198.51.100.201"), "is one of the addresses the networks' gateways are given"},
		"a word that is no option":         {with(base, "net_adress", "198.51.100.200"), "no option net_adress"},
	} {
		if _, err := parse(driver.Params{Zone: "z", Options: c.o}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v (want %q)", name, err, c.want)
		}
	}
	// a zone that says nothing of networks cuts none, and says no flag
	plain := open(t, newAPI(), walledZone)
	cut := open(t, newAPI(), netZone)
	if has(plain.Capabilities(), driver.NetPrivate) || !has(cut.Capabilities(), driver.NetPrivate) || plain.NetworkRoomMB() != 0 || cut.NetworkRoomMB() != gatewayMemoryMB {
		t.Fatal("net.private is the flag of a zone that says net_bridge, and of no other")
	}
}

// A zone's options are judged by its driver with no engine to ask: what
// `hangar check` holds a file against before it lands.
func TestAZonesOptionsAreJudgedWithoutItsEngine(t *testing.T) {
	sound := with(netZone, "node", "n", "pool", "hangar", "storage", "s", "seed_storage", "seeds", "bridge", "vmbr0", "vmids", "11000-11009")
	if err := driver.Check(Name, driver.Params{Zone: "z", Endpoint: "https://192.0.2.1:8006", Options: sound}); err != nil {
		t.Fatalf("a sound zone: %v", err)
	}
	for name, c := range map[string]struct {
		o    map[string]string
		want string
	}{
		"an address past its range": {with(sound, "first_address", "198.51.100.250"), "outside what"},
		"a group that is no name":   {with(sound, "firewall_groups", "floor -j ACCEPT"), "no security group's name"},
		"a word mistyped":           {with(sound, "firewal", "on"), "no option firewal"},
		"a fingerprint that is not": {with(sound, "fingerprint", "zz"), "fingerprint"},
	} {
		if err := driver.Check(Name, driver.Params{Zone: "z", Options: c.o}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v (want %q)", name, err, c.want)
		}
	}
	if err := driver.Check("no-such-driver", driver.Params{}); err != nil {
		t.Fatalf("a driver that judges nothing says nothing: %v", err)
	}
}

// node is a Proxmox VE node of several guests, as the networks' tests need
// one: guests are made, started, stopped and deleted, and every such act is
// kept in the order it came.
type node struct {
	mu     sync.Mutex
	a      *api
	guests map[int]*pve
	pool   map[int]string
	on     map[int]bool
	forms  map[int]map[string]string // what each guest was created with
	acts   []string                  // "create 11200", "start 11200", "stop 11000", "delete 11200"
}

func newCluster(a *api) *node {
	n := &node{a: a, guests: map[int]*pve{}, pool: map[int]string{}, on: map[int]bool{}, forms: map[int]map[string]string{}}
	a.h["GET /cluster/resources"] = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("type") == "node" {
			data(w, a.nodes)
			return
		}
		n.mu.Lock()
		defer n.mu.Unlock()
		out := []resource{}
		for vmid, g := range n.guests {
			g.mu.Lock()
			out = append(out, resource{VMID: vmid, Node: "node-a", Type: "lxc", Pool: n.pool[vmid], Tags: fmt.Sprint(g.cfg["tags"]), Name: fmt.Sprint(g.cfg["hostname"])})
			if g.cfg["tags"] == nil {
				out[len(out)-1].Tags = ""
			}
			g.mu.Unlock()
		}
		sort.Slice(out, func(i, j int) bool { return out[i].VMID < out[j].VMID })
		data(w, out)
	}
	a.h["GET /cluster/nextid"] = func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.Atoi(r.URL.Query().Get("vmid"))
		n.mu.Lock()
		_, taken := n.guests[id]
		n.mu.Unlock()
		if taken {
			http.Error(w, `{"errors":{"vmid":"VM `+strconv.Itoa(id)+` already exists"}}`, http.StatusBadRequest)
			return
		}
		data(w, strconv.Itoa(id))
	}
	// no seed disc: a container has none
	a.h["GET /nodes/node-a/storage/seeds/content"] = func(w http.ResponseWriter, _ *http.Request) { data(w, []any{}) }
	a.h["POST /nodes/node-a/lxc"] = func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form := map[string]string{}
		for k, v := range r.PostForm {
			form[k] = v[0]
		}
		vmid, _ := strconv.Atoi(form["vmid"])
		cfg := map[string]any{"hostname": form["hostname"], "description": form["description"], "net0": form["net0"], "memory": form["memory"]}
		// a card written with no MAC is given one, as Proxmox draws it
		cfg["net0"] = strings.Replace(form["net0"], "name=eth0", fmt.Sprintf("name=eth0,hwaddr=00:00:5E:00:53:%02X", vmid%256), 1)
		if form["net1"] != "" {
			cfg["net1"] = strings.Replace(form["net1"], "name=eth1", fmt.Sprintf("name=eth1,hwaddr=00:00:5E:00:54:%02X", vmid%256), 1)
		}
		g := &pve{base: "/nodes/node-a/lxc/" + form["vmid"], cfg: cfg, options: map[string]any{}, sets: map[string][]string{}}
		n.mu.Lock()
		n.guests[vmid], n.pool[vmid], n.forms[vmid] = g, form["pool"], form
		n.acts = append(n.acts, "create "+form["vmid"])
		n.mu.Unlock()
		data(w, a.task("OK", ""))
	}
	a.any = func(w http.ResponseWriter, r *http.Request, p string) bool {
		rest, ok := strings.CutPrefix(p, "/nodes/node-a/lxc/")
		if !ok {
			return false
		}
		id, what, _ := strings.Cut(rest, "/")
		vmid, _ := strconv.Atoi(id)
		n.mu.Lock()
		g, here := n.guests[vmid]
		n.mu.Unlock()
		if !here {
			http.Error(w, `{"message":"Configuration file 'nodes/node-a/lxc/`+id+`.conf' does not exist"}`, http.StatusInternalServerError)
			return true
		}
		act := func(word string, on bool) bool {
			n.mu.Lock()
			if n.on[vmid] == on {
				n.mu.Unlock()
				data(w, a.task("CT "+id+" already "+map[bool]string{true: "running", false: "stopped"}[on], ""))
				return true
			}
			n.on[vmid] = on
			n.acts = append(n.acts, word+" "+id)
			n.mu.Unlock()
			data(w, a.task("OK", ""))
			return true
		}
		switch {
		case what == "" && r.Method == http.MethodDelete:
			n.mu.Lock()
			delete(n.guests, vmid)
			delete(n.on, vmid)
			n.acts = append(n.acts, "delete "+id)
			n.mu.Unlock()
			data(w, a.task("OK", ""))
			return true
		case what == "status/current":
			n.mu.Lock()
			st := map[bool]string{true: "running", false: "stopped"}[n.on[vmid]]
			n.mu.Unlock()
			data(w, map[string]any{"status": st, "mem": 20 << 20, "uptime": 7})
			return true
		case what == "status/start":
			return act("start", true)
		case what == "status/stop", what == "status/shutdown":
			return act("stop", false)
		}
		return g.serve(a, w, r, p)
	}
	return n
}

// did returns the acts since it was last asked, and forgets them.
func (n *node) did() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := n.acts
	n.acts = nil
	return out
}

func (n *node) guest(vmid int) *pve {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.guests[vmid]
}

const (
	jumpKeyA = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGXj2Dq6dbWAg2wXCN9pDWc3cS/cJEHWIr0sRd4b3M5V owner"
	jumpKeyB = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDeYZ0lS1d0FaLUkBZtD5OzcyHgvYtHhiRl8SLe1kcu8 laptop"
)

// A network is its gateway guest: made at the lowest free id of the
// gateways' own, with a card on the lane and a card on its network — both
// derived from that id —, the keys it lets jump, behind its wall, and born
// stopped.
func TestANetworkIsItsGateway(t *testing.T) {
	a := newAPI()
	n := newCluster(a)
	d := open(t, a, netZone)
	ctx := context.Background()
	spec := driver.NetworkSpec{ID: "net-a", Label: "lab · network of alice", JumpKeys: []string{jumpKeyA}}
	got, err := d.CreateNetwork(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	want := driver.Network{ID: "net-a", EngineRef: "node-a/lxc/11200", Node: "node-a", Label: "lab · network of alice",
		Range: "203.0.113.0/27", Gateway: "203.0.113.1", Jump: "jump@198.51.100.200", Wall: driver.WallExact, Keys: 1}
	if got != want {
		t.Fatalf("made as\n%+v, want\n%+v", got, want)
	}
	form := n.forms[11200]
	for k, v := range map[string]string{
		"ostemplate": "local:vztmpl/hangar-gateway-1.tar.zst", "pool": "hangar-nets", "unprivileged": "1", "onboot": "0",
		"memory": "128", "ssh-public-keys": jumpKeyA, "hostname": "gateway-0",
		"net0": "name=eth0,bridge=vmbr0,firewall=1,ip=198.51.100.200/24,gw=198.51.100.1",
		"net1": "name=eth1,bridge=hgnets,tag=100,firewall=1,ip=203.0.113.1/27",
	} {
		if form[k] != v {
			t.Errorf("created with %s=%q, want %q", k, form[k], v)
		}
	}
	if _, started := form["start"]; started || !slices.Equal(n.did(), []string{"create 11200"}) {
		t.Fatalf("a gateway is born stopped: it starts with its network's first guest")
	}
	g := n.guest(11200)
	if g.cfg["tags"] != idTag+".net-a" || !strings.HasPrefix(fmt.Sprint(g.cfg["description"]), "made by hangar: net-a\n") {
		t.Fatalf("its tag %v, its notes %q", g.cfg["tags"], g.cfg["description"])
	}
	// its wall: pinned to its one address on the lane; in from the lane what
	// the operator's group lets, in from its network what its network sends
	if lines := g.lines(); !slices.Equal(lines, []string{"GROUP floor", "IN ACCEPT -i net1 -source 203.0.113.0/27", "OUT DROP -p udp -sport 67"}) {
		t.Fatalf("its wall's lines: %q", lines)
	}
	if !slices.Equal(g.sets[ipfilterSet], []string{"198.51.100.200"}) || fmt.Sprintf("%v %v %v %v", g.options["enable"], g.options["policy_in"], g.options["ipfilter"], g.options["dhcp"]) != "1 DROP 0 0" {
		t.Fatalf("its wall pins it to %v, its options %v", g.sets[ipfilterSet], g.options)
	}
	// asked again — a create resumed — it is the same one, and nothing is written
	g.wrote()
	again, err := d.CreateNetwork(ctx, spec)
	if err != nil || again != want || len(g.wrote()) != 0 || len(n.did()) != 0 {
		t.Fatalf("a second create: %+v %v", again, err)
	}
	// the next network takes the next number, and everything follows from it
	b, err := d.CreateNetwork(ctx, driver.NetworkSpec{ID: "net-b"})
	if err != nil || b.EngineRef != "node-a/lxc/11201" || b.Range != "203.0.113.32/27" || b.Jump != "jump@198.51.100.201" || b.Keys != 0 {
		t.Fatalf("the second network: %+v %v", b, err)
	}
	if n.forms[11201]["net1"] != "name=eth1,bridge=hgnets,tag=101,firewall=1,ip=203.0.113.33/27" || n.forms[11201]["ssh-public-keys"] != "" {
		t.Fatalf("its gateway: %v", n.forms[11201])
	}
	if read, err := d.Network(ctx, "net-b"); err != nil || read != b {
		t.Fatalf("read back as %+v %v", read, err)
	}
	if _, err := d.Network(ctx, "net-none"); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("a network nobody made: %v", err)
	}
	// a zone that cuts none refuses, in words
	plain := open(t, newAPI(), walledZone)
	if _, err := plain.CreateNetwork(ctx, spec); !errors.Is(err, driver.ErrRefused) || !strings.Contains(err.Error(), "cuts no networks") {
		t.Fatalf("a zone with no net_bridge: %v", err)
	}
}

// born makes a container on a network (or on the lane) through the driver.
func born(t *testing.T, d *Driver, id, network string, stopped bool) driver.Guest {
	t.Helper()
	g, err := d.CreateGuest(context.Background(), driver.GuestSpec{ID: id, Kind: "container", Name: id, Cores: 1, MemoryMB: 256,
		Image: "local:vztmpl/debian.tar.zst", Network: network, Stopped: stopped, Tags: map[string]string{"class": "spot"}})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// A guest born on a network holds one card, there: its address its
// network's to give, its wall letting in what its own network sends and
// nothing else. And its network's gateway runs before it does.
func TestAGuestStandsOnItsNetwork(t *testing.T) {
	a := newAPI()
	n := newCluster(a)
	d := open(t, a, netZone)
	ctx := context.Background()
	for _, id := range []string{"net-a", "net-b"} {
		if _, err := d.CreateNetwork(ctx, driver.NetworkSpec{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	n.did()
	g := born(t, d, "m-one", "net-b", false)
	if form := n.forms[11000]; form["net0"] != "name=eth0,bridge=hgnets,tag=101,firewall=1,ip=203.0.113.42/27,gw=203.0.113.33" || form["nameserver"] != "198.51.100.1" || form["pool"] != "hangar" {
		t.Fatalf("born with %v", form)
	}
	if g.Address != "203.0.113.42" || g.Wall != driver.WallExact || !g.Running {
		t.Fatalf("born as %+v", g)
	}
	// its gateway first: never a guest running with no way out
	if acts := n.did(); !slices.Equal(acts, []string{"create 11000", "start 11201", "start 11000"}) {
		t.Fatalf("the order: %v", acts)
	}
	m := n.guest(11000)
	if lines := m.lines(); !slices.Equal(lines, []string{"IN ACCEPT -source 203.0.113.32/27", "OUT DROP -p udp -sport 67"}) {
		t.Fatalf("its wall's lines: %q — the operator's groups are the lane's, not a network's", lines)
	}
	if !slices.Equal(m.sets[ipfilterSet], []string{"203.0.113.42"}) {
		t.Fatalf("its wall pins it to %v", m.sets[ipfilterSet])
	}
	// a guest that names no network stands on the lane, as before
	lane := born(t, d, "m-lane", "", false)
	if lane.Address != "198.51.100.21" || !slices.Equal(n.guest(11001).lines(), []string{"GROUP floor", "OUT DROP -p udp -sport 67"}) {
		t.Fatalf("on the lane: %+v, %q", lane, n.guest(11001).lines())
	}
	if acts := n.did(); !slices.Equal(acts, []string{"create 11001", "start 11001"}) {
		t.Fatalf("a guest of the lane wakes no gateway: %v", acts)
	}
	// a network the engine does not have is refused before anything is made
	if _, err := d.CreateGuest(ctx, driver.GuestSpec{ID: "m-lost", Kind: "container", Cores: 1, MemoryMB: 256, Image: "x", Network: "net-none"}); !errors.Is(err, driver.ErrRefused) ||
		!strings.Contains(err.Error(), "no network net-none") || len(n.did()) != 0 {
		t.Fatalf("on a network nobody made: %v", err)
	}
}

// A gateway runs only while a guest of its network runs: started before the
// first, stopped after the last — and a look puts back what moved meanwhile.
func TestAGatewayRunsWhileAGuestOfItsNetworkDoes(t *testing.T) {
	a := newAPI()
	n := newCluster(a)
	d := open(t, a, netZone)
	ctx := context.Background()
	if _, err := d.CreateNetwork(ctx, driver.NetworkSpec{ID: "net-a"}); err != nil {
		t.Fatal(err)
	}
	born(t, d, "m-one", "net-a", false)
	born(t, d, "m-two", "net-a", false)
	born(t, d, "m-idle", "net-a", true)
	if acts := n.did(); !slices.Equal(acts, []string{"create 11200", "create 11000", "start 11200", "start 11000", "create 11001", "start 11001", "create 11002"}) {
		t.Fatalf("three guests born: %v", acts)
	}
	// one stops: another still runs, the gateway stays
	if _, err := d.SetPower(ctx, "m-one", false); err != nil {
		t.Fatal(err)
	}
	if acts := n.did(); !slices.Equal(acts, []string{"stop 11000"}) {
		t.Fatalf("one of two stopped: %v", acts)
	}
	// the last one stops: the gateway rests, after it
	if _, err := d.SetPower(ctx, "m-two", false); err != nil {
		t.Fatal(err)
	}
	if acts := n.did(); !slices.Equal(acts, []string{"stop 11001", "stop 11200"}) {
		t.Fatalf("the last one stopped: %v", acts)
	}
	if net, _ := d.Network(ctx, "net-a"); net.Running {
		t.Fatal("the network reads as up with no guest running")
	}
	// a start wakes it again, first
	if _, err := d.SetPower(ctx, "m-idle", true); err != nil {
		t.Fatal(err)
	}
	if acts := n.did(); !slices.Equal(acts, []string{"start 11200", "start 11002"}) {
		t.Fatalf("a start: %v", acts)
	}
	// while a guest of its network is starting, no stop puts it to rest
	done, err := d.wake(ctx, d.gateway(0))
	if err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	n.on[11002] = false // as if its last guest stopped at that very moment
	n.mu.Unlock()
	if stopped, err := d.rest(ctx, d.gateway(0), 0); err != nil || stopped {
		t.Fatalf("put to rest while a guest of its network starts: %v %v", stopped, err)
	}
	done()
	// the look that puts it back: a gateway left running with nothing behind it…
	if did, err := d.WayOut(ctx, "m-idle"); err != nil || did != "stopped" {
		t.Fatalf("a gateway with no guest running: %q %v", did, err)
	}
	// …and one stopped under a running guest (a hand, a node that acted alone)
	n.mu.Lock()
	n.on[11000] = true
	n.mu.Unlock()
	if did, err := d.WayOut(ctx, "m-one"); err != nil || did != "started" {
		t.Fatalf("a running guest with no gateway: %q %v", did, err)
	}
	if did, err := d.WayOut(ctx, "m-one"); err != nil || did != "" {
		t.Fatalf("a second look: %q %v", did, err)
	}
	if acts := n.did(); !slices.Equal(acts, []string{"stop 11200", "start 11200"}) {
		t.Fatalf("the looks: %v", acts)
	}
	// deleted, the last running guest takes the gateway to rest with it
	if err := d.DeleteGuest(ctx, "m-one"); err != nil {
		t.Fatal(err)
	}
	if acts := n.did(); !slices.Equal(acts, []string{"stop 11000", "delete 11000", "stop 11200"}) {
		t.Fatalf("its last running guest deleted: %v", acts)
	}
}

// A gateway keeps nothing and is never patched: born with other keys, or
// from another archive than its zone's, it is made again at its own id —
// and so is one that is gone, where its guests stand.
func TestAGatewayIsMadeAgainNeverPatched(t *testing.T) {
	a := newAPI()
	n := newCluster(a)
	d := open(t, a, netZone)
	ctx := context.Background()
	spec := driver.NetworkSpec{ID: "net-a", Label: "lab", JumpKeys: []string{jumpKeyA}}
	made, err := d.CreateNetwork(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	n.did()
	// as its spec says: a look writes nothing
	n.guest(11200).wrote()
	if _, fixed, err := d.TendNetwork(ctx, spec, made.EngineRef); err != nil || len(fixed) != 0 || len(n.guest(11200).wrote()) != 0 || len(n.did()) != 0 {
		t.Fatalf("a look at a gateway that stands: %v %v", fixed, err)
	}
	// its keys changed, a guest of its network running: made again at the
	// same id, with the new keys, behind its wall — and back at work
	born(t, d, "m-one", "net-a", false)
	n.did()
	spec.JumpKeys = []string{jumpKeyB, jumpKeyA}
	got, fixed, err := d.TendNetwork(ctx, spec, made.EngineRef)
	if err != nil || !slices.Equal(fixed, []string{"gateway (made again: its keys changed)", "wall (options, address, rules)", "name"}) {
		t.Fatalf("its keys changed: %v %v", fixed, err)
	}
	if acts := n.did(); !slices.Equal(acts, []string{"stop 11200", "delete 11200", "create 11200", "start 11200"}) {
		t.Fatalf("made again: %v", acts)
	}
	if got.EngineRef != made.EngineRef || got.Range != made.Range || got.Keys != 2 || !got.Running || got.Wall != driver.WallExact ||
		n.forms[11200]["ssh-public-keys"] != jumpKeyB+"\n"+jumpKeyA {
		t.Fatalf("after: %+v, born with %q", got, n.forms[11200]["ssh-public-keys"])
	}
	// the same keys in another order are the same keys
	spec.JumpKeys = []string{jumpKeyA, jumpKeyB}
	if _, fixed, err := d.TendNetwork(ctx, spec, made.EngineRef); err != nil || len(fixed) != 0 {
		t.Fatalf("the same keys, in another order: %v %v", fixed, err)
	}
	// the zone's archive moved: every gateway is made again from it
	moved := open(t, a, with(netZone, "net_archive", "local:vztmpl/hangar-gateway-2.tar.zst"))
	n.did()
	if _, fixed, err := moved.TendNetwork(ctx, spec, made.EngineRef); err != nil || len(fixed) == 0 ||
		fixed[0] != "gateway (made again: its archive is local:vztmpl/hangar-gateway-2.tar.zst now)" || n.forms[11200]["ostemplate"] != "local:vztmpl/hangar-gateway-2.tar.zst" {
		t.Fatalf("the archive moved: %v %v", fixed, err)
	}
	// gone — a hand, or a remake cut half-way: made again where it was
	n.mu.Lock()
	delete(n.guests, 11200)
	delete(n.on, 11200)
	n.mu.Unlock()
	n.did()
	back, fixed, err := moved.TendNetwork(ctx, spec, made.EngineRef)
	if err != nil || len(fixed) == 0 || fixed[0] != "gateway (it was gone: made again)" || back.EngineRef != made.EngineRef || !back.Running {
		t.Fatalf("gone: %+v %v %v", back, fixed, err)
	}
	// one never read has no place to be made again at: it is missing
	if _, _, err := moved.TendNetwork(ctx, driver.NetworkSpec{ID: "net-never"}, ""); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("a network never made: %v", err)
	}
}

// A network a guest stands on is not deleted; and while one stands on a
// number, no new network is given it — even when its gateway is gone.
func TestANetworkAGuestStandsOnIsNotDeleted(t *testing.T) {
	a := newAPI()
	n := newCluster(a)
	d := open(t, a, netZone)
	d.listLag = 0
	ctx := context.Background()
	if _, err := d.CreateNetwork(ctx, driver.NetworkSpec{ID: "net-a"}); err != nil {
		t.Fatal(err)
	}
	born(t, d, "m-one", "net-a", true)
	n.did()
	err := d.DeleteNetwork(ctx, "net-a")
	if !errors.Is(err, driver.ErrRefused) || !strings.Contains(err.Error(), "it still holds m-one: delete them first") || len(n.did()) != 0 {
		t.Fatalf("deleted under a guest: %v", err)
	}
	// its gateway gone (a remake cut half-way): its number is still its guest's
	n.mu.Lock()
	delete(n.guests, 11200)
	n.mu.Unlock()
	other, err := d.CreateNetwork(ctx, driver.NetworkSpec{ID: "net-b"})
	if err != nil || other.EngineRef != "node-a/lxc/11201" {
		t.Fatalf("a network made while number 0's gateway is gone took %s (%v): it would be on m-one's wire", other.EngineRef, err)
	}
	// its guest gone, a network goes — a running gateway stopped first
	if err := d.DeleteGuest(ctx, "m-one"); err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	n.on[11201] = true
	n.mu.Unlock()
	n.did()
	if err := d.DeleteNetwork(ctx, "net-b"); err != nil {
		t.Fatal(err)
	}
	if acts := n.did(); !slices.Equal(acts, []string{"stop 11201", "delete 11201"}) {
		t.Fatalf("a network deleted: %v", acts)
	}
	// one already gone is not an error
	if err := d.DeleteNetwork(ctx, "net-b"); err != nil {
		t.Fatalf("deleted twice: %v", err)
	}
	// every number taken: refused, in words
	for i := range 5 {
		if _, err = d.CreateNetwork(ctx, driver.NetworkSpec{ID: "net-" + strconv.Itoa(i)}); err != nil {
			break
		}
	}
	if !errors.Is(err, driver.ErrRefused) || !strings.Contains(err.Error(), "holds 4 networks") {
		t.Fatalf("a fifth network of four: %v", err)
	}
}

// The gateways' pool is the product's own: a token allowed there is fenced,
// and one that reaches a guest of another pool is not.
func TestTheFenceCountsTheGatewaysPool(t *testing.T) {
	a := newAPI()
	a.perms = map[string]map[string]int{
		"/pool/hangar":      {"VM.Audit": 1, "Pool.Audit": 1},
		"/pool/hangar-nets": {"VM.Allocate": 1, "VM.PowerMgmt": 1, "VM.Config.Network": 1},
	}
	if d := open(t, a, netZone); !has(d.Capabilities(), driver.FencePool) {
		t.Fatalf("the networks' own token: %s", d.FenceReport())
	}
	if d := open(t, a, walledZone); has(d.Capabilities(), driver.FencePool) || !strings.Contains(d.FenceReport(), "/pool/hangar-nets") {
		t.Fatalf("in a zone that cuts no networks, that pool is somebody else's: %q", d.FenceReport())
	}
}
