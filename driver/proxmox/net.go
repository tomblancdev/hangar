package proxmox

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/tomblancdev/hangar/driver"
)

// The card and the wall.
//
// A zone that says `subnet` GIVES each guest its address at its birth: no
// guest asks a DHCP. The address is derived, never counted — a guest's VMID
// is the one number Proxmox hands out with no race, so
//
//	address = first_address + (its VMID − the first of vmids)
//
// and two creates at the same moment cannot be given the same one. Nothing is
// allocated, nothing is kept: an address in a log gives the guest back by
// arithmetic. A container's card carries it (ip=, gw=: Proxmox writes it
// inside at every start); a VM reads it at its first boot on its seed disc
// (network-config, matched by the card's MAC — seed.go), and `ipconfig0`
// keeps it on the guest's own config: the line Proxmox uses for a cloud-init
// drive of its making, which these VMs do not have, so it is a record here —
// what the driver reads to know which address a VM was told.
//
// A zone that says `firewall: on` puts every guest behind Proxmox's firewall
// BEFORE ITS FIRST START, alone: nothing comes in but what the operator's own
// security groups let (firewall_groups, worn first), and it sends only as
// itself — its card's MAC, and the one address it was given (Proxmox turns
// that into an ARP filter too: it cannot answer for a neighbour, nor for the
// gateway). The cluster's firewall must be on for any of it to be enforced:
// that, and the groups, are the operator's (they take Sys.Modify on /, which
// no token of the product's holds).
//
// The driver owns the guest's whole firewall file, and writes it through the
// API (Proxmox checks each rule as it is made): its options, the set
// `ipfilter-net0`, and its rules — the operator's groups, then one line that
// refuses a guest answering as a DHCP server. A rule that is not one of
// those is taken out: a clone is born with its template's file (read on a
// bench — a template's `IN ACCEPT` came with it), and a hand that opened a
// door behind the brain's back is the drift a wall is repaired from.
//
// A WALL JUST WRITTEN IS NOT YET ON THE WIRE. Proxmox's firewall daemon
// applies a guest's file on its own pass, every ten seconds, and a start does
// not ask it to (pve_firewall.pm; read on a bench: a guest started right
// after its wall was written answered its neighbour's ping — and went on
// answering it once the rules were in, an open connection being let through
// from then on). So a guest whose wall was written is started only once that
// pass has come (wallSettle): the price of « never a first packet without a
// wall » is that wait, at a birth and at no other start.
//
// A guest born before its zone gave addresses still asks for a lease: its
// wall lets DHCP through and pins it to the zone's subnet, not to an address
// (driver.WallRange) — it cannot pose as anything outside its lane, and can
// still take a neighbour's address. It is exact from its next birth.

// lane is what a zone says of its guests' network.
type lane struct {
	subnet    netip.Prefix // not valid: its guests ask a DHCP
	first     netip.Addr   // the address of the first id of vmids
	lo        int          // the first id of vmids
	gateway   netip.Addr   // not valid: no way out is written
	resolvers []netip.Addr
	wall      bool
	groups    []string // the operator's security groups, worn first
	log       string   // the level a refusal is logged at on the node
}

// groupName: a Proxmox security group's name.
var groupName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{1,17}$`)

// logLevels are the levels Proxmox logs a refusal at.
var logLevels = []string{"nolog", "emerg", "alert", "crit", "err", "warning", "notice", "info", "debug"}

// wallSettle is how long a wall just written is given before its guest may
// start: the daemon's ten seconds, the second it sleeps by, and its own pass.
const wallSettle = 15 * time.Second

// ipfilterSet is the set Proxmox reads a card's own addresses from: what
// net0 may send as, and answer ARP for.
const ipfilterSet = "ipfilter-net0"

// laneOf reads a zone's network options.
func laneOf(o map[string]string, lo, hi int) (lane, error) {
	l := lane{lo: lo, log: "nolog"}
	if o["subnet"] == "" {
		for _, k := range []string{"first_address", "gateway", "resolvers"} {
			if o[k] != "" {
				return l, fmt.Errorf("%s is said with subnet: the range its guests are given their address in", k)
			}
		}
	} else {
		p, err := netip.ParsePrefix(o["subnet"])
		if err != nil || !p.Addr().Is4() || p.Masked() != p || p.Bits() > 30 {
			return l, fmt.Errorf("subnet %q: a range of IPv4 addresses, such as 192.0.2.0/24", o["subnet"])
		}
		l.subnet = p
		if l.first, err = netip.ParseAddr(o["first_address"]); err != nil || !l.first.Is4() {
			return l, fmt.Errorf("first_address %q: the address the first id of vmids is given (id %d), in %s", o["first_address"], lo, p)
		}
		last := l.address(hi)
		if !l.host(l.first) || !l.host(last) || last.Less(l.first) {
			return l, fmt.Errorf("first_address %s: vmids holds %d ids, so their addresses run to %s — outside what %s gives its guests",
				l.first, hi-lo+1, last, p)
		}
		if v := o["gateway"]; v != "" {
			if l.gateway, err = netip.ParseAddr(v); err != nil || !l.host(l.gateway) {
				return l, fmt.Errorf("gateway %q: an address of %s", v, p)
			}
			if !l.gateway.Less(l.first) && !last.Less(l.gateway) {
				return l, fmt.Errorf("gateway %s is one of the addresses its guests are given (%s to %s)", l.gateway, l.first, last)
			}
			l.resolvers = []netip.Addr{l.gateway}
		}
		if v := o["resolvers"]; v != "" {
			l.resolvers = nil
			for _, s := range strings.Split(v, ",") {
				a, err := netip.ParseAddr(strings.TrimSpace(s))
				if err != nil {
					return l, fmt.Errorf("resolvers %q: addresses, comma-separated", v)
				}
				l.resolvers = append(l.resolvers, a)
			}
		}
	}
	// on, or true: what a YAML reader makes of an unquoted `on`
	switch o["firewall"] {
	case "", "off", "false":
		for _, k := range []string{"firewall_groups", "firewall_log"} {
			if o[k] != "" {
				return l, fmt.Errorf("%s is said with firewall: on", k)
			}
		}
		return l, nil
	case "on", "true":
	default:
		return l, fmt.Errorf("firewall %q: on, or off", o["firewall"])
	}
	if !l.gives() {
		return l, fmt.Errorf("firewall: on needs subnet: a wall pins a guest to the address it was given")
	}
	l.wall = true
	for _, g := range strings.Split(o["firewall_groups"], ",") {
		if g = strings.TrimSpace(g); g == "" {
			continue
		}
		if !groupName.MatchString(g) {
			return l, fmt.Errorf("firewall_groups: %q is no security group's name", g)
		}
		l.groups = append(l.groups, g)
	}
	if v := o["firewall_log"]; v != "" {
		if !slices.Contains(logLevels, v) {
			return l, fmt.Errorf("firewall_log %q: one of %s", v, strings.Join(logLevels, ", "))
		}
		l.log = v
	}
	return l, nil
}

// gives: the zone gives its guests their address.
func (l lane) gives() bool { return l.subnet.IsValid() }

// host: a is an address a guest of the subnet may hold — in it, and neither
// its first nor its last (the network's own, and its broadcast).
func (l lane) host(a netip.Addr) bool {
	if !a.Is4() || !l.subnet.Contains(a) || a == l.subnet.Addr() {
		return false
	}
	return l.subnet.Contains(a.Next()) // the last address has no next in the range
}

// address is the address a guest is given: derived from its own VMID.
func (l lane) address(vmid int) netip.Addr { return addrPlus(l.first, vmid-l.lo) }

// addrPlus is the address n after a, as arithmetic has it.
func addrPlus(a netip.Addr, n int) netip.Addr {
	b := a.As4()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v += uint32(n)
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// prefix is a guest's address as its card says it: with the subnet's length.
func (l lane) prefix(vmid int) netip.Prefix {
	return netip.PrefixFrom(l.address(vmid), l.subnet.Bits())
}

// cardOpt reads one option of a card's line (net0), wherever it stands.
func cardOpt(line, name string) string {
	for _, part := range strings.Split(line, ",") {
		if k, v, ok := strings.Cut(part, "="); ok && k == name {
			return v
		}
	}
	return ""
}

var macForm = regexp.MustCompile(`^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$`)

// macOf reads a card's MAC: a container's hwaddr, a VM's model=MAC.
func macOf(line string) string {
	if m := cardOpt(line, "hwaddr"); m != "" {
		return m
	}
	_, m, _ := strings.Cut(strings.SplitN(line, ",", 2)[0], "=")
	if macForm.MatchString(m) {
		return m
	}
	return ""
}

// place is where a guest's one card stands: the zone's lane, or a network of
// the cloud's own (networks.go) — the bridge and tag it is plugged on, the
// address it is given there, its way out.
type place struct {
	bridge    string
	tag       int          // 0: none
	addr      netip.Prefix // not valid: it asks a DHCP
	gateway   netip.Addr   // not valid: no way out is written
	resolvers []netip.Addr
}

// onLane is a guest's place on the zone's own lane.
func (d *Driver) onLane(vmid int) place {
	pl := place{bridge: d.bridge, tag: d.vlan}
	if d.lane.gives() {
		pl.addr, pl.gateway, pl.resolvers = d.lane.prefix(vmid), d.lane.gateway, d.lane.resolvers
	}
	return pl
}

// card is a guest's network card where it stands. mac: the one it has
// already ("" lets Proxmox draw one, by its own prefix) — a card written
// again keeps it, or the VM's first boot would not know its own disc.
func (d *Driver) card(container bool, pl place, mac string) string {
	var b strings.Builder
	switch {
	case container:
		b.WriteString("name=eth0")
		if mac != "" {
			b.WriteString(",hwaddr=" + mac)
		}
	case mac != "":
		b.WriteString("virtio=" + mac)
	default:
		b.WriteString("virtio")
	}
	b.WriteString(",bridge=" + pl.bridge)
	if pl.tag > 0 {
		fmt.Fprintf(&b, ",tag=%d", pl.tag)
	}
	if d.lane.wall {
		b.WriteString(",firewall=1")
	}
	if container {
		if !pl.addr.IsValid() {
			b.WriteString(",ip=dhcp")
		} else {
			b.WriteString(",ip=" + pl.addr.String())
			if pl.gateway.IsValid() {
				b.WriteString(",gw=" + pl.gateway.String())
			}
		}
	}
	return b.String()
}

// resolverList is the zone's resolvers as Proxmox takes a container's.
func (l lane) resolverList() string { return resolverList(l.resolvers) }

func resolverList(rs []netip.Addr) string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.String()
	}
	return strings.Join(out, " ")
}

// vmNet is a VM's network, written again from what it is now: its card (the
// MAC it has kept; none yet, Proxmox draws one and it is read back), the
// address it is given where it stands as `ipconfig0`, and what its seed disc
// says of both (nil: it asks a DHCP).
func (d *Driver) vmNet(ctx context.Context, r resource, pl place) (url.Values, *seedNet, error) {
	var cfg map[string]any
	if err := d.c.call(ctx, http.MethodGet, r.path()+"/config", nil, &cfg); err != nil {
		return nil, nil, err
	}
	mac := macOf(str(cfg["net0"]))
	if mac == "" {
		if err := d.c.run(ctx, http.MethodPost, r.path()+"/config", url.Values{"net0": {d.card(false, pl, "")}}); err != nil {
			return nil, nil, err
		}
		if err := d.c.call(ctx, http.MethodGet, r.path()+"/config", nil, &cfg); err != nil {
			return nil, nil, err
		}
		if mac = macOf(str(cfg["net0"])); mac == "" {
			return nil, nil, fmt.Errorf("%w: its card says no MAC: %q", driver.ErrRefused, str(cfg["net0"]))
		}
	}
	p := url.Values{"net0": {d.card(false, pl, mac)}}
	if !pl.addr.IsValid() {
		if str(cfg["ipconfig0"]) != "" { // a clone begins with its template's
			p.Set("delete", "ipconfig0")
		}
		return p, nil, nil
	}
	sn := &seedNet{mac: mac, address: pl.addr, gateway: pl.gateway, resolvers: pl.resolvers}
	line := "ip=" + sn.address.String()
	if sn.gateway.IsValid() {
		line += ",gw=" + sn.gateway.String()
	}
	p.Set("ipconfig0", line)
	return p, sn, nil
}

// given reads the address a guest was told at its birth: a container's on
// its card, a VM's on ipconfig0. None: it asks a DHCP.
func given(typ string, cfg map[string]any) (netip.Addr, bool) {
	v := cardOpt(str(cfg["net0"]), "ip")
	if typ == "qemu" {
		v = cardOpt(str(cfg["ipconfig0"]), "ip")
	}
	p, err := netip.ParsePrefix(v)
	if err != nil {
		return netip.Addr{}, false
	}
	return p.Addr(), true
}

// ---- The wall ---------------------------------------------------------------

// wallRule is one line of a guest's firewall.
type wallRule struct {
	Type, Action, Proto, Sport, Dport, Source, Dest, Iface, Macro, Comment string
	Enable, Pos                                                            int
}

// ruleOf reads a line as the API lists it: a port may come back a number.
func ruleOf(m map[string]any) wallRule {
	return wallRule{
		Type: str(m["type"]), Action: str(m["action"]), Proto: str(m["proto"]), Sport: str(m["sport"]), Dport: str(m["dport"]),
		Source: str(m["source"]), Dest: str(m["dest"]), Iface: str(m["iface"]), Macro: str(m["macro"]), Comment: str(m["comment"]),
		Enable: num(m["enable"]), Pos: num(m["pos"]),
	}
}

// same: both lines let, or refuse, the same thing. Their comments are for
// people.
func (a wallRule) same(b wallRule) bool {
	return a.Type == b.Type && a.Action == b.Action && a.Proto == b.Proto && a.Sport == b.Sport && a.Dport == b.Dport &&
		a.Source == b.Source && a.Dest == b.Dest && a.Iface == b.Iface && a.Macro == b.Macro && a.Enable == b.Enable
}

// rules are the lines every guest of the zone wears, in order: the
// operator's groups first — what every machine must hear, and what none may —
// then no guest answers as a DHCP server, whatever else it may send.
func (l lane) rules() []wallRule {
	var out []wallRule
	for _, g := range l.groups {
		out = append(out, wallRule{Type: "group", Action: g, Enable: 1})
	}
	return append(out, noDHCPServer)
}

// wallOf says what a guest's wall pins it to: the address it was given, or —
// a guest that asks for a lease — its zone's subnet.
func (l lane) wallOf(typ string, cfg map[string]any) (pin, word string, leased bool) {
	if a, ok := given(typ, cfg); ok {
		return a.String(), driver.WallExact, false
	}
	return l.subnet.String(), driver.WallRange, true
}

// Wall brings a guest's wall to what its zone says, and returns what it had
// to put back (none: it stood). A zone that keeps no wall has nothing to put
// back.
func (d *Driver) Wall(ctx context.Context, id string) ([]string, error) {
	if !d.lane.wall {
		return nil, nil
	}
	r, err := d.find(ctx, id)
	if err != nil {
		return nil, d.engine(err)
	}
	fixed, err := d.wallUp(ctx, r)
	return fixed, d.engine(err)
}

// walling is a wall as it is to be written: what the first card may send as,
// the lines it wears, and the cards that stand behind it.
type walling struct {
	pin    string
	leased bool // it asks for a lease: DHCP passes
	rules  []wallRule
	cards  []string // net0 — and a gateway's net1
}

// wallUp writes a guest's wall — a machine's, a builder's: one card, where
// it stands. On the lane: the operator's groups. On a network of the cloud's
// own: whatever its own network sends, and nothing else — nobody but its
// network is on that wire, and its gateway lets nothing in.
func (d *Driver) wallUp(ctx context.Context, r resource) ([]string, error) {
	if !d.lane.wall {
		return nil, nil
	}
	var cfg map[string]any
	if err := d.c.call(ctx, http.MethodGet, r.path()+"/config", nil, &cfg); err != nil {
		return nil, err
	}
	card := str(cfg["net0"])
	if card == "" {
		return nil, nil // no card: nothing of it is on any wire
	}
	pin, _, leased := d.lane.wallOf(r.Type, cfg)
	w := walling{pin: pin, leased: leased, rules: d.lane.rules(), cards: []string{"net0"}}
	if n, on := d.nets.of(card); on {
		w.rules = d.nets.memberRules(n)
	}
	return d.wallWrite(ctx, r, cfg, w)
}

// wallWrite writes a wall, each piece only where it differs: its options,
// the address it may send as, its rules — and the cards' own flag LAST, so
// that a running guest walled for the first time is never behind a wall half
// written. Idempotent: a create's retry, and every look after.
func (d *Driver) wallWrite(ctx context.Context, r resource, cfg map[string]any, w walling) ([]string, error) {
	fw := r.path() + "/firewall"
	var fixed []string

	// an option is always written, so that Proxmox's default never decides:
	// one left unsaid lets DHCP through (read on a bench; its page says not)
	want := map[string]string{
		"enable": "1", "policy_in": "DROP", "policy_out": "ACCEPT", "macfilter": "1",
		// the set below is the first card's alone; ipfilter would pin every
		// card from its config, a card that forwards among them
		"ipfilter": "0", "dhcp": flag(w.leased), "log_level_in": d.lane.log,
	}
	var have map[string]any
	if err := d.c.call(ctx, http.MethodGet, fw+"/options", nil, &have); err != nil {
		return nil, err
	}
	p, differs := url.Values{}, false
	for k, v := range want {
		p.Set(k, v)
		differs = differs || str(have[k]) != v
	}
	if differs {
		if err := d.c.call(ctx, http.MethodPut, fw+"/options", p, nil); err != nil {
			return nil, err
		}
		fixed = append(fixed, "options")
	}

	var sets []struct {
		Name string `json:"name"`
	}
	if err := d.c.call(ctx, http.MethodGet, fw+"/ipset", nil, &sets); err != nil {
		return nil, err
	}
	var members []struct {
		CIDR string `json:"cidr"`
	}
	made := false
	for _, s := range sets {
		made = made || s.Name == ipfilterSet
	}
	if made {
		if err := d.c.call(ctx, http.MethodGet, fw+"/ipset/"+ipfilterSet, nil, &members); err != nil {
			return nil, err
		}
	} else if err := d.c.call(ctx, http.MethodPost, fw+"/ipset", url.Values{"name": {ipfilterSet},
		"comment": {"hangar: what this guest may send as"}}, nil); err != nil {
		return nil, err
	}
	pinned := false
	for _, m := range members {
		if strings.TrimSuffix(m.CIDR, "/32") == w.pin {
			pinned = true
			continue
		}
		if err := d.c.call(ctx, http.MethodDelete, fw+"/ipset/"+ipfilterSet+"/"+url.PathEscape(m.CIDR), nil, nil); err != nil {
			return nil, err
		}
	}
	if !pinned {
		if err := d.c.call(ctx, http.MethodPost, fw+"/ipset/"+ipfilterSet, url.Values{"cidr": {w.pin}}, nil); err != nil {
			return nil, err
		}
	}
	if !pinned || len(members) > 1 {
		fixed = append(fixed, "address")
	}

	var listed []map[string]any
	if err := d.c.call(ctx, http.MethodGet, fw+"/rules", nil, &listed); err != nil {
		return nil, err
	}
	rules := make([]wallRule, len(listed))
	for i, m := range listed {
		rules[i] = ruleOf(m)
	}
	if !slices.EqualFunc(rules, w.rules, wallRule.same) {
		// taken out from the last, so that no position moves under the next;
		// then made from the last too: the API puts each new line first
		for i := len(rules) - 1; i >= 0; i-- {
			if err := d.c.call(ctx, http.MethodDelete, fmt.Sprintf("%s/rules/%d", fw, rules[i].Pos), nil, nil); err != nil {
				return nil, err
			}
		}
		for i := len(w.rules) - 1; i >= 0; i-- {
			l := w.rules[i]
			// a line made without enable is a dead one
			p := url.Values{"type": {l.Type}, "action": {l.Action}, "enable": {"1"}}
			for k, v := range map[string]string{"proto": l.Proto, "sport": l.Sport, "dport": l.Dport, "source": l.Source,
				"dest": l.Dest, "iface": l.Iface, "macro": l.Macro, "comment": l.Comment} {
				if v != "" {
					p.Set(k, v)
				}
			}
			if err := d.c.call(ctx, http.MethodPost, fw+"/rules", p, nil); err != nil {
				return nil, err
			}
		}
		fixed = append(fixed, "rules")
	}

	for _, key := range w.cards {
		card := str(cfg[key])
		if card == "" || cardOpt(card, "firewall") == "1" {
			continue
		}
		// only the flag moves: a running guest's card is plugged again
		// behind its wall, and its open connections go on (read on a bench)
		if err := d.setConfig(ctx, r, url.Values{key: {withCardOpt(card, "firewall", "1")}}); err != nil {
			return nil, err
		}
		if !slices.Contains(fixed, "card") {
			fixed = append(fixed, "card")
		}
	}
	if len(fixed) > 0 {
		d.wmu.Lock()
		d.walledAt[r.VMID] = time.Now()
		d.wmu.Unlock()
	}
	return fixed, nil
}

// settled waits until a guest's wall, if it was just written, is on the
// wire: its start comes after.
func (d *Driver) settled(ctx context.Context, r resource) error {
	d.wmu.Lock()
	at, written := d.walledAt[r.VMID]
	d.wmu.Unlock()
	if !written {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Until(at.Add(d.wallWait))):
	}
	d.wmu.Lock()
	if d.walledAt[r.VMID].Equal(at) {
		delete(d.walledAt, r.VMID)
	}
	d.wmu.Unlock()
	return nil
}

// withCardOpt writes a card's line back with one option set, the rest kept.
func withCardOpt(line, name, value string) string {
	var out []string
	for _, part := range strings.Split(line, ",") {
		if k, _, ok := strings.Cut(part, "="); !ok || k != name {
			out = append(out, part)
		}
	}
	return strings.Join(append(out, name+"="+value), ",")
}

// walled reads whether a guest stands behind its wall, and pinned to what:
// its card filtered and its firewall on. What the wall holds is Wall's to
// check.
func (d *Driver) walled(ctx context.Context, r resource, cfg map[string]any) string {
	if !d.lane.wall || cardOpt(str(cfg["net0"]), "firewall") != "1" {
		return ""
	}
	var o map[string]any
	if err := d.c.call(ctx, http.MethodGet, r.path()+"/firewall/options", nil, &o); err != nil || num(o["enable"]) != 1 {
		return ""
	}
	_, word, _ := d.lane.wallOf(r.Type, cfg)
	return word
}

var _ driver.Walls = (*Driver)(nil)
