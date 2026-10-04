package networks

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/tomblancdev/hangar/sdk"
	"github.com/tomblancdev/hangar/sdk/pluginpb"
)

const (
	keyA = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGXj2Dq6dbWAg2wXCN9pDWc3cS/cJEHWIr0sRd4b3M5V laptop"
	keyB = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDeYZ0lS1d0FaLUkBZtD5OzcyHgvYtHhiRl8SLe1kcu8 phone"
)

func pair(id, key string) *pluginpb.Resource {
	return &pluginpb.Resource{Id: id, Type: "keypair", Spec: sdk.JSON(map[string]string{"public_key": key})}
}

func open(t *testing.T, caps string) *Plugin {
	t.Helper()
	p := New()
	zone := &pluginpb.ZoneConfig{Name: "z", Driver: "fake"}
	if caps != "" {
		zone.Options = map[string]string{"capabilities": caps}
	}
	if _, err := p.Configure(context.Background(), &pluginpb.ConfigureRequest{Zones: []*pluginpb.ZoneConfig{zone}}); err != nil {
		t.Fatal(err)
	}
	return p
}

// A network's sentence names only what a network carries — and never its
// keys themselves: the spec keeps them under another word than the count.
func TestANetworksSentence(t *testing.T) {
	var doc struct {
		Summary []string                        `json:"x-hangar-summary"`
		Status  struct{ Field, On, Off string } `json:"x-hangar-status"`
	}
	if err := json.Unmarshal([]byte(networkSchema), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Summary[0] != "{range}" || doc.Status.Field != "running" || doc.Status.On != "up" || doc.Status.Off != "idle" {
		t.Fatalf("%+v", doc)
	}
	spec, _ := json.Marshal(Spec{KeyPairs: []string{"kp-1"}, Keys: []string{keyA}, SharedWith: []string{"family"}})
	observed, _ := json.Marshal(Observed{Range: "r", Gateway: "g", Jump: "j", Wall: "exact", Running: true, Keys: 1})
	var s, o map[string]any
	_ = json.Unmarshal(spec, &s)
	_ = json.Unmarshal(observed, &o)
	for _, part := range doc.Summary {
		for _, hole := range strings.Split(part, "{")[1:] {
			name, _, _ := strings.Cut(hole, "}")
			if _, ok := s[name]; !ok && o[name] == nil {
				t.Errorf("the sentence names %q, which a network does not carry", name)
			}
		}
	}
	if _, inSpec := s["keys"]; inSpec || s["jump_keys"] == nil {
		t.Fatalf("the spec keeps its public keys under %v: {keys} in the sentence would print them", s)
	}
}

// What a request leaves a network as: the key pairs it names, with the keys
// they hold; whom it is shared with, in order; one of them counted, and its
// gateway's memory booked.
func TestANetworkIsPlanned(t *testing.T) {
	p := open(t, "")
	ctx := context.Background()
	plan, err := p.Plan(ctx, &pluginpb.PlanRequest{Type: "network", Zone: "z",
		Spec: sdk.JSON(map[string]any{"key_pairs": []string{"kp-b", "kp-a"}, "shared_with": []string{"friends", "family", "friends"}}),
		Refs: []*pluginpb.Resource{pair("kp-a", keyA), pair("kp-b", keyB)}})
	if err != nil || len(plan.GetRefusals()) != 0 {
		t.Fatalf("%v %v", err, plan.GetRefusals())
	}
	var s Spec
	_ = json.Unmarshal(plan.GetSpec(), &s)
	if !slices.Equal(s.Keys, []string{keyB, keyA}) || !slices.Equal(s.SharedWith, []string{"family", "friends"}) || plan.GetUsage()["networks.count"] != 1 ||
		plan.GetChoices()["networks.visibility"] != Shared || plan.GetRoom().GetGuaranteedMb() != 64 || plan.GetRoom().GetSpotMb() != 0 {
		t.Fatalf("planned %+v, usage %v, choices %v, room %v", s, plan.GetUsage(), plan.GetChoices(), plan.GetRoom())
	}
	// a key pair the core did not hand over is none
	plan, _ = p.Plan(ctx, &pluginpb.PlanRequest{Type: "network", Zone: "z", Spec: sdk.JSON(map[string]any{"key_pairs": []string{"kp-a"}})})
	if rs := plan.GetRefusals(); len(rs) != 1 || rs[0].GetField() != "/key_pairs/0" {
		t.Fatalf("a key pair nobody handed over: %v", rs)
	}
	// shared with everyone is public — a word a tier opens or not
	plan, _ = p.Plan(ctx, &pluginpb.PlanRequest{Type: "network", Zone: "z", Spec: sdk.JSON(map[string]any{"shared_with": []string{"family", "*"}})})
	_ = json.Unmarshal(plan.GetSpec(), &s)
	if plan.GetChoices()["networks.visibility"] != Public || !slices.Equal(s.SharedWith, []string{"*"}) {
		t.Fatalf("shared with everyone: %v %v", plan.GetChoices(), s.SharedWith)
	}
	// an action's plan starts from what the network is: its keys stay when it is shared
	cur := &pluginpb.Resource{Id: "net-1", Type: "network", Zone: "z", Spec: sdk.JSON(Spec{KeyPairs: []string{"kp-a"}, Keys: []string{keyA}})}
	plan, err = p.Plan(ctx, &pluginpb.PlanRequest{Type: "network", Zone: "z", Action: "share", Current: cur, Params: sdk.JSON(map[string]any{"shared_with": []string{"family"}})})
	_ = json.Unmarshal(plan.GetSpec(), &s)
	if err != nil || !slices.Equal(s.Keys, []string{keyA}) || !slices.Equal(s.KeyPairs, []string{"kp-a"}) || !slices.Equal(s.SharedWith, []string{"family"}) {
		t.Fatalf("shared: %+v %v", s, err)
	}
	plan, err = p.Plan(ctx, &pluginpb.PlanRequest{Type: "network", Zone: "z", Action: "set_key_pairs", Current: cur, Params: sdk.JSON(map[string]any{"key_pairs": []string{}})})
	s = Spec{}
	_ = json.Unmarshal(plan.GetSpec(), &s)
	if err != nil || len(s.Keys) != 0 || len(s.KeyPairs) != 0 || plan.GetRoom().GetGuaranteedMb() != 64 {
		t.Fatalf("no key pair at all: %+v %v", s, err)
	}
	// a zone whose driver makes no networks is not one it is open on
	closed := New()
	resp, err := closed.Configure(ctx, &pluginpb.ConfigureRequest{Zones: []*pluginpb.ZoneConfig{{Name: "z", Driver: "fake", Options: map[string]string{"capabilities": "kind.vm,fence.pool"}}}})
	if err != nil || len(resp.GetZones()) != 1 || slices.Contains(resp.GetZones()[0].GetCapabilities(), "net.private") {
		t.Fatalf("a zone with no net.private: %v %v", resp, err)
	}
}

// What brings a network to another spec: its key pairs and whom it is shared
// with, each through its own action — and nothing of it is set at its birth.
func TestANetworksChange(t *testing.T) {
	p := open(t, "")
	cur := &pluginpb.Resource{Id: "net-1", Type: "network", Zone: "z", Spec: sdk.JSON(Spec{KeyPairs: []string{"kp-a"}, Keys: []string{keyA}, SharedWith: []string{"family"}})}
	change := func(want map[string]any) []string {
		t.Helper()
		resp, err := p.PlanChange(context.Background(), &pluginpb.PlanChangeRequest{Current: cur, Spec: sdk.JSON(want)})
		if err != nil || len(resp.GetFixed()) != 0 {
			t.Fatalf("%v %v", err, resp.GetFixed())
		}
		var out []string
		for _, s := range resp.GetSteps() {
			out = append(out, s.GetAction()+" "+string(s.GetParams()))
		}
		return out
	}
	if steps := change(map[string]any{"key_pairs": []string{"kp-a"}, "shared_with": []string{"family"}}); len(steps) != 0 {
		t.Fatalf("as it is: %v", steps)
	}
	if steps := change(map[string]any{"key_pairs": []string{"kp-b", "kp-a"}, "shared_with": []string{"family"}}); !slices.Equal(steps, []string{`set_key_pairs {"key_pairs":["kp-b","kp-a"]}`}) {
		t.Fatalf("another key pair: %v", steps)
	}
	if steps := change(map[string]any{}); !slices.Equal(steps, []string{`set_key_pairs {"key_pairs":[]}`, `share {"shared_with":[]}`}) {
		t.Fatalf("nothing named: %v", steps)
	}
}
