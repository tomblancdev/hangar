// Package networks is the plugin that makes networks of one's own: a private
// network only its machines are on, each given its address there, with one
// way out — a gateway the cloud makes — on any driver with the networks
// facet (net.private).
//
// One type, network (net-…): create · set_key_pairs · share · delete.
//
// A person chooses no addresses: a network's range is its zone's to give, a
// machine's address its engine's. A machine names its network at its birth
// (the machines plugin's `network`) and is on it for its life; a machine
// that names none is put on its owner's network called default, made then if
// it is not there (the core's doing — a reference with a default).
//
// Inside a network its machines reach each other freely. Out, they go
// through its gateway, to what the zone's operator lets out. In, nothing
// comes unasked: its owner reaches a machine THROUGH the gateway, with a key
// pair the network names (key_pairs) —
//
//	ssh -J jump@<the gateway> <user>@<the machine's address>
//
// — a jump that opens that network and nothing else, never a shell on the
// gateway. A gateway keeps nothing and is never patched: when a network's
// key pairs change, its gateway is made again (its machines' way out is cut
// for the half minute that takes).
//
// A network may be shared (shared_with, "x-hangar-share"): the people it is
// shared with put machines on it — a family's game night; only its owner
// changes or deletes it. And it is not deleted while a machine is on it (the
// core's rule for a reference marked "x-hangar-member"; the engine's own
// refusal is the net under it).
//
// Its gateway runs only while one of its machines does (the machines
// plugin's driver starts and stops it), and its memory is booked in the
// zone's guaranteed pool for as long as the network exists: a machine that
// may start can always have its way out.
//
// Limits: networks.count, and the choice networks.visibility (private,
// shared, public).
//
// It requires fence.pool and net.private.
package networks

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/tomblancdev/hangar/driver"
	_ "github.com/tomblancdev/hangar/driver/fake"
	_ "github.com/tomblancdev/hangar/driver/proxmox"
	"github.com/tomblancdev/hangar/sdk"
	"github.com/tomblancdev/hangar/sdk/pluginpb"
)

// Name is the plugin's own name: the operator enables it as "networks".
const Name = "networks"

// Whom a network is shared with, as networks.visibility counts it.
const (
	Private  = "private"
	Shared   = "shared"
	Public   = "public"
	Everyone = "*"
)

// Spec is a network's desired state.
type Spec struct {
	// KeyPairs: the key pairs that may jump through its gateway, by id.
	KeyPairs []string `json:"key_pairs,omitempty"`
	// Keys: those key pairs' public keys, as they were when the network last
	// named them — the plugin's own writing (never a request's): what its
	// gateway is born with, and held against at every look.
	Keys       []string `json:"jump_keys,omitempty"`
	SharedWith []string `json:"shared_with,omitempty"`
}

// Observed is a network as its engine reports it.
type Observed struct {
	EngineRef string `json:"engine_ref"`
	Node      string `json:"node,omitempty"`
	// Range: the addresses its machines are given theirs in; Gateway: their
	// way out.
	Range   string `json:"range"`
	Gateway string `json:"gateway"`
	// Jump: where its key pairs jump through, as ssh's -J takes it.
	Jump string `json:"jump,omitempty"`
	// Wall: its gateway stands behind its zone's wall.
	Wall string `json:"wall,omitempty"`
	// Running: its gateway runs — while one of its machines does.
	Running bool `json:"running"`
	// Keys: how many keys its gateway was born with.
	Keys int `json:"keys"`
}

const networkSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "x-hangar-summary": ["{range}", "jump through {jump}", "{keys} keys", "shared with {shared_with}"],
  "x-hangar-status": { "field": "running", "on": "up", "off": "idle" },
  "properties": {
    "key_pairs":   { "type": "array", "maxItems": 10, "uniqueItems": true,
                     "items": { "type": "string", "x-hangar-ref": "keypair" },
                     "description": "Your key pairs that may jump through its gateway to its machines (ssh -J): by name or id. None: nobody comes in." },
    "shared_with": { "type": "array", "x-hangar-share": true, "maxItems": 20, "uniqueItems": true,
                     "items": { "type": "string", "minLength": 1, "maxLength": 128 },
                     "description": "Groups you are in that may put machines on it; * = everyone. Private when absent." }
  }
}`

const keyPairsSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["key_pairs"],
  "properties": {
    "key_pairs": { "type": "array", "maxItems": 10, "uniqueItems": true,
                   "items": { "type": "string", "x-hangar-ref": "keypair" },
                   "description": "The key pairs that may jump through its gateway now: by name or id; [] = nobody." }
  }
}`

const shareSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["shared_with"],
  "properties": {
    "shared_with": { "type": "array", "x-hangar-share": true, "maxItems": 20, "uniqueItems": true,
                     "items": { "type": "string", "minLength": 1, "maxLength": 128 },
                     "description": "Whom it is shared with now: groups you are in, * = everyone, [] = no one but you." }
  }
}`

// Plugin is the networks plugin. Its zero value is not usable; call New.
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
			Name: "network", IdPrefix: "net", Title: "Network",
			Description: "A private network of yours: only its machines are on it, each given its address there; they go out through its gateway, and nothing comes in but your own jump through it.",
			Schema:      []byte(networkSchema),
			Actions: []*pluginpb.Action{
				{Name: "set_key_pairs", Description: "Say which key pairs may jump through its gateway. Its gateway is made again with them: its machines' way out is cut for half a minute.",
					ParamsSchema: []byte(keyPairsSchema), ChangesUsage: true},
				{Name: "share", Description: "Say whom it is shared with: groups you are in, * = everyone, [] = no one but you. They put machines on it; only you change or delete it.",
					ParamsSchema: []byte(shareSchema), ChangesUsage: true},
			},
		}},
		Dimensions: []*pluginpb.Dimension{
			{Name: "networks.count", Kind: pluginpb.DimensionKind_DIMENSION_KIND_QUANTITY, Description: "How many networks."},
			{Name: "networks.visibility", Kind: pluginpb.DimensionKind_DIMENSION_KIND_CHOICE, Description: "Whom they are shared with: private, shared (groups), public (everyone)."},
		},
		Requires: []string{driver.FencePool, driver.NetPrivate},
		Credential: &pluginpb.Credential{Required: false,
			Description: "Per zone, the engine's credential fenced to the product's own guests, allowed the networks' gateways and nothing of the machines but to read them (Proxmox: an API token, user@realm!name=secret — the least it needs is in docs/proxmox.md). None for the fake engine."},
		Events: []string{"network.created", "network.deleted", "network.key_pairs_set", "network.shared", "network.repaired"},
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
		if _, ok := d.(driver.Networks); !ok {
			_ = d.Close()
			resp.Zones = append(resp.Zones, &pluginpb.ZoneReport{Name: z.GetName(), Error: "its driver makes no networks"})
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

func (p *Plugin) networks(zone string) (driver.Networks, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	d, ok := p.zones[zone]
	if !ok {
		return nil, sdk.NotNow("zone %s is not open to this plugin", zone)
	}
	return d.(driver.Networks), nil
}

var groupName = regexp.MustCompile(`^\S(.*\S)?$`)

// visibility is who a network is shared with, as a word.
func visibility(groups []string) string {
	switch {
	case slices.Contains(groups, Everyone):
		return Public
	case len(groups) > 0:
		return Shared
	}
	return Private
}

// normalize: sorted, no repeats; everyone alone says it all.
func normalize(groups []string) ([]string, *pluginpb.Refusal) {
	for _, g := range groups {
		if !groupName.MatchString(g) {
			return nil, &pluginpb.Refusal{Field: "/shared_with", Reason: fmt.Sprintf("%q: a group's name, or * for everyone", g)}
		}
	}
	if slices.Contains(groups, Everyone) {
		return []string{Everyone}, nil
	}
	out := slices.Clone(groups)
	slices.Sort(out)
	return slices.Compact(out), nil
}

// keysOf are the public keys of the key pairs a network names, in the order
// it names them — handed over by the core, which checked each is the owner's.
func keysOf(pairs []string, refs []*pluginpb.Resource) ([]string, *pluginpb.Refusal) {
	var out []string
	for i, id := range pairs {
		found := false
		for _, r := range refs {
			if r.GetId() != id || r.GetType() != "keypair" {
				continue
			}
			var k struct {
				PublicKey string `json:"public_key"`
			}
			if err := sdk.Decode(r.GetSpec(), &k); err != nil || k.PublicKey == "" {
				return nil, &pluginpb.Refusal{Field: fmt.Sprintf("/key_pairs/%d", i), Reason: fmt.Sprintf("%s holds no public key", id)}
			}
			out, found = append(out, k.PublicKey), true
			break
		}
		if !found {
			return nil, &pluginpb.Refusal{Field: fmt.Sprintf("/key_pairs/%d", i), Reason: fmt.Sprintf("no key pair %s", id)}
		}
	}
	return out, nil
}

// next is a network's spec once a request is done — for Plan to admit and
// for Act to bring the engine to — and why it is refused, if it is.
func next(cur Spec, action string, raw []byte, refs []*pluginpb.Resource) (Spec, []*pluginpb.Refusal, error) {
	s := cur
	var refusals []*pluginpb.Refusal
	refuse := func(r *pluginpb.Refusal) {
		if r != nil {
			refusals = append(refusals, r)
		}
	}
	var r *pluginpb.Refusal
	switch action {
	case "":
		s = Spec{}
		if err := sdk.Decode(raw, &s); err != nil {
			return s, nil, err
		}
		s.Keys, r = keysOf(s.KeyPairs, refs)
		refuse(r)
		s.SharedWith, r = normalize(s.SharedWith)
		refuse(r)
	case "set_key_pairs":
		var kp struct {
			KeyPairs []string `json:"key_pairs"`
		}
		if err := sdk.Decode(raw, &kp); err != nil {
			return s, nil, err
		}
		s.KeyPairs = kp.KeyPairs
		s.Keys, r = keysOf(s.KeyPairs, refs)
		refuse(r)
	case "share":
		var sp struct {
			SharedWith []string `json:"shared_with"`
		}
		if err := sdk.Decode(raw, &sp); err != nil {
			return s, nil, err
		}
		s.SharedWith, r = normalize(sp.SharedWith)
		refuse(r)
	default:
		return s, nil, sdk.Refuse("no action %q on a network", action)
	}
	return s, refusals, nil
}

func (p *Plugin) Plan(_ context.Context, req *pluginpb.PlanRequest) (*pluginpb.PlanResponse, error) {
	if req.GetType() != "network" {
		return nil, sdk.Refuse("no type %q here", req.GetType())
	}
	n, err := p.networks(req.GetZone())
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
	s, refusals, err := next(cur, req.GetAction(), raw, req.GetRefs())
	if err != nil {
		return nil, err
	}
	return &pluginpb.PlanResponse{
		Spec:     sdk.JSON(s),
		Usage:    map[string]int64{"networks.count": 1},
		Choices:  map[string]string{"networks.visibility": visibility(s.SharedWith)},
		Refusals: refusals,
		// its gateway's memory, promised for as long as the network exists:
		// a machine of it that may start can always have its way out
		Room: &pluginpb.Room{GuaranteedMb: int64(n.NetworkRoomMB())},
	}, nil
}

// PlanChange says what brings a network to another spec: its key pairs
// through set_key_pairs, whom it is shared with through share. Nothing of a
// network is set at its birth but what its engine gave it.
func (p *Plugin) PlanChange(_ context.Context, req *pluginpb.PlanChangeRequest) (*pluginpb.PlanChangeResponse, error) {
	if req.GetCurrent().GetType() != "network" {
		return nil, sdk.Refuse("no type %q here", req.GetCurrent().GetType())
	}
	var was, want Spec
	if err := sdk.Decode(req.GetCurrent().GetSpec(), &was); err != nil {
		return nil, err
	}
	if err := sdk.Decode(req.GetSpec(), &want); err != nil {
		return nil, err
	}
	out := &pluginpb.PlanChangeResponse{}
	if !sameSet(want.KeyPairs, was.KeyPairs) {
		pairs := want.KeyPairs
		if pairs == nil {
			pairs = []string{}
		}
		out.Steps = append(out.Steps, sdk.Step("set_key_pairs", map[string]any{"key_pairs": pairs}))
	}
	shared, r := normalize(want.SharedWith)
	if r != nil {
		return nil, sdk.Refuse("%s: %s", r.GetField(), r.GetReason())
	}
	if !slices.Equal(shared, was.SharedWith) {
		if shared == nil {
			shared = []string{}
		}
		out.Steps = append(out.Steps, sdk.Step("share", map[string]any{"shared_with": shared}))
	}
	return out, nil
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(slices.Compact(a), slices.Compact(b))
}

func observe(n driver.Network) Observed {
	return Observed{EngineRef: n.EngineRef, Node: n.Node, Range: n.Range, Gateway: n.Gateway, Jump: n.Jump, Wall: n.Wall,
		Running: n.Running, Keys: n.Keys}
}

// engineErr: a refusal or a missing thing will not change by trying again;
// anything else might (the core retries UNAVAILABLE).
func engineErr(err error) error {
	if errors.Is(err, driver.ErrRefused) || errors.Is(err, driver.ErrNotFound) {
		return sdk.NotNow("%v", err)
	}
	return sdk.Unreachable("%v", err)
}

func (p *Plugin) network(r *pluginpb.Resource) (driver.Networks, Spec, error) {
	n, err := p.networks(r.GetZone())
	if err != nil {
		return nil, Spec{}, err
	}
	var s Spec
	if err := sdk.Decode(r.GetSpec(), &s); err != nil {
		return nil, Spec{}, err
	}
	return n, s, nil
}

func specOf(r *pluginpb.Resource, s Spec) driver.NetworkSpec {
	return driver.NetworkSpec{ID: r.GetId(), Label: sdk.Label(r, "network"), JumpKeys: s.Keys}
}

func (p *Plugin) Create(ctx context.Context, req *pluginpb.CreateRequest) (*pluginpb.CreateResponse, error) {
	r := req.GetResource()
	n, s, err := p.network(r)
	if err != nil {
		return nil, err
	}
	got, err := n.CreateNetwork(ctx, specOf(r, s))
	if err != nil {
		return nil, engineErr(err)
	}
	return &pluginpb.CreateResponse{Observed: sdk.JSON(observe(got)),
		Events: []*pluginpb.Event{sdk.Event("network.created", "", map[string]string{"engine_ref": got.EngineRef, "range": got.Range})}}, nil
}

func (p *Plugin) Delete(ctx context.Context, req *pluginpb.DeleteRequest) (*pluginpb.DeleteResponse, error) {
	r := req.GetResource()
	n, _, err := p.network(r)
	if err != nil {
		return nil, err
	}
	if err := n.DeleteNetwork(ctx, r.GetId()); err != nil && !errors.Is(err, driver.ErrNotFound) {
		return nil, engineErr(err)
	}
	return &pluginpb.DeleteResponse{Events: []*pluginpb.Event{sdk.Event("network.deleted", "", nil)}}, nil
}

// wasAt is where a network's gateway was when it was last read.
func wasAt(r *pluginpb.Resource) string {
	var o Observed
	_ = sdk.Decode(r.GetObserved(), &o)
	return o.EngineRef
}

func (p *Plugin) Act(ctx context.Context, req *pluginpb.ActRequest) (*pluginpb.ActResponse, error) {
	r := req.GetResource()
	// every action is planned: the spec the core hands over is already the
	// one the action asked for — the engine is brought to it, whether this is
	// the first run or one resumed after the brain stopped half-way
	n, s, err := p.network(r)
	if err != nil {
		return nil, err
	}
	var ev *pluginpb.Event
	switch req.GetAction() {
	case "set_key_pairs":
		ev = sdk.Event("network.key_pairs_set", "", map[string]string{"key_pairs": strings.Join(s.KeyPairs, ",")})
	case "share":
		ev = sdk.Event("network.shared", "", map[string]string{"shared_with": strings.Join(s.SharedWith, ","), "visibility": visibility(s.SharedWith)})
	default:
		return nil, sdk.Refuse("no action %q on a network", req.GetAction())
	}
	got, _, err := n.TendNetwork(ctx, specOf(r, s), wasAt(r))
	if err != nil {
		return nil, engineErr(err)
	}
	return &pluginpb.ActResponse{Spec: sdk.JSON(s), Observed: sdk.JSON(observe(got)), Events: []*pluginpb.Event{ev}}, nil
}

func (p *Plugin) Reconcile(ctx context.Context, req *pluginpb.ReconcileRequest) (*pluginpb.ReconcileResponse, error) {
	r := req.GetResource()
	n, s, err := p.network(r)
	if err != nil {
		return nil, err
	}
	got, fixed, err := n.TendNetwork(ctx, specOf(r, s), wasAt(r))
	if errors.Is(err, driver.ErrNotFound) {
		return &pluginpb.ReconcileResponse{Drift: pluginpb.Drift_DRIFT_MISSING, Detail: "the engine has no such network"}, nil
	}
	if errors.Is(err, driver.ErrRefused) {
		// as it was last read: what could not be put back is said
		return &pluginpb.ReconcileResponse{Drift: pluginpb.Drift_DRIFT_DRIFTED, Observed: r.GetObserved(),
			Detail: fmt.Sprintf("it could not be brought to what it should be (%s): %v", strings.Join(append(fixed, "…"), ", "), err)}, nil
	}
	if err != nil {
		return nil, engineErr(err) // not reached: the core tries again at its next pass
	}
	resp := &pluginpb.ReconcileResponse{Drift: pluginpb.Drift_DRIFT_IN_SYNC, Observed: sdk.JSON(observe(got))}
	if len(fixed) > 0 {
		resp.Drift, resp.Detail = pluginpb.Drift_DRIFT_REPAIRED, "put back: "+strings.Join(fixed, ", ")
		resp.Events = []*pluginpb.Event{sdk.Event("network.repaired", resp.Detail, nil)}
	}
	return resp, nil
}
