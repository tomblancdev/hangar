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
	"strings"
	"sync"
	"time"
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
	// Watch: guests outside the fence the zone's reservations name; the
	// driver may read their power (Watcher), and nothing more.
	Watch []string
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
	// DeleteGuest removes it; a guest already gone is not an error. A guest
	// that holds volumes (the Volumes facet) is ErrRefused: deleting it would
	// take their data with it.
	DeleteGuest(ctx context.Context, id string) error
	// SetPower starts it, or stops it the way a person would (the guest is
	// asked to shut down, and made to after a while).
	SetPower(ctx context.Context, id string, on bool) (Guest, error)
	// Reboot restarts a running guest.
	Reboot(ctx context.Context, id string) (Guest, error)
	// ResizeGuest sets cores and memory. What a RUNNING guest can change is
	// what Traits says for its kind; anything else is ErrRefused.
	ResizeGuest(ctx context.Context, id string, cores, memoryMB int) (Guest, error)
	// SetCPULimit caps the CPU time a guest may use to that many cores' worth,
	// at once, running or not; 0 lifts the cap. Where the engine lacks
	// resize.live.cpu_cap it is ErrRefused.
	SetCPULimit(ctx context.Context, id string, cores int) (Guest, error)
	// Retag writes a guest's tags and holds anew (the core's id stays).
	Retag(ctx context.Context, id string, tags map[string]string, holds []string) (Guest, error)
	// Traits says what a guest of this kind can take on this engine.
	Traits(kind string) Traits
}

// Watcher is the facet of a driver that reads what a zone's reservations wait
// on: a guest outside its fence that it was allowed to watch, a node of the
// engine, and whether the zone itself answers. A driver that cannot read them
// does not implement it; a zone of it then keeps no conditional reservation.
type Watcher interface {
	// GuestRunning: whether a watched guest runs now, read live.
	GuestRunning(ctx context.Context, ref string) (bool, error)
	// NodeDown: whether a node of the engine is down.
	NodeDown(ctx context.Context, node string) (bool, error)
	// Awake: whether the zone's engine answers and the node its guests run
	// on is up. A zone that sleeps is not awake; an error is not either.
	Awake(ctx context.Context) (bool, error)
}

// Traits is what a guest of one kind can take on one engine — finer than a
// capability flag, which speaks for the whole engine.
type Traits struct {
	// UserData: its first boot runs user data (cloud-init).
	UserData bool `json:"user_data"`
	// What a RUNNING guest can change without being stopped.
	LiveCores      bool `json:"live_cores"`
	LiveMemoryUp   bool `json:"live_memory_up"`
	LiveMemoryDown bool `json:"live_memory_down"`
}

// GuestSpec is what a guest is created with.
type GuestSpec struct {
	ID   string // the core's resource id, written on the guest
	Kind string // "container" or "vm"
	// Name is its host name; the id when empty.
	Name     string
	Cores    int
	MemoryMB int
	// DiskGB is its root disk; 0 = the image's own size.
	DiskGB int
	// Image is the engine's own name for what the guest starts from, as the
	// operator declared it for this kind (a template, an archive, an AMI).
	// Empty = a blank guest, where the engine has such a thing.
	Image string
	// SSHKeys are public keys, in OpenSSH's one-line form, the guest lets in.
	SSHKeys []string
	// UserData is handed to its first boot, where Traits says it takes any.
	UserData []byte
	Tags     map[string]string
	// Holds: the keys of the reservations holding its room back, written on
	// it for a node that acts without the brain to read.
	Holds []string
	// CPULimit: a cap from birth (cores' worth; 0 = none).
	CPULimit int
	// Stopped: create it without starting it.
	Stopped bool
}

// Guest is a guest as the engine reports it.
type Guest struct {
	ID        string            `json:"id"`
	EngineRef string            `json:"engine_ref"` // the engine's own name for it
	Kind      string            `json:"kind"`
	Name      string            `json:"name,omitempty"`
	Node      string            `json:"node,omitempty"`
	Cores     int               `json:"cores"`
	MemoryMB  int               `json:"memory_mb"`
	DiskGB    int               `json:"disk_gb,omitempty"`
	Running   bool              `json:"running"`
	Addresses []string          `json:"addresses,omitempty"`
	Tags      map[string]string `json:"tags,omitempty"`
	// Holds: the reservations' keys written on it (see GuestSpec.Holds).
	Holds []string `json:"holds,omitempty"`
	// CPULimit: its CPU cap in cores' worth; 0 = none.
	CPULimit int `json:"cpu_limit,omitempty"`
	// MemoryUsedMB: what a running guest holds now, where the engine says
	// (a limit written below it is refused); 0 = unknown or stopped.
	MemoryUsedMB int `json:"memory_used_mb,omitempty"`
}

// ---- The volumes facet ------------------------------------------------------

// A volume's content, fixed at its birth: a disk its VM formats itself, or a
// directory its container mounts at a path. An engine keeps them apart.
const (
	ContentBlock      = "block"
	ContentFilesystem = "filesystem"
)

// Volumes is the facet of a driver whose disks outlive their guest. A volume
// is made, placed on a guest or parked, grown and deleted — idempotent on the
// core's id, like a guest. Where the engine keeps no disk without a guest,
// the driver parks one on a stopped « shelf » guest of its owner, which it
// makes, finds and never starts; a plugin never sees a shelf.
type Volumes interface {
	// CreateVolume makes a volume where spec.At says: on a guest, or parked.
	// A second call finds the volume the first one made.
	CreateVolume(ctx context.Context, spec VolumeSpec) (Volume, error)
	// Volume finds a volume by the id the core minted for it.
	Volume(ctx context.Context, id string) (Volume, error)
	// PlaceVolume puts a volume where at says — on a guest (by the core's
	// id), from its shelf or from another guest, or back on its owner's
	// shelf (at.Guest "") — keeping its data, its size and its backup flag.
	// What a running guest cannot let go of, or take, is ErrRefused, in
	// words; so is a guest of the other content's kind.
	PlaceVolume(ctx context.Context, id string, at Place) (Volume, error)
	// ResizeVolume grows it to sizeGB; a volume never shrinks (ErrRefused).
	ResizeVolume(ctx context.Context, id string, sizeGB int) (Volume, error)
	// SetVolumeBackup says whether the engine's backups take it.
	SetVolumeBackup(ctx context.Context, id string, backup bool) (Volume, error)
	// DeleteVolume destroys a parked volume. One on a guest is ErrRefused;
	// one already gone is not an error.
	DeleteVolume(ctx context.Context, id string) error
	// CanPark says why this zone cannot keep a volume of that content on no
	// guest (nil: it can).
	CanPark(content string) error
}

// Place is where a volume goes.
type Place struct {
	// Guest: the core's id of the guest it is plugged into; "" = parked.
	Guest string
	// Mount: a filesystem volume's path in its container.
	Mount string
	// Owner: whose shelf it parks on — an opaque key, the same for every
	// volume of one owner; a volume in transit may rest there too.
	Owner string
}

// VolumeSpec is what a volume is created with.
type VolumeSpec struct {
	ID      string // the core's resource id, written where the engine keeps it
	Content string // ContentBlock or ContentFilesystem
	SizeGB  int
	Backup  bool
	At      Place
}

// Volume is a volume as the engine reports it.
type Volume struct {
	ID string `json:"id"`
	// EngineRef: the engine's own name for it now — it may change as the
	// volume moves (on Proxmox VE a disk is named after its guest).
	EngineRef string `json:"engine_ref"`
	Content   string `json:"content"`
	SizeGB    int    `json:"size_gb"`
	Backup    bool   `json:"backup"`
	// Guest: the core's id of the guest it is plugged into; "" = parked.
	Guest string `json:"guest,omitempty"`
	Mount string `json:"mount,omitempty"`
	// Device: where it is plugged on the guest (or on its shelf).
	Device string `json:"device,omitempty"`
	Node   string `json:"node,omitempty"`
	// InGuest: how its guest finds it — a block volume's stable device path,
	// a filesystem volume's mount.
	InGuest string `json:"in_guest,omitempty"`
}

// SerialOf is the serial number a block volume shows its guest: the id
// without its dash (vol-0123… → vol0123…), 20 characters at most — AWS's own
// form for its volumes, and all a disk's serial holds.
func SerialOf(id string) string {
	s := strings.ReplaceAll(id, "-", "")
	if len(s) > 20 {
		s = s[:20]
	}
	return s
}

// ---- The images facet -------------------------------------------------------

// An image's states, as its engine reports them.
const (
	// ImagePending: its bake runs (its builder is at work).
	ImagePending = "pending"
	// ImageWaiting: its bake waits to start over — the zone's room was
	// needed, or its builder stopped before it finished.
	ImageWaiting = "waiting"
	// ImageAvailable: machines may be born from it.
	ImageAvailable = "available"
	// ImageFailed: its bake ended in errors; Detail says which, in the
	// words of the guest's own first boot.
	ImageFailed = "failed"
)

// Images is the facet of a driver that makes images: frozen system disks
// that guests are born from (GuestSpec.Image names one by its Ref).
//
// A bake takes minutes, so it is not one call: Bake moves it forward as far
// as it goes at once and says where it stands — the plugin calls it at the
// create and at every reconcile after, and keeps nothing but the attempt's
// number; everything else is on the engine. A save is one call.
type Images interface {
	// ImageKinds are the kinds of guest this engine makes images for.
	ImageKinds() []string
	// Bake moves a bake forward: it makes and starts its builder (a guest
	// that boots the base once, with the recipe as its first boot), reads
	// how that first boot went, and makes the image when it finished clean
	// — or says why not (ImageFailed; the builder is gone). Held: the zone's
	// room is needed — a builder at work is let go at once and the bake
	// waits (ImageWaiting), to start over from the beginning when called
	// unheld. A builder that stopped before it finished starts over too.
	// Idempotent on spec.ID and spec.Attempt.
	Bake(ctx context.Context, spec BakeSpec, held bool) (Image, error)
	// SaveImage makes an image of a stopped guest's system disk (guest: the
	// core's id), idempotent on id. A running guest, or one holding volumes
	// (an image never carries someone's data), is ErrRefused.
	SaveImage(ctx context.Context, id, guest string) (Image, error)
	// Image reads an image, or its bake, by the core's id.
	Image(ctx context.Context, id string) (Image, error)
	// DeleteImage removes an image, or its bake; one already gone is not an
	// error. An image guests born from it still use (a clone that shares
	// its disk) is ErrRefused, naming them where the engine can.
	DeleteImage(ctx context.Context, id string) error
}

// BakeSpec is what a bake makes an image from.
type BakeSpec struct {
	ID string // the core's resource id, written on the builder and on the image
	// Attempt: the bake's number (a failed one baked again is the next): a
	// builder of another attempt is not this one's.
	Attempt int
	Kind    string // the kind of guest the image is for ("vm")
	// Base: the engine's own name of what the builder starts from (Proxmox:
	// a disk image on an import storage, or a template's name).
	Base     string
	DiskGB   int
	Cores    int
	MemoryMB int
	// UserData: the builder's first boot — the recipe's; the driver adds its
	// own last step, which makes the disk ready to be cloned.
	UserData []byte
	// Timeout: how long the builder may work before its bake is failed.
	Timeout time.Duration
	// Tags: written on the builder for the engine's node to read (a builder
	// borrows its room: class spot, the memory admitted).
	Tags map[string]string
}

// Image is an image as the engine reports it.
type Image struct {
	ID        string `json:"id"`
	EngineRef string `json:"engine_ref"`
	Kind      string `json:"kind"`
	Node      string `json:"node,omitempty"`
	State     string `json:"state"`
	Detail    string `json:"detail,omitempty"`
	// Ref: the engine's own name a guest is born from (GuestSpec.Image).
	Ref     string `json:"ref,omitempty"`
	SizeGB  int    `json:"size_gb,omitempty"`
	Attempt int    `json:"attempt,omitempty"`
}
