// Package volumes is the plugin that makes volumes: disks that belong to
// their owner rather than to a machine — plugged into one of the owner's
// machines, unplugged, plugged into another, grown, backed up or not — on any
// driver with the volumes facet.
//
// One type, volume (vol-…): create · attach · detach · move · resize ·
// set_backup · delete.
//
// A volume's content is fixed at its birth: a disk its VM formats itself
// (block — its guest sees it under the serial vol0123…, the id without its
// dash), or a directory its container mounts at a path (filesystem). A
// volume plugged into nothing is PARKED: where the engine keeps no disk
// without a guest, its driver keeps it on a stopped shelf guest of its owner
// (the plugin never sees one).
//
// A volume names its machine ("machine", x-hangar-ref) and is ATTACHED to it
// (x-hangar-attached): the core deletes neither while it is — the engine
// would take the volume's data along with the machine.
//
// Limits: volumes.count, volumes.size_gb, and volumes.backup_gb — the size of
// the volumes the engine's backups take, so a tier that names no budget gets
// no backup at all.
//
// It requires fence.pool (a zone whose credential reaches beyond the
// product's own guests is not one it acts on) and volume.move_between_guests.
package volumes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/tomblancdev/hangar/driver"
	_ "github.com/tomblancdev/hangar/driver/fake"
	_ "github.com/tomblancdev/hangar/driver/proxmox"
	"github.com/tomblancdev/hangar/sdk"
	"github.com/tomblancdev/hangar/sdk/pluginpb"
)

// Name is the plugin's own name: the operator enables it as "volumes".
const Name = "volumes"

// Spec is a volume's desired state.
type Spec struct {
	SizeGB  int    `json:"size_gb"`
	Content string `json:"content"`
	Backup  bool   `json:"backup"`
	// Machine: the machine it is plugged into; "" = parked.
	Machine string `json:"machine,omitempty"`
	// Mount: a filesystem volume's path in its container — kept while it is
	// parked, the path it takes again when attached without one.
	Mount string `json:"mount,omitempty"`
}

// Observed is a volume as its engine reports it.
type Observed struct {
	EngineRef string `json:"engine_ref"`
	Node      string `json:"node,omitempty"`
	Content   string `json:"content"`
	SizeGB    int    `json:"size_gb"`
	Backup    bool   `json:"backup"`
	Machine   string `json:"machine,omitempty"`
	Mount     string `json:"mount,omitempty"`
	Device    string `json:"device,omitempty"`
	// InGuest: where its machine finds it — a disk's path where it is plugged
	// now, or the mount. Its serial is what follows a disk between machines.
	InGuest string `json:"in_guest,omitempty"`
}

const volumeSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["size_gb"],
  "x-hangar-summary": ["{size_gb} GB", "on {machine}[ at {mount}]", "{backup?backed up}"],
  "x-hangar-status": { "field": "machine", "on": "attached", "off": "parked" },
  "properties": {
    "size_gb": { "type": "integer", "minimum": 1, "maximum": 65536,
                 "description": "Its size. It only ever grows." },
    "content": { "type": "string", "enum": ["block", "filesystem"],
                 "description": "A disk its VM formats itself (block), or a directory its container mounts at a path (filesystem) — fixed at birth. Default: filesystem when mount is given, block otherwise." },
    "backup":  { "type": "boolean", "default": false,
                 "description": "Whether the engine's backups take it; its size counts against your volumes.backup_gb." },
    "machine": { "type": "string", "x-hangar-ref": "machine", "x-hangar-attached": true,
                 "description": "A machine of yours to plug it into; parked on none when absent. Neither is deleted while it is plugged in." },
    "mount":   { "type": "string", "pattern": "^(/[A-Za-z0-9._-]+)+$", "maxLength": 255,
                 "description": "A filesystem volume's path in its container." }
  }
}`

const placeSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["machine"],
  "properties": {
    "machine": { "type": "string", "x-hangar-ref": "machine",
                 "description": "A machine of yours, in the volume's zone." },
    "mount":   { "type": "string", "pattern": "^(/[A-Za-z0-9._-]+)+$", "maxLength": 255,
                 "description": "A filesystem volume's path in the container; the one it had when absent." }
  }
}`

const sizeSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["size_gb"],
  "properties": {
    "size_gb": { "type": "integer", "minimum": 1, "maximum": 65536 }
  }
}`

const backupSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["backup"],
  "properties": {
    "backup": { "type": "boolean" }
  }
}`

// Plugin is the volumes plugin. Its zero value is not usable; call New.
type Plugin struct {
	pluginpb.UnimplementedPluginServiceServer

	mu    sync.RWMutex
	zones map[string]driver.Driver
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
		Types: []*pluginpb.ResourceType{{
			Name: "volume", IdPrefix: "vol", Title: "Volume",
			Description: "A disk of yours: plugged into one of your machines, or parked on none, its data kept either way.",
			Schema:      []byte(volumeSchema),
			Actions: []*pluginpb.Action{
				{Name: "attach", Description: "Plug it into a machine of yours (a running VM takes it live; so does a container).",
					ParamsSchema: []byte(placeSchema), ChangesUsage: true},
				{Name: "detach", Description: "Unplug it: it rests parked, its data kept. A running VM lets go of it live; a container only once stopped.",
					ChangesUsage: true},
				{Name: "move", Description: "Unplug it from its machine and plug it into another of yours, in one go.",
					ParamsSchema: []byte(placeSchema), ChangesUsage: true},
				{Name: "resize", Description: "Grow it to size_gb (a volume never shrinks); its machine may need to grow its filesystem itself.",
					ParamsSchema: []byte(sizeSchema), ChangesUsage: true},
				{Name: "set_backup", Description: "Whether the engine's backups take it.",
					ParamsSchema: []byte(backupSchema), ChangesUsage: true},
			},
		}},
		Dimensions: []*pluginpb.Dimension{
			{Name: "volumes.count", Kind: pluginpb.DimensionKind_DIMENSION_KIND_QUANTITY, Description: "How many volumes."},
			{Name: "volumes.size_gb", Kind: pluginpb.DimensionKind_DIMENSION_KIND_QUANTITY, Unit: "GB", Description: "Their sizes, added up."},
			{Name: "volumes.backup_gb", Kind: pluginpb.DimensionKind_DIMENSION_KIND_QUANTITY, Unit: "GB",
				Description: "The sizes of those the backups take, added up; a tier that names none backs up none."},
		},
		Requires: []string{driver.FencePool, driver.VolumeMoveBetweenGuests},
		Credential: &pluginpb.Credential{Required: false,
			Description: "Per zone, the engine's credential fenced to the product's own guests, allowed their disks and nothing of their power or network (Proxmox: an API token, user@realm!name=secret — the least it needs is in docs/proxmox.md). None for the fake engine."},
		Events: []string{"volume.created", "volume.attached", "volume.detached", "volume.moved", "volume.resized",
			"volume.backup_set", "volume.deleted", "volume.repaired"},
	}, nil
}

func (p *Plugin) Configure(ctx context.Context, req *pluginpb.ConfigureRequest) (*pluginpb.ConfigureResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
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
		if _, ok := d.(driver.Volumes); !ok {
			_ = d.Close()
			resp.Zones = append(resp.Zones, &pluginpb.ZoneReport{Name: z.GetName(), Error: "its driver keeps no volumes"})
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

func (p *Plugin) volumes(zone string) (driver.Volumes, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	d, ok := p.zones[zone]
	if !ok {
		return nil, sdk.NotNow("zone %s is not open to this plugin", zone)
	}
	return d.(driver.Volumes), nil
}

type placeParams struct {
	Machine string `json:"machine"`
	Mount   string `json:"mount"`
}

// kindOf reads a referenced machine's kind: its spec says, its engine agrees.
func kindOf(refs []*pluginpb.Resource, id string) string {
	for _, r := range refs {
		if r.GetId() != id {
			continue
		}
		var m struct {
			Kind string `json:"kind"`
		}
		if sdk.Decode(r.GetSpec(), &m) == nil && m.Kind != "" {
			return m.Kind
		}
		if sdk.Decode(r.GetObserved(), &m) == nil {
			return m.Kind
		}
	}
	return ""
}

// kindFor is the machine kind a content goes on.
func kindFor(content string) string {
	if content == driver.ContentFilesystem {
		return "container"
	}
	return "vm"
}

func kindWord(kind string) string {
	if kind == "vm" {
		return "VM"
	}
	return kind
}

// mountRefusal: a path of real segments ("/a/b"), never "." or "..".
func mountRefusal(field, mount string) *pluginpb.Refusal {
	for _, seg := range strings.Split(mount, "/")[1:] {
		if seg == "." || seg == ".." {
			return &pluginpb.Refusal{Field: field, Reason: "a path of plain names: no . or .. in it"}
		}
	}
	return nil
}

// next is a volume's spec once a request is done — for Plan to admit and for
// Act to write, the same function so the two never disagree — and why it is
// refused, if it is.
func next(v driver.Volumes, cur Spec, action string, raw []byte, refs []*pluginpb.Resource) (Spec, []*pluginpb.Refusal, error) {
	s := cur
	var refusals []*pluginpb.Refusal
	refuse := func(field, format string, a ...any) {
		refusals = append(refusals, &pluginpb.Refusal{Field: field, Reason: fmt.Sprintf(format, a...)})
	}
	// placed checks a volume may go on a machine, and gives its mount
	placed := func(field, machine, mount string) {
		kind := kindOf(refs, machine)
		if kind != "" && kind != kindFor(s.Content) {
			refuse(field, "a %s volume goes on a %s, and %s is a %s", s.Content, kindWord(kindFor(s.Content)), machine, kindWord(kind))
		}
		switch {
		case s.Content == driver.ContentBlock && mount != "":
			refuse("/mount", "a block volume is a disk its VM mounts itself: it takes no path")
		case s.Content == driver.ContentFilesystem && mount == "" && s.Mount == "":
			refuse("/mount", "a filesystem volume on a container needs its path there")
		case mount != "":
			s.Mount = mount
		}
		s.Machine = machine
	}
	switch action {
	case "":
		if err := sdk.Decode(raw, &s); err != nil {
			return s, nil, err
		}
		if s.Content == "" {
			s.Content = driver.ContentBlock
			if s.Mount != "" {
				s.Content = driver.ContentFilesystem
			}
		}
		if r := mountRefusal("/mount", s.Mount); s.Mount != "" && r != nil {
			refusals = append(refusals, r)
		}
		m := s.Mount
		s.Mount = ""
		if s.Machine != "" {
			placed("/machine", s.Machine, m)
			break
		}
		if s.Content == driver.ContentBlock && m != "" {
			refuse("/mount", "a block volume is a disk its VM mounts itself: it takes no path")
		}
		s.Mount = m // remembered, for its first attach
		if err := v.CanPark(s.Content); err != nil {
			refuse("/machine", "%s — name a machine for it", strings.TrimPrefix(err.Error(), driver.ErrRefused.Error()+": "))
		}
	case "attach", "move":
		var pp placeParams
		if err := sdk.Decode(raw, &pp); err != nil {
			return s, nil, err
		}
		if r := mountRefusal("/mount", pp.Mount); pp.Mount != "" && r != nil {
			refusals = append(refusals, r)
		}
		switch {
		case action == "attach" && cur.Machine != "":
			refuse("", "it is plugged into %s: detach it first, or move it", cur.Machine)
		case action == "move" && cur.Machine == "":
			refuse("", "it is parked: attach it")
		case action == "move" && cur.Machine == pp.Machine:
			refuse("/machine", "it is on %s already", pp.Machine)
		default:
			placed("/machine", pp.Machine, pp.Mount)
		}
	case "detach":
		if cur.Machine == "" {
			refuse("", "it is parked already")
		} else if err := v.CanPark(s.Content); err != nil {
			refuse("", "%s — move it to another machine instead", strings.TrimPrefix(err.Error(), driver.ErrRefused.Error()+": "))
		}
		s.Machine = ""
	case "resize":
		var sp struct {
			SizeGB int `json:"size_gb"`
		}
		if err := sdk.Decode(raw, &sp); err != nil {
			return s, nil, err
		}
		if sp.SizeGB < cur.SizeGB {
			refuse("/size_gb", "it is %d GB, and a volume only grows", cur.SizeGB)
		}
		s.SizeGB = sp.SizeGB
	case "set_backup":
		var bp struct {
			Backup bool `json:"backup"`
		}
		if err := sdk.Decode(raw, &bp); err != nil {
			return s, nil, err
		}
		s.Backup = bp.Backup
	default:
		return s, nil, sdk.Refuse("no action %q on a volume", action)
	}
	return s, refusals, nil
}

// PlanChange says what brings a volume to another spec: grown by resize,
// its backup by set_backup, its place by attach, detach or move — the
// machine it is on is changed last. Its content is set at its birth, and it
// never shrinks.
func (p *Plugin) PlanChange(_ context.Context, req *pluginpb.PlanChangeRequest) (*pluginpb.PlanChangeResponse, error) {
	if req.GetCurrent().GetType() != "volume" {
		return nil, sdk.Refuse("no type %q here", req.GetCurrent().GetType())
	}
	var was, want Spec
	if err := sdk.Decode(req.GetCurrent().GetSpec(), &was); err != nil {
		return nil, err
	}
	if err := sdk.Decode(req.GetSpec(), &want); err != nil {
		return nil, err
	}
	if want.Content == "" {
		want.Content = driver.ContentBlock
		if want.Mount != "" {
			want.Content = driver.ContentFilesystem
		}
	}
	out := &pluginpb.PlanChangeResponse{}
	if want.Content != was.Content {
		// its path and its place follow from what it is: nothing more to say
		out.Fixed = append(out.Fixed, sdk.Fixed("/content", "a "+was.Content+" volume", "a volume's content"))
		return out, nil
	}
	switch {
	case want.SizeGB < was.SizeGB:
		out.Fixed = append(out.Fixed, &pluginpb.Refusal{Field: "/size_gb", Reason: fmt.Sprintf("it is %d GB, and a volume only grows", was.SizeGB)})
	case want.SizeGB > was.SizeGB:
		out.Steps = append(out.Steps, sdk.Step("resize", map[string]any{"size_gb": want.SizeGB}))
	}
	if want.Backup != was.Backup {
		out.Steps = append(out.Steps, sdk.Step("set_backup", map[string]any{"backup": want.Backup}))
	}
	// the path goes with the place: a filesystem volume takes it when it is
	// attached (a parked one keeps its last for its next attach)
	at := func(machine string) map[string]any {
		pp := map[string]any{"machine": machine}
		if want.Mount != "" {
			pp["mount"] = want.Mount
		}
		return pp
	}
	switch {
	case want.Machine == was.Machine && want.Mount != "" && want.Mount != was.Mount && was.Machine != "":
		out.Steps = append(out.Steps, sdk.Step("detach", nil), sdk.Step("attach", at(want.Machine)))
	case want.Machine == was.Machine && want.Mount != was.Mount && was.Machine == "":
		out.Fixed = append(out.Fixed, &pluginpb.Refusal{Field: "/mount", Reason: fmt.Sprintf("it is parked with %s, and a volume takes a new path only when it is attached", or(was.Mount, "no path"))})
	case want.Machine == was.Machine:
	case was.Machine == "":
		out.Steps = append(out.Steps, sdk.Step("attach", at(want.Machine)))
	case want.Machine == "":
		out.Steps = append(out.Steps, sdk.Step("detach", nil))
	default:
		out.Steps = append(out.Steps, sdk.Step("move", at(want.Machine)))
	}
	return out, nil
}

func or(s, none string) string {
	if s == "" {
		return none
	}
	return s
}

func usage(s Spec) map[string]int64 {
	u := map[string]int64{"volumes.count": 1, "volumes.size_gb": int64(s.SizeGB), "volumes.backup_gb": 0}
	if s.Backup {
		u["volumes.backup_gb"] = int64(s.SizeGB)
	}
	return u
}

func (p *Plugin) Plan(_ context.Context, req *pluginpb.PlanRequest) (*pluginpb.PlanResponse, error) {
	if req.GetType() != "volume" {
		return nil, sdk.Refuse("no type %q here", req.GetType())
	}
	v, err := p.volumes(req.GetZone())
	if err != nil {
		return nil, err
	}
	var cur Spec
	raw := req.GetSpec()
	if req.GetAction() != "" {
		if err := sdk.Decode(req.GetCurrent().GetSpec(), &cur); err != nil {
			return nil, err
		}
		raw = req.GetParams()
	}
	s, refusals, err := next(v, cur, req.GetAction(), raw, req.GetRefs())
	if err != nil {
		return nil, err
	}
	return &pluginpb.PlanResponse{Spec: sdk.JSON(s), Usage: usage(s), Refusals: refusals}, nil
}

func observe(v driver.Volume) Observed {
	return Observed{EngineRef: v.EngineRef, Node: v.Node, Content: v.Content, SizeGB: v.SizeGB, Backup: v.Backup,
		Machine: v.Guest, Mount: v.Mount, Device: v.Device, InGuest: v.InGuest}
}

// engineErr: a refusal or a missing thing will not change by trying again;
// anything else might (the core retries UNAVAILABLE).
func engineErr(err error) error {
	if errors.Is(err, driver.ErrRefused) || errors.Is(err, driver.ErrNotFound) {
		return sdk.NotNow("%v", err)
	}
	return sdk.Unreachable("%v", err)
}

func (p *Plugin) volume(r *pluginpb.Resource) (driver.Volumes, Spec, error) {
	v, err := p.volumes(r.GetZone())
	if err != nil {
		return nil, Spec{}, err
	}
	var s Spec
	if err := sdk.Decode(r.GetSpec(), &s); err != nil {
		return nil, Spec{}, err
	}
	return v, s, nil
}

func place(r *pluginpb.Resource, s Spec) driver.Place {
	pl := driver.Place{Guest: s.Machine, Owner: r.GetOwner()}
	if s.Content == driver.ContentFilesystem {
		pl.Mount = s.Mount
	}
	return pl
}

func (p *Plugin) Create(ctx context.Context, req *pluginpb.CreateRequest) (*pluginpb.CreateResponse, error) {
	r := req.GetResource()
	v, s, err := p.volume(r)
	if err != nil {
		return nil, err
	}
	got, err := v.CreateVolume(ctx, driver.VolumeSpec{ID: r.GetId(), Content: s.Content, SizeGB: s.SizeGB, Backup: s.Backup, At: place(r, s),
		Label: r.GetName()})
	if err != nil {
		return nil, engineErr(err)
	}
	fields := map[string]string{"engine_ref": got.EngineRef}
	if s.Machine != "" {
		fields["machine"] = s.Machine
	}
	return &pluginpb.CreateResponse{Observed: sdk.JSON(observe(got)),
		Events: []*pluginpb.Event{sdk.Event("volume.created", "", fields)}}, nil
}

func (p *Plugin) Delete(ctx context.Context, req *pluginpb.DeleteRequest) (*pluginpb.DeleteResponse, error) {
	r := req.GetResource()
	v, _, err := p.volume(r)
	if err != nil {
		return nil, err
	}
	if err := v.DeleteVolume(ctx, r.GetId()); err != nil && !errors.Is(err, driver.ErrNotFound) {
		return nil, engineErr(err)
	}
	return &pluginpb.DeleteResponse{Events: []*pluginpb.Event{sdk.Event("volume.deleted", "", nil)}}, nil
}

func (p *Plugin) Act(ctx context.Context, req *pluginpb.ActRequest) (*pluginpb.ActResponse, error) {
	r := req.GetResource()
	// every action is planned: the spec the core hands over is already the
	// one the action asked for (written at its admission) — the engine is
	// brought to it, whether this is the first run or one resumed after the
	// brain stopped half-way
	v, s, err := p.volume(r)
	if err != nil {
		return nil, err
	}
	var got driver.Volume
	var ev *pluginpb.Event
	switch req.GetAction() {
	case "attach", "move", "detach":
		was, _ := v.Volume(ctx, r.GetId())
		got, err = v.PlaceVolume(ctx, r.GetId(), place(r, s))
		name := map[string]string{"attach": "volume.attached", "move": "volume.moved", "detach": "volume.detached"}[req.GetAction()]
		ev = sdk.Event(name, "", map[string]string{"from": was.Guest, "to": s.Machine})
	case "resize":
		got, err = v.ResizeVolume(ctx, r.GetId(), s.SizeGB)
		ev = sdk.Event("volume.resized", "", map[string]string{"size_gb": fmt.Sprint(s.SizeGB)})
	case "set_backup":
		got, err = v.SetVolumeBackup(ctx, r.GetId(), s.Backup)
		ev = sdk.Event("volume.backup_set", "", map[string]string{"backup": fmt.Sprint(s.Backup)})
	default:
		return nil, sdk.Refuse("no action %q on a volume", req.GetAction())
	}
	if err != nil {
		return nil, engineErr(err)
	}
	return &pluginpb.ActResponse{Spec: sdk.JSON(s), Observed: sdk.JSON(observe(got)), Events: []*pluginpb.Event{ev}}, nil
}

func (p *Plugin) Reconcile(ctx context.Context, req *pluginpb.ReconcileRequest) (*pluginpb.ReconcileResponse, error) {
	r := req.GetResource()
	v, s, err := p.volume(r)
	if err != nil {
		return nil, err
	}
	got, err := v.Volume(ctx, r.GetId())
	if errors.Is(err, driver.ErrNotFound) {
		return &pluginpb.ReconcileResponse{Drift: pluginpb.Drift_DRIFT_MISSING, Detail: "the engine has no such volume"}, nil
	}
	if err != nil {
		return nil, engineErr(err)
	}
	var fixed, left []string
	try := func(what string, f func() (driver.Volume, error)) {
		next, err := f()
		if err != nil {
			left = append(left, fmt.Sprintf("%s: %v", what, err))
			return
		}
		got = next
		fixed = append(fixed, what)
	}
	wantMount := s.Content == driver.ContentFilesystem && s.Machine != "" && got.Mount != s.Mount
	if got.Guest != s.Machine || wantMount {
		// where it is not what its owner last asked (a move cut half-way):
		// put back where the registry says
		try("place", func() (driver.Volume, error) { return v.PlaceVolume(ctx, r.GetId(), place(r, s)) })
	}
	switch {
	case got.SizeGB < s.SizeGB:
		try("size", func() (driver.Volume, error) { return v.ResizeVolume(ctx, r.GetId(), s.SizeGB) })
	case got.SizeGB > s.SizeGB:
		left = append(left, fmt.Sprintf("it is %d GB on the engine, %d GB here: a volume never shrinks — resize it to %d GB to say so", got.SizeGB, s.SizeGB, got.SizeGB))
	}
	if got.Backup != s.Backup {
		try("backup", func() (driver.Volume, error) { return v.SetVolumeBackup(ctx, r.GetId(), s.Backup) })
	}
	// what it is called, where the engine shows it: a line for people — one
	// that cannot be written now is written at the next look, and says nothing
	if got.Label != r.GetName() {
		if named, err := v.RelabelVolume(ctx, r.GetId(), r.GetName()); err == nil {
			got = named
			fixed = append(fixed, "name")
		}
	}
	resp := &pluginpb.ReconcileResponse{Observed: sdk.JSON(observe(got))}
	switch {
	case len(left) > 0:
		resp.Drift = pluginpb.Drift_DRIFT_DRIFTED
		resp.Detail = strings.Join(left, "; ")
	case len(fixed) > 0:
		resp.Drift, resp.Detail = pluginpb.Drift_DRIFT_REPAIRED, "put back: "+strings.Join(fixed, ", ")
		resp.Events = []*pluginpb.Event{sdk.Event("volume.repaired", resp.Detail, nil)}
	default:
		resp.Drift = pluginpb.Drift_DRIFT_IN_SYNC
	}
	return resp, nil
}
