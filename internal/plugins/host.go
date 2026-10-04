// Package plugins is the plugin host: it starts each enabled plugin as its
// own process, reads what it declares, refuses a declaration that would
// collide with another plugin's or name a capability nobody documents, hands
// it its zones with its own credentials — and nothing else — and keeps it
// running.
//
// A plugin starts with an EMPTY environment (go-plugin copies the host's by
// default: the core's secrets would reach every plugin), one directory of
// its own for its socket, and mutual TLS on that socket.
package plugins

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"google.golang.org/protobuf/proto"

	"github.com/tomblancdev/hangar/driver"
	"github.com/tomblancdev/hangar/internal/config"
	"github.com/tomblancdev/hangar/internal/ids"
	"github.com/tomblancdev/hangar/internal/limits"
	"github.com/tomblancdev/hangar/sdk"
	"github.com/tomblancdev/hangar/sdk/pluginpb"
)

// reserved: prefixes and action names the core keeps for itself.
var (
	reservedPrefixes = []string{ids.Operation, ids.Token}
	reservedActions  = []string{"create", "delete", "rename", "describe"}
)

// Host runs the plugins.
type Host struct {
	log     *slog.Logger
	plugins map[string]*Plugin
	order   []string
	types   map[string]*Type
	prefix  map[string]*Type
	dims    map[string]limits.Dimension
}

// Plugin is one running plugin.
type Plugin struct {
	Name string
	Desc *pluginpb.DescribeResponse
	// Zones: what the plugin reported per zone it is enabled on.
	Zones map[string]ZoneState

	cfg     config.Plugin
	launch  func() (*exec.Cmd, *goplugin.SecureConfig, error)
	sockDir string
	logs    io.Writer
	confReq *pluginpb.ConfigureRequest
	log     *slog.Logger

	mu        sync.Mutex
	client    *goplugin.Client
	rpc       pluginpb.PluginServiceClient
	lastStart time.Time
	down      error
}

// ZoneState is a zone as one plugin sees it.
type ZoneState struct {
	Capabilities []string `json:"capabilities"`
	// Error: why the plugin is not usable there (its driver refused to open,
	// or lacks a capability the plugin requires).
	Error string `json:"error,omitempty"`
}

// Type is a resource type a plugin declared, ready to validate against.
type Type struct {
	Name        string
	Prefix      string
	Plugin      string
	Title       string
	Description string
	Schema      json.RawMessage
	// Requires: the plugin's flags and the type's own.
	Requires []string
	Actions  []*Action
	// Refs: the spec's fields that name other resources.
	Refs []Ref
	// Share: the spec's field that says whom its owner opened it to — an
	// array of group names, "*" = everyone — marked "x-hangar-share": true;
	// "" = a type that is its owner's alone.
	Share string

	schema *jsonschema.Schema
	// how one of its resources reads: its schema's x-hangar-summary and
	// x-hangar-status (summary.go)
	summary *Summary
	status  *StatusRule
}

// Ref is a field of a spec, or of an action's params, that names other
// resources by id: a top-level property its schema marks
// "x-hangar-ref": "<type>" — a string, or an array whose items are marked.
// The core resolves each id before the plugin is asked (the owner's own, in
// the same zone, ready) and hands the plugin the resources themselves.
//
// A spec's reference the schema also marks "x-hangar-attached": true is an
// attachment: the resource lives inside the one it names on the engine (a
// volume plugged into a machine), and neither is deleted while it is.
//
// One marked "x-hangar-member": true makes the resource a member of what it
// names (a machine on a network): what it names is not deleted while it has
// members — the member itself goes freely. Unlike an attachment it may name
// what someone shares.
//
// One marked "x-hangar-default": "<name>" — a single reference — names
// something by itself when a create leaves it out, where its type is made:
// the owner's resource of that type called <name>, made first if they have
// none (an ordinary create, in their name, within their tier).
type Ref struct {
	Field    string `json:"field"`
	Type     string `json:"type"`
	Many     bool   `json:"many"`
	Attached bool   `json:"attached,omitempty"`
	Member   bool   `json:"member,omitempty"`
	Default  string `json:"default,omitempty"`
}

// SharedWith reads the groups a spec opens its resource to ("*" =
// everyone), sorted, without repeats; none for a type that is not shared.
func (t *Type) SharedWith(spec json.RawMessage) []string {
	if t == nil || t.Share == "" {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(spec, &fields) != nil {
		return nil
	}
	var groups []string
	_ = json.Unmarshal(fields[t.Share], &groups)
	slices.Sort(groups)
	return slices.Compact(groups)
}

// Ref returns the type's reference of a field, or nil.
func (t *Type) Ref(field string) *Ref {
	for i := range t.Refs {
		if t.Refs[i].Field == field {
			return &t.Refs[i]
		}
	}
	return nil
}

// Action is an action a type declared.
type Action struct {
	Name         string
	Description  string
	ParamsSchema json.RawMessage
	ChangesUsage bool
	Requires     []string
	Refs         []Ref

	params *jsonschema.Schema
}

// Options tell the host how to start plugins.
type Options struct {
	// DataDir holds each plugin's socket directory.
	DataDir string
	// Self is this binary, which runs the builtin plugins ("<self> plugin
	// <name>"). Default: os.Executable().
	Self string
	// Command, when set, replaces how a plugin's process is made (tests).
	Command func(config.Plugin) *exec.Cmd
	// Logs is where the plugins' own log lines go (default: stdout).
	Logs io.Writer
}

// Start starts every enabled plugin and configures it on its zones. A plugin
// that cannot start, or declares something the host refuses, stops the start:
// a brain that quietly runs without half its plugins would refuse requests
// nobody could explain.
func Start(ctx context.Context, cfg *config.Config, opt Options, log *slog.Logger) (*Host, error) {
	h := &Host{
		log: log, plugins: map[string]*Plugin{}, types: map[string]*Type{},
		prefix: map[string]*Type{}, dims: map[string]limits.Dimension{},
	}
	if opt.Self == "" {
		self, err := os.Executable()
		if err != nil {
			return nil, err
		}
		opt.Self = self
	}
	for _, pc := range cfg.Plugins {
		p, err := h.start(ctx, cfg, opt, pc)
		if err != nil {
			h.Close()
			return nil, fmt.Errorf("plugin %s: %w", pc.Name, err)
		}
		h.plugins[p.Name] = p
		h.order = append(h.order, p.Name)
	}
	for _, w := range h.checkRefs() {
		log.Warn("a reference no request can use", "why", w.Error())
	}
	return h, nil
}

// checkRefs lists the references to a type no enabled plugin declares: a
// request that names one is refused ("no plugin here makes …"), and the rest
// of the type works — a machine may name an image by id only where images
// are made, and by the operator's names everywhere.
func (h *Host) checkRefs() []error {
	var errs []error
	for _, t := range h.Types() {
		for _, r := range t.Refs {
			if h.types[r.Type] == nil {
				errs = append(errs, fmt.Errorf("type %s: field %s names type %s, which no enabled plugin declares", t.Name, r.Field, r.Type))
			}
		}
		for _, a := range t.Actions {
			for _, r := range a.Refs {
				if h.types[r.Type] == nil {
					errs = append(errs, fmt.Errorf("type %s, action %s: field %s names type %s, which no enabled plugin declares", t.Name, a.Name, r.Field, r.Type))
				}
			}
		}
	}
	return errs
}

// shareOf reads the field a schema marks "x-hangar-share": true — at most
// one, an array of strings.
func shareOf(raw []byte) (string, error) {
	var doc struct {
		Properties map[string]struct {
			Share bool   `json:"x-hangar-share"`
			Type  string `json:"type"`
			Items *struct {
				Type string `json:"type"`
			} `json:"items"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", err
	}
	var out string
	for _, field := range slices.Sorted(maps.Keys(doc.Properties)) {
		p := doc.Properties[field]
		switch {
		case !p.Share:
		case out != "":
			return "", fmt.Errorf("x-hangar-share on %s and on %s: one field says whom a resource is shared with", out, field)
		case p.Type != "array" || p.Items == nil || p.Items.Type != "string":
			return "", fmt.Errorf("field %s: x-hangar-share marks an array of strings (group names, * = everyone)", field)
		default:
			out = field
		}
	}
	return out, nil
}

// defaultName: what a reference's default may be called — a resource's name.
var defaultName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// refsOf reads the fields a schema marks as references.
func refsOf(raw []byte) ([]Ref, error) {
	var doc struct {
		Properties map[string]struct {
			Ref      string `json:"x-hangar-ref"`
			Attached bool   `json:"x-hangar-attached"`
			Member   bool   `json:"x-hangar-member"`
			Default  string `json:"x-hangar-default"`
			Items    *struct {
				Ref string `json:"x-hangar-ref"`
			} `json:"items"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	var out []Ref
	for field, p := range doc.Properties {
		switch {
		case p.Ref != "" && p.Items != nil && p.Items.Ref != "":
			return nil, fmt.Errorf("field %s: x-hangar-ref on the field or on its items, not both", field)
		case p.Attached && p.Ref == "" && (p.Items == nil || p.Items.Ref == ""):
			return nil, fmt.Errorf("field %s: x-hangar-attached marks a reference, and it names none", field)
		case p.Member && p.Ref == "" && (p.Items == nil || p.Items.Ref == ""):
			return nil, fmt.Errorf("field %s: x-hangar-member marks a reference, and it names none", field)
		case p.Member && p.Attached:
			return nil, fmt.Errorf("field %s: x-hangar-attached (it lives inside what it names) or x-hangar-member (it stands on it), not both", field)
		case p.Default != "" && p.Ref == "":
			return nil, fmt.Errorf("field %s: x-hangar-default marks a reference to one resource, and it names none", field)
		case p.Default != "" && !defaultName.MatchString(p.Default):
			return nil, fmt.Errorf("field %s: x-hangar-default %q: a name — a-z, 0-9 and -", field, p.Default)
		case p.Ref != "":
			out = append(out, Ref{Field: field, Type: p.Ref, Attached: p.Attached, Member: p.Member, Default: p.Default})
		case p.Items != nil && p.Items.Ref != "":
			out = append(out, Ref{Field: field, Type: p.Items.Ref, Many: true, Attached: p.Attached, Member: p.Member})
		}
	}
	slices.SortFunc(out, func(a, b Ref) int { return strings.Compare(a.Field, b.Field) })
	return out, nil
}

func (h *Host) start(ctx context.Context, cfg *config.Config, opt Options, pc config.Plugin) (*Plugin, error) {
	sockDir := filepath.Join(opt.DataDir, "plugins", pc.Name)
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		return nil, err
	}
	p := &Plugin{Name: pc.Name, cfg: pc, sockDir: sockDir, log: h.log.With("plugin", pc.Name), logs: opt.Logs}
	if p.logs == nil {
		p.logs = os.Stdout
	}
	p.launch = func() (*exec.Cmd, *goplugin.SecureConfig, error) {
		var cmd *exec.Cmd
		var sec *goplugin.SecureConfig
		switch {
		case opt.Command != nil:
			cmd = opt.Command(pc)
		case pc.Builtin != "":
			cmd = exec.Command(opt.Self, "plugin", pc.Builtin)
		default:
			cmd = exec.Command(pc.Path, pc.Args...)
			if pc.SHA256 != "" {
				sum, err := hex.DecodeString(pc.SHA256)
				if err != nil {
					return nil, nil, fmt.Errorf("sha256: %w", err)
				}
				sec = &goplugin.SecureConfig{Checksum: sum, Hash: sha256.New()}
			}
		}
		// The plugin's whole environment: where to put its socket.
		cmd.Env = []string{"TMPDIR=" + sockDir}
		return cmd, sec, nil
	}
	if err := p.spawn(); err != nil {
		return nil, err
	}

	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	desc, err := p.rpc.Describe(cctx, &pluginpb.DescribeRequest{Protocol: sdk.Protocol})
	if err != nil {
		p.kill()
		return nil, fmt.Errorf("describe: %w", err)
	}
	if err := h.accept(pc, desc); err != nil {
		p.kill()
		return nil, err
	}
	p.Desc = desc

	req, err := configureRequest(cfg, pc)
	if err != nil {
		p.kill()
		return nil, err
	}
	p.confReq = req
	if err := p.configure(ctx); err != nil {
		p.kill()
		return nil, err
	}
	for _, z := range pc.Zones {
		if st := p.Zones[z]; st.Error != "" {
			p.log.Warn("zone not usable by this plugin", "zone", z, "why", st.Error)
		}
	}
	return p, nil
}

// spawn starts the plugin's process and connects to it. Called with p.mu held
// (or before p is shared).
func (p *Plugin) spawn() error {
	cmd, sec, err := p.launch()
	if err != nil {
		return err
	}
	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig:  sdk.Handshake,
		Plugins:          map[string]goplugin.Plugin{sdk.Dispense: &sdk.GRPCPlugin{}},
		Cmd:              cmd,
		SkipHostEnv:      true,
		SecureConfig:     sec,
		AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolGRPC},
		AutoMTLS:         true,
		Logger: hclog.New(&hclog.LoggerOptions{
			Name: "plugin." + p.Name, Output: p.logs, JSONFormat: true, Level: hclog.Info,
		}),
	})
	rpcClient, err := client.Client()
	if err != nil {
		client.Kill()
		return err
	}
	raw, err := rpcClient.Dispense(sdk.Dispense)
	if err != nil {
		client.Kill()
		return err
	}
	p.client, p.rpc, p.lastStart, p.down = client, raw.(pluginpb.PluginServiceClient), time.Now(), nil
	return nil
}

func (p *Plugin) kill() {
	if p.client != nil {
		p.client.Kill()
	}
}

// configure hands the plugin its zones and records what it reports.
func (p *Plugin) configure(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	resp, err := p.rpc.Configure(cctx, p.confReq)
	if err != nil {
		return fmt.Errorf("configure: %w", err)
	}
	zones := map[string]ZoneState{}
	for _, z := range p.confReq.GetZones() {
		zones[z.GetName()] = ZoneState{Error: "the plugin did not report on this zone"}
	}
	need := p.Desc.GetRequires()
	for _, r := range resp.GetZones() {
		if _, asked := zones[r.GetName()]; !asked {
			continue
		}
		st := ZoneState{Capabilities: slices.Sorted(slices.Values(r.GetCapabilities())), Error: r.GetError()}
		if st.Error == "" {
			if miss := driver.Missing(need, st.Capabilities); len(miss) > 0 {
				st.Error = "its driver lacks " + strings.Join(miss, ", ")
			}
		}
		zones[r.GetName()] = st
	}
	p.Zones = zones
	return nil
}

// configureRequest builds what one plugin is told: its zones, and for each,
// ITS credential. The function reads no other plugin's secrets — it is given
// only this plugin's config.
func configureRequest(cfg *config.Config, pc config.Plugin) (*pluginpb.ConfigureRequest, error) {
	settings := []byte("{}")
	if pc.Settings != nil {
		b, err := json.Marshal(pc.Settings)
		if err != nil {
			return nil, fmt.Errorf("settings: %w", err)
		}
		settings = b
	}
	req := &pluginpb.ConfigureRequest{Settings: settings}
	for _, name := range pc.Zones {
		z, _ := cfg.Zone(name)
		zc := &pluginpb.ZoneConfig{Name: z.Name, Driver: z.Driver, Endpoint: z.Endpoint, Options: z.Options, Watch: z.Watch()}
		if s, ok := pc.Credentials[name]; ok {
			v, err := s.Read()
			if err != nil {
				return nil, fmt.Errorf("credential for zone %s: %w", name, err)
			}
			zc.Credential = v
		}
		req.Zones = append(req.Zones, zc)
	}
	return req, nil
}

// accept checks a description and indexes its types and dimensions.
func (h *Host) accept(pc config.Plugin, d *pluginpb.DescribeResponse) error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if d.GetName() != pc.Name {
		bad("it calls itself %q: the config enables it as %q (the wrong program?)", d.GetName(), pc.Name)
	}
	for _, c := range d.GetRequires() {
		if !driver.IsKnown(c) {
			bad("it requires %q, a capability no driver documents", c)
		}
	}
	dims := map[string]limits.Dimension{}
	for _, dm := range d.GetDimensions() {
		name := dm.GetName()
		switch {
		case !strings.HasPrefix(name, pc.Name+".") || len(name) == len(pc.Name)+1:
			bad("dimension %q: a plugin's dimensions are spelled %s.<name>", name, pc.Name)
		case strings.HasSuffix(name, ".*"):
			bad("dimension %q: * is the tiers' wildcard", name)
		case dims[name].Name != "":
			bad("dimension %q twice", name)
		}
		kind := map[pluginpb.DimensionKind]string{
			pluginpb.DimensionKind_DIMENSION_KIND_QUANTITY: limits.Quantity,
			pluginpb.DimensionKind_DIMENSION_KIND_CHOICE:   limits.Choice,
			pluginpb.DimensionKind_DIMENSION_KIND_METER:    limits.Meter,
		}[dm.GetKind()]
		if kind == "" {
			bad("dimension %q has no kind", name)
		}
		dims[name] = limits.Dimension{Name: name, Kind: kind, Unit: dm.GetUnit(), Description: dm.GetDescription(), Plugin: pc.Name}
	}
	var types []*Type
	for _, rt := range d.GetTypes() {
		t, err := compileType(pc.Name, d, rt)
		if err != nil {
			bad("type %q: %v", rt.GetName(), err)
			continue
		}
		if other, ok := h.types[t.Name]; ok {
			bad("type %q is already plugin %s's", t.Name, other.Plugin)
		}
		if other, ok := h.prefix[t.Prefix]; ok {
			bad("id prefix %q is already type %s's", t.Prefix, other.Name)
		}
		types = append(types, t)
	}
	if len(d.GetTypes()) == 0 {
		bad("it declares no resource type")
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	for _, t := range types {
		h.types[t.Name], h.prefix[t.Prefix] = t, t
	}
	for n, dm := range dims {
		h.dims[n] = dm
	}
	return nil
}

func compileType(plugin string, d *pluginpb.DescribeResponse, rt *pluginpb.ResourceType) (*Type, error) {
	var errs []error
	if !ids.ValidPrefix(rt.GetName()) { // a type's name has the shape of a prefix
		errs = append(errs, fmt.Errorf("a type's name is a lowercase word of at most 8 characters"))
	}
	if !ids.ValidPrefix(rt.GetIdPrefix()) || slices.Contains(reservedPrefixes, rt.GetIdPrefix()) {
		errs = append(errs, fmt.Errorf("id prefix %q: a lowercase word of at most 8, not %v", rt.GetIdPrefix(), reservedPrefixes))
	}
	t := &Type{
		Name: rt.GetName(), Prefix: rt.GetIdPrefix(), Plugin: plugin, Title: rt.GetTitle(),
		Description: rt.GetDescription(), Schema: rt.GetSchema(),
	}
	t.Requires = slices.Sorted(slices.Values(append(slices.Clone(d.GetRequires()), rt.GetRequires()...)))
	t.Requires = slices.Compact(t.Requires)
	for _, c := range rt.GetRequires() {
		if !driver.IsKnown(c) {
			errs = append(errs, fmt.Errorf("it requires %q, a capability no driver documents", c))
		}
	}
	s, err := compileSchema("type/"+t.Name, rt.GetSchema())
	if err != nil {
		errs = append(errs, fmt.Errorf("schema: %w", err))
	} else if t.Refs, err = refsOf(rt.GetSchema()); err != nil {
		errs = append(errs, fmt.Errorf("schema: %w", err))
	} else if t.Share, err = shareOf(rt.GetSchema()); err != nil {
		errs = append(errs, fmt.Errorf("schema: %w", err))
	} else if t.summary, t.status, err = readOf(rt.GetSchema()); err != nil {
		errs = append(errs, fmt.Errorf("schema: %w", err))
	}
	t.schema = s
	seen := map[string]bool{}
	for _, a := range rt.GetActions() {
		switch {
		case slices.Contains(reservedActions, a.GetName()):
			errs = append(errs, fmt.Errorf("action %q is the core's", a.GetName()))
		case !validActionName(a.GetName()):
			errs = append(errs, fmt.Errorf("action %q: lowercase letters and _", a.GetName()))
		case seen[a.GetName()]:
			errs = append(errs, fmt.Errorf("action %q twice", a.GetName()))
		}
		seen[a.GetName()] = true
		for _, c := range a.GetRequires() {
			if !driver.IsKnown(c) {
				errs = append(errs, fmt.Errorf("action %s requires %q, a capability no driver documents", a.GetName(), c))
			}
		}
		act := &Action{
			Name: a.GetName(), Description: a.GetDescription(), ParamsSchema: a.GetParamsSchema(),
			ChangesUsage: a.GetChangesUsage(), Requires: slices.Sorted(slices.Values(a.GetRequires())),
		}
		if len(a.GetParamsSchema()) > 0 {
			ps, err := compileSchema("type/"+t.Name+"/"+a.GetName(), a.GetParamsSchema())
			if err != nil {
				errs = append(errs, fmt.Errorf("action %s: params schema: %w", a.GetName(), err))
			} else if act.Refs, err = refsOf(a.GetParamsSchema()); err != nil {
				errs = append(errs, fmt.Errorf("action %s: params schema: %w", a.GetName(), err))
			}
			act.params = ps
		}
		t.Actions = append(t.Actions, act)
	}
	return t, errors.Join(errs...)
}

func validActionName(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && r != '_' {
			return false
		}
	}
	return true
}

// compileSchema compiles a JSON Schema offline: a $ref to anywhere else is
// refused, never fetched.
func compileSchema(name string, raw []byte) (*jsonschema.Schema, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("empty")
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	url := "file:///hangar/" + name + ".json"
	if err := c.AddResource(url, doc); err != nil {
		return nil, err
	}
	return c.Compile(url)
}

// ---- What the core asks the host --------------------------------------------

// Plugins lists the plugins in the config's order.
func (h *Host) Plugins() []*Plugin {
	out := make([]*Plugin, 0, len(h.order))
	for _, n := range h.order {
		out = append(out, h.plugins[n])
	}
	return out
}

// Plugin returns a plugin by name.
func (h *Host) Plugin(name string) *Plugin { return h.plugins[name] }

// Types lists every type, sorted by name.
func (h *Host) Types() []*Type {
	out := make([]*Type, 0, len(h.types))
	for _, t := range h.types {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Type returns a type by name.
func (h *Host) Type(name string) *Type { return h.types[name] }

// TypeOf returns the type an id belongs to, by its prefix.
func (h *Host) TypeOf(id string) *Type { return h.prefix[ids.Prefix(id)] }

// Dimensions returns every dimension the plugins declared.
func (h *Host) Dimensions() map[string]limits.Dimension { return h.dims }

// Available says whether a type can be made in a zone, and why not.
func (h *Host) Available(t *Type, zone string) (bool, string) {
	p := h.plugins[t.Plugin]
	st, ok := p.Zones[zone]
	if !ok {
		return false, fmt.Sprintf("plugin %s is not enabled on zone %s", t.Plugin, zone)
	}
	if st.Error != "" {
		return false, fmt.Sprintf("plugin %s cannot work on zone %s: %s", t.Plugin, zone, st.Error)
	}
	if miss := driver.Missing(t.Requires, st.Capabilities); len(miss) > 0 {
		return false, fmt.Sprintf("zone %s's driver lacks %s", zone, strings.Join(miss, ", "))
	}
	return true, ""
}

// ActionAvailable says whether an action is offered in a zone, and why not.
func (h *Host) ActionAvailable(t *Type, a *Action, zone string) (bool, string) {
	if ok, why := h.Available(t, zone); !ok {
		return false, why
	}
	st := h.plugins[t.Plugin].Zones[zone]
	if miss := driver.Missing(a.Requires, st.Capabilities); len(miss) > 0 {
		return false, fmt.Sprintf("%s needs %s, which zone %s's driver lacks", a.Name, strings.Join(miss, ", "), zone)
	}
	return true, ""
}

// Action returns a type's action by name.
func (t *Type) Action(name string) *Action {
	for _, a := range t.Actions {
		if a.Name == name {
			return a
		}
	}
	return nil
}

// Validate checks a spec against the type's schema.
func (t *Type) Validate(spec json.RawMessage) []Violation { return validate(t.schema, spec) }

// Validate checks params against the action's schema; an action without one
// takes an empty object or nothing.
func (a *Action) Validate(params json.RawMessage) []Violation {
	if a.params == nil {
		p := bytes.TrimSpace(params)
		if len(p) == 0 || string(p) == "{}" || string(p) == "null" {
			return nil
		}
		return []Violation{{Field: "", Reason: "this action takes no params"}}
	}
	return validate(a.params, params)
}

// Violation is one way a document fails its schema.
type Violation struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

func validate(s *jsonschema.Schema, doc json.RawMessage) []Violation {
	if len(bytes.TrimSpace(doc)) == 0 {
		doc = json.RawMessage("{}")
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		return []Violation{{Reason: "not JSON: " + err.Error()}}
	}
	err = s.Validate(inst)
	if err == nil {
		return nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return []Violation{{Reason: err.Error()}}
	}
	var out []Violation
	for _, u := range ve.BasicOutput().Errors {
		if u.Error == nil {
			continue
		}
		out = append(out, Violation{Field: u.InstanceLocation, Reason: u.Error.String()})
	}
	if len(out) == 0 {
		out = []Violation{{Reason: ve.Error()}}
	}
	return out
}

// Client returns a connection to the plugin, restarting it when its process
// died. A restarted plugin must describe itself exactly as before and is
// configured again; one that changed stays down until the brain restarts.
func (p *Plugin) Client(ctx context.Context) (pluginpb.PluginServiceClient, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil && !p.client.Exited() {
		return p.rpc, nil
	}
	if p.down != nil && time.Since(p.lastStart) < 5*time.Second {
		return nil, p.down
	}
	p.log.Warn("plugin process gone; starting it again")
	p.lastStart = time.Now()
	if err := p.spawn(); err != nil {
		p.down = fmt.Errorf("plugin %s is down: %w", p.Name, err)
		return nil, p.down
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	desc, err := p.rpc.Describe(cctx, &pluginpb.DescribeRequest{Protocol: sdk.Protocol})
	if err != nil || !proto.Equal(desc, p.Desc) {
		p.kill()
		p.down = fmt.Errorf("plugin %s came back describing itself differently: restart hangar to accept it", p.Name)
		return nil, p.down
	}
	if err := p.configure(ctx); err != nil {
		p.kill()
		p.down = fmt.Errorf("plugin %s is down: %w", p.Name, err)
		return nil, p.down
	}
	return p.rpc, nil
}

// Up reports whether the plugin's process is running.
func (p *Plugin) Up() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.client != nil && !p.client.Exited()
}

// Close stops every plugin.
func (h *Host) Close() {
	for _, p := range h.plugins {
		p.mu.Lock()
		p.kill()
		p.mu.Unlock()
	}
}
