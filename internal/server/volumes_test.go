package server

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const volumesConfig = `
data_dir: %[1]s
identity:
  oidc: {issuer: %[2]s, audience: hangar}
tiers:
  - name: users
    groups: [users]
    zones: [v]
    limits:
      machines.count: 4
      machines.vcpu: 8
      machines.memory_gb: 16
      machines.disk_gb: 64
      machines.kind: [vm, container]
      machines.class: [spot]
      volumes.count: 2
      volumes.size_gb: 8
      volumes.backup_gb: 2
  - name: nobackup
    groups: [nobackup]
    zones: [v]
    limits:
      volumes.count: 1
      volumes.size_gb: 8
zones:
  - {name: v, driver: fake, endpoint: "%[1]s/zone-v.json"}
plugins:
  - name: machines
    path: %[3]s
    args: [hangar-test-plugin, machines]
    zones: [v]
    settings:
      images:
        debian-13: {vm: debian-13, container: "store:vztmpl/debian-13.tar.zst"}
  - name: volumes
    path: %[3]s
    args: [hangar-test-plugin, volumes]
    zones: [v]
reconcile:
  every: 1h
`

// A volume through the whole brain, on the fake engine the machines share:
// made parked, plugged into a container, moved to another — refused while
// the first runs — grown, backed up within the tier's budget, and never
// deleted, nor its machine, while it is plugged in.
func TestAVolumesLife(t *testing.T) {
	s := newStackWith(t, volumesConfig, "machines", "volumes")
	alice := s.token("alice", "users")
	ok := func(r reply) reply {
		t.Helper()
		if r.code != 202 {
			t.Fatalf("%d %v", r.code, r.body)
		}
		op := s.done(alice, r.str("operation", "id"))
		if op.str("state") != "succeeded" {
			t.Fatalf("%v", op.body)
		}
		return r
	}
	machine := func(kind string) string {
		return s.create(alice, map[string]any{"type": "machine", "zone": "v", "spec": map[string]any{"image": "debian-13", "kind": kind}}).str("resource", "id")
	}
	act := func(id, action string, params map[string]any) reply {
		return s.do("POST", "/v1/resources/"+id+"/actions/"+action, alice, map[string]any{"params": params})
	}
	refused := func(r reply, code int, words string) {
		t.Helper()
		if r.code != code || !strings.Contains(r.str("detail"), words) {
			t.Fatalf("want %d %q: %d %v", code, words, r.code, r.body)
		}
	}
	rels := func(id string) [][2]string {
		t.Helper()
		got, err := s.store.Relations(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	a, b, vm := machine("container"), machine("container"), machine("vm")

	// the plan sees what the spec names: a disk is refused on a container,
	// a directory on a VM, before anything is admitted
	refused(s.do("POST", "/v1/resources", alice, map[string]any{"type": "volume", "zone": "v",
		"spec": map[string]any{"size_gb": 1, "mount": "/data", "machine": vm}}), 422, "a filesystem volume goes on a container, and "+vm+" is a VM")
	refused(s.do("POST", "/v1/resources", alice, map[string]any{"type": "volume", "zone": "v",
		"spec": map[string]any{"size_gb": 1, "content": "block", "mount": "/data"}}), 422, "takes no path")
	refused(s.do("POST", "/v1/resources", alice, map[string]any{"type": "volume", "zone": "v",
		"spec": map[string]any{"size_gb": 1, "mount": "/a/../etc"}}), 422, "no . or ..")

	vol := s.create(alice, map[string]any{"type": "volume", "zone": "v", "spec": map[string]any{"size_gb": 1, "mount": "/data"}}).str("resource", "id")
	if !strings.HasPrefix(vol, "vol-") {
		t.Fatalf("id %q", vol)
	}
	got := s.do("GET", "/v1/resources/"+vol, alice, nil)
	if got.str("spec", "content") != "filesystem" || got.str("spec", "backup") != "false" || got.str("observed", "machine") != "<nil>" {
		t.Fatalf("parked as %v", got.body)
	}

	ok(act(vol, "attach", map[string]any{"machine": a}))
	if got := rels(vol); len(got) != 1 || got[0] != [2]string{"machine", a} {
		t.Fatalf("relations after attach: %v", got)
	}
	if got := s.do("GET", "/v1/resources/"+vol, alice, nil); got.str("observed", "in_guest") != "/data" || got.str("spec", "machine") != a {
		t.Fatalf("attached as %v", got.body)
	}
	refused(act(vol, "attach", map[string]any{"machine": b}), 422, "it is plugged into "+a)
	// neither end goes while it is plugged in
	refused(s.do("DELETE", "/v1/resources/"+a, alice, nil), 409, a+" has "+vol+" attached: detach it first — it keeps its data")
	refused(s.do("DELETE", "/v1/resources/"+vol, alice, nil), 409, vol+" is attached to "+a)

	// a running container lets go of nothing: the move fails, and the volume
	// is where it was — its spec and its relation given back
	r := act(vol, "move", map[string]any{"machine": b, "mount": "/srv"})
	if r.code != 202 {
		t.Fatalf("%d %v", r.code, r.body)
	}
	if op := s.done(alice, r.str("operation", "id")); op.str("state") != "failed" || !strings.Contains(op.str("error"), "running container") {
		t.Fatalf("the move from a running container: %v", op.body)
	}
	if got := s.do("GET", "/v1/resources/"+vol, alice, nil); got.str("spec", "machine") != a || got.str("state") != "ready" {
		t.Fatalf("after the failed move: %v", got.body)
	}
	if got := rels(vol); len(got) != 1 || got[0] != [2]string{"machine", a} {
		t.Fatalf("relations after the failed move: %v", got)
	}

	ok(act(a, "stop", nil))
	ok(act(vol, "move", map[string]any{"machine": b, "mount": "/srv"}))
	if got := rels(vol); len(got) != 1 || got[0] != [2]string{"machine", b} {
		t.Fatalf("relations after the move: %v", got)
	}
	if got := s.do("GET", "/v1/resources/"+vol, alice, nil); got.str("observed", "machine") != b || got.str("observed", "mount") != "/srv" {
		t.Fatalf("moved as %v", got.body)
	}
	ok(s.do("DELETE", "/v1/resources/"+a, alice, nil)) // nothing on it any more

	// limits, with the numbers: the backup budget is a size
	refused(act(vol, "resize", map[string]any{"size_gb": 0}), 422, "")
	refused(act(vol, "resize", map[string]any{"size_gb": 9}), 403, "volumes.size_gb")
	ok(act(vol, "resize", map[string]any{"size_gb": 2}))
	ok(act(vol, "set_backup", map[string]any{"backup": true}))
	refused(act(vol, "resize", map[string]any{"size_gb": 3}), 403, "volumes.backup_gb")
	refused(act(vol, "resize", map[string]any{"size_gb": 1}), 422, "only grows")
	vol2 := s.create(alice, map[string]any{"type": "volume", "zone": "v", "spec": map[string]any{"size_gb": 1, "machine": vm}}).str("resource", "id")
	if got := s.do("GET", "/v1/resources/"+vol2, alice, nil); got.str("spec", "content") != "block" || !strings.HasPrefix(got.str("observed", "in_guest"), "/dev/disk/by-id/") {
		t.Fatalf("a VM's volume: %v", got.body)
	}
	refused(act(vol2, "set_backup", map[string]any{"backup": true}), 403, "volumes.backup_gb")
	refused(s.do("POST", "/v1/resources", alice, map[string]any{"type": "volume", "zone": "v", "spec": map[string]any{"size_gb": 1}}), 403, "2 of 2 volumes.count used")
	nob := s.token("bob", "nobackup")
	refused(s.do("POST", "/v1/resources", nob, map[string]any{"type": "volume", "zone": "v", "spec": map[string]any{"size_gb": 1, "backup": true}}), 403, "volumes.backup_gb")

	// the engine behind the brain's back: a flag put back, a volume gone
	var engine map[string]any
	file := s.engine[:strings.LastIndex(s.engine, "/")] + "/zone-v.json"
	edit := func(f func(vols map[string]any)) {
		b, _ := os.ReadFile(file)
		_ = json.Unmarshal(b, &engine)
		f(engine["volumes"].(map[string]any))
		b, _ = json.Marshal(engine)
		if err := os.WriteFile(file, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	edit(func(vols map[string]any) { vols[vol].(map[string]any)["backup"] = false })
	s.core.ReconcileOnce(context.Background())
	if got := s.do("GET", "/v1/resources/"+vol, alice, nil); got.str("observed", "backup") != "true" || got.str("drift") != "<nil>" {
		t.Fatalf("after reconcile: %v", got.body)
	}
	if !strings.Contains(s.logs.String(), "volume.repaired") {
		t.Fatalf("no repair event in the audit")
	}

	// out: stop, detach, delete — and a machine goes once its volume has
	ok(act(b, "stop", nil))
	ok(act(vol, "detach", nil))
	if got := rels(vol); len(got) != 0 {
		t.Fatalf("relations after detach: %v", got)
	}
	refused(act(vol, "detach", nil), 422, "parked already")
	ok(s.do("DELETE", "/v1/resources/"+b, alice, nil))
	ok(s.do("DELETE", "/v1/resources/"+vol, alice, nil))
	edit(func(vols map[string]any) { delete(vols, vol2) })
	s.core.ReconcileOnce(context.Background())
	if got := s.do("GET", "/v1/resources/"+vol2, alice, nil); got.str("state") != "lost" {
		t.Fatalf("a volume gone from the engine: %v", got.body)
	}
	// a lost volume binds nothing: its machine and it both go
	ok(s.do("DELETE", "/v1/resources/"+vm, alice, nil))
	ok(s.do("DELETE", "/v1/resources/"+vol2, alice, nil))
}
