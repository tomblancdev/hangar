package server

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const machinesConfig = `
data_dir: %[1]s
identity:
  oidc: {issuer: %[2]s, audience: hangar}
tiers:
  - name: users
    groups: [users]
    zones: [m, n]
    limits:
      machines.count: 1
      machines.vcpu_hours: 1000
      machines.vcpu: 4
      machines.memory_gb: 8
      machines.disk_gb: 64
      machines.key_pairs: 3
      machines.kind: [vm, container]
      machines.class: [spot]
zones:
  - {name: m, driver: fake, endpoint: "%[1]s/zone-m.json"}
  - {name: n, driver: fake}
plugins:
  - name: machines
    path: %[3]s
    args: [hangar-test-plugin, machines]
    zones: [m, n]
    settings:
      images:
        debian-13: {vm: debian-13, container: "store:vztmpl/debian-13.tar.zst"}
reconcile:
  every: 1h
`

const aliceKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGXj2Dq6dbWAg2wXCN9pDWc3cS/cJEHWIr0sRd4b3M5V alice@example.com"

// A machine names its key pairs by id. The core checks each is the owner's
// own, in the same zone and ready, records the link, and hands the plugin the
// key pairs themselves — the plugin never reads the registry.
func TestAMachineAndItsKeyPairs(t *testing.T) {
	s := newStackWith(t, machinesConfig, "machines")
	alice, bob := s.token("alice", "users"), s.token("bob", "users")

	kp := s.create(alice, map[string]any{"type": "keypair", "zone": "m", "spec": map[string]any{"public_key": aliceKey}})
	kpID := kp.str("resource", "id")
	if !strings.HasPrefix(kpID, "kp-") {
		t.Fatalf("%d %v", kp.code, kp.body)
	}
	if got := s.do("GET", "/v1/resources/"+kpID, alice, nil); got.str("observed", "fingerprint") != "SHA256:F+9VABth99MG4W12t/As1lYGhzAIehvMTHI5261zPBY" {
		t.Fatalf("the key pair reads %v", got.body)
	}
	if r := s.do("POST", "/v1/resources", alice, map[string]any{"type": "keypair", "zone": "m", "spec": map[string]any{"public_key": "ssh-ed25519 !!!"}}); r.code != 422 || !strings.Contains(r.str("detail"), "base64") {
		t.Fatalf("a broken key: %d %v", r.code, r.body)
	}
	kpN := s.create(alice, map[string]any{"type": "keypair", "zone": "n", "spec": map[string]any{"public_key": aliceKey}}).str("resource", "id")

	machine := func(who string, keys ...string) reply {
		return s.do("POST", "/v1/resources", who, map[string]any{"type": "machine", "zone": "m",
			"spec": map[string]any{"image": "debian-13", "kind": "container", "key_pairs": keys}})
	}
	// someone else's key pair reads exactly like one that does not exist
	for _, id := range []string{kpID, "kp-00000000000000000"} {
		r := machine(bob, id)
		if r.code != 422 || r.str("violations", "0", "field") != "/key_pairs/0" || !strings.Contains(r.str("detail"), "you have no keypair "+id) {
			t.Fatalf("bob naming %s: %d %v", id, r.code, r.body)
		}
	}
	if r := machine(alice, kpN); r.code != 422 || !strings.Contains(r.str("detail"), "is in zone n, not m") {
		t.Fatalf("a key pair of another zone: %d %v", r.code, r.body)
	}

	r := machine(alice, kpID)
	if r.code != 202 {
		t.Fatalf("%d %v", r.code, r.body)
	}
	if op := s.done(alice, r.str("operation", "id")); op.str("state") != "succeeded" {
		t.Fatalf("%v", op.body)
	}
	id := r.str("resource", "id")
	var engine struct {
		Specs map[string]struct {
			SSHKeys []string `json:"SSHKeys"`
			Image   string   `json:"Image"`
			Tags    map[string]string
		} `json:"specs"`
	}
	b, _ := os.ReadFile(s.engine[:strings.LastIndex(s.engine, "/")] + "/zone-m.json")
	_ = json.Unmarshal(b, &engine)
	got := engine.Specs[id]
	if len(got.SSHKeys) != 1 || got.SSHKeys[0] != aliceKey || got.Image != "store:vztmpl/debian-13.tar.zst" || got.Tags["class"] != "spot" {
		t.Fatalf("the engine was handed %+v", got)
	}
	rels, err := s.store.Relations(context.Background(), id)
	if err != nil || len(rels) != 1 || rels[0] != [2]string{"key_pairs", kpID} {
		t.Fatalf("relations %v %v", rels, err)
	}

	// the tier's limits, with the numbers
	if r := machine(alice, kpID); r.code != 403 || !strings.Contains(r.str("detail"), "1 of 1 machines.count used") {
		t.Fatalf("a second machine: %d %v", r.code, r.body)
	}
	act := func(params map[string]any) reply {
		return s.do("POST", "/v1/resources/"+id+"/actions/resize", alice, map[string]any{"params": params})
	}
	if r := act(map[string]any{"memory_gb": 8}); r.code != 202 || s.done(alice, r.str("operation", "id")).str("state") != "succeeded" {
		t.Fatalf("resize within the limit: %d %v", r.code, r.body)
	}
	if r := act(map[string]any{"memory_gb": 9}); r.code != 403 || !strings.Contains(r.str("detail"), "machines.memory_gb") {
		t.Fatalf("resize beyond the limit: %d %v", r.code, r.body)
	}
	if got := s.do("GET", "/v1/resources/"+id, alice, nil); got.str("spec", "memory_gb") != "8" || got.str("observed", "memory_mb") != "8192" {
		t.Fatalf("after the resize: %v", got.body)
	}
}

// A brain with no images plugin: a machine naming an image by id is refused
// at the request — the machines plugin still starts, and a machine by the
// operator's name for an image is made as ever.
func TestAnImageIdWithoutTheImagesPlugin(t *testing.T) {
	s := newStackWith(t, machinesConfig, "machines")
	alice := s.token("alice", "users")
	r := s.do("POST", "/v1/resources", alice, map[string]any{"type": "machine", "zone": "m",
		"spec": map[string]any{"image_id": "img-00000000000000000"}})
	if r.code != 422 || !strings.Contains(r.str("detail"), "no plugin here makes the type image") {
		t.Fatalf("an image by id with no images plugin: %d %v", r.code, r.body)
	}
	if r := s.do("POST", "/v1/resources", alice, map[string]any{"type": "machine", "zone": "m", "spec": map[string]any{}}); r.code != 422 ||
		!strings.Contains(r.str("detail"), "image (the operator's name) or image_id") {
		t.Fatalf("a machine from nothing: %d %v", r.code, r.body)
	}
	if id := s.create(alice, map[string]any{"type": "machine", "zone": "m", "spec": map[string]any{"image": "debian-13"}}).str("resource", "id"); !strings.HasPrefix(id, "m-") {
		t.Fatalf("a machine by the operator's name: %s", id)
	}
	if !strings.Contains(s.logs.String(), "a reference no request can use") {
		t.Fatal("the start says which reference no request can use")
	}
}
