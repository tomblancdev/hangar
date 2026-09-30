// Package images is the plugin that makes images: frozen system disks that
// machines are born from (a machine names one by id, image_id) — baked from
// a recipe the operator wrote, or saved from a person's own stopped machine;
// private at birth, shared with groups or with everyone, retired when a newer
// one replaces it.
//
// One type, image (img-…): create (bake or save) · share · retire · rebake ·
// delete.
//
//   - A BAKE takes minutes: the create answers at once (pending, as AWS says
//     of an image being made) and the brain's reconcile moves it forward —
//     available, or failed with the words of the builder's own first boot.
//     A builder borrows spot room while it works: it wakes a sleeping zone,
//     is born waiting while the zone's room is held, and is let go at once
//     when the room is needed — its bake starts over, by itself, once the
//     room is back. A bake that failed on its own (its recipe) is never
//     retried blindly: rebake runs it again, from the recipe as it is then.
//   - A SAVE takes a stopped machine's system disk, never its volumes (an
//     image may be shared: no one's data goes with it).
//   - SHARE opens an image to groups the owner is in, or to everyone ("*"):
//     they see it and machines of theirs are born from it; only its owner
//     changes or deletes it (the core's rule for a field marked
//     "x-hangar-share").
//   - RETIRE: no machine is born from it any more; those born from it run on.
//   - DELETE is refused by the engine while machines born from it still
//     share its disk (Proxmox VE's linked clones): retire it meanwhile.
//
// Limits: images.count and images.size_gb (one's own images), and two
// choices — images.source (recipe: who may bake; machine: who may save) and
// images.visibility (private, shared, public).
//
// It requires fence.pool, and a driver with the images facet.
//
// Settings (the operator's, one JSON object):
//
//	recipes:
//	  debian-13:
//	    family: debian-13                 # the images a recipe makes are one family (default: its name)
//	    base: {vm: "local:import/debian-13-genericcloud-amd64.qcow2"}   # per kind, the engine's own name
//	    disk_gb: 4
//	    cores: 2                          # the builder's (default 2)
//	    memory_mb: 2048                   # the builder's (default 2048): the spot room a bake borrows
//	    timeout: 30m                      # the builder's time to finish (default 30m)
//	    user_data: |                      # its first boot: #cloud-config, or a #! script
//	      #cloud-config
//	      package_update: true
//	      package_upgrade: true
//	      packages: [qemu-guest-agent]
//
// A recipe is copied into each image it bakes (spec.from): an image says
// what it was made from, and whoever sees the image sees its recipe — no
// secret in one.
package images

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tomblancdev/hangar/driver"
	_ "github.com/tomblancdev/hangar/driver/fake"
	_ "github.com/tomblancdev/hangar/driver/proxmox"
	"github.com/tomblancdev/hangar/sdk"
	"github.com/tomblancdev/hangar/sdk/pluginpb"
)

// Name is the plugin's own name: the operator enables it as "images".
const Name = "images"

// The sources of an image, and its visibilities: the values of the plugin's
// two choice dimensions.
const (
	SourceRecipe  = "recipe"
	SourceMachine = "machine"

	Private = "private"
	Shared  = "shared"
	Public  = "public"
)

// Everyone is the group that shares an image with everyone.
const Everyone = "*"

// maxRestarts: a builder that stops before it finishes this many times in a
// row, unasked, fails its bake — a recipe that powers its builder off would
// otherwise start over forever.
const maxRestarts = 3

// Recipe is how an image is baked, as the operator writes it.
type Recipe struct {
	Family   string            `json:"family,omitempty"`
	Base     map[string]string `json:"base"`
	DiskGB   int               `json:"disk_gb"`
	Cores    int               `json:"cores,omitempty"`
	MemoryMB int               `json:"memory_mb,omitempty"`
	Timeout  string            `json:"timeout,omitempty"`
	UserData string            `json:"user_data"`
}

// From is the recipe an image was baked from, as it was then.
type From struct {
	Name string `json:"recipe"`
	Recipe
}

// Settings are the operator's.
type Settings struct {
	Recipes map[string]Recipe `json:"recipes"`
}

// Spec is an image's desired state.
type Spec struct {
	Name string `json:"name,omitempty"`
	// Recipe: baked from this recipe of the operator's.
	Recipe string `json:"recipe,omitempty"`
	// Machine: saved from this stopped machine of the owner's.
	Machine    string   `json:"machine,omitempty"`
	SharedWith []string `json:"shared_with,omitempty"`
	Retired    bool     `json:"retired,omitempty"`
	// Set by the plan:
	Kind   string `json:"kind"`
	Family string `json:"family,omitempty"`
	SizeGB int    `json:"size_gb"`
	From   *From  `json:"from,omitempty"`
	// Attempt: a bake's number; rebake makes the next.
	Attempt int `json:"attempt,omitempty"`
}

// Observed is an image as its engine reports it.
type Observed struct {
	State     string `json:"state"`
	Detail    string `json:"detail,omitempty"`
	Attempt   int    `json:"attempt,omitempty"`
	EngineRef string `json:"engine_ref,omitempty"`
	Node      string `json:"node,omitempty"`
	SizeGB    int    `json:"size_gb,omitempty"`
	// Forms: the engine's own name for it, per kind — what a machine born
	// from it is made from.
	Forms map[string]string `json:"forms,omitempty"`
	// MadeAt: when it became available.
	MadeAt string `json:"made_at,omitempty"`
	// Restarts: its builder stopped before it finished, unasked, this many
	// times in a row.
	Restarts int `json:"restarts,omitempty"`
}

const imageSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "name":        { "type": "string", "pattern": "^[a-z0-9]([a-z0-9._-]{0,62}[a-z0-9])?$",
                     "description": "A name of yours for it." },
    "recipe":      { "type": "string", "minLength": 1, "maxLength": 64,
                     "description": "Bake it from this recipe of the operator's. Or save it from a machine." },
    "machine":     { "type": "string", "x-hangar-ref": "machine",
                     "description": "Save it from this stopped machine of yours: its system disk, never its volumes." },
    "shared_with": { "type": "array", "x-hangar-share": true, "maxItems": 20, "uniqueItems": true,
                     "items": { "type": "string", "minLength": 1, "maxLength": 128 },
                     "description": "Groups you are in that may see it and have machines born from it; * = everyone. Private when absent." }
  }
}`

const shareSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["shared_with"],
  "properties": {
    "shared_with": { "type": "array", "maxItems": 20, "uniqueItems": true,
                     "items": { "type": "string", "minLength": 1, "maxLength": 128 },
                     "description": "Whom it is shared with now: groups you are in, * = everyone, [] = no one but you." }
  }
}`

// Plugin is the images plugin. Its zero value is not usable; call New.
type Plugin struct {
	pluginpb.UnimplementedPluginServiceServer

	mu       sync.RWMutex
	zones    map[string]driver.Driver
	settings Settings

	// Now is the clock (tests move it).
	Now func() time.Time
}

// New returns a plugin with no zone configured.
func New() *Plugin { return &Plugin{zones: map[string]driver.Driver{}, Now: time.Now} }

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
			Name: "image", IdPrefix: "img", Title: "Image",
			Description: "A frozen system disk machines are born from: baked from a recipe, or saved from a stopped machine of yours.",
			Schema:      []byte(imageSchema),
			Actions: []*pluginpb.Action{
				{Name: "share", Description: "Say whom it is shared with: groups you are in, * = everyone, [] = no one but you. They see it and have machines born from it; only you change or delete it.",
					ParamsSchema: []byte(shareSchema), ChangesUsage: true},
				{Name: "retire", Description: "No machine is born from it any more; those born from it run on.", ChangesUsage: true},
				{Name: "rebake", Description: "Bake a failed image again, from its recipe as it is now.", ChangesUsage: true},
			},
		}},
		Dimensions: []*pluginpb.Dimension{
			{Name: "images.count", Kind: pluginpb.DimensionKind_DIMENSION_KIND_QUANTITY, Description: "How many images of your own."},
			{Name: "images.size_gb", Kind: pluginpb.DimensionKind_DIMENSION_KIND_QUANTITY, Unit: "GB", Description: "Their system disks, added up."},
			{Name: "images.source", Kind: pluginpb.DimensionKind_DIMENSION_KIND_CHOICE, Description: "How images are made: recipe (bake), machine (save)."},
			{Name: "images.visibility", Kind: pluginpb.DimensionKind_DIMENSION_KIND_CHOICE, Description: "Whom they are shared with: private, shared (groups), public (everyone)."},
		},
		Requires: []string{driver.FencePool},
		Credential: &pluginpb.Credential{Required: false,
			Description: "Per zone, the engine's credential fenced to the product's own guests, allowed to make templates and builders in the images pool and to clone its machines (Proxmox: an API token, user@realm!name=secret — the least it needs is in docs/proxmox.md). None for the fake engine."},
		Events: []string{"image.baking", "image.waiting", "image.restarted", "image.baked", "image.bake_failed", "image.saved",
			"image.shared", "image.retired", "image.deleted"},
	}, nil
}

var groupName = regexp.MustCompile(`^\S(.*\S)?$`)

func (p *Plugin) Configure(ctx context.Context, req *pluginpb.ConfigureRequest) (*pluginpb.ConfigureResponse, error) {
	var set Settings
	if err := sdk.Decode(req.GetSettings(), &set); err != nil {
		return nil, err
	}
	for name, r := range set.Recipes {
		if err := checkRecipe(r); err != nil {
			return nil, sdk.Refuse("settings: recipe %q: %v", name, err)
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
		if _, ok := d.(driver.Images); !ok {
			_ = d.Close()
			resp.Zones = append(resp.Zones, &pluginpb.ZoneReport{Name: z.GetName(), Error: "its driver makes no images"})
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

// checkRecipe refuses a recipe the brain could not bake as written.
func checkRecipe(r Recipe) error {
	switch {
	case len(r.Base) == 0:
		return errors.New("base: the engine's own name of what it starts from, per kind (vm: …)")
	case r.DiskGB < 1 || r.DiskGB > 4096:
		return errors.New("disk_gb: 1 to 4096")
	case r.Cores < 0 || r.Cores > 64:
		return errors.New("cores: 1 to 64")
	case r.MemoryMB != 0 && (r.MemoryMB < 512 || r.MemoryMB > 65536):
		return errors.New("memory_mb: 512 to 65536")
	}
	for kind, base := range r.Base {
		if (kind != "vm" && kind != "container") || base == "" {
			return fmt.Errorf("base: a form per kind, vm or container, each the engine's own name")
		}
	}
	if _, err := timeout(r); err != nil {
		return err
	}
	ud := strings.TrimLeft(r.UserData, " \t\r\n")
	switch {
	case strings.HasPrefix(ud, "#cloud-config"):
		// its builder is powered off by the brain once it finished: a
		// recipe that powers it off first would read as one interrupted
		for _, line := range strings.Split(ud, "\n") {
			if strings.HasPrefix(line, "power_state:") {
				return errors.New("user_data: no power_state — the brain powers the builder off once its first boot finished")
			}
		}
	case strings.HasPrefix(ud, "#!"):
	default:
		return errors.New("user_data: a #cloud-config document, or a #! script")
	}
	if len(r.UserData) > 65536 {
		return errors.New("user_data: at most 64 KB")
	}
	return nil
}

func timeout(r Recipe) (time.Duration, error) {
	if r.Timeout == "" {
		return 30 * time.Minute, nil
	}
	d, err := time.ParseDuration(r.Timeout)
	if err != nil || d < time.Minute || d > 6*time.Hour {
		return 0, fmt.Errorf("timeout %q: a duration, 1m to 6h", r.Timeout)
	}
	return d, nil
}

func (p *Plugin) images(zone string) (driver.Images, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	d, ok := p.zones[zone]
	if !ok {
		return nil, sdk.NotNow("zone %s is not open to this plugin", zone)
	}
	return d.(driver.Images), nil
}

func (p *Plugin) recipe(name string) (Recipe, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	r, ok := p.settings.Recipes[name]
	return r, ok
}

func (p *Plugin) recipeNames() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	names := slices.Sorted(maps.Keys(p.settings.Recipes))
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// withDefaults fills a recipe's builder size.
func withDefaults(r Recipe) Recipe {
	if r.Cores == 0 {
		r.Cores = 2
	}
	if r.MemoryMB == 0 {
		r.MemoryMB = 2048
	}
	return r
}

// visibility is who an image is shared with, as a word.
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

// baking: an image whose bake is not over (its builder borrows room).
func baking(s Spec, o Observed) bool {
	if s.Recipe == "" {
		return false
	}
	switch o.State {
	case driver.ImageAvailable:
		return false
	case driver.ImageFailed:
		return o.Attempt != s.Attempt
	}
	return true
}

// room is what an image takes from its zone: its builder's memory, borrowed,
// while its bake is not over; nothing after.
func room(s Spec, o Observed) *pluginpb.Room {
	if !baking(s, o) || s.From == nil {
		return &pluginpb.Room{}
	}
	return &pluginpb.Room{SpotMb: int64(withDefaults(s.From.Recipe).MemoryMB), Running: true}
}

func usage(s Spec) map[string]int64 {
	return map[string]int64{"images.count": 1, "images.size_gb": int64(s.SizeGB)}
}

func choices(s Spec) map[string]string {
	src := SourceRecipe
	if s.Machine != "" {
		src = SourceMachine
	}
	return map[string]string{"images.source": src, "images.visibility": visibility(s.SharedWith)}
}

// machineRef reads a referenced machine: its kind, whether it runs, its disk.
func machineRef(refs []*pluginpb.Resource, id string) (kind string, running bool, diskGB int, ok bool) {
	for _, r := range refs {
		if r.GetId() != id {
			continue
		}
		var m struct {
			Kind    string `json:"kind"`
			Running bool   `json:"running"`
			DiskGB  int    `json:"disk_gb"`
		}
		if sdk.Decode(r.GetSpec(), &m) != nil {
			return "", false, 0, false
		}
		return m.Kind, m.Running, m.DiskGB, true
	}
	return "", false, 0, false
}

func kindWord(kind string) string {
	if kind == "vm" {
		return "VM"
	}
	return kind
}

func (p *Plugin) Plan(_ context.Context, req *pluginpb.PlanRequest) (*pluginpb.PlanResponse, error) {
	if req.GetType() != "image" {
		return nil, sdk.Refuse("no type %q here", req.GetType())
	}
	im, err := p.images(req.GetZone())
	if err != nil {
		return nil, err
	}
	kinds := im.ImageKinds()
	var s Spec
	var o Observed
	var refusals []*pluginpb.Refusal
	refuse := func(field, format string, a ...any) {
		refusals = append(refusals, &pluginpb.Refusal{Field: field, Reason: fmt.Sprintf(format, a...)})
	}
	if req.GetAction() != "" {
		if err := sdk.Decode(req.GetCurrent().GetSpec(), &s); err != nil {
			return nil, err
		}
		_ = sdk.Decode(req.GetCurrent().GetObserved(), &o)
	}
	switch req.GetAction() {
	case "":
		if err := sdk.Decode(req.GetSpec(), &s); err != nil {
			return nil, err
		}
		switch {
		case s.Recipe != "" && s.Machine != "":
			refuse("/recipe", "a recipe to bake, or a machine to save — not both")
		case s.Recipe != "":
			r, ok := p.recipe(s.Recipe)
			if !ok {
				refuse("/recipe", "no recipe %q here (there are: %s)", s.Recipe, p.recipeNames())
				break
			}
			s.Kind = ""
			for _, k := range []string{"vm", "container"} {
				if r.Base[k] != "" && slices.Contains(kinds, k) {
					s.Kind = k
					break
				}
			}
			if s.Kind == "" {
				refuse("/recipe", "recipe %s bakes for %s, and zone %s makes images for %s", s.Recipe,
					strings.Join(slices.Sorted(maps.Keys(r.Base)), " and "), req.GetZone(), orNone(kinds))
			}
			s.Family, s.SizeGB, s.Attempt = r.Family, r.DiskGB, 1
			if s.Family == "" {
				s.Family = s.Recipe
			}
			s.From = &From{Name: s.Recipe, Recipe: r}
		case s.Machine != "":
			kind, running, disk, ok := machineRef(req.GetRefs(), s.Machine)
			switch {
			case !ok:
				refuse("/machine", "no machine %s", s.Machine)
			case !slices.Contains(kinds, kind):
				refuse("/machine", "%s is a %s, and zone %s saves images of %s", s.Machine, kindWord(kind), req.GetZone(), orNone(kinds))
			case running:
				refuse("/machine", "%s runs: stop it first — an image is saved from a stopped machine", s.Machine)
			}
			s.Kind, s.SizeGB = kind, disk
		default:
			refuse("/recipe", "a recipe to bake, or a machine to save")
		}
		o = Observed{}
	case "share":
		var sp struct {
			SharedWith []string `json:"shared_with"`
		}
		if err := sdk.Decode(req.GetParams(), &sp); err != nil {
			return nil, err
		}
		s.SharedWith = sp.SharedWith
	case "retire":
		if s.Retired {
			refuse("", "it is retired already")
		}
		s.Retired = true
	case "rebake":
		switch {
		case s.Recipe == "":
			refuse("", "it was saved from a machine: only a baked image is baked again")
		case o.State != driver.ImageFailed || o.Attempt != s.Attempt:
			refuse("", "it is %s: only a failed bake is baked again", orWord(o.State, driver.ImagePending))
		case s.Retired:
			refuse("", "it is retired")
		default:
			r, ok := p.recipe(s.Recipe)
			if !ok {
				refuse("", "recipe %s is no longer here (there are: %s)", s.Recipe, p.recipeNames())
				break
			}
			if r.Base[s.Kind] == "" {
				refuse("", "recipe %s no longer bakes for a %s", s.Recipe, kindWord(s.Kind))
				break
			}
			s.Attempt++
			s.SizeGB, s.From = r.DiskGB, &From{Name: s.Recipe, Recipe: r}
		}
	default:
		return nil, sdk.Refuse("no action %q on an image", req.GetAction())
	}
	shared, r := normalize(s.SharedWith)
	if r != nil {
		refusals = append(refusals, r)
	}
	s.SharedWith = shared
	return &pluginpb.PlanResponse{Spec: sdk.JSON(s), Usage: usage(s), Choices: choices(s), Refusals: refusals, Room: room(s, o)}, nil
}

func orNone(xs []string) string {
	if len(xs) == 0 {
		return "none"
	}
	return strings.Join(xs, " and ")
}

func orWord(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// engineErr: a refusal or a missing thing will not change by trying again;
// anything else might (the core retries UNAVAILABLE).
func engineErr(err error) error {
	if errors.Is(err, driver.ErrRefused) || errors.Is(err, driver.ErrNotFound) {
		return sdk.NotNow("%v", err)
	}
	return sdk.Unreachable("%v", err)
}

func (p *Plugin) image(r *pluginpb.Resource) (driver.Images, Spec, Observed, error) {
	im, err := p.images(r.GetZone())
	if err != nil {
		return nil, Spec{}, Observed{}, err
	}
	var s Spec
	if err := sdk.Decode(r.GetSpec(), &s); err != nil {
		return nil, Spec{}, Observed{}, err
	}
	var o Observed
	_ = sdk.Decode(r.GetObserved(), &o)
	return im, s, o, nil
}

// bakeSpec is what the driver bakes an image's current attempt from.
func bakeSpec(id string, s Spec) (driver.BakeSpec, error) {
	if s.From == nil {
		return driver.BakeSpec{}, sdk.Refuse("%s has no recipe to bake from", id)
	}
	r := withDefaults(s.From.Recipe)
	t, err := timeout(r)
	if err != nil {
		return driver.BakeSpec{}, sdk.Refuse("%v", err)
	}
	return driver.BakeSpec{
		ID: id, Attempt: s.Attempt, Kind: s.Kind, Base: r.Base[s.Kind], DiskGB: r.DiskGB, Cores: r.Cores, MemoryMB: r.MemoryMB,
		UserData: []byte(r.UserData), Timeout: t,
		// the builder borrows its room: a node acting without the brain
		// stops it for a priority guest, as it stops a spot machine
		Tags: map[string]string{"class": "spot", "admitted": strconv.Itoa(r.MemoryMB)},
	}, nil
}

// observe is what the plugin reports of an image, carried over from what it
// reported before where the engine no longer says (a failure's words, when
// it was made).
func (p *Plugin) observe(got driver.Image, was Observed) Observed {
	o := Observed{State: got.State, Detail: got.Detail, Attempt: got.Attempt, EngineRef: got.EngineRef, Node: got.Node,
		SizeGB: got.SizeGB, MadeAt: was.MadeAt, Restarts: was.Restarts}
	if got.Ref != "" {
		o.Forms = map[string]string{got.Kind: got.Ref}
	}
	if got.State == driver.ImageAvailable && o.MadeAt == "" {
		o.MadeAt = p.Now().UTC().Format(time.RFC3339)
	}
	return o
}

func (p *Plugin) Create(ctx context.Context, req *pluginpb.CreateRequest) (*pluginpb.CreateResponse, error) {
	r := req.GetResource()
	im, s, _, err := p.image(r)
	if err != nil {
		return nil, err
	}
	var got driver.Image
	var ev *pluginpb.Event
	if s.Machine != "" {
		got, err = im.SaveImage(ctx, r.GetId(), s.Machine)
		ev = sdk.Event("image.saved", "", map[string]string{"machine": s.Machine})
	} else {
		var bs driver.BakeSpec
		if bs, err = bakeSpec(r.GetId(), s); err != nil {
			return nil, err
		}
		// born held while the zone's room is: its bake waits for the room
		got, err = im.Bake(ctx, bs, r.GetHold() != "")
		ev = sdk.Event("image.baking", "", map[string]string{"recipe": s.Recipe, "attempt": strconv.Itoa(s.Attempt)})
		if err == nil && got.State == driver.ImageWaiting {
			ev = sdk.Event("image.waiting", "its bake waits for the zone's room", map[string]string{"hold": r.GetHold()})
		}
	}
	if err != nil {
		return nil, engineErr(err)
	}
	return &pluginpb.CreateResponse{Observed: sdk.JSON(p.observe(got, Observed{})), Events: []*pluginpb.Event{ev}}, nil
}

func (p *Plugin) Delete(ctx context.Context, req *pluginpb.DeleteRequest) (*pluginpb.DeleteResponse, error) {
	r := req.GetResource()
	im, _, _, err := p.image(r)
	if err != nil {
		return nil, err
	}
	if err := im.DeleteImage(ctx, r.GetId()); err != nil && !errors.Is(err, driver.ErrNotFound) {
		return nil, engineErr(err)
	}
	return &pluginpb.DeleteResponse{Events: []*pluginpb.Event{sdk.Event("image.deleted", "", nil)}}, nil
}

func (p *Plugin) Act(ctx context.Context, req *pluginpb.ActRequest) (*pluginpb.ActResponse, error) {
	r := req.GetResource()
	// every action is planned: the spec handed over is the one it asked for
	im, s, o, err := p.image(r)
	if err != nil {
		return nil, err
	}
	var ev *pluginpb.Event
	switch req.GetAction() {
	case "share":
		ev = sdk.Event("image.shared", "", map[string]string{"shared_with": strings.Join(s.SharedWith, ","), "visibility": visibility(s.SharedWith)})
	case "retire":
		ev = sdk.Event("image.retired", "no machine is born from it any more", nil)
	case "rebake":
		bs, err := bakeSpec(r.GetId(), s)
		if err != nil {
			return nil, err
		}
		got, err := im.Bake(ctx, bs, r.GetHold() != "")
		if err != nil {
			return nil, engineErr(err)
		}
		o = p.observe(got, Observed{})
		ev = sdk.Event("image.baking", "baked again", map[string]string{"recipe": s.Recipe, "attempt": strconv.Itoa(s.Attempt)})
	default:
		return nil, sdk.Refuse("no action %q on an image", req.GetAction())
	}
	return &pluginpb.ActResponse{Spec: sdk.JSON(s), Observed: sdk.JSON(o), Events: []*pluginpb.Event{ev}}, nil
}

// Reconcile moves a bake forward — a step each pass: its builder started,
// let go while the zone's room is held, read, made an image or failed — and
// gives back the room it borrowed once it is over. A saved or made image is
// only looked for.
func (p *Plugin) Reconcile(ctx context.Context, req *pluginpb.ReconcileRequest) (*pluginpb.ReconcileResponse, error) {
	r := req.GetResource()
	im, s, was, err := p.image(r)
	if err != nil {
		return nil, err
	}
	resp := &pluginpb.ReconcileResponse{Drift: pluginpb.Drift_DRIFT_IN_SYNC}
	o := was
	if !baking(s, was) {
		if was.State == driver.ImageFailed {
			// its failure's words stay; nothing is on the engine
			resp.Observed = sdk.JSON(was)
		} else {
			got, err := im.Image(ctx, r.GetId())
			if errors.Is(err, driver.ErrNotFound) {
				return &pluginpb.ReconcileResponse{Drift: pluginpb.Drift_DRIFT_MISSING, Detail: "the engine has no such image"}, nil
			}
			if err != nil {
				return nil, engineErr(err)
			}
			o = p.observe(got, was)
			resp.Observed = sdk.JSON(o)
		}
	} else {
		bs, err := bakeSpec(r.GetId(), s)
		if err != nil {
			return nil, err
		}
		got, err := im.Bake(ctx, bs, r.GetHold() != "")
		if err != nil {
			return nil, engineErr(err)
		}
		o = p.observe(got, was)
		if got.Attempt != was.Attempt {
			o.Restarts = 0
		}
		switch {
		case got.State == was.State && got.Detail == was.Detail:
		case got.State == driver.ImageAvailable:
			o.Restarts = 0
			resp.Events = append(resp.Events, sdk.Event("image.baked", "", map[string]string{"attempt": strconv.Itoa(got.Attempt)}))
		case got.State == driver.ImageFailed:
			resp.Events = append(resp.Events, sdk.Event("image.bake_failed", got.Detail, map[string]string{"attempt": strconv.Itoa(got.Attempt)}))
		case got.State == driver.ImagePending && was.State == driver.ImageWaiting && got.Detail == "":
			resp.Events = append(resp.Events, sdk.Event("image.baking", "the zone's room is back: baked again from the start", nil))
		case got.State == driver.ImageWaiting && r.GetHold() != "":
			resp.Events = append(resp.Events, sdk.Event("image.waiting", "the zone's room is needed: it bakes again from the start once the room is back",
				map[string]string{"hold": r.GetHold()}))
		case got.State == driver.ImagePending && got.Detail != "" && r.GetHold() == "":
			// its builder had stopped before it finished, unasked, and was
			// started over — a recipe powering it off would do it forever
			o.Restarts++
			resp.Events = append(resp.Events, sdk.Event("image.restarted", got.Detail, map[string]string{"restarts": strconv.Itoa(o.Restarts)}))
			if o.Restarts >= maxRestarts {
				if err := im.DeleteImage(ctx, r.GetId()); err != nil {
					return nil, engineErr(err)
				}
				o.State, o.Detail = driver.ImageFailed, fmt.Sprintf("its builder stopped %d times before its first boot finished: does its recipe power it off?", o.Restarts)
				resp.Events = append(resp.Events, sdk.Event("image.bake_failed", o.Detail, nil))
			}
		}
		resp.Observed = sdk.JSON(o)
	}
	// what it takes from its zone follows where its bake stands
	want := room(s, o)
	if cur := r.GetRoom(); cur.GetSpotMb() != want.GetSpotMb() || cur.GetGuaranteedMb() != want.GetGuaranteedMb() || cur.GetRunning() != want.GetRunning() {
		resp.Spec, resp.Room = r.GetSpec(), want
	}
	return resp, nil
}
