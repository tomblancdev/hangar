// Package driver is how a plugin reaches an engine — a hypervisor, a cloud,
// or the fake one the tests run on.
//
// A driver advertises CAPABILITY FLAGS. Plugins and the core decide with the
// flags, never with the engine's name: "can this zone shrink a running
// guest's memory?" is `resize.live.memory_down`, whatever answers it.
//
// Drivers register themselves by name (an import for its side effect, the way
// database/sql drivers do) and a plugin opens one per zone with that zone's
// endpoint, options and the plugin's own credential.
package driver

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
)

// Capability is a flag a driver sets when its engine can do the thing.
type Capability = string

// The flags ARCHITECTURE.md's driver table names. A driver may only advertise
// these; a plugin may only require these.
const (
	KindContainer           Capability = "kind.container"
	KindVM                  Capability = "kind.vm"
	ResizeLiveMemoryDown    Capability = "resize.live.memory_down"
	ResizeLiveCPUCap        Capability = "resize.live.cpu_cap"
	VolumeMoveBetweenGuests Capability = "volume.move_between_guests"
	GuestSuspendToDisk      Capability = "guest.suspend_to_disk"
	GuestTags               Capability = "guest.tags"
	HookPreStart            Capability = "hook.pre_start"
	GPUShared               Capability = "gpu.shared"
	GPUPassthrough          Capability = "gpu.passthrough"
	FencePool               Capability = "fence.pool"
)

// Known lists every flag, in the order the documentation gives them.
var Known = []Capability{
	KindContainer, KindVM, ResizeLiveMemoryDown, ResizeLiveCPUCap,
	VolumeMoveBetweenGuests, GuestSuspendToDisk, GuestTags, HookPreStart,
	GPUShared, GPUPassthrough, FencePool,
}

// IsKnown reports whether c is one of the documented flags.
func IsKnown(c Capability) bool { return slices.Contains(Known, c) }

// Missing returns the flags of need that have does not carry, sorted.
func Missing(need, have []Capability) []Capability {
	var out []Capability
	for _, n := range need {
		if !slices.Contains(have, n) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// Params is what a plugin opens a driver with, for one zone.
type Params struct {
	Zone       string
	Endpoint   string
	Options    map[string]string
	Credential []byte
}

// Driver is an open connection to one zone's engine.
type Driver interface {
	// Capabilities is what this engine, reached this way, can do.
	Capabilities() []Capability
	Close() error
}

// Opener opens a driver for one zone.
type Opener func(ctx context.Context, p Params) (Driver, error)

var (
	mu      sync.RWMutex
	openers = map[string]Opener{}
)

// Register makes a driver available by name. It panics on a second
// registration of a name: two drivers answering to one name is a build bug.
func Register(name string, open Opener) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := openers[name]; dup {
		panic("driver: registered twice: " + name)
	}
	openers[name] = open
}

// Open opens the named driver for one zone.
func Open(ctx context.Context, name string, p Params) (Driver, error) {
	mu.RLock()
	open, ok := openers[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("no driver named %q (this plugin carries %v)", name, Names())
	}
	d, err := open(ctx, p)
	if err != nil {
		return nil, err
	}
	for _, c := range d.Capabilities() {
		if !IsKnown(c) {
			_ = d.Close()
			return nil, fmt.Errorf("driver %q advertises an unknown capability %q", name, c)
		}
	}
	return d, nil
}

// Names lists the registered drivers.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(openers))
	for n := range openers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ---- The guests facet -------------------------------------------------------

// ErrNotFound: the engine has no such guest (or the driver's fence hides it).
var ErrNotFound = errors.New("not found")

// ErrRefused: the engine will not do it in the guest's present state.
var ErrRefused = errors.New("refused by the engine")

// Guests is the facet of a driver that runs guests: containers and VMs. A
// driver that runs none does not implement it.
type Guests interface {
	// CreateGuest is idempotent on spec.ID: a second call finds the guest the
	// first one made (by the id written on it) and returns it.
	CreateGuest(ctx context.Context, spec GuestSpec) (Guest, error)
	// Guest finds a guest by the id the core minted for it.
	Guest(ctx context.Context, id string) (Guest, error)
	// Guests lists every guest the driver's fence lets it see.
	Guests(ctx context.Context) ([]Guest, error)
	// DeleteGuest removes it; a guest already gone is not an error.
	DeleteGuest(ctx context.Context, id string) error
	SetPower(ctx context.Context, id string, on bool) (Guest, error)
	// ResizeGuest sets cores and memory. A running guest's memory goes down
	// only where the engine has resize.live.memory_down (else ErrRefused).
	ResizeGuest(ctx context.Context, id string, cores, memoryMB int) (Guest, error)
}

// GuestSpec is what a guest is created with.
type GuestSpec struct {
	ID       string // the core's resource id, written on the guest
	Kind     string // "container" or "vm"
	Cores    int
	MemoryMB int
	Tags     map[string]string
}

// Guest is a guest as the engine reports it.
type Guest struct {
	ID        string            `json:"id"`
	EngineRef string            `json:"engine_ref"` // the engine's own name for it
	Kind      string            `json:"kind"`
	Cores     int               `json:"cores"`
	MemoryMB  int               `json:"memory_mb"`
	Running   bool              `json:"running"`
	Tags      map[string]string `json:"tags,omitempty"`
}
