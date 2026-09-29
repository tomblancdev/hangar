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
// Zone options:
//
//	capabilities              comma-separated flags; default: every documented
//	                          flag but the GPU ones
//	expect_credential_sha256  refuse to open unless the plugin's credential
//	                          hashes to this (proves the credential arrived)
package fake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/tomblancdev/hangar/driver"
)

// Name is the name the fake registers under.
const Name = "fake"

func init() { driver.Register(Name, Open) }

// Engine is one fake zone.
type Engine struct {
	mu       sync.Mutex
	path     string // "" = memory only
	caps     []driver.Capability
	state    state
	failNext error
}

type state struct {
	Seq    int                      `json:"seq"`
	Guests map[string]*driver.Guest `json:"guests"`
}

// Open opens a fake zone. The endpoint is empty (memory) or a file path.
func Open(_ context.Context, p driver.Params) (driver.Driver, error) {
	if want := p.Options["expect_credential_sha256"]; want != "" {
		sum := sha256.Sum256(p.Credential)
		if hex.EncodeToString(sum[:]) != strings.ToLower(want) {
			return nil, fmt.Errorf("fake zone %s: the credential is not the one this zone expects", p.Zone)
		}
	}
	e := &Engine{state: state{Guests: map[string]*driver.Guest{}}}
	e.caps = defaultCaps()
	if list := p.Options["capabilities"]; list != "" {
		e.caps = nil
		for _, c := range strings.Split(list, ",") {
			if c = strings.TrimSpace(c); c != "" {
				e.caps = append(e.caps, c)
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
// so every call starts from the engine as it is. Called with e.mu held.
func (e *Engine) fail() error {
	if err := e.failNext; err != nil {
		e.failNext = nil
		return err
	}
	return e.load()
}

func (e *Engine) CreateGuest(_ context.Context, s driver.GuestSpec) (driver.Guest, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return driver.Guest{}, err
	}
	if g, ok := e.state.Guests[s.ID]; ok {
		return clone(g), nil
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
	g := &driver.Guest{
		ID: s.ID, EngineRef: fmt.Sprintf("fake-%d", e.state.Seq), Kind: s.Kind,
		Cores: s.Cores, MemoryMB: s.MemoryMB, Running: true, Tags: maps.Clone(s.Tags),
	}
	e.state.Guests[s.ID] = g
	return clone(g), e.save()
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
	return clone(g), nil
}

func (e *Engine) Guests(_ context.Context) ([]driver.Guest, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return nil, err
	}
	out := make([]driver.Guest, 0, len(e.state.Guests))
	for _, g := range e.state.Guests {
		out = append(out, clone(g))
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
	delete(e.state.Guests, id)
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
	g.Running = on
	return clone(g), e.save()
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
	g.Cores, g.MemoryMB = cores, memoryMB
	return clone(g), e.save()
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

// FailNext makes the next call return err.
func (e *Engine) FailNext(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.failNext = err
}

func clone(g *driver.Guest) driver.Guest {
	c := *g
	c.Tags = maps.Clone(g.Tags)
	return c
}

var _ driver.Guests = (*Engine)(nil)
