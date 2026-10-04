package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tomblancdev/hangar/internal/config"
	"github.com/tomblancdev/hangar/sdk"
	"github.com/tomblancdev/hangar/sdk/pluginpb"
)

// The test binary is also the plugin: run as "<test> hangar-test-plugin
// probe <variant>", it serves a probe that reports what it was given — its
// environment and, per zone, the hash of the credential it received.
func TestMain(m *testing.M) {
	if len(os.Args) == 4 && os.Args[1] == "hangar-test-plugin" && os.Args[2] == "probe" {
		sdk.Serve(&probe{variant: os.Args[3]})
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type probe struct {
	pluginpb.UnimplementedPluginServiceServer
	variant string
}

func (p *probe) name() string {
	if p.variant == "wrong-name" {
		return "someone-else"
	}
	name, _, _ := strings.Cut(p.variant, "-")
	return name
}

func (p *probe) Describe(context.Context, *pluginpb.DescribeRequest) (*pluginpb.DescribeResponse, error) {
	n := p.name()
	d := &pluginpb.DescribeResponse{
		Name: n,
		Types: []*pluginpb.ResourceType{{
			Name: n, IdPrefix: n, Schema: []byte(`{"type":"object"}`),
			Actions: []*pluginpb.Action{{Name: "poke"}},
		}},
		Dimensions: []*pluginpb.Dimension{{Name: n + ".things", Kind: pluginpb.DimensionKind_DIMENSION_KIND_QUANTITY}},
		Requires:   []string{"guest.tags"},
	}
	switch p.variant {
	case "probe-bad-dimension":
		d.Dimensions[0].Name = "other.things"
	case "probe-reserved-prefix":
		d.Types[0].IdPrefix = "op"
	case "probe-unknown-capability":
		d.Requires = []string{"kind.teleport"}
	case "probe-core-action":
		d.Types[0].Actions[0].Name = "delete"
	case "probe-bad-schema":
		d.Types[0].Schema = []byte(`{"type": 12}`)
	}
	// what the process sees of its environment, by name only
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		d.Events = append(d.Events, k)
	}
	sort.Strings(d.Events)
	return d, nil
}

func (p *probe) Configure(_ context.Context, req *pluginpb.ConfigureRequest) (*pluginpb.ConfigureResponse, error) {
	resp := &pluginpb.ConfigureResponse{}
	for _, z := range req.GetZones() {
		sum := sha256.Sum256(z.GetCredential())
		caps := []string{"guest.tags", "cred:" + hex.EncodeToString(sum[:])}
		if p.variant == "probe-no-tags" {
			caps = caps[1:]
		}
		resp.Zones = append(resp.Zones, &pluginpb.ZoneReport{Name: z.GetName(), Capabilities: caps})
	}
	return resp, nil
}

func shortDir(t *testing.T) string {
	// a unix socket's path is at most ~100 bytes: t.TempDir() can be longer
	d, err := os.MkdirTemp("", "h")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func probeConfig(t *testing.T, variants ...string) *config.Config {
	cfg := &config.Config{DataDir: shortDir(t), Zones: []config.Zone{{Name: "z", Driver: "fake"}}}
	for _, v := range variants {
		name, _, _ := strings.Cut(v, "-")
		cfg.Plugins = append(cfg.Plugins, config.Plugin{
			Name: name, Path: os.Args[0], Args: []string{"hangar-test-plugin", "probe", v}, Zones: []string{"z"},
		})
	}
	return cfg
}

func start(t *testing.T, cfg *config.Config) (*Host, error) {
	t.Helper()
	h, err := Start(context.Background(), cfg, Options{DataDir: cfg.DataDir, Logs: io.Discard}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Cleanup(h.Close)
	}
	return h, err
}

func credOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "cred:" + hex.EncodeToString(sum[:])
}

func TestAPluginStartsWithAnEmptyEnvironment(t *testing.T) {
	t.Setenv("HANGAR_CANARY", "the core's secret")
	h, err := start(t, probeConfig(t, "probe"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range h.Plugin("probe").Desc.GetEvents() {
		ok := name == "TMPDIR" || name == "HANGAR_PLUGIN" || strings.HasPrefix(name, "PLUGIN_")
		if !ok {
			t.Errorf("the plugin sees %s from the core's environment", name)
		}
	}
	if !slices.Contains(h.Plugin("probe").Desc.GetEvents(), "TMPDIR") {
		t.Fatal("the control: the probe does report its environment (TMPDIR missing)")
	}
}

func TestEachPluginGetsOnlyItsOwnCredential(t *testing.T) {
	cfg := probeConfig(t, "probe", "other")
	f := cfg.DataDir + "/cred-a"
	if err := os.WriteFile(f, []byte("probe's key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HANGAR_TEST_CRED_B", "other's key")
	cfg.Plugins[0].Credentials = map[string]config.Secret{"z": {File: f}}
	cfg.Plugins[1].Credentials = map[string]config.Secret{"z": {Env: "HANGAR_TEST_CRED_B"}}
	h, err := start(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	a, b := h.Plugin("probe").Zones["z"].Capabilities, h.Plugin("other").Zones["z"].Capabilities
	if !slices.Contains(a, credOf("probe's key")) || slices.Contains(a, credOf("other's key")) {
		t.Errorf("probe got %v", a)
	}
	if !slices.Contains(b, credOf("other's key")) || slices.Contains(b, credOf("probe's key")) {
		t.Errorf("other got %v", b)
	}
}

func TestADeclarationIsRefused(t *testing.T) {
	for variant, want := range map[string]string{
		"wrong-name":               `calls itself "someone-else"`,
		"probe-bad-dimension":      "are spelled probe.<name>",
		"probe-reserved-prefix":    `id prefix "op"`,
		"probe-unknown-capability": `"kind.teleport", a capability no driver documents`,
		"probe-core-action":        `action "delete" is the core's`,
		"probe-bad-schema":         "schema",
	} {
		cfg := probeConfig(t, variant)
		_, err := start(t, cfg)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", variant, err, want)
		}
	}
}

func TestTwoPluginsCannotClaimOneType(t *testing.T) {
	cfg := probeConfig(t, "probe")
	cfg.Plugins = append(cfg.Plugins, cfg.Plugins[0])
	cfg.Plugins[1].Name = "probe" // the config refuses a duplicate name; this is the host's own guard
	if _, err := start(t, cfg); err == nil || !strings.Contains(err.Error(), "already plugin probe's") {
		t.Fatalf("got %v", err)
	}
}

func TestAZoneLackingARequiredCapabilityIsUnusable(t *testing.T) {
	h, err := start(t, probeConfig(t, "probe-no-tags"))
	if err != nil {
		t.Fatal(err)
	}
	st := h.Plugin("probe").Zones["z"]
	if !strings.Contains(st.Error, "lacks guest.tags") {
		t.Fatalf("%+v", st)
	}
	if ok, why := h.Available(h.Type("probe"), "z"); ok || !strings.Contains(why, "lacks guest.tags") {
		t.Fatalf("%v %s", ok, why)
	}
}

func TestAProgramWhoseChecksumDiffersIsNotStarted(t *testing.T) {
	cfg := probeConfig(t, "probe")
	cfg.Plugins[0].SHA256 = strings.Repeat("00", 32)
	if _, err := start(t, cfg); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("got %v", err)
	}
	b, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	cfg = probeConfig(t, "probe")
	cfg.Plugins[0].SHA256 = hex.EncodeToString(sum[:])
	if _, err := start(t, cfg); err != nil {
		t.Fatalf("the right checksum: %v", err)
	}
}

func TestADeadPluginIsStartedAgain(t *testing.T) {
	h, err := start(t, probeConfig(t, "probe"))
	if err != nil {
		t.Fatal(err)
	}
	p := h.Plugin("probe")
	p.mu.Lock()
	p.client.Kill()
	p.mu.Unlock()
	if p.Up() {
		t.Fatal("the control: the kill took")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := p.Client(ctx); err != nil {
		t.Fatal(err)
	}
	if !p.Up() || p.Zones["z"].Error != "" {
		t.Fatalf("up %v, zone %+v", p.Up(), p.Zones["z"])
	}
}

// A reference is read from the schema: a field, or its items, marked
// x-hangar-ref; x-hangar-attached makes it an attachment, and marks nothing
// else.
func TestReferencesAreReadFromTheSchema(t *testing.T) {
	refs, err := refsOf([]byte(`{"properties": {
		"machine": {"type": "string", "x-hangar-ref": "machine", "x-hangar-attached": true},
		"keys":    {"type": "array", "items": {"type": "string", "x-hangar-ref": "keypair"}},
		"name":    {"type": "string"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []Ref{{Field: "keys", Type: "keypair", Many: true}, {Field: "machine", Type: "machine", Attached: true}}
	if len(refs) != 2 || refs[0] != want[0] || refs[1] != want[1] {
		t.Fatalf("read %+v", refs)
	}
	if _, err := refsOf([]byte(`{"properties": {"name": {"type": "string", "x-hangar-attached": true}}}`)); err == nil {
		t.Fatal("x-hangar-attached on a field that names nothing was accepted")
	}
	// a member stands on what it names; a default names something by itself
	refs, err = refsOf([]byte(`{"properties": {
		"network": {"type": "string", "x-hangar-ref": "network", "x-hangar-member": true, "x-hangar-default": "default"},
		"groups":  {"type": "array", "x-hangar-member": true, "items": {"type": "string", "x-hangar-ref": "group"}}}}`))
	if err != nil || len(refs) != 2 || refs[0] != (Ref{Field: "groups", Type: "group", Many: true, Member: true}) ||
		refs[1] != (Ref{Field: "network", Type: "network", Member: true, Default: "default"}) {
		t.Fatalf("read %+v %v", refs, err)
	}
	for name, schema := range map[string]string{
		"a member that names nothing":       `{"properties": {"name": {"type": "string", "x-hangar-member": true}}}`,
		"inside and on at once":             `{"properties": {"m": {"type": "string", "x-hangar-ref": "machine", "x-hangar-attached": true, "x-hangar-member": true}}}`,
		"a default that names nothing":      `{"properties": {"name": {"type": "string", "x-hangar-default": "default"}}}`,
		"a default for a list":              `{"properties": {"keys": {"type": "array", "x-hangar-default": "default", "items": {"type": "string", "x-hangar-ref": "keypair"}}}}`,
		"a default nothing could be called": `{"properties": {"n": {"type": "string", "x-hangar-ref": "network", "x-hangar-default": "My Network"}}}`,
	} {
		if _, err := refsOf([]byte(schema)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
