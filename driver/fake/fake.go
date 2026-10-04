// Package fake is an engine with no hypervisor behind it: guests are entries
// in a map, or in a JSON file when the zone's endpoint names one — and then
// the FILE is the engine: every call reads it first, so editing it by hand is
// changing the engine behind the brain's back.
//
// It ships first so that the whole core and every plugin can be proved with
// nothing to install. Tests make the engine disagree with the registry (by
// the file, or in-process with Forget, Tamper, FailNext), which is what
// reconcile is for.
//
// Images live there too ("images"): a bake takes two calls (images.go).
//
// Volumes live in the same file ("volumes"): each on a guest, or parked with
// no guest at all (the fake needs no shelf); a running container lets go of
// none, as on Proxmox VE, and a guest holding one is not deleted.
//
// The file also holds what a zone's reservations wait on: "watched" (the
// power of guests outside the fence, by name), "nodes_down", and "asleep" —
// a zone that sleeps answers no guest call until the file says otherwise, as
// a webhook that wakes it would.
//
// And it holds the engine's clock and its guests' activity: "now" freezes
// the engine's time there (absent: the real time) — a running guest says
// since when it runs ("started_at") and every reading is stamped with that
// clock, so moving "now" forward is hours passing; "busy_until" says up to
// when each guest was last busy (absent: quiet since it started), which is
// all the fake keeps of a history of CPU and network.
//
// A zone that carries net.firewall gives each guest an address at its birth
// and puts it behind a wall ("address", "wall" in the file): taking the wall
// off there is someone opening a guest behind the brain's back, and Wall
// puts it back.
//
// Zone options:
//
//	capabilities              comma-separated flags; default: every documented
//	                          flag but the GPU ones
//	expect_credential_sha256  refuse to open unless the plugin's credential
//	                          hashes to this (proves the credential arrived)
//	image_kinds               the kinds it makes images for; default: the
//	                          kinds it runs
package fake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tomblancdev/hangar/driver"
)

// Name is the name the fake registers under.
const Name = "fake"

func init() { driver.Register(Name, Open) }

// Engine is one fake zone.
type Engine struct {
	mu    sync.Mutex
	path  string // "" = memory only
	caps  []driver.Capability
	watch []string
	// imageKinds: the kinds it makes images for (nil: the kinds it runs)
	imageKinds []string
	state      state
	failNext   error
}

type state struct {
	Seq    int                      `json:"seq"`
	Guests map[string]*driver.Guest `json:"guests"`
	// Specs: what each guest was created with (its keys, user data, image),
	// which a Guest does not report — kept so tests can read what arrived.
	Specs map[string]driver.GuestSpec `json:"specs,omitempty"`
	// Watched: whether each guest outside the fence runs, by name.
	Watched map[string]bool `json:"watched,omitempty"`
	// NodesDown: the engine's nodes that are down.
	NodesDown map[string]bool `json:"nodes_down,omitempty"`
	// Asleep: the zone sleeps; its guests cannot be reached.
	Asleep bool `json:"asleep,omitempty"`
	// WatchFails: the watched guests' power cannot be read (an API hiccup).
	WatchFails bool `json:"watch_fails,omitempty"`
	// Volumes, by the core's id.
	Volumes map[string]*driver.Volume `json:"volumes,omitempty"`
	// Images, and their bakes, by the core's id.
	Images map[string]*fakeImage `json:"images,omitempty"`
	// BuildersFail: every builder's first boot reports errors, whatever its
	// recipe (a package mirror down that day).
	BuildersFail bool `json:"builders_fail,omitempty"`
	// Now: the engine's clock, frozen there; nil = the real time.
	Now *time.Time `json:"now,omitempty"`
	// BusyUntil: up to when each guest was last busy, by the engine's clock;
	// absent = quiet since it started.
	BusyUntil map[string]time.Time `json:"busy_until,omitempty"`
	// ActivityFails: the guests' history cannot be read.
	ActivityFails bool `json:"activity_fails,omitempty"`
}

// now is the engine's clock. Called with e.mu held, the file read.
func (e *Engine) now() time.Time {
	if e.state.Now != nil {
		return e.state.Now.UTC()
	}
	return time.Now().UTC()
}

// Now is the engine's clock: a plugin on the fake engine reads its time
// there, so a test moves the hours by the file alone.
func (e *Engine) Now() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.load()
	return e.now()
}

// errAsleep is what a sleeping zone answers: an engine that cannot be reached
// (the plugin reports it as such, and the core tries again later).
var errAsleep = errors.New("the zone is asleep: its engine does not answer")

// Open opens a fake zone. The endpoint is empty (memory) or a file path.
func Open(_ context.Context, p driver.Params) (driver.Driver, error) {
	if want := p.Options["expect_credential_sha256"]; want != "" {
		sum := sha256.Sum256(p.Credential)
		if hex.EncodeToString(sum[:]) != strings.ToLower(want) {
			return nil, fmt.Errorf("fake zone %s: the credential is not the one this zone expects", p.Zone)
		}
	}
	e := &Engine{state: state{Guests: map[string]*driver.Guest{}}, watch: slices.Clone(p.Watch)}
	e.caps = defaultCaps()
	if list := p.Options["capabilities"]; list != "" {
		e.caps = nil
		for _, c := range strings.Split(list, ",") {
			if c = strings.TrimSpace(c); c != "" {
				e.caps = append(e.caps, c)
			}
		}
	}
	if list, ok := p.Options["image_kinds"]; ok {
		e.imageKinds = []string{}
		for _, k := range strings.Split(list, ",") {
			if k = strings.TrimSpace(k); k != "" {
				e.imageKinds = append(e.imageKinds, k)
			}
		}
	}
	if p.Endpoint != "" && p.Endpoint != "memory" {
		e.path = p.Endpoint
		if err := e.load(); err != nil {
			return nil, fmt.Errorf("fake zone %s: %w", p.Zone, err)
		}
	}
	return e, nil
}

// load reads the file, when there is one. Called with e.mu held (or before e
// is shared).
func (e *Engine) load() error {
	if e.path == "" {
		return nil
	}
	b, err := os.ReadFile(e.path)
	if os.IsNotExist(err) {
		e.state = state{Guests: map[string]*driver.Guest{}}
		return nil
	}
	if err != nil {
		return err
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("%s: %w", e.path, err)
	}
	if st.Guests == nil {
		st.Guests = map[string]*driver.Guest{}
	}
	e.state = st
	return nil
}

func defaultCaps() []driver.Capability {
	var out []driver.Capability
	for _, c := range driver.Known {
		if !strings.HasPrefix(c, "gpu.") {
			out = append(out, c)
		}
	}
	return out
}

func (e *Engine) Capabilities() []driver.Capability { return slices.Clone(e.caps) }
func (e *Engine) Close() error                      { return nil }

func (e *Engine) has(c driver.Capability) bool { return slices.Contains(e.caps, c) }

// save writes the state through a temporary file, so a crash never leaves a
// half-written one. Called with e.mu held.
func (e *Engine) save() error {
	if e.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(e.state, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(e.path), ".fake-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), e.path)
}

// fail returns the error a test planted, once; otherwise it reads the file,
// so every call starts from the engine as it is — and a zone asleep answers
// nothing. Called with e.mu held.
func (e *Engine) fail() error {
	if err := e.failNext; err != nil {
		e.failNext = nil
		return err
	}
	if err := e.load(); err != nil {
		return err
	}
	if e.state.Asleep {
		return errAsleep
	}
	return nil
}

func (e *Engine) CreateGuest(_ context.Context, s driver.GuestSpec) (driver.Guest, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return driver.Guest{}, err
	}
	if g, ok := e.state.Guests[s.ID]; ok {
		return e.clone(g), nil
	}
	switch s.Kind {
	case "container":
		if !e.has(driver.KindContainer) {
			return driver.Guest{}, fmt.Errorf("%w: this zone runs no containers", driver.ErrRefused)
		}
	case "vm":
		if !e.has(driver.KindVM) {
			return driver.Guest{}, fmt.Errorf("%w: this zone runs no VMs", driver.ErrRefused)
		}
	default:
		return driver.Guest{}, fmt.Errorf("%w: no kind %q", driver.ErrRefused, s.Kind)
	}
	e.state.Seq++
	name := s.Name
	if name == "" {
		name = s.ID
	}
	if s.CPULimit > 0 && !e.has(driver.ResizeLiveCPUCap) {
		return driver.Guest{}, fmt.Errorf("%w: this zone caps no CPU", driver.ErrRefused)
	}
	switch {
	case s.CPU != "" && s.CPU != driver.CPUModelHost:
		return driver.Guest{}, fmt.Errorf("%w: no processor %q: %s, or none", driver.ErrRefused, s.CPU, driver.CPUModelHost)
	case s.CPU != "" && (s.Kind != "vm" || !e.has(driver.CPUHost)):
		return driver.Guest{}, fmt.Errorf("%w: this zone gives no %s its host's processor", driver.ErrRefused, s.Kind)
	case s.Virtualization && (s.Kind != "vm" || !e.has(driver.CPUNested)):
		return driver.Guest{}, fmt.Errorf("%w: this zone lets no %s run VMs of its own", driver.ErrRefused, s.Kind)
	case driver.WeightOf(s.CPUWeight) != driver.FullWeight && !e.has(driver.CPUWeight):
		return driver.Guest{}, fmt.Errorf("%w: this zone weighs no CPU", driver.ErrRefused)
	case s.CPUWeight < 0 || s.CPUWeight > driver.FullWeight:
		return driver.Guest{}, fmt.Errorf("%w: a weight of %d", driver.ErrRefused, s.CPUWeight)
	}
	g := &driver.Guest{
		ID: s.ID, EngineRef: fmt.Sprintf("fake-%d", e.state.Seq), Kind: s.Kind, Name: name, Node: "fake",
		Label: s.Label,
		Cores: s.Cores, MemoryMB: s.MemoryMB, DiskGB: s.DiskGB, Running: !s.Stopped, Tags: maps.Clone(s.Tags),
		Holds: sortedHolds(s.Holds), CPULimit: s.CPULimit,
		CPU: s.CPU, Virtualization: s.Virtualization, CPUWeight: driver.WeightOf(s.CPUWeight),
	}
	if g.Running {
		at := e.now()
		g.StartedAt = &at
	}
	if e.has(driver.NetFirewall) {
		// derived from the one number the engine hands out, as a real one's
		g.Address = fmt.Sprintf("203.0.113.%d", (e.state.Seq-1)%254+1)
		g.Wall = driver.WallExact
	}
	e.state.Guests[s.ID] = g
	if e.state.Specs == nil {
		e.state.Specs = map[string]driver.GuestSpec{}
	}
	e.state.Specs[s.ID] = s
	return e.clone(g), e.save()
}

func (e *Engine) Guest(_ context.Context, id string) (driver.Guest, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return driver.Guest{}, err
	}
	g, ok := e.state.Guests[id]
	if !ok {
		return driver.Guest{}, driver.ErrNotFound
	}
	return e.clone(g), nil
}

func (e *Engine) Guests(_ context.Context) ([]driver.Guest, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return nil, err
	}
	out := make([]driver.Guest, 0, len(e.state.Guests))
	for _, g := range e.state.Guests {
		out = append(out, e.clone(g))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (e *Engine) DeleteGuest(_ context.Context, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return err
	}
	if held := e.volumesOn(id); len(held) > 0 {
		return fmt.Errorf("%w: it holds %s: detach them first — they keep their data", driver.ErrRefused, strings.Join(held, ", "))
	}
	delete(e.state.Guests, id)
	delete(e.state.Specs, id)
	return e.save()
}

func (e *Engine) SetPower(_ context.Context, id string, on bool) (driver.Guest, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return driver.Guest{}, err
	}
	g, ok := e.state.Guests[id]
	if !ok {
		return driver.Guest{}, driver.ErrNotFound
	}
	switch {
	case on && !g.Running:
		at := e.now()
		g.StartedAt = &at
	case !on:
		g.StartedAt = nil
	}
	g.Running = on
	return e.clone(g), e.save()
}

func (e *Engine) Reboot(_ context.Context, id string) (driver.Guest, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return driver.Guest{}, err
	}
	g, ok := e.state.Guests[id]
	if !ok {
		return driver.Guest{}, driver.ErrNotFound
	}
	if !g.Running {
		return driver.Guest{}, fmt.Errorf("%w: a stopped guest does not reboot", driver.ErrRefused)
	}
	return e.clone(g), nil
}

// Traits: every kind takes user data and changes live, memory down only
// where the zone's flags say so.
func (e *Engine) Traits(string) driver.Traits {
	return driver.Traits{UserData: true, LiveCores: true, LiveMemoryUp: true, LiveMemoryDown: e.has(driver.ResizeLiveMemoryDown)}
}

func (e *Engine) ResizeGuest(_ context.Context, id string, cores, memoryMB int) (driver.Guest, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return driver.Guest{}, err
	}
	g, ok := e.state.Guests[id]
	if !ok {
		return driver.Guest{}, driver.ErrNotFound
	}
	if g.Running && memoryMB < g.MemoryMB && !e.has(driver.ResizeLiveMemoryDown) {
		return driver.Guest{}, fmt.Errorf("%w: a running guest's memory only grows here", driver.ErrRefused)
	}
	if g.Running && g.MemoryUsedMB > 0 && memoryMB < g.MemoryMB && memoryMB < g.MemoryUsedMB+64 {
		return driver.Guest{}, fmt.Errorf("%w: it holds %d MB now; its memory goes no lower than %d MB while it runs",
			driver.ErrRefused, g.MemoryUsedMB, g.MemoryUsedMB+64)
	}
	g.Cores, g.MemoryMB = cores, memoryMB
	return e.clone(g), e.save()
}

func (e *Engine) SetCPULimit(_ context.Context, id string, cores int) (driver.Guest, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return driver.Guest{}, err
	}
	g, ok := e.state.Guests[id]
	if !ok {
		return driver.Guest{}, driver.ErrNotFound
	}
	if !e.has(driver.ResizeLiveCPUCap) {
		return driver.Guest{}, fmt.Errorf("%w: this zone caps no CPU", driver.ErrRefused)
	}
	g.CPULimit = cores
	return e.clone(g), e.save()
}

func (e *Engine) SetCPUWeight(_ context.Context, id string, weight int) (driver.Guest, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return driver.Guest{}, err
	}
	g, ok := e.state.Guests[id]
	if !ok {
		return driver.Guest{}, driver.ErrNotFound
	}
	if !e.has(driver.CPUWeight) {
		return driver.Guest{}, fmt.Errorf("%w: this zone weighs no CPU", driver.ErrRefused)
	}
	if weight < 0 || weight > driver.FullWeight {
		return driver.Guest{}, fmt.Errorf("%w: a weight of %d", driver.ErrRefused, weight)
	}
	g.CPUWeight = driver.WeightOf(weight)
	return e.clone(g), e.save()
}

func (e *Engine) Relabel(_ context.Context, id, label string) (driver.Guest, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return driver.Guest{}, err
	}
	g, ok := e.state.Guests[id]
	if !ok {
		return driver.Guest{}, driver.ErrNotFound
	}
	g.Label = label
	return e.clone(g), e.save()
}

func (e *Engine) Retag(_ context.Context, id string, tags map[string]string, holds []string) (driver.Guest, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return driver.Guest{}, err
	}
	g, ok := e.state.Guests[id]
	if !ok {
		return driver.Guest{}, driver.ErrNotFound
	}
	g.Tags, g.Holds = maps.Clone(tags), sortedHolds(holds)
	return e.clone(g), e.save()
}

func sortedHolds(h []string) []string {
	if len(h) == 0 {
		return nil
	}
	out := slices.Clone(h)
	slices.Sort(out)
	return slices.Compact(out)
}

// ---- The watcher facet ------------------------------------------------------

// GuestRunning reads a watched guest's power from the file; a guest the zone
// was not asked to watch is refused, as a fenced engine would.
func (e *Engine) GuestRunning(_ context.Context, ref string) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !slices.Contains(e.watch, ref) {
		return false, fmt.Errorf("%w: guest %s is not one this zone watches", driver.ErrRefused, ref)
	}
	if err := e.load(); err != nil {
		return false, err
	}
	if e.state.WatchFails {
		return false, errors.New("the engine did not answer")
	}
	return e.state.Watched[ref], nil
}

func (e *Engine) NodeDown(_ context.Context, node string) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.load(); err != nil {
		return false, err
	}
	return e.state.NodesDown[node], nil
}

func (e *Engine) Awake(context.Context) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.load(); err != nil {
		return false, err
	}
	return !e.state.Asleep, nil
}

// ---- The activity facet -----------------------------------------------------

// QuietFor: how long a running guest has been quiet — since it started, or
// since the file's busy_until — by the engine's clock, no further back than
// window. The thresholds are not read: the file says when it was busy.
func (e *Engine) QuietFor(_ context.Context, id string, window time.Duration, _ driver.Quiet) (time.Duration, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return 0, err
	}
	if e.state.ActivityFails {
		return 0, errors.New("the engine's history did not answer")
	}
	g, ok := e.state.Guests[id]
	if !ok {
		return 0, driver.ErrNotFound
	}
	if !g.Running || g.StartedAt == nil {
		return 0, nil
	}
	since := *g.StartedAt
	if busy, ok := e.state.BusyUntil[id]; ok && busy.After(since) {
		since = busy
	}
	return max(0, min(e.now().Sub(since), window)), nil
}

// Wall puts a guest back behind its wall, where the zone keeps one: pinned to
// the address it was given, or — a guest of the file that has none — to its
// zone's range.
func (e *Engine) Wall(_ context.Context, id string) ([]string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return nil, err
	}
	g, ok := e.state.Guests[id]
	if !ok {
		return nil, driver.ErrNotFound
	}
	want := driver.WallExact
	if g.Address == "" {
		want = driver.WallRange
	}
	if !e.has(driver.NetFirewall) || g.Wall == want {
		return nil, nil
	}
	g.Wall = want
	return []string{"card"}, e.save()
}

// ---- What tests do to the engine behind the registry's back ----------------

// Forget drops a guest as if someone deleted it on the engine directly.
func (e *Engine) Forget(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.load()
	delete(e.state.Guests, id)
	_ = e.save()
}

// Tamper changes a guest as if someone edited it on the engine directly.
func (e *Engine) Tamper(id string, f func(*driver.Guest)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.load()
	if g, ok := e.state.Guests[id]; ok {
		f(g)
		_ = e.save()
	}
}

// SetNow freezes the engine's clock at a time; hours pass when a test moves it.
func (e *Engine) SetNow(at time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.load()
	at = at.UTC()
	e.state.Now = &at
	_ = e.save()
}

// SetBusy says up to when a guest was last busy, by the engine's clock.
func (e *Engine) SetBusy(id string, until time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.load()
	if e.state.BusyUntil == nil {
		e.state.BusyUntil = map[string]time.Time{}
	}
	e.state.BusyUntil[id] = until.UTC()
	_ = e.save()
}

// FailActivity makes the guests' history unreadable, or readable again.
func (e *Engine) FailActivity(fails bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.load()
	e.state.ActivityFails = fails
	_ = e.save()
}

// Spec returns what a guest was created with.
func (e *Engine) Spec(id string) (driver.GuestSpec, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.load()
	s, ok := e.state.Specs[id]
	return s, ok
}

// FailNext makes the next call return err.
func (e *Engine) FailNext(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.failNext = err
}

// clone is a guest as a reading: a copy, stamped with the engine's clock.
// Called with e.mu held.
func (e *Engine) clone(g *driver.Guest) driver.Guest {
	c := *g
	c.Tags = maps.Clone(g.Tags)
	c.Holds = slices.Clone(g.Holds)
	if g.StartedAt != nil {
		at := *g.StartedAt
		c.StartedAt = &at
	}
	c.At = e.now()
	return c
}

var (
	_ driver.Guests   = (*Engine)(nil)
	_ driver.Watcher  = (*Engine)(nil)
	_ driver.Volumes  = (*Engine)(nil)
	_ driver.Activity = (*Engine)(nil)
	_ driver.Walls    = (*Engine)(nil)
)
