package server

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

const endConfig = `
data_dir: %[1]s
identity:
  oidc: {issuer: %[2]s, audience: hangar}
tiers:
  - name: users
    groups: [users]
    zones: [e]
    limits:
      machines.count: 5
      machines.vcpu_hours: 1000
      machines.vcpu: 16
      machines.memory_gb: 16
      machines.disk_gb: 64
      machines.kind: [vm, container]
      machines.class: [spot]
      volumes.count: 2
      volumes.size_gb: 8
zones:
  - {name: e, driver: fake, endpoint: "%[1]s/zone-e.json"}
plugins:
  - name: machines
    path: %[3]s
    args: [hangar-test-plugin, machines]
    zones: [e]
    settings:
      images:
        debian-13: {vm: debian-13, container: "store:vztmpl/debian-13.tar.zst"}
  - name: volumes
    path: %[3]s
    args: [hangar-test-plugin, volumes]
    zones: [e]
reconcile:
  every: 1h
`

// A machine's end through the whole brain: its own power-off read at a look,
// and the brain deletes it — an operation of its owner's, the brain its
// actor — or, when something is attached to it, leaves it stopped and says
// why, once; its until come, deleted the same way; one that says nothing is
// started again, as ever.
func TestAMachinesEndThroughTheBrain(t *testing.T) {
	s := newStackWith(t, endConfig, "machines", "volumes")
	alice := s.token("alice", "users")
	file := strings.TrimSuffix(s.engine, "zone-z.json") + "zone-e.json"
	edit := func(f func(m map[string]any)) {
		t.Helper()
		m := map[string]any{}
		if b, err := os.ReadFile(file); err == nil {
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatal(err)
			}
		}
		f(m)
		b, _ := json.Marshal(m)
		if err := os.WriteFile(file, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	edit(func(m map[string]any) { m["now"] = at.Format(time.RFC3339) })
	guest := func(m map[string]any, id string) map[string]any {
		g, _ := m["guests"].(map[string]any)[id].(map[string]any)
		return g
	}
	off := func(id string) {
		t.Helper()
		edit(func(m map[string]any) { g := guest(m, id); g["running"] = false; delete(g, "started_at") })
	}
	pass := func(after time.Duration) {
		t.Helper()
		at = at.Add(after)
		edit(func(m map[string]any) { m["now"] = at.Format(time.RFC3339) })
		s.core.ReconcileOnce(context.Background())
		s.core.Wait()
	}
	machine := func(spec map[string]any) string {
		t.Helper()
		full := map[string]any{"image": "debian-13", "kind": "container", "cores": 1, "memory_gb": 1}
		for k, v := range spec {
			full[k] = v
		}
		return s.create(alice, map[string]any{"type": "machine", "zone": "e", "spec": full}).str("resource", "id")
	}
	audit := func(words ...string) []string {
		var out []string
	lines:
		for _, l := range strings.Split(s.logs.String(), "\n") {
			if !strings.Contains(l, `"kind":"audit"`) {
				continue
			}
			for _, w := range words {
				if !strings.Contains(l, w) {
					continue lines
				}
			}
			out = append(out, l)
		}
		return out
	}
	get := func(id string) reply { t.Helper(); return s.do("GET", "/v1/resources/"+id, alice, nil) }
	onEngine := func(id string) bool {
		var m map[string]any
		edit(func(x map[string]any) { m = x })
		return guest(m, id) != nil
	}

	gone := machine(map[string]any{"on_poweroff": "delete"})
	kept := machine(map[string]any{"on_poweroff": "delete"})
	vol := s.create(alice, map[string]any{"type": "volume", "zone": "e", "spec": map[string]any{"size_gb": 1, "mount": "/data", "machine": kept}}).str("resource", "id")
	stays := machine(map[string]any{"on_poweroff": "stop"})
	again := machine(nil)
	leased := machine(map[string]any{"for": "30m", "at_end": "delete"})
	pass(time.Minute)

	for _, id := range []string{gone, kept, stays, again} {
		off(id)
	}
	// the first look that finds them so: one that says nothing is started
	// again; the others wait for the next
	pass(time.Minute)
	if g := get(gone); g.str("state") != "ready" || !onEngine(gone) || !strings.Contains(g.str("drift"), "if it is still stopped 30s after") {
		t.Fatalf("the first look: %v", g.body)
	}
	pass(30 * time.Second)

	// its own power-off: deleted, by an operation of its owner's that the
	// brain made — the engine has it no more, its room and its count free
	if g := get(gone); g.str("state") != "deleted" || onEngine(gone) {
		t.Fatalf("on_poweroff delete: %v", g.body)
	}
	ops := s.do("GET", "/v1/operations?resource="+gone, alice, nil)
	if !strings.Contains(ops.str("operations"), "kind:delete") || !strings.Contains(ops.str("operations"), "state:succeeded") {
		t.Fatalf("its delete is an operation its owner sees: %v", ops.body)
	}
	if len(audit(`"action":"end"`, `"resource":"`+gone+`"`, `"result":"accepted"`, "its on_poweroff says delete")) != 1 ||
		len(audit(`"action":"operation"`, `"actor":"hangar"`, `"resource":"`+gone+`"`, `"ended":"true"`, `"result":"succeeded"`)) != 1 ||
		len(audit(`"result":"machine.ended"`, `"resource":"`+gone+`"`, `"by":"poweroff"`)) != 1 {
		t.Fatalf("the audit says it: %s", strings.Join(audit(gone), "\n"))
	}
	if len(audit(`"resource":"`+gone+`"`, "put back")) != 0 {
		t.Fatal("it was started again on its way to its end")
	}

	// a volume on it: not deleted — it stays stopped, said once
	if g := get(kept); g.str("state") != "ready" || g.str("spec", "running") != "false" || !onEngine(kept) {
		t.Fatalf("a machine with a volume on it: %v", g.body)
	}
	if got := audit(`"action":"end"`, `"resource":"`+kept+`"`, `"result":"refused"`); len(got) != 1 ||
		!strings.Contains(got[0], "has "+vol+" attached: detach it first") {
		t.Fatalf("the refusal said: %v", got)
	}

	// stop: stays stopped; nothing: started again
	if g := get(stays); g.str("state") != "ready" || g.str("spec", "running") != "false" || len(audit(`"result":"machine.stopped_itself"`, stays)) != 1 {
		t.Fatalf("on_poweroff stop: %v", g.body)
	}
	if g := get(again); g.str("spec", "running") != "true" || len(audit(`"resource":"`+again+`"`, "put back: [power]")) == 0 {
		t.Fatalf("one that says nothing: %v", g.body)
	}

	// pass after pass, nothing more of any of them — the refusal said once
	pass(time.Hour)
	pass(time.Minute)
	if len(audit(`"action":"end"`, kept)) != 1 || len(audit(`"action":"end"`, gone)) != 1 || get(stays).str("spec", "running") != "false" || get(kept).str("state") != "ready" {
		t.Fatalf("a look after: %s", strings.Join(audit(`"action":"end"`), "\n"))
	}
	// the volume detached, its end is carried out at the next look
	if r := s.do("POST", "/v1/resources/"+vol+"/actions/detach", alice, map[string]any{}); r.code != 202 || s.done(alice, r.str("operation", "id")).str("state") != "succeeded" {
		t.Fatalf("detach: %d %v", r.code, r.body)
	}
	pass(time.Minute)
	if g := get(kept); g.str("state") != "deleted" || onEngine(kept) || len(audit(`"action":"end"`, kept, `"result":"accepted"`)) != 1 {
		t.Fatalf("its end, once nothing holds on to it: %v", g.body)
	}

	// its until came (30m from its birth; an hour and two minutes went by)
	if g := get(leased); g.str("state") != "deleted" || onEngine(leased) || len(audit(`"result":"machine.ended"`, leased, `"by":"until"`)) != 1 {
		t.Fatalf("until: %v", g.body)
	}

	// five made of five: the three deleted give their places back, and no more
	for i := range 4 {
		r := s.do("POST", "/v1/resources", alice, map[string]any{"type": "machine", "zone": "e",
			"spec": map[string]any{"image": "debian-13", "kind": "container", "cores": 1, "memory_gb": 1}})
		if want := map[bool]int{true: 202, false: 403}[i < 3]; r.code != want {
			t.Fatalf("machine %d of the places given back: %d %v", i+1, r.code, r.body)
		}
		if r.code == 202 {
			s.done(alice, r.str("operation", "id"))
		}
	}
}
