// Package toy is the smallest plugin that exercises the whole contract: a
// "box" — a pretend guest on any driver with the guests facet — with a create,
// a delete, three actions (one that changes what it holds, one that needs a
// capability not every zone has), quantity and choice dimensions, events and a
// reconcile that repairs what it can.
//
// It is the example to copy when writing a plugin, and the one the core's
// tests run end to end on the fake engine. It is not a machines plugin: it
// knows nothing of images, networks or disks.
package toy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"

	"github.com/tomblancdev/hangar/driver"
	_ "github.com/tomblancdev/hangar/driver/fake" // the engine it is proved on
	"github.com/tomblancdev/hangar/sdk"
	"github.com/tomblancdev/hangar/sdk/pluginpb"
)

// Name is the plugin's own name: the operator enables it as "toy".
const Name = "toy"

// Spec is a box's desired state.
type Spec struct {
	Kind     string `json:"kind"`
	Cores    int    `json:"cores"`
	MemoryGB int    `json:"memory_gb"`
	Running  bool   `json:"running"`
}

// Observed is what the engine reports of a box.
type Observed struct {
	EngineRef string `json:"engine_ref"`
	Kind      string `json:"kind"`
	Cores     int    `json:"cores"`
	MemoryGB  int    `json:"memory_gb"`
	Running   bool   `json:"running"`
}

type resizeParams struct {
	Cores    *int `json:"cores"`
	MemoryGB *int `json:"memory_gb"`
}

const boxSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "kind":      { "type": "string", "enum": ["container", "vm"], "default": "container",
                   "description": "A container shares the host's kernel; a VM has its own." },
    "cores":     { "type": "integer", "minimum": 1, "maximum": 64, "default": 1 },
    "memory_gb": { "type": "integer", "minimum": 1, "maximum": 1024, "default": 1 }
  }
}`

const resizeSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "minProperties": 1,
  "properties": {
    "cores":     { "type": "integer", "minimum": 1, "maximum": 64 },
    "memory_gb": { "type": "integer", "minimum": 1, "maximum": 1024 }
  }
}`

// Plugin is the toy plugin. Its zero value is not usable; call New.
type Plugin struct {
	pluginpb.UnimplementedPluginServiceServer

	mu    sync.RWMutex
	zones map[string]driver.Driver
}

// New returns a plugin with no zone configured.
func New() *Plugin { return &Plugin{zones: map[string]driver.Driver{}} }

// Driver returns the driver opened for a zone — for tests, which reach the
// fake engine behind it.
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
			Name:        "box",
			IdPrefix:    "box",
			Title:       "Box",
			Description: "A pretend guest with cores and memory; the plugin that proves the contract.",
			Schema:      []byte(boxSchema),
			Actions: []*pluginpb.Action{
				{Name: "start", Description: "Power the box on."},
				{Name: "stop", Description: "Power the box off."},
				{Name: "resize", Description: "Set its cores and memory.", ParamsSchema: []byte(resizeSchema), ChangesUsage: true},
				{Name: "suspend", Description: "Power it off keeping its memory on disk.", Requires: []string{driver.GuestSuspendToDisk}},
			},
		}},
		Dimensions: []*pluginpb.Dimension{
			{Name: "toy.boxes", Kind: pluginpb.DimensionKind_DIMENSION_KIND_QUANTITY, Description: "How many boxes."},
			{Name: "toy.cores", Kind: pluginpb.DimensionKind_DIMENSION_KIND_QUANTITY, Unit: "vCPU", Description: "Cores across every box."},
			{Name: "toy.memory_gb", Kind: pluginpb.DimensionKind_DIMENSION_KIND_QUANTITY, Unit: "GB", Description: "Memory across every box."},
			{Name: "toy.kind", Kind: pluginpb.DimensionKind_DIMENSION_KIND_CHOICE, Description: "The kinds of box allowed: container, vm."},
		},
		Requires:   []string{driver.GuestTags},
		Credential: &pluginpb.Credential{Required: false, Description: "None for the fake engine, unless its zone expects one."},
		Events:     []string{"box.created", "box.deleted", "box.started", "box.stopped", "box.resized", "box.suspended", "box.repaired"},
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
			Zone: z.GetName(), Endpoint: z.GetEndpoint(), Options: z.GetOptions(), Credential: z.GetCredential(),
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

// PlanChange says what brings a box to another spec: its cores and memory
// through resize; its kind is set at its birth.
func (p *Plugin) PlanChange(_ context.Context, req *pluginpb.PlanChangeRequest) (*pluginpb.PlanChangeResponse, error) {
	if req.GetCurrent().GetType() != "box" {
		return nil, sdk.Refuse("no type %q here", req.GetCurrent().GetType())
	}
	var was Spec
	if err := sdk.Decode(req.GetCurrent().GetSpec(), &was); err != nil {
		return nil, err
	}
	want := Spec{Kind: "container", Cores: 1, MemoryGB: 1}
	if err := sdk.Decode(req.GetSpec(), &want); err != nil {
		return nil, err
	}
	out := &pluginpb.PlanChangeResponse{}
	if want.Kind != was.Kind {
		out.Fixed = append(out.Fixed, sdk.Fixed("/kind", "a "+was.Kind, "a box's kind"))
	}
	if want.Cores != was.Cores || want.MemoryGB != was.MemoryGB {
		out.Steps = append(out.Steps, sdk.Step("resize", map[string]any{"cores": want.Cores, "memory_gb": want.MemoryGB}))
	}
	return out, nil
}

func (p *Plugin) Plan(_ context.Context, req *pluginpb.PlanRequest) (*pluginpb.PlanResponse, error) {
	if req.GetType() != "box" {
		return nil, sdk.Refuse("no type %q here", req.GetType())
	}
	_, caps, err := p.guests(req.GetZone())
	if err != nil {
		return nil, err
	}
	var s Spec
	var refusals []*pluginpb.Refusal
	switch req.GetAction() {
	case "":
		s = Spec{Kind: "container", Cores: 1, MemoryGB: 1}
		if err := sdk.Decode(req.GetSpec(), &s); err != nil {
			return nil, err
		}
		s.Running = true
		need := map[string]driver.Capability{"container": driver.KindContainer, "vm": driver.KindVM}[s.Kind]
		if !slices.Contains(caps, need) {
			refusals = append(refusals, &pluginpb.Refusal{Field: "/kind", Reason: fmt.Sprintf("zone %s runs no %s", req.GetZone(), s.Kind)})
		}
	case "start", "stop", "suspend":
		if err := sdk.Decode(req.GetCurrent().GetSpec(), &s); err != nil {
			return nil, err
		}
		s.Running = req.GetAction() == "start"
	case "resize":
		if err := sdk.Decode(req.GetCurrent().GetSpec(), &s); err != nil {
			return nil, err
		}
		var rp resizeParams
		if err := sdk.Decode(req.GetParams(), &rp); err != nil {
			return nil, err
		}
		was := s
		if rp.Cores != nil {
			s.Cores = *rp.Cores
		}
		if rp.MemoryGB != nil {
			s.MemoryGB = *rp.MemoryGB
		}
		if s.Running && s.MemoryGB < was.MemoryGB && !slices.Contains(caps, driver.ResizeLiveMemoryDown) {
			refusals = append(refusals, &pluginpb.Refusal{Field: "/memory_gb",
				Reason: fmt.Sprintf("zone %s cannot shrink a running box's memory: stop it first", req.GetZone())})
		}
	default:
		return nil, sdk.Refuse("no action %q on a box", req.GetAction())
	}
	return &pluginpb.PlanResponse{
		Spec:     sdk.JSON(s),
		Usage:    map[string]int64{"toy.boxes": 1, "toy.cores": int64(s.Cores), "toy.memory_gb": int64(s.MemoryGB)},
		Choices:  map[string]string{"toy.kind": s.Kind},
		Refusals: refusals,
	}, nil
}

func (p *Plugin) Create(ctx context.Context, req *pluginpb.CreateRequest) (*pluginpb.CreateResponse, error) {
	r := req.GetResource()
	g, s, err := p.box(r)
	if err != nil {
		return nil, err
	}
	tags := map[string]string{"hangar-owner": r.GetOwner()}
	for k, v := range r.GetTags() {
		tags[k] = v
	}
	guest, err := g.CreateGuest(ctx, driver.GuestSpec{ID: r.GetId(), Kind: s.Kind, Cores: s.Cores, MemoryMB: s.MemoryGB * 1024, Tags: tags})
	if err != nil {
		return nil, engineErr(err)
	}
	return &pluginpb.CreateResponse{
		Observed: sdk.JSON(observe(guest)),
		Events:   []*pluginpb.Event{sdk.Event("box.created", "the box exists", map[string]string{"engine_ref": guest.EngineRef})},
	}, nil
}

func (p *Plugin) Delete(ctx context.Context, req *pluginpb.DeleteRequest) (*pluginpb.DeleteResponse, error) {
	g, _, err := p.box(req.GetResource())
	if err != nil {
		return nil, err
	}
	if err := g.DeleteGuest(ctx, req.GetResource().GetId()); err != nil && !errors.Is(err, driver.ErrNotFound) {
		return nil, engineErr(err)
	}
	return &pluginpb.DeleteResponse{Events: []*pluginpb.Event{sdk.Event("box.deleted", "the box is gone", nil)}}, nil
}

func (p *Plugin) Act(ctx context.Context, req *pluginpb.ActRequest) (*pluginpb.ActResponse, error) {
	r := req.GetResource()
	g, s, err := p.box(r)
	if err != nil {
		return nil, err
	}
	var guest driver.Guest
	var ev *pluginpb.Event
	switch req.GetAction() {
	case "start", "stop", "suspend":
		s.Running = req.GetAction() == "start"
		guest, err = g.SetPower(ctx, r.GetId(), s.Running)
		ev = sdk.Event("box."+map[string]string{"start": "started", "stop": "stopped", "suspend": "suspended"}[req.GetAction()], "", nil)
	case "resize":
		var rp resizeParams
		if err := sdk.Decode(req.GetParams(), &rp); err != nil {
			return nil, err
		}
		if rp.Cores != nil {
			s.Cores = *rp.Cores
		}
		if rp.MemoryGB != nil {
			s.MemoryGB = *rp.MemoryGB
		}
		guest, err = g.ResizeGuest(ctx, r.GetId(), s.Cores, s.MemoryGB*1024)
		ev = sdk.Event("box.resized", "", map[string]string{"cores": strconv.Itoa(s.Cores), "memory_gb": strconv.Itoa(s.MemoryGB)})
	default:
		return nil, sdk.Refuse("no action %q on a box", req.GetAction())
	}
	if err != nil {
		return nil, engineErr(err)
	}
	return &pluginpb.ActResponse{Spec: sdk.JSON(s), Observed: sdk.JSON(observe(guest)), Events: []*pluginpb.Event{ev}}, nil
}

func (p *Plugin) Reconcile(ctx context.Context, req *pluginpb.ReconcileRequest) (*pluginpb.ReconcileResponse, error) {
	r := req.GetResource()
	g, s, err := p.box(r)
	if err != nil {
		return nil, err
	}
	guest, err := g.Guest(ctx, r.GetId())
	if errors.Is(err, driver.ErrNotFound) {
		return &pluginpb.ReconcileResponse{Drift: pluginpb.Drift_DRIFT_MISSING, Detail: "the engine has no such box"}, nil
	}
	if err != nil {
		return nil, engineErr(err)
	}
	var fixed []string
	if guest.Cores != s.Cores || guest.MemoryMB != s.MemoryGB*1024 {
		if guest, err = g.ResizeGuest(ctx, r.GetId(), s.Cores, s.MemoryGB*1024); err != nil {
			return &pluginpb.ReconcileResponse{Drift: pluginpb.Drift_DRIFT_DRIFTED, Observed: sdk.JSON(observe(guest)),
				Detail: "its size differs from the spec and could not be put back: " + err.Error()}, nil
		}
		fixed = append(fixed, "size")
	}
	if guest.Running != s.Running {
		if guest, err = g.SetPower(ctx, r.GetId(), s.Running); err != nil {
			return nil, engineErr(err)
		}
		fixed = append(fixed, "power")
	}
	if len(fixed) == 0 {
		return &pluginpb.ReconcileResponse{Drift: pluginpb.Drift_DRIFT_IN_SYNC, Observed: sdk.JSON(observe(guest))}, nil
	}
	detail := fmt.Sprintf("put back: %v", fixed)
	return &pluginpb.ReconcileResponse{
		Drift: pluginpb.Drift_DRIFT_REPAIRED, Detail: detail, Observed: sdk.JSON(observe(guest)),
		Events: []*pluginpb.Event{sdk.Event("box.repaired", detail, nil)},
	}, nil
}

// box resolves a resource's zone to its guests facet and decodes its spec.
func (p *Plugin) box(r *pluginpb.Resource) (driver.Guests, Spec, error) {
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
	return Observed{EngineRef: g.EngineRef, Kind: g.Kind, Cores: g.Cores, MemoryGB: g.MemoryMB / 1024, Running: g.Running}
}

// engineErr turns a driver's error into the status the core reads: a refusal
// or a missing guest will not change by trying again; anything else might.
func engineErr(err error) error {
	if errors.Is(err, driver.ErrRefused) || errors.Is(err, driver.ErrNotFound) {
		return sdk.NotNow("%v", err)
	}
	return sdk.Unreachable("%v", err)
}
