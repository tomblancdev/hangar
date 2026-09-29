package machines

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tomblancdev/hangar/driver"
	"github.com/tomblancdev/hangar/driver/fake"
	"github.com/tomblancdev/hangar/sdk/pluginpb"
)

// strict is the fake engine with Proxmox's traits: a container changes
// everything live and boots no user data; a VM boots user data, grows its
// memory live and changes nothing else while it runs.
type strict struct{ *fake.Engine }

func (strict) Traits(kind string) driver.Traits {
	if kind == "container" {
		return driver.Traits{LiveCores: true, LiveMemoryUp: true, LiveMemoryDown: true}
	}
	return driver.Traits{UserData: true, LiveMemoryUp: true}
}

func init() {
	driver.Register("strict", func(ctx context.Context, p driver.Params) (driver.Driver, error) {
		d, err := fake.Open(ctx, p)
		if err != nil {
			return nil, err
		}
		return strict{d.(*fake.Engine)}, nil
	})
}

const settings = `{
  "images": {
    "debian-13": {"vm": "debian-13", "container": "store:vztmpl/debian-13.tar.zst"},
    "appliance": {"vm": "appliance-1"}
  },
  "types": {"dev": {"cores": 12, "memory_gb": 40}}
}`

func configured(t *testing.T, drv, caps string) *Plugin {
	t.Helper()
	p := New()
	opts := map[string]string{}
	if caps != "" {
		opts["capabilities"] = caps
	}
	resp, err := p.Configure(context.Background(), &pluginpb.ConfigureRequest{
		Settings: []byte(settings),
		Zones:    []*pluginpb.ZoneConfig{{Name: "z", Driver: drv, Options: opts}},
	})
	if err != nil || resp.GetZones()[0].GetError() != "" {
		t.Fatalf("%v %v", resp, err)
	}
	return p
}

func plan(t *testing.T, p *Plugin, spec string) (*pluginpb.PlanResponse, Spec) {
	t.Helper()
	resp, err := p.Plan(context.Background(), &pluginpb.PlanRequest{Type: "machine", Zone: "z", Spec: []byte(spec)})
	if err != nil {
		t.Fatal(err)
	}
	var s Spec
	_ = json.Unmarshal(resp.GetSpec(), &s)
	return resp, s
}

func reasons(r *pluginpb.PlanResponse) string {
	var out []string
	for _, x := range r.GetRefusals() {
		out = append(out, x.GetField()+" "+x.GetReason())
	}
	return strings.Join(out, "; ")
}

func TestAMachinesSize(t *testing.T) {
	p := configured(t, "strict", "")
	r, s := plan(t, p, `{"image":"debian-13"}`)
	if len(r.GetRefusals()) > 0 || s.Type != "t3.micro" || s.Cores != 2 || s.MemoryGB != 1 || s.Kind != "vm" || s.Class != "spot" || s.DiskGB != 8 || !s.Running {
		t.Fatalf("defaults: %+v %s", s, reasons(r))
	}
	if u := r.GetUsage(); u["machines.count"] != 1 || u["machines.vcpu"] != 2 || u["machines.memory_gb"] != 1 || u["machines.disk_gb"] != 8 {
		t.Fatalf("usage %v", u)
	}
	if c := r.GetChoices(); c["machines.kind"] != "vm" || c["machines.class"] != "spot" {
		t.Fatalf("choices %v", c)
	}
	if _, s = plan(t, p, `{"image":"debian-13","type":"dev"}`); s.Cores != 12 || s.MemoryGB != 40 {
		t.Fatalf("the operator's alias: %+v", s)
	}
	if _, s = plan(t, p, `{"image":"debian-13","cores":3,"memory_gb":5}`); s.Type != "" || s.Cores != 3 || s.MemoryGB != 5 {
		t.Fatalf("free size: %+v", s)
	}
	for spec, want := range map[string]string{
		`{"image":"debian-13","type":"t3.medium","cores":4}`:                   "not both",
		`{"image":"debian-13","cores":4}`:                                      "go together",
		`{"image":"debian-13","type":"x9.huge"}`:                               "t3.micro",
		`{"image":"nothing"}`:                                                  "there are: appliance, debian-13",
		`{"image":"appliance","kind":"container"}`:                             "comes as vm, not as a container",
		`{"image":"debian-13","kind":"container","user_data":"#cloud-config"}`: "boots no user data",
	} {
		if r, _ := plan(t, p, spec); !strings.Contains(reasons(r), want) {
			t.Errorf("%s: %q, want %q", spec, reasons(r), want)
		}
	}
	if r, _ := plan(t, configured(t, "strict", "kind.container,guest.tags,fence.pool"), `{"image":"debian-13"}`); !strings.Contains(reasons(r), "runs no VM") {
		t.Errorf("a VM in a zone of containers: %s", reasons(r))
	}
}

func TestResizingARunningMachineAsksItsKind(t *testing.T) {
	p := configured(t, "strict", "")
	vm := &pluginpb.Resource{Spec: []byte(`{"kind":"vm","cores":2,"memory_gb":4,"disk_gb":8,"class":"spot","running":true}`)}
	ct := &pluginpb.Resource{Spec: []byte(`{"kind":"container","cores":2,"memory_gb":4,"disk_gb":8,"class":"spot","running":true}`)}
	for _, c := range []struct {
		cur    *pluginpb.Resource
		params string
		want   string // "" = accepted
	}{
		{vm, `{"memory_gb":8}`, ""},
		{vm, `{"memory_gb":2}`, "memory does not go that way"},
		{vm, `{"cores":4,"memory_gb":4}`, "stop it first"},
		{vm, `{"type":"t3.large"}`, ""}, // 2 cores, 8 GB: only memory grows
		{ct, `{"cores":4,"memory_gb":1}`, ""},
		{ct, `{"type":"t3.small"}`, ""},
	} {
		r, err := p.Plan(context.Background(), &pluginpb.PlanRequest{Type: "machine", Zone: "z", Action: "resize", Params: []byte(c.params), Current: c.cur})
		if err != nil {
			t.Fatal(err)
		}
		if got := reasons(r); (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s %s: %q, want %q", string(c.cur.GetSpec())[9:20], c.params, got, c.want)
		}
	}
	stopped := &pluginpb.Resource{Spec: []byte(`{"kind":"vm","cores":2,"memory_gb":4,"running":false}`)}
	if r, _ := p.Plan(context.Background(), &pluginpb.PlanRequest{Type: "machine", Zone: "z", Action: "resize", Params: []byte(`{"cores":8,"memory_gb":1}`), Current: stopped}); reasons(r) != "" {
		t.Errorf("a stopped VM: %s", reasons(r))
	}
}

func TestAZoneThatIsNotFencedIsRefused(t *testing.T) {
	p := New()
	resp, err := p.Configure(context.Background(), &pluginpb.ConfigureRequest{Zones: []*pluginpb.ZoneConfig{
		{Name: "z", Driver: fake.Name, Options: map[string]string{"capabilities": "kind.vm,guest.tags"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	// the core refuses a zone lacking a required flag; the plugin says which it requires
	d, _ := p.Describe(context.Background(), nil)
	if !contains(d.GetRequires(), driver.FencePool) || resp.GetZones()[0].GetError() != "" {
		t.Fatalf("requires %v, zone %v", d.GetRequires(), resp.GetZones())
	}
}

func TestKeyPairs(t *testing.T) {
	const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGXj2Dq6dbWAg2wXCN9pDWc3cS/cJEHWIr0sRd4b3M5V alice@example.com"
	canonical, obs, err := parseKey("  " + strings.Replace(key, " ", "   ", 1) + "\n")
	if err != nil || canonical != key || obs.KeyType != "ssh-ed25519" || obs.Comment != "alice@example.com" {
		t.Fatalf("%q %+v %v", canonical, obs, err)
	}
	// the fingerprint ssh-keygen -lf prints for that key
	if obs.Fingerprint != "SHA256:F+9VABth99MG4W12t/As1lYGhzAIehvMTHI5261zPBY" {
		t.Errorf("fingerprint %s", obs.Fingerprint)
	}
	for _, bad := range []string{
		"", "ssh-ed25519", "ssh-dss AAAAB3NzaC1kc3M=", "ssh-ed25519 !!!",
		// an RSA body under an ed25519 name
		"ssh-ed25519 AAAAB3NzaC1yc2EAAAADAQABAAABAQC7",
	} {
		if _, _, err := parseKey(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// A machine's create hands the engine what it is made from: the image's
// engine name for its kind, the key pairs' public keys, its user data, its
// class as a tag.
func TestCreateHandsTheEngineItsMakings(t *testing.T) {
	p := configured(t, "strict", "")
	kp := &pluginpb.Resource{Id: "kp-0000000000000000a", Type: "keypair", Spec: []byte(`{"public_key":"ssh-ed25519 AAAA k"}`)} // as the registry holds it
	_, s := plan(t, p, `{"image":"debian-13","name":"dev","user_data":"#cloud-config\n","class":"guaranteed"}`)
	resp, err := p.Create(context.Background(), &pluginpb.CreateRequest{
		Resource: &pluginpb.Resource{Id: "m-0000000000000000b", Type: "machine", Zone: "z", Spec: mustJSON(s)},
		Refs:     []*pluginpb.Resource{kp},
	})
	if err != nil {
		t.Fatal(err)
	}
	var obs Observed
	_ = json.Unmarshal(resp.GetObserved(), &obs)
	if !obs.Running || obs.Name != "dev" || obs.MemoryMB != 1024 {
		t.Fatalf("%+v", obs)
	}
	got, _ := p.Driver("z").(strict).Spec("m-0000000000000000b")
	if got.Image != "debian-13" || len(got.SSHKeys) != 1 || got.SSHKeys[0] != "ssh-ed25519 AAAA k" ||
		string(got.UserData) != "#cloud-config\n" || got.Tags["class"] != "guaranteed" || got.DiskGB != 8 {
		t.Fatalf("the engine got %+v", got)
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
