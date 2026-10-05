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
	"io"
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
	CPUHost                 Capability = "cpu.host"
	CPUNested               Capability = "cpu.nested"
	CPUWeight               Capability = "cpu.weight"
	VolumeMoveBetweenGuests Capability = "volume.move_between_guests"
	GuestSuspendToDisk      Capability = "guest.suspend_to_disk"
	GuestTags               Capability = "guest.tags"
	GuestActivity           Capability = "guest.activity"
	GuestConsole            Capability = "guest.console"
	HookPreStart            Capability = "hook.pre_start"
	GPUShared               Capability = "gpu.shared"
	GPUPassthrough          Capability = "gpu.passthrough"
	FencePool               Capability = "fence.pool"
	NetFirewall             Capability = "net.firewall"
	NetPrivate              Capability = "net.private"
)

// Known lists every flag, in the order the documentation gives them.
var Known = []Capability{
	KindContainer, KindVM, ResizeLiveMemoryDown, ResizeLiveCPUCap, CPUHost, CPUNested, CPUWeight,
	VolumeMoveBetweenGuests, GuestSuspendToDisk, GuestTags, GuestActivity, GuestConsole, HookPreStart,
	GPUShared, GPUPassthrough, FencePool, NetFirewall, NetPrivate,
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

// A driver may say whether a zone's options are sound without reaching its
// engine: what `hangar check` asks before a config lands, with no network.
var checkers = map[string]func(Params) error{}

// RegisterCheck makes a driver's own check of a zone's options available by
// the driver's name.
func RegisterCheck(name string, check func(Params) error) {
	mu.Lock()
	defer mu.Unlock()
	checkers[name] = check
}

// Check asks the named driver whether a zone's options are sound, the engine
// not reached. A driver that registered no check says nothing (nil).
func Check(name string, p Params) error {
	mu.RLock()
	check := checkers[name]
	mu.RUnlock()
	if check == nil {
		return nil
	}
	return check(p)
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
	// SetCPUWeight sets a guest's share of the cores when they are contended
	// (1 to 100; 100, or 0, a full share), at once, running or not. Where the
	// engine lacks cpu.weight it is ErrRefused.
	SetCPUWeight(ctx context.Context, id string, weight int) (Guest, error)
	// Retag writes a guest's tags and holds anew (the core's id stays).
	Retag(ctx context.Context, id string, tags map[string]string, holds []string) (Guest, error)
	// Relabel writes a guest's label anew (see GuestSpec.Label); "" takes it
	// off. A label is for people: no guest is ever found by it.
	Relabel(ctx context.Context, id, label string) (Guest, error)
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

// Activity is the facet of a driver whose engine keeps its own history of
// each guest's CPU and network (guest.activity): what a machine's idle_after
// is read from, with nothing installed in the guest. A driver that has none
// does not implement it, and no machine is stopped for idleness in its zones.
type Activity interface {
	// QuietFor: how long a running guest has stayed quiet up to its newest
	// reading — its CPU and what it sends under q, sample after sample — as
	// far back as window asks (and no further). Zero: it is not quiet now, or
	// the history cannot say (no reading yet, a hole, a guest that does not
	// run) — what cannot be read is never taken for idleness.
	QuietFor(ctx context.Context, id string, window time.Duration, q Quiet) (time.Duration, error)
}

// Consoles is the facet of a driver whose engine carries a guest's own
// console — its screen and keyboard: a VM's serial port — to whoever the
// driver's credential lets open it (guest.console). Nothing runs in the
// guest for it and nothing of its network is used: it is the way into a
// guest that lets no key in. A driver whose engine has none does not
// implement it.
type Consoles interface {
	// Console opens a running guest's console. A guest that does not run, or
	// whose kind has none here (Traits), is ErrRefused. An engine serves one
	// console per guest at a time: who holds the driver keeps it to one.
	Console(ctx context.Context, id string, size ConsoleSize) (Console, error)
}

// Console is a guest's console, held open. Read gives what the guest's
// console says, as it comes; Write sends what is typed. Read ends with
// ErrConsoleStopped once the guest no longer runs, and with
// ErrConsoleRestarted once it runs anew — stopped and started, or rebooted
// by its engine: the port's other end is then another process, and this
// console is open on nothing (an engine does not always say either itself:
// the driver looks). It ends with io.EOF when the engine let go, and Close
// lets go of it from this side (a Read in flight then ends).
type Console interface {
	io.ReadWriteCloser
	// Resize tells the engine the window changed. A guest's serial port
	// carries no window size: the guest learns its grid by asking the
	// terminal at the other end, when its login starts.
	Resize(size ConsoleSize) error
}

// ConsoleSize is a terminal's grid, in characters; zero = unknown.
type ConsoleSize struct {
	Cols, Rows int
}

// ErrConsoleStopped: the guest a console was open on no longer runs.
var ErrConsoleStopped = errors.New("the guest stopped")

// ErrConsoleRestarted: the guest a console was open on runs anew; its
// console is to be opened again.
var ErrConsoleRestarted = errors.New("the guest was started again")

// Walls is the facet of a driver whose engine filters a guest's own network
// card (net.firewall): where the zone says so, every guest is born behind a
// wall — nothing in but what the zone's operator lets every guest hear, and
// nothing out but as itself — written before its first start. A driver whose
// engine has none does not implement it.
type Walls interface {
	// Wall brings a guest's wall to what its zone says and returns what it
	// had to put back, in the engine's own words (none: it stood). A zone
	// that keeps no wall has nothing to put back. Idempotent.
	Wall(ctx context.Context, id string) ([]string, error)
}

// WaysOut is the facet of a driver whose guests reach out through a guest of
// its own making — a network's gateway (net.private) — that runs only while a
// guest of its network does, so that a zone with nothing running can sleep.
// CreateGuest, SetPower and DeleteGuest keep it so as they go: the gateway
// started before its network's first guest starts, stopped after its last one
// stops. WayOut is the look that puts it back.
type WaysOut interface {
	// WayOut brings the gateway of a guest's network to what its guests need
	// — running while one of them runs, stopped once none does — and says
	// what it did: "started", "stopped", or "" (it was so already, or the
	// guest is on no network).
	WayOut(ctx context.Context, id string) (string, error)
}

// What a guest's wall pins it to (Guest.Wall).
const (
	// WallExact: it sends only as the address it was given.
	WallExact = "exact"
	// WallRange: it sends only from its zone's own range — a guest that asks
	// for a lease, born before its zone gave addresses: it cannot pose as
	// anything outside its lane, and may still take a neighbour's address.
	WallRange = "range"
)

// Quiet is what a guest stays under to count as idle, sample after sample of
// its engine's history.
type Quiet struct {
	// CPU: cores' worth (0.1 = a tenth of one core).
	CPU float64
	// SentBps: bytes a second it sends. What it receives is not counted: a
	// network's broadcasts reach every guest, whatever it does.
	SentBps float64
}

// CPUModelHost is the one processor a guest names (GuestSpec.CPU): its
// host's own, every instruction of it. A guest that names none sees the
// model its zone gives everyone.
const CPUModelHost = "host"

// FullWeight is a guest's whole share of the cores (GuestSpec.CPUWeight): a
// guest that names no weight has it.
const FullWeight = 100

// WeightOf reads a weight as a share: none named is a full one.
func WeightOf(w int) int {
	if w == 0 {
		return FullWeight
	}
	return w
}

// Traits is what a guest of one kind can take on one engine — finer than a
// capability flag, which speaks for the whole engine.
type Traits struct {
	// UserData: its first boot runs user data (cloud-init).
	UserData bool `json:"user_data"`
	// Console: its console can be opened (the Consoles facet) — and, where
	// its first boot runs user data, born signed in (GuestSpec.SignedIn).
	Console bool `json:"console"`
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
	Name string
	// Label is one line a person reads where the engine shows the guest (a
	// Proxmox guest's notes): what it is called, what it is and whose.
	Label    string
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
	// SignedIn: its console asks nothing — whoever opens it lands in a shell
	// as the guest's own user (guest.console, a kind whose Traits say both
	// Console and UserData). The driver adds the step that makes it so to
	// its first boot, beside UserData and without touching it (FirstBoot).
	SignedIn bool
	Tags     map[string]string
	// Holds: the keys of the reservations holding its room back, written on
	// it for a node that acts without the brain to read.
	Holds []string
	// CPULimit: a cap from birth (cores' worth; 0 = none).
	CPULimit int
	// CPU: the processor it sees — "" the model its zone gives everyone, or
	// CPUModelHost (cpu.host): its host's own. A VM's; it then runs on a
	// host of that kind only.
	CPU string
	// Virtualization: it may run VMs of its own (cpu.nested). Whatever its
	// processor, a guest that does not say so is given none of it.
	Virtualization bool
	// CPUWeight: its share of the cores when they are contended, 1 to 100
	// (cpu.weight); 0 = a full share. It loses nothing while cores are free.
	CPUWeight int
	// Network: the network it is on, by the core's id (net.private) — one
	// card, there and nowhere else, for its life; "" = the zone's own lane.
	Network string
	// Stopped: create it without starting it.
	Stopped bool
}

// Guest is a guest as the engine reports it.
type Guest struct {
	ID        string            `json:"id"`
	EngineRef string            `json:"engine_ref"` // the engine's own name for it
	Kind      string            `json:"kind"`
	Name      string            `json:"name,omitempty"`
	Label     string            `json:"label,omitempty"` // see GuestSpec.Label
	Node      string            `json:"node,omitempty"`
	Cores     int               `json:"cores"`
	MemoryMB  int               `json:"memory_mb"`
	DiskGB    int               `json:"disk_gb,omitempty"`
	Running   bool              `json:"running"`
	Addresses []string          `json:"addresses,omitempty"`
	Tags      map[string]string `json:"tags,omitempty"`
	// Holds: the reservations' keys written on it (see GuestSpec.Holds).
	Holds []string `json:"holds,omitempty"`
	// Address: the address it was given at its birth, where its zone gives
	// one — its own for its life, read on the engine, running or not; "" =
	// it asks its network for one (Addresses says which it got).
	Address string `json:"address,omitempty"`
	// Wall: it stands behind its engine's firewall, pinned to WallExact or
	// WallRange; "" = no wall (its zone keeps none, or its own is down).
	Wall string `json:"wall,omitempty"`
	// CPULimit: its CPU cap in cores' worth; 0 = none.
	CPULimit int `json:"cpu_limit,omitempty"`
	// CPU, Virtualization, CPUWeight: as GuestSpec's, read on the engine.
	CPU            string `json:"cpu,omitempty"`
	Virtualization bool   `json:"virtualization,omitempty"`
	CPUWeight      int    `json:"cpu_weight,omitempty"`
	// MemoryUsedMB: what a running guest holds now, where the engine says
	// (a limit written below it is refused); 0 = unknown or stopped.
	MemoryUsedMB int `json:"memory_used_mb,omitempty"`
	// StartedAt: since when a running guest runs, where the engine says; nil
	// = stopped, or unknown. The hours it ran are counted from it.
	StartedAt *time.Time `json:"started_at,omitempty"`
	// At: when this was read, by the clock the reading was made with.
	At time.Time `json:"-"`
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
	// RelabelVolume writes what the volume is called where the engine shows
	// it (see VolumeSpec.Label); "" takes it off.
	RelabelVolume(ctx context.Context, id, label string) (Volume, error)
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
	// Label is what its owner calls it, for a person reading the engine's
	// own screen; nothing is found by it.
	Label string
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
	Label     string `json:"label,omitempty"` // see VolumeSpec.Label
	// Guest: the core's id of the guest it is plugged into; "" = parked.
	Guest string `json:"guest,omitempty"`
	Mount string `json:"mount,omitempty"`
	// Device: where it is plugged on the guest (or on its shelf).
	Device string `json:"device,omitempty"`
	Node   string `json:"node,omitempty"`
	// InGuest: how its guest finds it — a block volume's device path where
	// it is plugged now, a filesystem volume's mount. What follows a block
	// volume from guest to guest is its serial (SerialOf).
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

// ---- The networks facet -----------------------------------------------------

// Networks is the facet of a driver that makes networks of the cloud's own
// (net.private): a private network only its guests are on, each given its
// address there at its birth, with one way out — a gateway the driver makes —
// that forwards what its guests send, lets nothing in, and lets the keys the
// network names jump through to its guests and nowhere else. A guest names
// its network at its birth (GuestSpec.Network). Idempotent on the core's id,
// like a guest.
type Networks interface {
	// CreateNetwork makes a network — its gateway, born stopped: it starts
	// with the network's first guest. A second call finds the first's.
	CreateNetwork(ctx context.Context, spec NetworkSpec) (Network, error)
	// Network reads a network by the id the core minted for it.
	Network(ctx context.Context, id string) (Network, error)
	// TendNetwork brings a network to its spec and returns what it had to put
	// back, in the engine's own words (none: it stood). A gateway keeps
	// nothing and is never patched: one born with other keys, or from another
	// archive than its zone's, is made again — at the same place, its guests'
	// way out cut for as long as that takes. was: the engine's own name for
	// its gateway as it was last read ("" = never): one that is gone is made
	// again there, the place its guests stand on.
	TendNetwork(ctx context.Context, spec NetworkSpec, was string) (Network, []string, error)
	// DeleteNetwork removes it; one already gone is not an error. A network a
	// guest is still on is ErrRefused, naming them.
	DeleteNetwork(ctx context.Context, id string) error
	// NetworkRoomMB is the memory a network's gateway holds while it runs: what
	// a network books in its zone (0: the engine's networks cost none).
	NetworkRoomMB() int
}

// NetworkSpec is what a network is made with.
type NetworkSpec struct {
	ID string // the core's resource id, written on the gateway
	// Label is one line a person reads where the engine shows the gateway.
	Label string
	// JumpKeys are the public keys, in OpenSSH's one-line form, that may jump
	// through its gateway to its guests.
	JumpKeys []string
}

// Network is a network as the engine reports it.
type Network struct {
	ID        string `json:"id"`
	EngineRef string `json:"engine_ref"` // the engine's own name for its gateway
	Node      string `json:"node,omitempty"`
	Label     string `json:"label,omitempty"` // see NetworkSpec.Label
	// Range: the addresses its guests are given theirs in ("203.0.113.0/27");
	// Gateway: their way out, the first of them.
	Range   string `json:"range"`
	Gateway string `json:"gateway"`
	// Jump: where its keys jump through — "jump@" and its gateway's address
	// on the zone's lane, as ssh's -J takes it.
	Jump string `json:"jump,omitempty"`
	// Wall: its gateway stands behind the engine's firewall (WallExact).
	Wall string `json:"wall,omitempty"`
	// Running: its gateway runs — as long as one of its guests does.
	Running bool `json:"running"`
	// Keys: how many keys its gateway was born with.
	Keys int `json:"keys"`
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
