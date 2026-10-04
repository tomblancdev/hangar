package proxmox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tomblancdev/hangar/driver"
)

// Networks of the cloud's own (net.private).
//
// A zone that says `net_bridge` cuts private networks for its guests on a
// bridge of its own: no physical port, VLAN-aware — a private switch on the
// zone's node. A network is one tag there: two tags never meet, so two
// networks are apart because no wire joins them, not because a rule says so.
//
// A NETWORK IS ITS GATEWAY GUEST — a small unprivileged container born from
// the operator's archive (tools/gateway), with two cards: the zone's lane
// and the network. Making the network makes that guest, and Proxmox gives it
// an id no other guest has. Everything else is derived from that one number,
// never counted:
//
//	the network's number n   its gateway's id − the first of net_vmids
//	its tag                  net_tag + n
//	its range                the n-th /net_size of net_block
//	its gateway's address    the first of that range
//	the gateway on the lane  net_address + n
//	a guest's address in it  the range's tenth, plus (its id − the first of vmids)
//
// so there is no allocator, nothing to keep and no race: the one thing ever
// allocated is a guest id, by Proxmox. (A network's first ten addresses are
// the cloud's own; its guests' begin after them.)
//
// A guest names its network at its birth and holds ONE card, there. Its wall
// (net.go) pins it to its exact address and lets in what its own network
// sends, nothing else.
//
// A gateway forwards and masquerades its network out through the lane, lets
// no new connection in, and lets the keys it was born with jump through to
// its network — its archive's own doing. On Proxmox it stands behind a wall
// like every guest: its lane card pinned to its one address, in from the lane
// only what the operator's groups let (who may knock on the jump), in from
// its network whatever its network sends. IT KEEPS NOTHING: its addresses are
// Proxmox's to write, its keys given at its birth — so it is never patched.
// One born with other keys, or from another archive than the zone's, is made
// again at the same id, and so is one that is gone.
//
// IT RUNS ONLY WHILE A GUEST OF ITS NETWORK RUNS, or a node with nothing
// running could never sleep: started before its network's first guest
// starts, stopped after its last one stops. That is the machines' token's
// doing (the power of a gateway and nothing of its config), as its guests
// start and stop — one process, so one lock holds the decision: a gateway is
// never put to rest while a guest of its network is starting.

// gatewayMemoryMB is a gateway's memory: read on a bench, one at rest holds
// 13 MB and peaked at 58 with its page cache; each jump through it is two
// small processes.
const gatewayMemoryMB = 128

// gatewayDiskGB is a gateway's root disk: its archive unpacks to 0.6 GB.
const gatewayDiskGB = 2

// firstMember: a network's first ten addresses are the cloud's own — its
// gateway the first; its guests' begin after them.
const firstMember = 10

// JumpUser is the one user a gateway lets in, and gives no shell.
const JumpUser = "jump"

// gatewayWord begins the description line that says what a gateway was born
// from: its archive, and the keys it was given.
const gatewayWord = "hangar gateway"

// nets is what a zone says of the networks it cuts for its guests.
type nets struct {
	bridge  string       // "" = it cuts none
	tag     int          // the first network's tag
	block   netip.Prefix // what networks are cut from
	bits    int          // a network's own length
	lo, hi  int          // the gateways' ids
	pool    string       // the gateways' pool
	first   netip.Addr   // the first gateway's address on the lane
	archive string       // what a gateway is born from
}

var bridgeName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,14}$`)

// netsOf reads a zone's networks' options.
func netsOf(o map[string]string, d *Driver) (nets, error) {
	var n nets
	with := []string{"net_tag", "net_block", "net_vmids", "net_pool", "net_address", "net_archive"}
	if o["net_bridge"] == "" {
		for _, k := range append(with, "net_size") {
			if o[k] != "" {
				return n, fmt.Errorf("%s is said with net_bridge: the bridge its networks are cut on", k)
			}
		}
		return n, nil
	}
	var missing []string
	for _, k := range with {
		if o[k] == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return n, fmt.Errorf("net_bridge needs %s", strings.Join(missing, ", "))
	}
	if !d.lane.wall || !d.lane.gateway.IsValid() {
		return n, fmt.Errorf("net_bridge needs subnet, gateway and firewall: on — a network's gateway is given its address on the zone's lane, goes out through the lane's, and stands behind its wall")
	}
	n.bridge, n.pool, n.archive = o["net_bridge"], o["net_pool"], o["net_archive"]
	if !bridgeName.MatchString(n.bridge) || n.bridge == d.bridge {
		return n, fmt.Errorf("net_bridge %q: a bridge of the cloud's own — no port, VLAN-aware — not the lane's", n.bridge)
	}
	if n.pool == d.pool || n.pool == d.images {
		return n, fmt.Errorf("net_pool %q: the gateways' own pool, not the machines' nor the images'", n.pool)
	}
	var ok bool
	if n.lo, n.hi, ok = idRange(o["net_vmids"]); !ok {
		return n, fmt.Errorf("net_vmids %q: a range such as 11200-11219", o["net_vmids"])
	}
	if n.lo <= d.hi && d.lo <= n.hi {
		return n, fmt.Errorf("net_vmids %s holds ids of vmids (%d-%d): a gateway's id is no machine's", o["net_vmids"], d.lo, d.hi)
	}
	var err error
	if n.tag, err = strconv.Atoi(o["net_tag"]); err != nil || n.tag < 1 || n.tag+n.count()-1 > 4094 {
		return n, fmt.Errorf("net_tag %q: the first network's tag — 1 to 4094, and net_vmids holds %d networks after it", o["net_tag"], n.count())
	}
	if n.block, err = netip.ParsePrefix(o["net_block"]); err != nil || !n.block.Addr().Is4() || n.block.Masked() != n.block {
		return n, fmt.Errorf("net_block %q: a range of IPv4 addresses, such as 203.0.113.0/24", o["net_block"])
	}
	if n.block.Overlaps(d.lane.subnet) {
		return n, fmt.Errorf("net_block %s holds addresses of the lane (%s)", n.block, d.lane.subnet)
	}
	n.bits = 24
	if v := o["net_size"]; v != "" {
		if n.bits, err = strconv.Atoi(v); err != nil || n.bits > 28 {
			return n, fmt.Errorf("net_size %q: a network's length — 28 at most (a /28 holds four guests)", v)
		}
	}
	if n.bits < n.block.Bits() {
		return n, fmt.Errorf("net_size %d: a network is no wider than the block it is cut from (%s)", n.bits, n.block)
	}
	if holds := 1 << (n.bits - n.block.Bits()); holds < n.count() {
		return n, fmt.Errorf("net_block %s holds %d networks of /%d, and net_vmids names %d", n.block, holds, n.bits, n.count())
	}
	if room := 1<<(32-n.bits) - 2; firstMember+(d.hi-d.lo) > room {
		return n, fmt.Errorf("a network of /%d gives its guests the addresses %d to %d, and vmids names %d ids: a smaller net_size, or fewer ids",
			n.bits, firstMember, room, d.hi-d.lo+1)
	}
	if n.first, err = netip.ParseAddr(o["net_address"]); err != nil {
		return n, fmt.Errorf("net_address %q: the first gateway's address on the lane, in %s", o["net_address"], d.lane.subnet)
	}
	last := n.lane(n.count() - 1)
	if !d.lane.host(n.first) || !d.lane.host(last) || last.Less(n.first) {
		return n, fmt.Errorf("net_address %s: net_vmids holds %d ids, so the gateways' addresses run to %s — outside what %s gives",
			n.first, n.count(), last, d.lane.subnet)
	}
	inRun := func(a netip.Addr) bool { return !a.Less(n.first) && !last.Less(a) }
	if guestsLast := d.lane.address(d.hi); !last.Less(d.lane.first) && !guestsLast.Less(n.first) {
		return n, fmt.Errorf("net_address: the gateways' addresses (%s to %s) run into the guests' (%s to %s)", n.first, last, d.lane.first, guestsLast)
	}
	if inRun(d.lane.gateway) {
		return n, fmt.Errorf("net_address: the lane's gateway %s is one of the addresses the networks' gateways are given (%s to %s)", d.lane.gateway, n.first, last)
	}
	return n, nil
}

func (n nets) on() bool   { return n.bridge != "" }
func (n nets) count() int { return n.hi - n.lo + 1 }

// rangeOf is the i-th network's range: the i-th /bits of the block.
func (n nets) rangeOf(i int) netip.Prefix {
	return netip.PrefixFrom(addrPlus(n.block.Addr(), i<<(32-n.bits)), n.bits)
}

// gateway is the i-th network's way out: the first address of its range.
func (n nets) gateway(i int) netip.Addr { return addrPlus(n.rangeOf(i).Addr(), 1) }

// lane is the i-th network's gateway on the zone's lane.
func (n nets) lane(i int) netip.Addr { return addrPlus(n.first, i) }

// place is where the zone's k-th guest stands on the i-th network.
func (n nets) place(i, k int, resolvers []netip.Addr) place {
	return place{bridge: n.bridge, tag: n.tag + i, gateway: n.gateway(i), resolvers: resolvers,
		addr: netip.PrefixFrom(addrPlus(n.rangeOf(i).Addr(), firstMember+k), n.bits)}
}

// of reads the network a card stands on: its number, or false — the lane's.
func (n nets) of(card string) (int, bool) {
	if !n.on() || cardOpt(card, "bridge") != n.bridge {
		return 0, false
	}
	tag, err := strconv.Atoi(cardOpt(card, "tag"))
	if err != nil || tag < n.tag || tag >= n.tag+n.count() {
		return 0, false
	}
	return tag - n.tag, true
}

// noDHCPServer is the line every wall ends on.
var noDHCPServer = wallRule{Type: "out", Action: "DROP", Proto: "udp", Sport: "67", Enable: 1,
	Comment: "hangar: no guest answers as a DHCP server"}

// memberRules are the lines a guest of the i-th network wears: whatever its
// own network sends comes in — nobody else is on that wire.
func (n nets) memberRules(i int) []wallRule {
	return []wallRule{
		{Type: "in", Action: "ACCEPT", Source: n.rangeOf(i).String(), Enable: 1, Comment: "hangar: its own network"},
		noDHCPServer,
	}
}

// gatewayRules are the lines the i-th network's gateway wears: from the
// lane, what the operator's groups let (who may knock on the jump); from its
// network, what its network sends — out through it.
func (d *Driver) gatewayRules(i int) []wallRule {
	var out []wallRule
	for _, g := range d.lane.groups {
		out = append(out, wallRule{Type: "group", Action: g, Enable: 1})
	}
	return append(out,
		wallRule{Type: "in", Action: "ACCEPT", Iface: "net1", Source: d.nets.rangeOf(i).String(), Enable: 1, Comment: "hangar: its network, out through it"},
		noDHCPServer)
}

// ---- A guest and its network's gateway --------------------------------------

// networkNumber reads which network a guest is born on: the number of the
// one whose gateway carries the core's id; -1: none named — the lane.
func (d *Driver) networkNumber(ctx context.Context, id string) (int, error) {
	if id == "" {
		return -1, nil
	}
	if !d.nets.on() {
		return 0, fmt.Errorf("%w: zone %s cuts no networks (net_bridge)", driver.ErrRefused, d.zone)
	}
	r, err := d.findIn(ctx, d.nets.pool, id)
	if errors.Is(err, driver.ErrNotFound) {
		return 0, fmt.Errorf("%w: the engine has no network %s", driver.ErrRefused, id)
	}
	if err != nil {
		return 0, err
	}
	return r.VMID - d.nets.lo, nil
}

// gateway is the i-th network's gateway guest: derived, never looked up.
func (d *Driver) gateway(i int) resource {
	return resource{VMID: d.nets.lo + i, Node: d.node, Type: "lxc", Pool: d.nets.pool}
}

// gatewayOf is the gateway of the network a guest's card stands on; false: it
// stands on the lane.
func (d *Driver) gatewayOf(ctx context.Context, r resource) (resource, bool, error) {
	if !d.nets.on() {
		return resource{}, false, nil
	}
	var cfg map[string]any
	if err := d.c.call(ctx, http.MethodGet, r.path()+"/config", nil, &cfg); err != nil {
		return resource{}, false, err
	}
	i, on := d.nets.of(str(cfg["net0"]))
	return d.gateway(i), on, nil
}

// wake starts a network's gateway unless it runs, for a guest about to
// start: until done is called the gateway is not put to rest.
func (d *Driver) wake(ctx context.Context, gw resource) (done func(), err error) {
	d.gmu.Lock()
	defer d.gmu.Unlock()
	d.starting[gw.VMID]++
	done = func() {
		d.gmu.Lock()
		if d.starting[gw.VMID]--; d.starting[gw.VMID] <= 0 {
			delete(d.starting, gw.VMID)
		}
		d.gmu.Unlock()
	}
	if on, err := d.running(ctx, gw); err != nil || on {
		if err != nil {
			d.starting[gw.VMID]--
			return nil, err
		}
		return done, nil
	}
	err = d.c.run(ctx, http.MethodPost, gw.path()+"/status/start", nil)
	if err != nil && !strings.Contains(err.Error(), "already running") {
		d.starting[gw.VMID]--
		return nil, err
	}
	return done, nil
}

// rest stops a network's gateway once no guest of its network runs — gone: a
// guest being deleted, counted out — and says whether it did. One whose
// guests cannot all be read is left running.
func (d *Driver) rest(ctx context.Context, gw resource, gone int) (bool, error) {
	d.gmu.Lock()
	defer d.gmu.Unlock()
	if d.starting[gw.VMID] > 0 {
		return false, nil
	}
	if on, err := d.running(ctx, gw); err != nil || !on {
		return false, err
	}
	members, err := d.membersOf(ctx, gw.VMID-d.nets.lo)
	if err != nil {
		return false, err
	}
	for _, m := range members {
		if m.VMID == gone {
			continue
		}
		if on, err := d.running(ctx, m); err != nil || on {
			return false, err
		}
	}
	// it keeps nothing: stopped at once
	return true, d.c.run(ctx, http.MethodPost, gw.path()+"/status/stop", nil)
}

// membersOf are the guests whose card stands on the i-th network.
func (d *Driver) membersOf(ctx context.Context, i int) ([]resource, error) {
	rs, err := d.resources(ctx)
	if err != nil {
		return nil, err
	}
	var out []resource
	for _, r := range rs {
		if r.Pool != d.pool || r.Template != 0 {
			continue
		}
		var cfg map[string]any
		if err := d.c.call(ctx, http.MethodGet, r.path()+"/config", nil, &cfg); err != nil {
			if apiGone(err) {
				continue // deleted since it was listed
			}
			return nil, err
		}
		if n, on := d.nets.of(str(cfg["net0"])); on && n == i {
			r.Tags, r.Name = str(cfg["tags"]), cmpOr(str(cfg["hostname"]), str(cfg["name"]), r.Name)
			out = append(out, r)
		}
	}
	return out, nil
}

// apiGone: the API says a guest it listed a moment ago is no longer there.
func apiGone(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && (ae.Status == http.StatusNotFound || strings.Contains(ae.Message, "does not exist"))
}

func cmpOr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// WayOut brings the gateway of a guest's network to what its guests need:
// running while one of them runs, stopped once none does.
func (d *Driver) WayOut(ctx context.Context, id string) (string, error) {
	if !d.nets.on() {
		return "", nil
	}
	r, err := d.find(ctx, id)
	if err != nil {
		return "", d.engine(err)
	}
	gw, member, err := d.gatewayOf(ctx, r)
	if err != nil || !member {
		return "", d.engine(err)
	}
	on, err := d.running(ctx, r)
	if err != nil {
		return "", d.engine(err)
	}
	if !on {
		stopped, err := d.rest(ctx, gw, 0)
		if stopped {
			return "stopped", d.engine(err)
		}
		return "", d.engine(err)
	}
	if up, err := d.running(ctx, gw); err != nil || up {
		return "", d.engine(err)
	}
	done, err := d.wake(ctx, gw)
	if err != nil {
		return "", d.engine(err)
	}
	done()
	return "started", nil
}

// ---- The networks facet -----------------------------------------------------

// NetworkRoomMB is what a network's gateway holds while it runs.
func (d *Driver) NetworkRoomMB() int {
	if !d.nets.on() {
		return 0
	}
	return gatewayMemoryMB
}

// keysDigest names a set of keys: how many, and a hash of them in order.
func keysDigest(keys []string) string {
	ks := make([]string, 0, len(keys))
	for _, k := range keys {
		if k = strings.TrimSpace(k); k != "" {
			ks = append(ks, k)
		}
	}
	sort.Strings(ks)
	sum := sha256.Sum256([]byte(strings.Join(slices.Compact(ks), "\n")))
	return strconv.Itoa(len(ks)) + ":" + hex.EncodeToString(sum[:8])
}

// bornLine is the description line that says what a gateway was born from.
func (d *Driver) bornLine(s driver.NetworkSpec) string {
	return fmt.Sprintf("%s %s %s %s", gatewayWord, s.ID, d.nets.archive, keysDigest(s.JumpKeys))
}

// bornOf reads that line: the archive and the keys' digest; false: none.
func bornOf(desc, id string) (archive, keys string, ok bool) {
	for _, l := range strings.Split(desc, "\n") {
		f := strings.Fields(l)
		if len(f) == 5 && f[0]+" "+f[1] == gatewayWord && f[2] == id {
			return f[3], f[4], true
		}
	}
	return "", "", false
}

// createGateway makes a network's gateway at an id: stopped, its two cards
// and its keys Proxmox's to write.
func (d *Driver) createGateway(ctx context.Context, vmid int, s driver.NetworkSpec) error {
	i := vmid - d.nets.lo
	lane := place{bridge: d.bridge, tag: d.vlan, gateway: d.lane.gateway,
		addr: netip.PrefixFrom(d.nets.lane(i), d.lane.subnet.Bits())}
	p := url.Values{
		"vmid":         {strconv.Itoa(vmid)},
		"ostemplate":   {d.nets.archive},
		"hostname":     {"gateway-" + strconv.Itoa(i)},
		"cores":        {"1"},
		"memory":       {strconv.Itoa(gatewayMemoryMB)},
		"swap":         {"0"},
		"rootfs":       {fmt.Sprintf("%s:%d", d.storage, gatewayDiskGB)},
		"net0":         {d.card(true, lane, "")},
		"net1":         {fmt.Sprintf("name=eth1,bridge=%s,tag=%d,firewall=1,ip=%s", d.nets.bridge, d.nets.tag+i, netip.PrefixFrom(d.nets.gateway(i), d.nets.bits))},
		"pool":         {d.nets.pool},
		"unprivileged": {"1"},
		"features":     {"nesting=1"},
		"onboot":       {"0"},
		"description":  {marker(s.ID) + "\n" + d.bornLine(s)},
	}
	if len(s.JumpKeys) > 0 {
		p.Set("ssh-public-keys", strings.Join(s.JumpKeys, "\n"))
	}
	return d.c.run(ctx, http.MethodPost, "/nodes/"+url.PathEscape(d.node)+"/lxc", p)
}

// taken are the gateways' ids no new network may take though they are free:
// those a guest's card still stands on. A gateway being made again leaves its
// id free for a moment — and a network made on it then would be on its
// guests' wire.
func (d *Driver) taken(ctx context.Context) (map[int]bool, error) {
	rs, err := d.resources(ctx)
	if err != nil {
		return nil, err
	}
	out := map[int]bool{}
	for _, r := range rs {
		if r.Pool != d.pool || r.Template != 0 {
			continue
		}
		var cfg map[string]any
		if err := d.c.call(ctx, http.MethodGet, r.path()+"/config", nil, &cfg); err != nil {
			if apiGone(err) {
				continue // deleted since it was listed
			}
			return nil, err // one that cannot be read may stand anywhere: no number is given
		}
		if i, on := d.nets.of(str(cfg["net0"])); on {
			out[d.nets.lo+i] = true
		}
	}
	return out, nil
}

// gatewayReady finishes a gateway — idempotent, so a retry finishes a create
// that stopped half-way: its tag, its wall before any start, what it is
// called; then Proxmox's pass is waited for, once, so that none of its starts
// has to.
func (d *Driver) gatewayReady(ctx context.Context, r resource, s driver.NetworkSpec) ([]string, error) {
	tagStr, err := tags(s.ID, nil, nil)
	if err != nil {
		return nil, err
	}
	if r.Tags != tagStr {
		if err := d.c.call(ctx, http.MethodPut, r.path()+"/config", url.Values{"tags": {tagStr}}, nil); err != nil {
			return nil, err
		}
	}
	var cfg map[string]any
	if err := d.c.call(ctx, http.MethodGet, r.path()+"/config", nil, &cfg); err != nil {
		return nil, err
	}
	i := r.VMID - d.nets.lo
	fixed, err := d.wallWrite(ctx, r, cfg, walling{pin: d.nets.lane(i).String(), rules: d.gatewayRules(i), cards: []string{"net0", "net1"}})
	if err != nil {
		return nil, err
	}
	if len(fixed) > 0 {
		fixed = []string{"wall (" + strings.Join(fixed, ", ") + ")"}
	}
	if nameIn(strings.Split(str(cfg["description"]), "\n"), s.ID) != oneLine(s.Label) {
		// a line for people: one that cannot be written now is at the next look
		if d.relabel(ctx, r, s.ID, s.Label) == nil {
			fixed = append(fixed, "name")
		}
	}
	return fixed, d.settled(ctx, r)
}

// CreateNetwork makes a network: its gateway, born stopped behind its wall.
func (d *Driver) CreateNetwork(ctx context.Context, s driver.NetworkSpec) (driver.Network, error) {
	if !d.nets.on() {
		return driver.Network{}, fmt.Errorf("%w: zone %s cuts no networks (net_bridge)", driver.ErrRefused, d.zone)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	r, err := d.findIn(ctx, d.nets.pool, s.ID)
	switch {
	case err == nil: // a retry: finish what the first call began
	case errors.Is(err, driver.ErrNotFound):
		skip, err := d.taken(ctx)
		if err != nil {
			return driver.Network{}, d.engine(err)
		}
		var made int
		err = d.withFreeIn(ctx, d.nets.lo, d.nets.hi, skip, func(vmid int) error {
			made = vmid
			return d.createGateway(ctx, vmid, s)
		})
		if err != nil {
			if strings.Contains(err.Error(), "is taken") {
				err = fmt.Errorf("%w: zone %s holds %d networks, and each is made", driver.ErrRefused, d.zone, d.nets.count())
			}
			return driver.Network{}, d.engine(err)
		}
		r = resource{VMID: made, Node: d.node, Type: "lxc", Pool: d.nets.pool}
	default:
		return driver.Network{}, d.engine(err)
	}
	if _, err := d.gatewayReady(ctx, r, s); err != nil {
		return driver.Network{}, d.engine(err)
	}
	return d.readNetwork(ctx, r, s.ID)
}

// Network reads a network by the core's id.
func (d *Driver) Network(ctx context.Context, id string) (driver.Network, error) {
	if !d.nets.on() {
		return driver.Network{}, driver.ErrNotFound
	}
	r, err := d.findIn(ctx, d.nets.pool, id)
	if err != nil {
		return driver.Network{}, d.engine(err)
	}
	return d.readNetwork(ctx, r, id)
}

func (d *Driver) readNetwork(ctx context.Context, r resource, id string) (driver.Network, error) {
	var cfg map[string]any
	if err := d.c.call(ctx, http.MethodGet, r.path()+"/config", nil, &cfg); err != nil {
		return driver.Network{}, d.engine(err)
	}
	on, err := d.running(ctx, r)
	if err != nil {
		return driver.Network{}, d.engine(err)
	}
	i := r.VMID - d.nets.lo
	n := driver.Network{
		ID: id, EngineRef: fmt.Sprintf("%s/%s/%d", r.Node, r.Type, r.VMID), Node: r.Node, Running: on,
		Label: nameIn(strings.Split(str(cfg["description"]), "\n"), id),
		Range: d.nets.rangeOf(i).String(), Gateway: d.nets.gateway(i).String(), Jump: JumpUser + "@" + d.nets.lane(i).String(),
		Wall: d.walled(ctx, r, cfg),
	}
	if _, keys, ok := bornOf(str(cfg["description"]), id); ok {
		n.Keys, _ = strconv.Atoi(strings.SplitN(keys, ":", 2)[0])
	}
	return n, nil
}

// vmidOf reads the id in an engine ref (node/type/vmid); 0: none.
func vmidOf(ref string) int {
	n, _ := strconv.Atoi(ref[strings.LastIndex(ref, "/")+1:])
	return n
}

// TendNetwork brings a network's gateway to its spec. One born from another
// archive, with other keys, or gone, is made again at its id — a gateway
// keeps nothing, and is never patched.
func (d *Driver) TendNetwork(ctx context.Context, s driver.NetworkSpec, was string) (driver.Network, []string, error) {
	if !d.nets.on() {
		return driver.Network{}, nil, driver.ErrNotFound
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var fixed []string
	r, err := d.findIn(ctx, d.nets.pool, s.ID)
	switch {
	case errors.Is(err, driver.ErrNotFound):
		// gone — a remake cut half-way, or a hand: made again where it was,
		// the one place its guests' cards stand on
		vmid := vmidOf(was)
		if vmid < d.nets.lo || vmid > d.nets.hi {
			return driver.Network{}, nil, driver.ErrNotFound
		}
		if err := d.createGateway(ctx, vmid, s); err != nil {
			return driver.Network{}, nil, d.engine(fmt.Errorf("its gateway is gone, and guest %d could not be made again: %w", vmid, err))
		}
		r = resource{VMID: vmid, Node: d.node, Type: "lxc", Pool: d.nets.pool}
		fixed = append(fixed, "gateway (it was gone: made again)")
	case err != nil:
		return driver.Network{}, nil, d.engine(err)
	default:
		var cfg map[string]any
		if err := d.c.call(ctx, http.MethodGet, r.path()+"/config", nil, &cfg); err != nil {
			return driver.Network{}, nil, d.engine(err)
		}
		archive, keys, ok := bornOf(str(cfg["description"]), s.ID)
		why := ""
		switch {
		case !ok || archive != d.nets.archive:
			why = "its archive is " + d.nets.archive + " now"
		case keys != keysDigest(s.JumpKeys):
			why = "its keys changed"
		}
		if why != "" {
			if err := d.remake(ctx, r, s); err != nil {
				return driver.Network{}, fixed, d.engine(err)
			}
			r.Tags = ""
			fixed = append(fixed, "gateway (made again: "+why+")")
		}
	}
	put, err := d.gatewayReady(ctx, r, s)
	fixed = append(fixed, put...)
	if err != nil {
		return driver.Network{}, fixed, d.engine(err)
	}
	// a gateway made again goes back to work: its guests may be running
	if len(fixed) > 0 && strings.HasPrefix(fixed[0], "gateway") {
		if members, err := d.membersOf(ctx, r.VMID-d.nets.lo); err == nil {
			for _, m := range members {
				if on, _ := d.running(ctx, m); on {
					if err := d.c.run(ctx, http.MethodPost, r.path()+"/status/start", nil); err != nil && !strings.Contains(err.Error(), "already running") {
						return driver.Network{}, fixed, d.engine(err)
					}
					break
				}
			}
		}
	}
	n, err := d.readNetwork(ctx, r, s.ID)
	return n, fixed, err
}

// remake deletes a gateway and makes it again at the same id.
func (d *Driver) remake(ctx context.Context, r resource, s driver.NetworkSpec) error {
	if on, err := d.running(ctx, r); err != nil {
		return err
	} else if on {
		if err := d.c.run(ctx, http.MethodPost, r.path()+"/status/stop", nil); err != nil {
			return err
		}
	}
	if err := d.c.run(ctx, http.MethodDelete, r.path(), url.Values{"purge": {"1"}, "destroy-unreferenced-disks": {"1"}}); err != nil {
		return err
	}
	return d.createGateway(ctx, r.VMID, s)
}

// DeleteNetwork removes a network's gateway. A network a guest still stands
// on is refused: its guests would be left on a wire with no way out, and the
// next network made would be given that wire.
func (d *Driver) DeleteNetwork(ctx context.Context, id string) error {
	if !d.nets.on() {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	r, err := d.findIn(ctx, d.nets.pool, id)
	if errors.Is(err, driver.ErrNotFound) {
		// made a moment ago, it may not be listed yet: looked for again after
		// pvestatd's pass, before it is called gone
		select {
		case <-time.After(d.listLag):
		case <-ctx.Done():
			return ctx.Err()
		}
		r, err = d.findIn(ctx, d.nets.pool, id)
	}
	if errors.Is(err, driver.ErrNotFound) {
		return nil
	}
	if err != nil {
		return d.engine(err)
	}
	members, err := d.membersOf(ctx, r.VMID-d.nets.lo)
	if err != nil {
		return d.engine(err)
	}
	if len(members) > 0 {
		names := make([]string, len(members))
		for i, m := range members {
			names[i] = cmpOr(guestID(m.Tags), m.Name, strconv.Itoa(m.VMID))
		}
		sort.Strings(names)
		return fmt.Errorf("%w: it still holds %s: delete them first", driver.ErrRefused, strings.Join(names, ", "))
	}
	if on, err := d.running(ctx, r); err != nil {
		return d.engine(err)
	} else if on {
		if err := d.c.run(ctx, http.MethodPost, r.path()+"/status/stop", nil); err != nil {
			return d.engine(err)
		}
	}
	return d.engine(d.c.run(ctx, http.MethodDelete, r.path(), url.Values{"purge": {"1"}, "destroy-unreferenced-disks": {"1"}}))
}

var (
	_ driver.Networks = (*Driver)(nil)
	_ driver.WaysOut  = (*Driver)(nil)
)
