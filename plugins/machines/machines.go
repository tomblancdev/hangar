// Package machines is the plugin that makes machines: containers and VMs on
// any driver with the guests facet, sized by AWS's instance type names (or
// the operator's aliases, or plain cores and memory), started from an image
// the operator names, with the owner's key pairs and — where the kind boots
// it — cloud-init user data.
//
// Two types:
//
//   - machine (m-…): create · start · stop · reboot · resize · delete.
//   - keypair (kp-…): a public key, imported. A pair is never generated
//     here: the brain would then hold a private key. A machine names its key
//     pairs by id ("x-hangar-ref"); the core checks they are the owner's own
//     and hands them over with the create.
//
// A machine takes room in its zone by its class (ARCHITECTURE.md §6):
// guaranteed (all its memory booked), spot (all borrowed: it stops when a
// reservation needs the room, and starts again when the room returns unless
// it says resume: false), or guaranteed+spot (a floor booked, the rest
// borrowed: it shrinks to its floor instead of stopping — only where a
// running guest of its kind gives memory back). cores_beside caps the CPU of
// the ones that stay while the room is held. The class, floor, cap and the
// size admitted are written on the guest as tags, for the engine's node to
// act on when the brain cannot be reached.
//
// It requires fence.pool: a zone whose credential reaches beyond the
// product's own guests is not one it will act on.
//
// Settings (the operator's, one JSON object):
//
//	images:                        # a name people ask for → the engine's own, per kind
//	  debian-13:
//	    vm: debian-13              #   Proxmox: a VM template, by name, in the images pool
//	    container: local:vztmpl/debian-13-standard_13.6-1_amd64.tar.zst
//	types:                         # aliases beside AWS's names
//	  dev: {cores: 12, memory_gb: 40}
package machines

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/tomblancdev/hangar/driver"
	_ "github.com/tomblancdev/hangar/driver/fake"
	_ "github.com/tomblancdev/hangar/driver/proxmox"
	"github.com/tomblancdev/hangar/sdk"
	"github.com/tomblancdev/hangar/sdk/pluginpb"
)

// Name is the plugin's own name: the operator enables it as "machines".
const Name = "machines"

// The classes: where a machine's memory is counted in its zone's pools.
const (
	Guaranteed     = "guaranteed"
	Spot           = "spot"
	GuaranteedSpot = "guaranteed+spot"
)

// usedMargin is what a running guest's memory must stay above what it holds
// by when a hold shrinks it: a limit written below its use makes the kernel
// kill inside it (read on Proxmox VE: the container's init died).
const usedMargin = 64

// Size is cores and memory.
type Size struct {
	Cores    int `json:"cores"`
	MemoryGB int `json:"memory_gb"`
}

// Types are AWS's names, for the sizes people already know. A burstable t3
// is not burstable here: the name is a size, nothing more.
var Types = map[string]Size{
	"t3.micro": {2, 1}, "t3.small": {2, 2}, "t3.medium": {2, 4}, "t3.large": {2, 8}, "t3.xlarge": {4, 16}, "t3.2xlarge": {8, 32},
	"m5.large": {2, 8}, "m5.xlarge": {4, 16}, "m5.2xlarge": {8, 32}, "m5.4xlarge": {16, 64},
	"c5.large": {2, 4}, "c5.xlarge": {4, 8}, "c5.2xlarge": {8, 16}, "c5.4xlarge": {16, 32},
	"r5.large": {2, 16}, "r5.xlarge": {4, 32}, "r5.2xlarge": {8, 64},
}

// DefaultType is the size of a machine that names none.
const DefaultType = "t3.micro"

// Spec is a machine's desired state.
type Spec struct {
	Name     string `json:"name,omitempty"`
	Kind     string `json:"kind"`
	Type     string `json:"type,omitempty"`
	Cores    int    `json:"cores"`
	MemoryGB int    `json:"memory_gb"`
	DiskGB   int    `json:"disk_gb"`
	Image    string `json:"image"`
	Class    string `json:"class"`
	// FloorGB: with guaranteed+spot, the memory that stays when the room is
	// held; the rest is borrowed.
	FloorGB int `json:"floor_gb,omitempty"`
	// CoresBeside: the CPU cap, in cores' worth, while the room is held (a
	// machine that stays); 0 = never capped.
	CoresBeside int `json:"cores_beside,omitempty"`
	// Resume: a spot machine a hold stopped starts again when the room
	// returns; false = it stays stopped until its owner starts it.
	Resume   bool     `json:"resume"`
	KeyPairs []string `json:"key_pairs,omitempty"`
	UserData string   `json:"user_data,omitempty"`
	Running  bool     `json:"running"`
}

// Observed is a machine as its engine reports it.
type Observed struct {
	EngineRef string   `json:"engine_ref"`
	Node      string   `json:"node,omitempty"`
	Kind      string   `json:"kind"`
	Name      string   `json:"name,omitempty"`
	Cores     int      `json:"cores"`
	MemoryMB  int      `json:"memory_mb"`
	DiskGB    int      `json:"disk_gb,omitempty"`
	Running   bool     `json:"running"`
	Addresses []string `json:"addresses,omitempty"`
	// CPULimit: its CPU cap while the room is held; 0 = none.
	CPULimit int `json:"cpu_limit,omitempty"`
	// Held: the reservations holding its room back, as its engine reads.
	Held []string `json:"held,omitempty"`
}

// KeyPair is a key pair's spec.
type KeyPair struct {
	PublicKey string `json:"public_key"`
}

// KeyPairObserved is what the plugin reads of a public key.
type KeyPairObserved struct {
	KeyType     string `json:"key_type"`
	Fingerprint string `json:"fingerprint"` // SHA256:… as ssh-keygen -l prints it
	Comment     string `json:"comment,omitempty"`
}

type sizeParams struct {
	Type     *string `json:"type"`
	Cores    *int    `json:"cores"`
	MemoryGB *int    `json:"memory_gb"`
}

const machineSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["image"],
  "properties": {
    "name":      { "type": "string", "pattern": "^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$",
                   "description": "Its host name; its id when absent." },
    "kind":      { "type": "string", "enum": ["vm", "container"], "default": "vm",
                   "description": "A VM has its own kernel; a container shares the host's." },
    "type":      { "type": "string", "maxLength": 64,
                   "description": "A size by name: t3.micro … r5.2xlarge, or an alias the operator declared. Or give cores and memory_gb." },
    "cores":     { "type": "integer", "minimum": 1, "maximum": 128 },
    "memory_gb": { "type": "integer", "minimum": 1, "maximum": 1024 },
    "disk_gb":   { "type": "integer", "minimum": 1, "maximum": 4096, "default": 8,
                   "description": "Its root disk." },
    "image":     { "type": "string", "minLength": 1, "maxLength": 128,
                   "description": "What it starts from, by the name the operator gave it." },
    "class":     { "type": "string", "enum": ["guaranteed", "spot", "guaranteed+spot"], "default": "spot",
                   "description": "Guaranteed room; room borrowed and given back when the zone needs it (it stops); or a guaranteed floor with the rest borrowed (it shrinks to its floor instead)." },
    "floor_gb":  { "type": "integer", "minimum": 1, "maximum": 1024,
                   "description": "With guaranteed+spot: the memory that stays when the room is needed." },
    "cores_beside": { "type": "integer", "minimum": 1, "maximum": 128,
                   "description": "While the zone's room is needed, its CPU is capped to this many cores' worth (a machine that keeps running)." },
    "resume":    { "type": "boolean", "default": true,
                   "description": "A spot machine stopped to give its room back starts again when the room returns; false = it stays stopped." },
    "key_pairs": { "type": "array", "maxItems": 10, "uniqueItems": true,
                   "items": { "type": "string", "x-hangar-ref": "keypair" },
                   "description": "Your key pairs, by id: their public keys let you in." },
    "user_data": { "type": "string", "maxLength": 16384,
                   "description": "Handed to its first boot (cloud-init), where its kind takes it." }
  }
}`

const keyPairSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["public_key"],
  "properties": {
    "public_key": { "type": "string", "maxLength": 16384,
                    "description": "One public key in OpenSSH's form: ssh-ed25519 AAAA… comment." }
  }
}`

const sizeSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "minProperties": 1,
  "properties": {
    "type":      { "type": "string", "maxLength": 64 },
    "cores":     { "type": "integer", "minimum": 1, "maximum": 128 },
    "memory_gb": { "type": "integer", "minimum": 1, "maximum": 1024 }
  }
}`

// Settings are the operator's.
type Settings struct {
	Images map[string]map[string]string `json:"images"`
	Types  map[string]Size              `json:"types"`
}

// Plugin is the machines plugin. Its zero value is not usable; call New.
type Plugin struct {
	pluginpb.UnimplementedPluginServiceServer

	mu       sync.RWMutex
	zones    map[string]driver.Driver
	settings Settings
}

// New returns a plugin with no zone configured.
func New() *Plugin { return &Plugin{zones: map[string]driver.Driver{}} }

// Driver returns the driver opened for a zone (tests reach the engine).
func (p *Plugin) Driver(zone string) driver.Driver {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.zones[zone]
}

func (p *Plugin) Describe(context.Context, *pluginpb.DescribeRequest) (*pluginpb.DescribeResponse, error) {
	return &pluginpb.DescribeResponse{
		Name:    Name,
		Version: "1",
		Types: []*pluginpb.ResourceType{
			{
				Name: "machine", IdPrefix: "m", Title: "Machine",
				Description: "A container or a VM, started from an image, sized by a type.",
				Schema:      []byte(machineSchema),
				Actions: []*pluginpb.Action{
					{Name: "start", Description: "Power it on.", ChangesUsage: true},
					{Name: "stop", Description: "Shut it down (asked, then made to).", ChangesUsage: true},
					{Name: "reboot", Description: "Restart it."},
					{Name: "resize", Description: "Set its size: a type, or cores and memory_gb. A running machine changes only what its kind can change live.",
						ParamsSchema: []byte(sizeSchema), ChangesUsage: true},
				},
			},
			{
				Name: "keypair", IdPrefix: "kp", Title: "Key pair",
				Description: "A public key you import; the machines that name it let its private half in.",
				Schema:      []byte(keyPairSchema),
			},
		},
		Dimensions: []*pluginpb.Dimension{
			{Name: "machines.count", Kind: pluginpb.DimensionKind_DIMENSION_KIND_QUANTITY, Description: "How many machines."},
			{Name: "machines.vcpu", Kind: pluginpb.DimensionKind_DIMENSION_KIND_QUANTITY, Unit: "vCPU", Description: "Cores across every machine."},
			{Name: "machines.memory_gb", Kind: pluginpb.DimensionKind_DIMENSION_KIND_QUANTITY, Unit: "GB", Description: "Memory across every machine."},
			{Name: "machines.disk_gb", Kind: pluginpb.DimensionKind_DIMENSION_KIND_QUANTITY, Unit: "GB", Description: "Root disks across every machine."},
			{Name: "machines.key_pairs", Kind: pluginpb.DimensionKind_DIMENSION_KIND_QUANTITY, Description: "How many key pairs."},
			{Name: "machines.kind", Kind: pluginpb.DimensionKind_DIMENSION_KIND_CHOICE, Description: "The kinds allowed: vm, container."},
			{Name: "machines.class", Kind: pluginpb.DimensionKind_DIMENSION_KIND_CHOICE, Description: "The classes allowed: guaranteed, spot, guaranteed+spot."},
		},
		Requires: []string{driver.GuestTags, driver.FencePool},
		Credential: &pluginpb.Credential{Required: false,
			Description: "Per zone, the engine's credential fenced to the product's own guests (Proxmox: an API token, user@realm!name=secret — the least it needs is in docs/proxmox.md). None for the fake engine."},
		Events: []string{"machine.created", "machine.deleted", "machine.started", "machine.stopped", "machine.rebooted",
			"machine.resized", "machine.repaired", "machine.held", "machine.released", "keypair.imported", "keypair.deleted"},
	}, nil
}

func (p *Plugin) Configure(ctx context.Context, req *pluginpb.ConfigureRequest) (*pluginpb.ConfigureResponse, error) {
	var set Settings
	if err := sdk.Decode(req.GetSettings(), &set); err != nil {
		return nil, err
	}
	for name, s := range set.Types {
		if _, aws := Types[name]; aws {
			return nil, sdk.Refuse("settings: type %q is AWS's name; an alias needs its own", name)
		}
		if s.Cores < 1 || s.MemoryGB < 1 {
			return nil, sdk.Refuse("settings: type %q needs cores and memory_gb", name)
		}
	}
	for name, forms := range set.Images {
		for kind, ref := range forms {
			if (kind != "vm" && kind != "container") || ref == "" {
				return nil, sdk.Refuse("settings: image %q: a form per kind, vm or container, each the engine's own name", name)
			}
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.settings = set
	for _, d := range p.zones {
		_ = d.Close()
	}
	p.zones = map[string]driver.Driver{}
	resp := &pluginpb.ConfigureResponse{}
	for _, z := range req.GetZones() {
		d, err := driver.Open(ctx, z.GetDriver(), driver.Params{
			Zone: z.GetName(), Endpoint: z.GetEndpoint(), Options: z.GetOptions(), Credential: z.GetCredential(), Watch: z.GetWatch(),
		})
		if err != nil {
			resp.Zones = append(resp.Zones, &pluginpb.ZoneReport{Name: z.GetName(), Error: err.Error()})
			continue
		}
		if _, ok := d.(driver.Guests); !ok {
			_ = d.Close()
			resp.Zones = append(resp.Zones, &pluginpb.ZoneReport{Name: z.GetName(), Error: "its driver runs no guests"})
			continue
		}
		if _, ok := d.(driver.Watcher); !ok && len(z.GetWatch()) > 0 {
			_ = d.Close()
			resp.Zones = append(resp.Zones, &pluginpb.ZoneReport{Name: z.GetName(), Error: "its reservations watch guests, and its driver cannot read them"})
			continue
		}
		if f, ok := d.(interface{ FenceReport() string }); ok && f.FenceReport() != "" {
			_ = d.Close()
			resp.Zones = append(resp.Zones, &pluginpb.ZoneReport{Name: z.GetName(), Error: "not fenced: " + f.FenceReport()})
			continue
		}
		p.zones[z.GetName()] = d
		resp.Zones = append(resp.Zones, &pluginpb.ZoneReport{Name: z.GetName(), Capabilities: d.Capabilities()})
	}
	return resp, nil
}

func (p *Plugin) guests(zone string) (driver.Guests, []driver.Capability, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	d, ok := p.zones[zone]
	if !ok {
		return nil, nil, sdk.NotNow("zone %s is not open to this plugin", zone)
	}
	return d.(driver.Guests), d.Capabilities(), nil
}

func (p *Plugin) typeSize(name string) (Size, bool) {
	if s, ok := Types[name]; ok {
		return s, true
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	s, ok := p.settings.Types[name]
	return s, ok
}

func (p *Plugin) typeNames() string {
	p.mu.RLock()
	aliases := slices.Sorted(maps.Keys(p.settings.Types))
	p.mu.RUnlock()
	names := slices.Sorted(maps.Keys(Types))
	if len(aliases) > 0 {
		return strings.Join(names, ", ") + "; and this operator's: " + strings.Join(aliases, ", ")
	}
	return strings.Join(names, ", ")
}

// imageRef is the engine's own name for an image in a kind.
func (p *Plugin) imageRef(image, kind string) (string, *pluginpb.Refusal) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	forms, ok := p.settings.Images[image]
	if !ok {
		names := slices.Sorted(maps.Keys(p.settings.Images))
		return "", &pluginpb.Refusal{Field: "/image", Reason: fmt.Sprintf("no image %q here (there are: %s)", image, strings.Join(names, ", "))}
	}
	ref, ok := forms[kind]
	if !ok {
		return "", &pluginpb.Refusal{Field: "/kind", Reason: fmt.Sprintf("image %s comes as %s, not as a %s", image,
			strings.Join(slices.Sorted(maps.Keys(forms)), " or "), kind)}
	}
	return ref, nil
}

// size applies a type, or cores and memory, to a spec. A create names both
// halves (or a type); a resize may name one, and the other stays.
func (p *Plugin) size(s *Spec, typ *string, cores, mem *int, create bool) *pluginpb.Refusal {
	switch {
	case typ != nil && (cores != nil || mem != nil):
		return &pluginpb.Refusal{Field: "/type", Reason: "a type, or cores and memory_gb — not both"}
	case typ != nil:
		sz, ok := p.typeSize(*typ)
		if !ok {
			return &pluginpb.Refusal{Field: "/type", Reason: fmt.Sprintf("no type %q (there are: %s)", *typ, p.typeNames())}
		}
		s.Type, s.Cores, s.MemoryGB = *typ, sz.Cores, sz.MemoryGB
	case create && (cores == nil) != (mem == nil):
		return &pluginpb.Refusal{Field: "/cores", Reason: "cores and memory_gb go together (or name a type)"}
	default:
		if cores != nil {
			s.Type, s.Cores = "", *cores
		}
		if mem != nil {
			s.Type, s.MemoryGB = "", *mem
		}
	}
	return nil
}

// room is what a machine takes from its zone, in MiB.
func room(s Spec) *pluginpb.Room {
	mem := int64(s.MemoryGB) * 1024
	r := &pluginpb.Room{Running: s.Running}
	switch s.Class {
	case Guaranteed:
		r.GuaranteedMb = mem
	case GuaranteedSpot:
		r.GuaranteedMb = int64(s.FloorGB) * 1024
		r.SpotMb = mem - r.GuaranteedMb
	default:
		r.SpotMb = mem
	}
	return r
}

// classRefusals checks what a class asks of the machine and of its zone.
func classRefusals(s Spec, zone string, g driver.Guests, caps []driver.Capability) []*pluginpb.Refusal {
	var out []*pluginpb.Refusal
	switch {
	case s.Class == GuaranteedSpot && s.FloorGB == 0:
		out = append(out, &pluginpb.Refusal{Field: "/floor_gb", Reason: "guaranteed+spot needs floor_gb: the memory that stays when the room is needed"})
	case s.Class == GuaranteedSpot && s.FloorGB >= s.MemoryGB:
		out = append(out, &pluginpb.Refusal{Field: "/floor_gb", Reason: fmt.Sprintf("floor_gb (%d) stays under memory_gb (%d): the rest is what it lends", s.FloorGB, s.MemoryGB)})
	case s.Class != GuaranteedSpot && s.FloorGB != 0:
		out = append(out, &pluginpb.Refusal{Field: "/floor_gb", Reason: "floor_gb goes with class guaranteed+spot"})
	}
	if s.Class == GuaranteedSpot && (!g.Traits(s.Kind).LiveMemoryDown || !slices.Contains(caps, driver.ResizeLiveMemoryDown)) {
		out = append(out, &pluginpb.Refusal{Field: "/class", Reason: fmt.Sprintf("a running %s in zone %s gives no memory back, so it cannot shrink to a floor: guaranteed or spot", kindWord(s.Kind), zone)})
	}
	switch {
	case s.CoresBeside != 0 && s.Class == Spot:
		out = append(out, &pluginpb.Refusal{Field: "/cores_beside", Reason: "a spot machine stops when the room is needed; cores_beside is for the ones that keep running"})
	case s.CoresBeside != 0 && s.CoresBeside < s.Cores && !slices.Contains(caps, driver.ResizeLiveCPUCap):
		out = append(out, &pluginpb.Refusal{Field: "/cores_beside", Reason: fmt.Sprintf("zone %s caps no CPU", zone)})
	}
	return out
}

func (p *Plugin) Plan(_ context.Context, req *pluginpb.PlanRequest) (*pluginpb.PlanResponse, error) {
	g, caps, err := p.guests(req.GetZone())
	if err != nil {
		return nil, err
	}
	switch req.GetType() {
	case "keypair":
		var kp KeyPair
		if err := sdk.Decode(req.GetSpec(), &kp); err != nil {
			return nil, err
		}
		canonical, _, err := parseKey(kp.PublicKey)
		if err != nil {
			return &pluginpb.PlanResponse{Refusals: []*pluginpb.Refusal{{Field: "/public_key", Reason: err.Error()}}}, nil
		}
		return &pluginpb.PlanResponse{Spec: sdk.JSON(KeyPair{PublicKey: canonical}), Usage: map[string]int64{"machines.key_pairs": 1}}, nil
	case "machine":
	default:
		return nil, sdk.Refuse("no type %q here", req.GetType())
	}

	var s Spec
	var refusals []*pluginpb.Refusal
	refuse := func(r ...*pluginpb.Refusal) {
		for _, x := range r {
			if x != nil {
				refusals = append(refusals, x)
			}
		}
	}
	held := req.GetCurrent().GetHold()
	switch req.GetAction() {
	case "":
		var in struct {
			Spec
			Type     *string `json:"type"`
			Cores    *int    `json:"cores"`
			MemoryGB *int    `json:"memory_gb"`
		}
		in.Kind, in.DiskGB, in.Class, in.Resume = "vm", 8, Spot, true
		if err := sdk.Decode(req.GetSpec(), &in); err != nil {
			return nil, err
		}
		s = in.Spec
		if in.Type == nil && in.Cores == nil && in.MemoryGB == nil {
			def := DefaultType
			in.Type = &def
		}
		refuse(p.size(&s, in.Type, in.Cores, in.MemoryGB, true))
		s.Running = true
		need := map[string]driver.Capability{"container": driver.KindContainer, "vm": driver.KindVM}[s.Kind]
		if !slices.Contains(caps, need) {
			refuse(&pluginpb.Refusal{Field: "/kind", Reason: fmt.Sprintf("zone %s runs no %s", req.GetZone(), kindWord(s.Kind))})
		} else {
			_, r := p.imageRef(s.Image, s.Kind)
			refuse(r)
			refuse(classRefusals(s, req.GetZone(), g, caps)...)
		}
		if s.UserData != "" && !g.Traits(s.Kind).UserData {
			refuse(&pluginpb.Refusal{Field: "/user_data", Reason: fmt.Sprintf("a %s in zone %s boots no user data", kindWord(s.Kind), req.GetZone())})
		}
	case "resize":
		if err := sdk.Decode(req.GetCurrent().GetSpec(), &s); err != nil {
			return nil, err
		}
		if held != "" {
			refuse(&pluginpb.Refusal{Reason: fmt.Sprintf("its room is held for %s: resize it once the room is back", held)})
		}
		var sp sizeParams
		if err := sdk.Decode(req.GetParams(), &sp); err != nil {
			return nil, err
		}
		was := s
		refuse(p.size(&s, sp.Type, sp.Cores, sp.MemoryGB, false))
		if s.Class == GuaranteedSpot && s.FloorGB >= s.MemoryGB {
			refuse(&pluginpb.Refusal{Field: "/memory_gb", Reason: fmt.Sprintf("its floor is %d GB: memory_gb stays above it", s.FloorGB)})
		}
		if s.CoresBeside != 0 && s.CoresBeside < s.Cores && !slices.Contains(caps, driver.ResizeLiveCPUCap) {
			refuse(&pluginpb.Refusal{Field: "/cores", Reason: fmt.Sprintf("zone %s caps no CPU, and cores_beside would cap this one", req.GetZone())})
		}
		if s.Running {
			t := g.Traits(s.Kind)
			if s.Cores != was.Cores && !t.LiveCores {
				refuse(&pluginpb.Refusal{Field: "/cores", Reason: fmt.Sprintf("a running %s's cores change only while it is stopped here: stop it first", kindWord(s.Kind))})
			}
			if s.MemoryGB > was.MemoryGB && !t.LiveMemoryUp || s.MemoryGB < was.MemoryGB && !t.LiveMemoryDown {
				refuse(&pluginpb.Refusal{Field: "/memory_gb", Reason: fmt.Sprintf("a running %s's memory does not go that way here: stop it first", kindWord(s.Kind))})
			}
		}
	case "start", "stop":
		if err := sdk.Decode(req.GetCurrent().GetSpec(), &s); err != nil {
			return nil, err
		}
		s.Running = req.GetAction() == "start"
	default:
		return nil, sdk.Refuse("no action %q to plan on a machine", req.GetAction())
	}
	return &pluginpb.PlanResponse{
		Spec:     sdk.JSON(s),
		Usage:    map[string]int64{"machines.count": 1, "machines.vcpu": int64(s.Cores), "machines.memory_gb": int64(s.MemoryGB), "machines.disk_gb": int64(s.DiskGB)},
		Choices:  map[string]string{"machines.kind": s.Kind, "machines.class": s.Class},
		Refusals: refusals,
		Room:     room(s),
	}, nil
}

func kindWord(kind string) string {
	if kind == "vm" {
		return "VM"
	}
	return kind
}

// tagsOf are the tags a machine carries for its engine's node to read: its
// class, the size admitted, its floor and CPU cap when it has them.
func tagsOf(s Spec) map[string]string {
	t := map[string]string{"class": s.Class, "admitted": strconv.Itoa(s.MemoryGB * 1024)}
	if s.Class == GuaranteedSpot {
		t["floor"] = strconv.Itoa(s.FloorGB * 1024)
	}
	if s.CoresBeside != 0 && s.CoresBeside < s.Cores {
		t["beside"] = strconv.Itoa(s.CoresBeside)
	}
	return t
}

// want is the engine state a machine is brought to: its spec, bent by a hold.
type want struct {
	running  bool
	memoryMB int
	cpuLimit int
	holds    []string
}

func wanted(s Spec, hold string) want {
	w := want{running: s.Running, memoryMB: s.MemoryGB * 1024}
	if hold == "" {
		return w
	}
	capped := s.CoresBeside != 0 && s.CoresBeside < s.Cores
	switch s.Class {
	case Spot:
		w.running, w.holds = false, []string{hold}
	case GuaranteedSpot:
		w.memoryMB, w.holds = s.FloorGB*1024, []string{hold}
	default:
		if capped {
			w.holds = []string{hold}
		}
	}
	if capped && s.Class != Spot {
		w.cpuLimit = s.CoresBeside
	}
	return w
}

func (p *Plugin) Create(ctx context.Context, req *pluginpb.CreateRequest) (*pluginpb.CreateResponse, error) {
	r := req.GetResource()
	if r.GetType() == "keypair" {
		if _, _, err := p.guests(r.GetZone()); err != nil {
			return nil, err
		}
		var kp KeyPair
		if err := sdk.Decode(r.GetSpec(), &kp); err != nil {
			return nil, err
		}
		_, obs, err := parseKey(kp.PublicKey)
		if err != nil {
			return nil, sdk.Refuse("%v", err)
		}
		return &pluginpb.CreateResponse{Observed: sdk.JSON(obs),
			Events: []*pluginpb.Event{sdk.Event("keypair.imported", "", map[string]string{"fingerprint": obs.Fingerprint})}}, nil
	}
	g, s, err := p.machine(r)
	if err != nil {
		return nil, err
	}
	ref, refusal := p.imageRef(s.Image, s.Kind)
	if refusal != nil {
		return nil, sdk.NotNow("%s", refusal.GetReason()) // the operator's images moved since the plan
	}
	var keys []string
	for _, kp := range req.GetRefs() {
		if kp.GetType() != "keypair" {
			continue
		}
		var k KeyPair
		if err := sdk.Decode(kp.GetSpec(), &k); err != nil {
			return nil, err
		}
		keys = append(keys, k.PublicKey)
	}
	// a machine born while its zone's room is held is born held: at its
	// floor, capped, or — spot — not started
	w := wanted(s, r.GetHold())
	guest, err := g.CreateGuest(ctx, driver.GuestSpec{
		ID: r.GetId(), Kind: s.Kind, Name: s.Name, Cores: s.Cores, MemoryMB: w.memoryMB, DiskGB: s.DiskGB,
		Image: ref, SSHKeys: keys, UserData: []byte(s.UserData), Tags: tagsOf(s), Holds: w.holds, CPULimit: w.cpuLimit,
		Stopped: !w.running,
	})
	if err != nil {
		return nil, engineErr(err)
	}
	return &pluginpb.CreateResponse{
		Observed: sdk.JSON(observe(guest)),
		Events:   []*pluginpb.Event{sdk.Event("machine.created", "", map[string]string{"engine_ref": guest.EngineRef})},
	}, nil
}

func (p *Plugin) Delete(ctx context.Context, req *pluginpb.DeleteRequest) (*pluginpb.DeleteResponse, error) {
	r := req.GetResource()
	if r.GetType() == "keypair" {
		return &pluginpb.DeleteResponse{Events: []*pluginpb.Event{sdk.Event("keypair.deleted", "the machines that name it keep the key they were born with", nil)}}, nil
	}
	g, _, err := p.machine(r)
	if err != nil {
		return nil, err
	}
	if err := g.DeleteGuest(ctx, r.GetId()); err != nil && !errors.Is(err, driver.ErrNotFound) {
		return nil, engineErr(err)
	}
	return &pluginpb.DeleteResponse{Events: []*pluginpb.Event{sdk.Event("machine.deleted", "", nil)}}, nil
}

func (p *Plugin) Act(ctx context.Context, req *pluginpb.ActRequest) (*pluginpb.ActResponse, error) {
	r := req.GetResource()
	g, s, err := p.machine(r)
	if err != nil {
		return nil, err
	}
	hold := r.GetHold()
	var guest driver.Guest
	var ev *pluginpb.Event
	switch req.GetAction() {
	case "start":
		if hold != "" && s.Class == Spot {
			return nil, sdk.NotNow("its room is held for %s: it starts when the room is back", hold)
		}
		s.Running = true
		guest, err = g.Guest(ctx, r.GetId())
		if err == nil {
			// started as the zone's room allows: at its floor, capped, while held
			guest, _, _, err = converge(ctx, g, r.GetId(), s, hold, guest)
		}
		ev = sdk.Event("machine.started", "", nil)
	case "stop":
		s.Running = false
		guest, err = g.SetPower(ctx, r.GetId(), false)
		ev = sdk.Event("machine.stopped", "", nil)
	case "reboot":
		guest, err = g.Reboot(ctx, r.GetId())
		ev = sdk.Event("machine.rebooted", "", nil)
	case "resize":
		if hold != "" {
			return nil, sdk.NotNow("its room is held for %s: resize it once the room is back", hold)
		}
		var sp sizeParams
		if err := sdk.Decode(req.GetParams(), &sp); err != nil {
			return nil, err
		}
		if refusal := p.size(&s, sp.Type, sp.Cores, sp.MemoryGB, false); refusal != nil {
			return nil, sdk.Refuse("%s", refusal.GetReason())
		}
		// a grown size is written on the guest before it takes effect, a
		// shrunk one after: a node reading "admitted" never refuses a size
		// the brain admitted
		before, rerr := g.Guest(ctx, r.GetId())
		if rerr != nil {
			return nil, engineErr(rerr)
		}
		grows := s.MemoryGB*1024 > before.MemoryMB
		if grows {
			if _, err := g.Retag(ctx, r.GetId(), tagsOf(s), before.Holds); err != nil {
				return nil, engineErr(err)
			}
		}
		guest, err = g.ResizeGuest(ctx, r.GetId(), s.Cores, s.MemoryGB*1024)
		if err == nil && !grows {
			guest, err = g.Retag(ctx, r.GetId(), tagsOf(s), before.Holds)
		}
		ev = sdk.Event("machine.resized", "", map[string]string{"cores": strconv.Itoa(s.Cores), "memory_gb": strconv.Itoa(s.MemoryGB)})
	default:
		return nil, sdk.Refuse("no action %q on a machine", req.GetAction())
	}
	if err != nil {
		return nil, engineErr(err)
	}
	return &pluginpb.ActResponse{Spec: sdk.JSON(s), Observed: sdk.JSON(observe(guest)), Events: []*pluginpb.Event{ev}}, nil
}

// converge brings a guest to its spec bent by a hold. Entering a hold, the
// tags lead (a node reading them sees the hold before its effect) and the
// guest is stopped, shrunk, capped; leaving one, it is regrown and uncapped
// first, the tags follow, and it starts last. It returns what it changed
// and, when a running guest holds more than its floor, what it could not
// give back.
func converge(ctx context.Context, g driver.Guests, id string, s Spec, hold string, guest driver.Guest) (driver.Guest, []string, string, error) {
	w := wanted(s, hold)
	tags := tagsOf(s)
	var fixed []string
	var short string
	var err error
	retag := func() error {
		if maps.Equal(guest.Tags, tags) && slices.Equal(guest.Holds, w.holds) {
			return nil
		}
		guest, err = g.Retag(ctx, id, tags, w.holds)
		if err == nil {
			fixed = append(fixed, "tags")
		}
		return err
	}
	entering := len(w.holds) > 0
	if entering {
		if err := retag(); err != nil {
			return guest, fixed, "", err
		}
	}
	if guest.Running && !w.running {
		if guest, err = g.SetPower(ctx, id, false); err != nil {
			return guest, fixed, "", err
		}
		fixed = append(fixed, "power")
	}
	if guest.Cores != s.Cores || guest.MemoryMB != w.memoryMB {
		target := w.memoryMB
		if guest.Running && target < guest.MemoryMB && guest.MemoryUsedMB > 0 && target < guest.MemoryUsedMB+usedMargin {
			// what it holds cannot be taken from under it: shrink as far as
			// it allows, and say what stays lent
			target = min(guest.MemoryMB, guest.MemoryUsedMB+usedMargin)
			short = fmt.Sprintf("it holds %d MB, above its floor of %d MB: %d MB could not be given back", guest.MemoryUsedMB, w.memoryMB, target-w.memoryMB)
		}
		if guest.Cores != s.Cores || guest.MemoryMB != target {
			if guest, err = g.ResizeGuest(ctx, id, s.Cores, target); err != nil {
				return guest, fixed, short, err
			}
			fixed = append(fixed, "size")
		}
	}
	if guest.CPULimit != w.cpuLimit {
		if guest, err = g.SetCPULimit(ctx, id, w.cpuLimit); err != nil {
			return guest, fixed, short, err
		}
		fixed = append(fixed, "cpu cap")
	}
	if !entering {
		if err := retag(); err != nil {
			return guest, fixed, short, err
		}
	}
	if !guest.Running && w.running {
		if guest, err = g.SetPower(ctx, id, true); err != nil {
			return guest, fixed, short, err
		}
		fixed = append(fixed, "power")
	}
	return guest, fixed, short, nil
}

func (p *Plugin) Reconcile(ctx context.Context, req *pluginpb.ReconcileRequest) (*pluginpb.ReconcileResponse, error) {
	r := req.GetResource()
	if r.GetType() == "keypair" {
		return &pluginpb.ReconcileResponse{Drift: pluginpb.Drift_DRIFT_IN_SYNC, Observed: r.GetObserved()}, nil
	}
	g, s, err := p.machine(r)
	if err != nil {
		return nil, err
	}
	guest, err := g.Guest(ctx, r.GetId())
	if errors.Is(err, driver.ErrNotFound) {
		return &pluginpb.ReconcileResponse{Drift: pluginpb.Drift_DRIFT_MISSING, Detail: "the engine has no such machine"}, nil
	}
	if err != nil {
		return nil, engineErr(err)
	}
	hold := r.GetHold()
	resp := &pluginpb.ReconcileResponse{}
	var events []*pluginpb.Event
	if hold != "" && s.Class == Spot && s.Running && !s.Resume {
		// stopped for good: it does not come back when the room does
		s.Running = false
		resp.Spec, resp.Room = sdk.JSON(s), room(s)
	}
	wasHeld := len(guest.Holds) > 0
	guest, fixed, short, err := converge(ctx, g, r.GetId(), s, hold, guest)
	if err != nil && !errors.Is(err, driver.ErrRefused) && !errors.Is(err, driver.ErrNotFound) {
		return nil, engineErr(err) // not reached: the core tries again at its next pass
	}
	if err != nil {
		resp.Drift, resp.Observed = pluginpb.Drift_DRIFT_DRIFTED, sdk.JSON(observe(guest))
		resp.Detail = fmt.Sprintf("it could not be brought to what it should be (%s): %v", strings.Join(append(fixed, "…"), ", "), err)
		return resp, nil
	}
	resp.Observed = sdk.JSON(observe(guest))
	switch held := len(guest.Holds) > 0; {
	case held && !wasHeld:
		events = append(events, sdk.Event("machine.held", fmt.Sprintf("its room is held for %s", hold), map[string]string{"hold": hold}))
	case !held && wasHeld:
		events = append(events, sdk.Event("machine.released", "its room is back", nil))
	}
	switch {
	case short != "":
		resp.Drift, resp.Detail = pluginpb.Drift_DRIFT_DRIFTED, short
	case len(fixed) == 0:
		resp.Drift = pluginpb.Drift_DRIFT_IN_SYNC
	default:
		resp.Drift, resp.Detail = pluginpb.Drift_DRIFT_REPAIRED, fmt.Sprintf("put back: %v", fixed)
		if hold != "" {
			resp.Detail = fmt.Sprintf("held for %s: %v", hold, fixed)
		}
		events = append(events, sdk.Event("machine.repaired", resp.Detail, nil))
	}
	resp.Events = events
	return resp, nil
}

// Survey reads what the zone's reservations wait on, which machines carry a
// hold on the engine, and whether the zone is awake.
func (p *Plugin) Survey(ctx context.Context, req *pluginpb.SurveyRequest) (*pluginpb.SurveyResponse, error) {
	p.mu.RLock()
	d, ok := p.zones[req.GetZone()]
	p.mu.RUnlock()
	if !ok {
		return nil, sdk.NotNow("zone %s is not open to this plugin", req.GetZone())
	}
	w, ok := d.(driver.Watcher)
	if !ok {
		return nil, sdk.NotNow("zone %s's driver reads no conditions", req.GetZone())
	}
	resp := &pluginpb.SurveyResponse{Holds: map[string]string{}}
	for _, c := range req.GetConditions() {
		var met bool
		var err error
		switch {
		case c.GetGuestRunning() != "":
			met, err = w.GuestRunning(ctx, c.GetGuestRunning())
		case c.GetNodeDown() != "":
			met, err = w.NodeDown(ctx, c.GetNodeDown())
		default:
			err = errors.New("an empty condition")
		}
		st := &pluginpb.ConditionState{Met: met}
		if err != nil {
			st = &pluginpb.ConditionState{Error: err.Error()}
		}
		resp.Conditions = append(resp.Conditions, st)
	}
	awake, err := w.Awake(ctx)
	resp.Awake = awake && err == nil
	if !resp.Awake {
		return resp, nil
	}
	gs, err := d.(driver.Guests).Guests(ctx)
	if err != nil {
		return nil, engineErr(err)
	}
	for _, gu := range gs {
		if len(gu.Holds) > 0 {
			resp.Holds[gu.ID] = gu.Holds[0]
		}
	}
	return resp, nil
}

func (p *Plugin) machine(r *pluginpb.Resource) (driver.Guests, Spec, error) {
	g, _, err := p.guests(r.GetZone())
	if err != nil {
		return nil, Spec{}, err
	}
	var s Spec
	if err := sdk.Decode(r.GetSpec(), &s); err != nil {
		return nil, Spec{}, err
	}
	return g, s, nil
}

func observe(g driver.Guest) Observed {
	return Observed{EngineRef: g.EngineRef, Node: g.Node, Kind: g.Kind, Name: g.Name, Cores: g.Cores,
		MemoryMB: g.MemoryMB, DiskGB: g.DiskGB, Running: g.Running, Addresses: g.Addresses, CPULimit: g.CPULimit, Held: g.Holds}
}

// engineErr: a refusal or a missing guest will not change by trying again;
// anything else might (the core retries UNAVAILABLE).
func engineErr(err error) error {
	if errors.Is(err, driver.ErrRefused) || errors.Is(err, driver.ErrNotFound) {
		return sdk.NotNow("%v", err)
	}
	return sdk.Unreachable("%v", err)
}

// ---- Public keys ------------------------------------------------------------

var keyTypes = []string{
	"ssh-ed25519", "ssh-rsa", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521",
	"sk-ssh-ed25519@openssh.com", "sk-ecdsa-sha2-nistp256@openssh.com",
}

// parseKey reads one public key in OpenSSH's one-line form: its type, its
// base64 blob (whose first field must name the same type) and a comment. It
// returns the key written back canonically and what it is.
func parseKey(line string) (string, KeyPairObserved, error) {
	f := strings.Fields(strings.TrimSpace(line))
	if len(f) < 2 {
		return "", KeyPairObserved{}, errors.New("one public key, in OpenSSH's form: ssh-ed25519 AAAA… comment")
	}
	if !slices.Contains(keyTypes, f[0]) {
		return "", KeyPairObserved{}, fmt.Errorf("key type %q is not one of %s", f[0], strings.Join(keyTypes, ", "))
	}
	blob, err := base64.StdEncoding.DecodeString(f[1])
	if err != nil || len(blob) < 4 {
		return "", KeyPairObserved{}, errors.New("the key's second field is not base64")
	}
	n := binary.BigEndian.Uint32(blob)
	if int(n) > len(blob)-4 || string(blob[4:4+n]) != f[0] {
		return "", KeyPairObserved{}, fmt.Errorf("the key's body is not a %s key", f[0])
	}
	sum := sha256.Sum256(blob)
	obs := KeyPairObserved{KeyType: f[0], Fingerprint: "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]), Comment: strings.Join(f[2:], " ")}
	canonical := f[0] + " " + f[1]
	if obs.Comment != "" {
		canonical += " " + obs.Comment
	}
	return canonical, obs, nil
}

// Names lists the types this plugin sizes by, for a door that shows them.
func Names() []string {
	out := slices.Collect(maps.Keys(Types))
	sort.Strings(out)
	return out
}
