package fake

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/tomblancdev/hangar/driver"
)

// Networks live in the file too ("networks"): each a range of its own, a
// gateway that runs while a guest of the network does, and the keys it was
// born with. A gateway is made again when its keys change — "made" counts how
// many times — and where one is gone ("networks" edited by hand) it is made
// again at its number.

// networkRoomMB is what a fake network's gateway holds.
const networkRoomMB = 64

// maxNetworks: a fake zone cuts its networks from one documentation range.
const maxNetworks = 8

type fakeNetwork struct {
	driver.Network
	Number int `json:"number"`
	// JumpKeys: the keys its gateway was born with.
	JumpKeys []string `json:"jump_keys,omitempty"`
	// Made: how many times its gateway was made (1: at its birth).
	Made int `json:"made"`
}

func (e *Engine) NetworkRoomMB() int {
	if !e.has(driver.NetPrivate) {
		return 0
	}
	return networkRoomMB
}

// born fills a network in from its number: its range, its gateway, where its
// keys jump through.
func (e *Engine) born(n *fakeNetwork, s driver.NetworkSpec) {
	n.ID, n.EngineRef, n.Node, n.Label = s.ID, "fake-net-"+strconv.Itoa(n.Number), "fake", s.Label
	n.Range = fmt.Sprintf("192.0.2.%d/27", n.Number*32)
	n.Gateway = fmt.Sprintf("192.0.2.%d", n.Number*32+1)
	n.Jump = fmt.Sprintf("jump@203.0.113.%d", 200+n.Number)
	n.JumpKeys, n.Keys = slices.Clone(s.JumpKeys), len(s.JumpKeys)
	n.Made++
	if e.has(driver.NetFirewall) {
		n.Wall = driver.WallExact
	}
	n.Running = e.memberRuns(s.ID, "")
}

// members are the guests on a network, sorted. Called with e.mu held.
func (e *Engine) members(id string) []string {
	var out []string
	for gid, s := range e.state.Specs {
		if _, here := e.state.Guests[gid]; here && s.Network == id {
			out = append(out, gid)
		}
	}
	sort.Strings(out)
	return out
}

// memberRuns: a guest of the network runs — but: one counted out.
func (e *Engine) memberRuns(id, but string) bool {
	for _, gid := range e.members(id) {
		if gid != but && e.state.Guests[gid].Running {
			return true
		}
	}
	return false
}

// follow brings a guest's network's gateway to what its guests need, and
// says what it did. Called with e.mu held.
func (e *Engine) follow(guest string) string {
	n, ok := e.state.Networks[e.state.Specs[guest].Network]
	if !ok {
		return ""
	}
	switch want := e.memberRuns(n.ID, ""); {
	case want && !n.Running:
		n.Running = true
		return "started"
	case !want && n.Running:
		n.Running = false
		return "stopped"
	}
	return ""
}

func (e *Engine) CreateNetwork(_ context.Context, s driver.NetworkSpec) (driver.Network, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return driver.Network{}, err
	}
	if !e.has(driver.NetPrivate) {
		return driver.Network{}, fmt.Errorf("%w: this zone cuts no networks", driver.ErrRefused)
	}
	if n, ok := e.state.Networks[s.ID]; ok {
		return n.Network, nil
	}
	// the lowest number no network holds — and no guest still stands on
	used := map[int]bool{}
	for _, n := range e.state.Networks {
		used[n.Number] = true
	}
	for gid, g := range e.state.Guests {
		if on := e.state.Specs[gid].Network; on != "" && g.Address != "" {
			var a, b, c, host int
			if _, err := fmt.Sscanf(g.Address, "%d.%d.%d.%d", &a, &b, &c, &host); err == nil {
				used[host/32] = true
			}
		}
	}
	number := 0
	for used[number] {
		number++
	}
	if number >= maxNetworks {
		return driver.Network{}, fmt.Errorf("%w: this zone holds %d networks, and each is made", driver.ErrRefused, maxNetworks)
	}
	n := &fakeNetwork{Number: number}
	e.born(n, s)
	if e.state.Networks == nil {
		e.state.Networks = map[string]*fakeNetwork{}
	}
	e.state.Networks[s.ID] = n
	return n.Network, e.save()
}

func (e *Engine) Network(_ context.Context, id string) (driver.Network, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return driver.Network{}, err
	}
	n, ok := e.state.Networks[id]
	if !ok {
		return driver.Network{}, driver.ErrNotFound
	}
	return n.Network, nil
}

func (e *Engine) TendNetwork(_ context.Context, s driver.NetworkSpec, was string) (driver.Network, []string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return driver.Network{}, nil, err
	}
	var fixed []string
	n, ok := e.state.Networks[s.ID]
	switch {
	case !ok:
		number, err := strconv.Atoi(strings.TrimPrefix(was, "fake-net-"))
		if err != nil || !strings.HasPrefix(was, "fake-net-") {
			return driver.Network{}, nil, driver.ErrNotFound
		}
		n = &fakeNetwork{Number: number}
		e.born(n, s)
		if e.state.Networks == nil {
			e.state.Networks = map[string]*fakeNetwork{}
		}
		e.state.Networks[s.ID] = n
		fixed = append(fixed, "gateway (it was gone: made again)")
	case !sameKeys(n.JumpKeys, s.JumpKeys):
		e.born(n, s)
		fixed = append(fixed, "gateway (made again: its keys changed)")
	}
	if e.has(driver.NetFirewall) && n.Wall != driver.WallExact {
		n.Wall = driver.WallExact
		fixed = append(fixed, "wall (card)")
	}
	if n.Label != s.Label {
		n.Label = s.Label
		fixed = append(fixed, "name")
	}
	return n.Network, fixed, e.save()
}

func sameKeys(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	sort.Strings(a)
	sort.Strings(b)
	return slices.Equal(slices.Compact(a), slices.Compact(b))
}

func (e *Engine) DeleteNetwork(_ context.Context, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return err
	}
	if on := e.members(id); len(on) > 0 {
		return fmt.Errorf("%w: it still holds %s: delete them first", driver.ErrRefused, strings.Join(on, ", "))
	}
	delete(e.state.Networks, id)
	return e.save()
}

// WayOut puts a guest's network's gateway back to what its guests need.
func (e *Engine) WayOut(_ context.Context, id string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return "", err
	}
	if _, ok := e.state.Guests[id]; !ok {
		return "", driver.ErrNotFound
	}
	did := e.follow(id)
	if did == "" {
		return "", nil
	}
	return did, e.save()
}

// ---- What tests do to a network behind the registry's back ------------------

// TamperNetwork changes a network as if someone edited it on the engine.
func (e *Engine) TamperNetwork(id string, f func(n *driver.Network)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.load()
	if n, ok := e.state.Networks[id]; ok {
		f(&n.Network)
		_ = e.save()
	}
}

// ForgetNetwork drops a network's gateway as if someone deleted it on the
// engine directly: its guests stay where they stand.
func (e *Engine) ForgetNetwork(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.load()
	delete(e.state.Networks, id)
	_ = e.save()
}

// NetworkMade says how many times a network's gateway was made, and the keys
// the last one was born with.
func (e *Engine) NetworkMade(id string) (int, []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.load()
	if n, ok := e.state.Networks[id]; ok {
		return n.Made, slices.Clone(n.JumpKeys)
	}
	return 0, nil
}

var (
	_ driver.Networks = (*Engine)(nil)
	_ driver.WaysOut  = (*Engine)(nil)
)
