package proxmox

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tomblancdev/hangar/driver"
)

// The zone the wall's tests open: ten ids, ten addresses, a gateway, the
// operator's two groups.
var walledZone = map[string]string{
	"subnet": "198.51.100.0/24", "first_address": "198.51.100.20", "gateway": "198.51.100.1",
	"firewall": "on", "firewall_groups": "floor, never", "firewall_log": "info",
}

func TestAZonesNetwork(t *testing.T) {
	l, err := laneOf(walledZone, 11000, 11009)
	if err != nil {
		t.Fatal(err)
	}
	// derived from the guest's own number, never counted
	for vmid, want := range map[int]string{11000: "198.51.100.20/24", 11003: "198.51.100.23/24", 11009: "198.51.100.29/24"} {
		if got := l.prefix(vmid).String(); got != want {
			t.Errorf("guest %d is given %s, want %s", vmid, got, want)
		}
	}
	if !l.wall || !slices.Equal(l.groups, []string{"floor", "never"}) || l.log != "info" || l.resolverList() != "198.51.100.1" {
		t.Fatalf("read as %+v", l)
	}
	// an address carries into the next byte, as arithmetic does
	wide, err := laneOf(map[string]string{"subnet": "198.51.100.0/23", "first_address": "198.51.100.250"}, 100, 199)
	if err != nil || wide.address(110).String() != "198.51.101.4" || wide.gateway.IsValid() || wide.resolverList() != "" { // no-environment: ok
		t.Fatalf("a wide range: %v %s", err, wide.address(110))
	}
	two, err := laneOf(map[string]string{"subnet": "203.0.113.0/24", "first_address": "203.0.113.10", "gateway": "203.0.113.1", "resolvers": "203.0.113.2, 192.0.2.9"}, 100, 109)
	if err != nil || two.resolverList() != "203.0.113.2 192.0.2.9" {
		t.Fatalf("resolvers of its own: %v %q", err, two.resolverList())
	}
	plain, err := laneOf(map[string]string{}, 100, 199)
	if err != nil || plain.gives() || plain.wall {
		t.Fatalf("a zone that says nothing: %+v %v", plain, err)
	}
	for name, c := range map[string]struct {
		o    map[string]string
		want string
	}{
		"not a range":                   {map[string]string{"subnet": "203.0.113.5/24", "first_address": "203.0.113.10"}, "a range of IPv4 addresses"},
		"IPv6":                          {map[string]string{"subnet": "2001:db8::/64", "first_address": "2001:db8::10"}, "a range of IPv4 addresses"},
		"no first address":              {map[string]string{"subnet": "203.0.113.0/24"}, "first_address"},
		"more ids than addresses":       {map[string]string{"subnet": "203.0.113.0/24", "first_address": "203.0.113.200"}, "run to 203.0.114.43"}, // no-environment: ok
		"the last id on the broadcast":  {map[string]string{"subnet": "203.0.113.0/24", "first_address": "203.0.113.156"}, "run to 203.0.113.255"},
		"the first on the network's":    {map[string]string{"subnet": "203.0.113.0/24", "first_address": "203.0.113.0"}, "outside what"},
		"a gateway elsewhere":           {map[string]string{"subnet": "203.0.113.0/24", "first_address": "203.0.113.10", "gateway": "192.0.2.1"}, "an address of 203.0.113.0/24"},
		"a gateway a guest would be":    {map[string]string{"subnet": "203.0.113.0/24", "first_address": "203.0.113.10", "gateway": "203.0.113.50"}, "one of the addresses its guests are given"},
		"an address with no range":      {map[string]string{"first_address": "203.0.113.10"}, "said with subnet"},
		"a wall with no address":        {map[string]string{"firewall": "on"}, "needs subnet"},
		"groups with no wall":           {map[string]string{"firewall_groups": "floor"}, "said with firewall: on"},
		"no word for on":                {map[string]string{"subnet": "203.0.113.0/24", "first_address": "203.0.113.10", "firewall": "yes"}, "on, or off"},
		"a group that is a rule":        {map[string]string{"subnet": "203.0.113.0/24", "first_address": "203.0.113.10", "firewall": "on", "firewall_groups": "floor -j ACCEPT"}, "no security group's name"},
		"a level Proxmox does not know": {map[string]string{"subnet": "203.0.113.0/24", "first_address": "203.0.113.10", "firewall": "on", "firewall_log": "loud"}, "one of nolog"},
	} {
		if _, err := laneOf(c.o, 100, 199); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v (want %q)", name, err, c.want)
		}
	}
}

// A card, as Proxmox is handed it: the zone's bridge and tag, the wall's flag,
// a container's address — and the MAC it has, kept.
func TestAGuestsCard(t *testing.T) {
	walled := open(t, newAPI(), walledZone)
	tagged := open(t, newAPI(), map[string]string{"vlan": "30"})
	if has(tagged.Capabilities(), driver.NetFirewall) || !has(walled.Capabilities(), driver.NetFirewall) {
		t.Fatal("net.firewall is the flag of a zone that turned its wall on, and of no other")
	}
	for name, c := range map[string]struct {
		got, want string
	}{
		"a container, given its address": {walled.card(true, 11003, ""), "name=eth0,bridge=vmbr0,firewall=1,ip=198.51.100.23/24,gw=198.51.100.1"},
		"a VM at its birth":              {walled.card(false, 11003, ""), "virtio,bridge=vmbr0,firewall=1"},
		"a VM, its MAC kept":             {walled.card(false, 11003, "00:00:5E:00:53:FA"), "virtio=00:00:5E:00:53:FA,bridge=vmbr0,firewall=1"},
		"a container that asks a DHCP":   {tagged.card(true, 11003, ""), "name=eth0,bridge=vmbr0,tag=30,ip=dhcp"},
		"a VM that asks a DHCP":          {tagged.card(false, 11003, ""), "virtio,bridge=vmbr0,tag=30"},
	} {
		if c.got != c.want {
			t.Errorf("%s: %q, want %q", name, c.got, c.want)
		}
	}
	for line, want := range map[string]string{
		"virtio=00:00:5E:00:53:FA,bridge=hbnet,firewall=1":                                  "00:00:5E:00:53:FA",
		"name=eth0,bridge=hbnet,gw=198.51.100.1,hwaddr=00:00:5E:00:53:E4,ip=dhcp,type=veth": "00:00:5E:00:53:E4",
		"virtio,bridge=hbnet": "",
		"":                    "",
	} {
		if got := macOf(line); got != want {
			t.Errorf("macOf(%q) = %q, want %q", line, got, want)
		}
	}
	// the address a guest was told is read where it was written
	for name, c := range map[string]struct {
		typ  string
		cfg  map[string]any
		want string
	}{
		"a container's card":       {"lxc", map[string]any{"net0": "name=eth0,bridge=b,hwaddr=00:00:5E:00:53:E4,ip=198.51.100.23/24,type=veth"}, "198.51.100.23"},
		"a container with a lease": {"lxc", map[string]any{"net0": "name=eth0,bridge=b,ip=dhcp"}, ""},
		"a VM's ipconfig0":         {"qemu", map[string]any{"net0": "virtio=00:00:5E:00:53:FA,bridge=b", "ipconfig0": "ip=198.51.100.24/24,gw=198.51.100.1"}, "198.51.100.24"},
		"a VM with a lease":        {"qemu", map[string]any{"net0": "virtio=00:00:5E:00:53:FA,bridge=b"}, ""},
	} {
		a, ok := given(c.typ, c.cfg)
		if got := map[bool]string{true: a.String(), false: ""}[ok]; got != c.want {
			t.Errorf("%s: given %q, want %q", name, got, c.want)
		}
	}
}

// What a VM's first boot is told of its network, on its seed disc.
func TestTheSeedDiscTellsAVMItsAddress(t *testing.T) {
	n := &seedNet{mac: "00:00:5E:00:53:FA", address: netip.MustParsePrefix("198.51.100.23/24"),
		gateway: netip.MustParseAddr("198.51.100.1"), resolvers: []netip.Addr{netip.MustParseAddr("198.51.100.1"), netip.MustParseAddr("192.0.2.9")}}
	files := discFiles(t, nocloudSeed("m-1", "dev", nil, []byte("#cloud-config\n"), n, time.Unix(0, 0)))
	want := "version: 2\nethernets:\n  net0:\n    match: {macaddress: \"00:00:5e:00:53:fa\"}\n    addresses: [\"198.51.100.23/24\"]\n" +
		"    routes: [{to: \"0.0.0.0/0\", via: \"198.51.100.1\"}]\n    nameservers: {addresses: [\"198.51.100.1\", \"192.0.2.9\"]}\n"
	if got := string(files["network-config"]); got != want {
		t.Fatalf("network-config:\n%s\nwant:\n%s", got, want)
	}
	if string(files["user-data"]) != "#cloud-config\n" || !strings.Contains(string(files["meta-data"]), "instance-id: m-1") {
		t.Fatalf("the rest of the disc: %v", keys(files))
	}
	// a network with no way out says no route and no resolver
	sealed := string((seedNet{mac: "00:00:5E:00:53:FA", address: netip.MustParsePrefix("203.0.113.12/24")}).config())
	if strings.Contains(sealed, "routes") || strings.Contains(sealed, "nameservers") || !strings.Contains(sealed, `addresses: ["203.0.113.12/24"]`) {
		t.Fatalf("a sealed network's disc:\n%s", sealed)
	}
}

// pve is one guest's config and firewall as Proxmox VE keeps them, behaving
// as a bench read them: a rule made lands FIRST and is dead unless the call
// says enable=1; an option comes back a number where it is one; a set's
// member is deleted by its own text, slash and all.
type pve struct {
	mu      sync.Mutex
	base    string // /nodes/<node>/<type>/<vmid>
	cfg     map[string]any
	options map[string]any
	sets    map[string][]string
	rules   []map[string]any
	writes  []string // every write, in order
}

func (g *pve) mount(a *api) {
	a.any = func(w http.ResponseWriter, r *http.Request, p string) bool {
		rest, ok := strings.CutPrefix(p, g.base)
		if !ok {
			return false
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		_ = r.ParseForm()
		form := map[string]string{}
		for k, v := range r.PostForm {
			form[k] = v[0]
		}
		if r.Method != http.MethodGet {
			g.writes = append(g.writes, r.Method+" "+rest)
		}
		val := func(v string) any {
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
			return v
		}
		switch {
		case rest == "/config" && r.Method == http.MethodGet:
			data(w, g.cfg)
		case rest == "/config":
			for k, v := range form {
				if k == "delete" {
					for _, d := range strings.Split(v, ",") {
						delete(g.cfg, d)
					}
					continue
				}
				// a card written with no MAC is given one, as Proxmox draws it
				if k == "net0" && macOf(v) == "" && strings.HasPrefix(v, "virtio,") {
					v = "virtio=00:00:5E:00:53:01," + strings.TrimPrefix(v, "virtio,")
				}
				g.cfg[k] = v
			}
			if strings.Contains(g.base, "/qemu/") {
				data(w, a.task("OK", ""))
			} else {
				data(w, nil)
			}
		case rest == "/firewall/options" && r.Method == http.MethodGet:
			data(w, g.options)
		case rest == "/firewall/options":
			for k, v := range form {
				g.options[k] = val(v)
			}
			data(w, nil)
		case rest == "/firewall/ipset" && r.Method == http.MethodGet:
			out := []map[string]string{}
			for name := range g.sets {
				out = append(out, map[string]string{"name": name})
			}
			data(w, out)
		case rest == "/firewall/ipset":
			g.sets[form["name"]] = nil
			data(w, nil)
		case strings.HasPrefix(rest, "/firewall/ipset/"):
			name, member, _ := strings.Cut(strings.TrimPrefix(rest, "/firewall/ipset/"), "/")
			switch r.Method {
			case http.MethodGet:
				out := []map[string]string{}
				for _, c := range g.sets[name] {
					out = append(out, map[string]string{"cidr": c})
				}
				data(w, out)
			case http.MethodPost:
				g.sets[name] = append(g.sets[name], form["cidr"])
				data(w, nil)
			case http.MethodDelete:
				g.sets[name] = slices.DeleteFunc(g.sets[name], func(c string) bool { return c == member })
				data(w, nil)
			}
		case rest == "/firewall/rules" && r.Method == http.MethodGet:
			out := []map[string]any{}
			for i, rule := range g.rules {
				line := map[string]any{"pos": i}
				for k, v := range rule {
					line[k] = v
				}
				out = append(out, line)
			}
			data(w, out)
		case rest == "/firewall/rules":
			rule := map[string]any{"enable": 0}
			for k, v := range form {
				rule[k] = val(v)
			}
			g.rules = append([]map[string]any{rule}, g.rules...)
			data(w, nil)
		case strings.HasPrefix(rest, "/firewall/rules/") && r.Method == http.MethodDelete:
			pos, _ := strconv.Atoi(strings.TrimPrefix(rest, "/firewall/rules/"))
			g.rules = slices.Delete(g.rules, pos, pos+1)
			data(w, nil)
		default:
			return false
		}
		return true
	}
}

// lines are the guest's rules as its firewall file would list them.
func (g *pve) lines() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, r := range g.rules {
		line := strings.ToUpper(fmt.Sprint(r["type"])) + " " + fmt.Sprint(r["action"])
		if r["proto"] != nil {
			line += fmt.Sprintf(" -p %v -sport %v", r["proto"], r["sport"])
		}
		if fmt.Sprint(r["enable"]) != "1" {
			line = "|" + line // Proxmox's own mark of a dead line
		}
		out = append(out, line)
	}
	return out
}

func (g *pve) wrote() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := g.writes
	g.writes = nil
	return out
}

func guestOf(typ string, vmid int, cfg map[string]any) (*pve, resource) {
	r := resource{VMID: vmid, Node: "node-a", Type: typ, Pool: "hangar", Tags: "hangar-id.m-wall"}
	return &pve{base: r.path(), cfg: cfg, options: map[string]any{}, sets: map[string][]string{}}, r
}

// The wall as it is written on a guest that has none — a clone's, with its
// template's own file — and what a second look finds to do: nothing.
func TestAWallIsWrittenWhole(t *testing.T) {
	a := newAPI()
	g, r := guestOf("qemu", 11004, map[string]any{"net0": "virtio=00:00:5E:00:53:FA,bridge=vmbr0", "ipconfig0": "ip=198.51.100.24/24,gw=198.51.100.1"})
	// what a clone was born with: its template's wall, pinned to the
	// template's builder, and a door somebody opened there
	g.options = map[string]any{"enable": 1, "policy_in": "ACCEPT"}
	g.sets[ipfilterSet] = []string{"198.51.100.29"}
	g.rules = []map[string]any{{"type": "in", "action": "ACCEPT", "proto": "icmp", "enable": 1}}
	g.mount(a)
	a.res = []resource{r}
	a.h["GET "+r.path()+"/status/current"] = func(w http.ResponseWriter, _ *http.Request) { data(w, map[string]any{"status": "stopped"}) }
	d := open(t, a, walledZone)
	ctx := context.Background()

	fixed, err := d.Wall(ctx, "m-wall")
	if err != nil || !slices.Equal(fixed, []string{"options", "address", "rules", "card"}) {
		t.Fatalf("put back %v, %v", fixed, err)
	}
	want := map[string]any{"enable": 1, "policy_in": "DROP", "policy_out": "ACCEPT", "macfilter": 1, "ipfilter": 0, "dhcp": 0, "log_level_in": "info"}
	for k, v := range want {
		if fmt.Sprint(g.options[k]) != fmt.Sprint(v) {
			t.Errorf("option %s is %v, want %v", k, g.options[k], v)
		}
	}
	if !slices.Equal(g.sets[ipfilterSet], []string{"198.51.100.24"}) {
		t.Errorf("it may send as %v, want its own address alone", g.sets[ipfilterSet])
	}
	// the operator's groups first, in the order the zone names them; the
	// template's door is gone; every line is live
	if got := g.lines(); !slices.Equal(got, []string{"GROUP floor", "GROUP never", "OUT DROP -p udp -sport 67"}) {
		t.Errorf("its rules: %q", got)
	}
	if got := g.cfg["net0"]; got != "virtio=00:00:5E:00:53:FA,bridge=vmbr0,firewall=1" {
		t.Errorf("its card: %q", got)
	}
	// the card's flag is the last thing written: a running guest is never
	// plugged behind a wall half made
	if w := g.wrote(); w[len(w)-1] != "POST /config" || slices.Index(w, "POST /config") != len(w)-1 {
		t.Errorf("the card was not written last: %v", w)
	}
	if fixed, err = d.Wall(ctx, "m-wall"); err != nil || len(fixed) != 0 || len(g.wrote()) != 0 {
		t.Fatalf("a wall that stands was written again: %v %v", fixed, err)
	}
	if got, err := d.Guest(ctx, "m-wall"); err != nil || got.Wall != driver.WallExact || got.Address != "198.51.100.24" {
		t.Fatalf("it reads as wall %q, address %q (%v)", got.Wall, got.Address, err)
	}
}

// What a hand does to a wall behind the brain's back, one thing at a time,
// and what the next look names and puts back.
func TestAWallIsPutBack(t *testing.T) {
	a := newAPI()
	g, r := guestOf("lxc", 11003, map[string]any{"net0": "name=eth0,bridge=vmbr0,firewall=1,gw=198.51.100.1,hwaddr=00:00:5E:00:53:E4,ip=198.51.100.23/24,type=veth"})
	g.mount(a)
	a.res = []resource{r}
	a.h["GET "+r.path()+"/status/current"] = func(w http.ResponseWriter, _ *http.Request) { data(w, map[string]any{"status": "stopped"}) }
	d := open(t, a, walledZone)
	ctx := context.Background()
	if _, err := d.Wall(ctx, "m-wall"); err != nil {
		t.Fatal(err)
	}
	stands := func() []string { return g.lines() }
	whole := stands()
	for name, c := range map[string]struct {
		breakIt func()
		want    []string
	}{
		"the firewall turned off":        {func() { g.options["enable"] = 0 }, []string{"options"}},
		"everything let in":              {func() { g.options["policy_in"] = "ACCEPT" }, []string{"options"}},
		"DHCP let through":               {func() { delete(g.options, "dhcp") }, []string{"options"}},
		"another address beside its own": {func() { g.sets[ipfilterSet] = append(g.sets[ipfilterSet], "198.51.100.0/24") }, []string{"address"}},
		"its own address taken out":      {func() { g.sets[ipfilterSet] = nil }, []string{"address"}},
		"the set itself deleted":         {func() { delete(g.sets, ipfilterSet) }, []string{"address"}},
		"a door opened first": {func() {
			g.rules = append([]map[string]any{{"type": "in", "action": "ACCEPT", "enable": 1}}, g.rules...)
		}, []string{"rules"}},
		"a door opened last":           {func() { g.rules = append(g.rules, map[string]any{"type": "in", "action": "ACCEPT", "enable": 1}) }, []string{"rules"}},
		"the operator's group dropped": {func() { g.rules = g.rules[1:] }, []string{"rules"}},
		"the groups the other way":     {func() { g.rules[0], g.rules[1] = g.rules[1], g.rules[0] }, []string{"rules"}},
		"a group's line made dead":     {func() { g.rules[0]["enable"] = 0 }, []string{"rules"}},
		"the card's flag taken off": {func() {
			g.cfg["net0"] = "name=eth0,bridge=vmbr0,gw=198.51.100.1,hwaddr=00:00:5E:00:53:E4,ip=198.51.100.23/24,type=veth"
		}, []string{"card"}},
	} {
		g.mu.Lock()
		c.breakIt()
		g.mu.Unlock()
		if got, err := d.Guest(ctx, "m-wall"); err != nil {
			t.Fatal(err)
		} else if open := name == "the firewall turned off" || name == "the card's flag taken off"; open != (got.Wall == "") {
			t.Errorf("%s: it reads as wall %q", name, got.Wall)
		}
		fixed, err := d.Wall(ctx, "m-wall")
		if err != nil || !slices.Equal(fixed, c.want) {
			t.Errorf("%s: put back %v, %v — want %v", name, fixed, err, c.want)
		}
		if !slices.Equal(stands(), whole) || !slices.Equal(g.sets[ipfilterSet], []string{"198.51.100.23"}) ||
			fmt.Sprintf("%v %v %v", g.options["enable"], g.options["policy_in"], g.options["dhcp"]) != "1 DROP 0" ||
			cardOpt(fmt.Sprint(g.cfg["net0"]), "firewall") != "1" {
			t.Errorf("%s: the wall after: %q, set %v, options %v, card %v", name, stands(), g.sets, g.options, g.cfg["net0"])
		}
		// the rest of the card is kept as it was: its MAC, its address
		if card := fmt.Sprint(g.cfg["net0"]); macOf(card) != "00:00:5E:00:53:E4" || cardOpt(card, "ip") != "198.51.100.23/24" {
			t.Errorf("%s: the card lost something: %q", name, card)
		}
		g.wrote()
	}
}

// A guest born before its zone gave addresses asks for a lease: its wall lets
// DHCP through and pins it to the zone's subnet — and says so.
func TestAGuestWithALeaseIsPinnedToItsZonesRange(t *testing.T) {
	a := newAPI()
	g, r := guestOf("lxc", 11002, map[string]any{"net0": "name=eth0,bridge=vmbr0,hwaddr=00:00:5E:00:53:E4,ip=dhcp,type=veth"})
	g.mount(a)
	a.res = []resource{r}
	a.h["GET "+r.path()+"/status/current"] = func(w http.ResponseWriter, _ *http.Request) { data(w, map[string]any{"status": "running"}) }
	a.h["GET "+r.path()+"/interfaces"] = func(w http.ResponseWriter, _ *http.Request) { data(w, []any{}) }
	d := open(t, a, walledZone)
	fixed, err := d.Wall(context.Background(), "m-wall")
	if err != nil || !slices.Equal(fixed, []string{"options", "address", "rules", "card"}) {
		t.Fatalf("put back %v, %v", fixed, err)
	}
	if !slices.Equal(g.sets[ipfilterSet], []string{"198.51.100.0/24"}) || fmt.Sprint(g.options["dhcp"]) != "1" {
		t.Fatalf("pinned to %v, dhcp %v", g.sets[ipfilterSet], g.options["dhcp"])
	}
	// its card keeps asking: an address is given at a birth, never under a
	// running guest
	if card := fmt.Sprint(g.cfg["net0"]); cardOpt(card, "ip") != "dhcp" || cardOpt(card, "firewall") != "1" {
		t.Fatalf("its card: %q", card)
	}
	if got, err := d.Guest(context.Background(), "m-wall"); err != nil || got.Wall != driver.WallRange || got.Address != "" {
		t.Fatalf("it reads as wall %q, address %q (%v)", got.Wall, got.Address, err)
	}
}

// A zone that keeps no wall writes none, and asks Proxmox nothing about one.
func TestAZoneWithNoWallWritesNone(t *testing.T) {
	a := newAPI()
	g, r := guestOf("lxc", 11003, map[string]any{"net0": "name=eth0,bridge=vmbr0,ip=dhcp"})
	g.mount(a)
	a.res = []resource{r}
	d := open(t, a, nil)
	if fixed, err := d.Wall(context.Background(), "m-wall"); err != nil || fixed != nil {
		t.Fatalf("%v %v", fixed, err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, c := range a.calls {
		if strings.Contains(c, "firewall") {
			t.Fatalf("asked %s", c)
		}
	}
}

// A wall just written is not on the wire yet: Proxmox applies it on its own
// pass. The start that follows waits for that pass; one whose wall stood
// already does not.
func TestAStartWaitsForAWallJustWritten(t *testing.T) {
	a := newAPI()
	g, r := guestOf("lxc", 11003, map[string]any{"net0": "name=eth0,bridge=vmbr0,hwaddr=00:00:5E:00:53:E4,ip=198.51.100.23/24,type=veth"})
	g.mount(a)
	a.res = []resource{r}
	var started time.Time
	a.h["GET "+r.path()+"/status/current"] = func(w http.ResponseWriter, _ *http.Request) { data(w, map[string]any{"status": "stopped"}) }
	a.h["GET "+r.path()+"/interfaces"] = func(w http.ResponseWriter, _ *http.Request) { data(w, []any{}) }
	a.h["POST "+r.path()+"/status/start"] = func(w http.ResponseWriter, _ *http.Request) {
		started = time.Now()
		data(w, a.task("OK", ""))
	}
	d := open(t, a, walledZone)
	d.wallWait = 300 * time.Millisecond
	ctx := context.Background()
	if fixed, err := d.Wall(ctx, "m-wall"); err != nil || len(fixed) == 0 {
		t.Fatalf("%v %v", fixed, err)
	}
	written := time.Now()
	if _, err := d.SetPower(ctx, "m-wall", true); err != nil {
		t.Fatal(err)
	}
	if waited := started.Sub(written); waited < 250*time.Millisecond {
		t.Fatalf("started %s after its wall was written: before Proxmox had it on the wire", waited)
	}
	// the control: the wall stands, nothing is written, the start is at once
	if fixed, err := d.Wall(ctx, "m-wall"); err != nil || len(fixed) != 0 {
		t.Fatalf("%v %v", fixed, err)
	}
	asked := time.Now()
	if _, err := d.SetPower(ctx, "m-wall", true); err != nil {
		t.Fatal(err)
	}
	if waited := started.Sub(asked); waited > 200*time.Millisecond {
		t.Fatalf("a start behind a wall that stood waited %s", waited)
	}
	// and a wait is given up with its request
	if _, err := d.wallUp(ctx, r); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	g.options["enable"] = 0
	g.mu.Unlock()
	if _, err := d.Wall(ctx, "m-wall"); err != nil {
		t.Fatal(err)
	}
	d.wallWait = time.Hour
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := d.SetPower(short, "m-wall", true); err == nil {
		t.Fatal("a start whose request was given up went on waiting, or started")
	}
}

// A VM's network, written again from what it is: the MAC its card has is
// kept (its seed disc names it), one that has none is given Proxmox's and
// told it; where the zone gives no address, a template's own is taken off.
func TestAVMsNetworkKeepsItsMAC(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		zone                      map[string]string
		cfg                       map[string]any
		card, ipconfig, del, told string
	}{
		"a clone, its zone gives addresses": {walledZone,
			map[string]any{"net0": "virtio=00:00:5E:00:53:FA,bridge=elsewhere,firewall=0", "ipconfig0": "ip=198.51.100.99/24"},
			"virtio=00:00:5E:00:53:FA,bridge=vmbr0,firewall=1", "ip=198.51.100.24/24,gw=198.51.100.1", "", "00:00:5E:00:53:FA"},
		"a template with no card": {walledZone, map[string]any{},
			"virtio=00:00:5E:00:53:01,bridge=vmbr0,firewall=1", "ip=198.51.100.24/24,gw=198.51.100.1", "", "00:00:5E:00:53:01"},
		"a clone, its zone gives none": {nil,
			map[string]any{"net0": "virtio=00:00:5E:00:53:FA,bridge=elsewhere,firewall=1", "ipconfig0": "ip=198.51.100.99/24"},
			"virtio=00:00:5E:00:53:FA,bridge=vmbr0", "", "ipconfig0", ""},
		"a zone that gives none, nothing to take off": {nil, map[string]any{"net0": "virtio=00:00:5E:00:53:FA,bridge=vmbr0"},
			"virtio=00:00:5E:00:53:FA,bridge=vmbr0", "", "", ""},
	} {
		a := newAPI()
		g, r := guestOf("qemu", 11004, c.cfg)
		g.mount(a)
		d := open(t, a, c.zone)
		p, sn, err := d.vmNet(ctx, r)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if p.Get("net0") != c.card || p.Get("ipconfig0") != c.ipconfig || p.Get("delete") != c.del {
			t.Errorf("%s: writes net0 %q, ipconfig0 %q, delete %q", name, p.Get("net0"), p.Get("ipconfig0"), p.Get("delete"))
		}
		switch {
		case c.told == "" && sn != nil:
			t.Errorf("%s: its disc is told a network: %+v", name, sn)
		case c.told != "" && (sn == nil || sn.mac != c.told || sn.address.String() != "198.51.100.24/24" || sn.gateway.String() != "198.51.100.1"):
			t.Errorf("%s: its disc is told %+v", name, sn)
		}
	}
}
