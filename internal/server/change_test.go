package server

import (
	"strings"
	"testing"
)

// A change's plan through the whole brain: the actions that bring a volume
// and a machine to a new spec, in order, or the fields set at their birth —
// and nothing changed by asking.
func TestAChangesPlan(t *testing.T) {
	s := newStackWith(t, volumesConfig, "machines", "volumes")
	alice, bob := s.token("alice", "users"), s.token("bob", "users")
	id := func(r reply) string { return r.str("resource", "id") }
	a := id(s.create(alice, map[string]any{"type": "machine", "zone": "v", "spec": map[string]any{"image": "debian-13", "kind": "container"}}))
	b := id(s.create(alice, map[string]any{"type": "machine", "zone": "v", "spec": map[string]any{"image": "debian-13", "kind": "container"}}))
	vol := id(s.create(alice, map[string]any{"type": "volume", "zone": "v", "spec": map[string]any{"size_gb": 1, "mount": "/data"}}))
	plan := func(who, id string, spec map[string]any) reply {
		t.Helper()
		return s.do("POST", "/v1/resources/"+id+"/plan", who, map[string]any{"spec": spec})
	}
	steps := func(r reply) string {
		t.Helper()
		if r.code != 200 {
			t.Fatalf("%d %v", r.code, r.body)
		}
		var out []string
		for _, st := range r.body["steps"].([]any) {
			m := st.(map[string]any)
			out = append(out, m["action"].(string)+strings.ReplaceAll(strings.TrimPrefix(reply{body: map[string]any{"p": m["params"]}}.str("p"), "map"), " ", ","))
		}
		return strings.Join(out, " ")
	}
	fixed := func(r reply) string {
		t.Helper()
		var out []string
		for _, f := range asList(r.body["fixed"]) {
			out = append(out, f.(map[string]any)["field"].(string))
		}
		return strings.Join(out, " ")
	}

	// in sync: nothing to do
	if got := steps(plan(alice, vol, map[string]any{"size_gb": 1, "mount": "/data"})); got != "" {
		t.Fatalf("in sync: %q", got)
	}
	// grown, backed up, plugged in: the machine last
	if got := steps(plan(alice, vol, map[string]any{"size_gb": 2, "mount": "/data", "backup": true, "machine": a})); got != "resize[size_gb:2] set_backup[backup:true] attach[machine:"+a+",mount:/data]" {
		t.Fatalf("grow, back up, attach: %q", got)
	}
	// asking changed nothing
	if r := s.do("GET", "/v1/resources/"+vol, alice, nil); r.str("spec", "size_gb") != "1" || r.str("spec", "machine") != "<nil>" {
		t.Fatalf("a plan is no change: %v", r.body)
	}
	// its content and a smaller size are set; the field is named
	r := plan(alice, vol, map[string]any{"size_gb": 1})
	if steps(r); fixed(r) != "/content" || !strings.Contains(r.str("fixed", "0", "reason"), "a filesystem volume, and a volume's content is set at its birth") {
		t.Fatalf("content: %v", r.body)
	}
	if r := plan(alice, vol, map[string]any{"size_gb": 1, "mount": "/data", "machine": a}); steps(r) != "attach[machine:"+a+",mount:/data]" {
		t.Fatal(r.body)
	}
	s.done(alice, s.do("POST", "/v1/resources/"+vol+"/actions/attach", alice, map[string]any{"params": map[string]any{"machine": a}}).str("operation", "id"))
	if got := steps(plan(alice, vol, map[string]any{"size_gb": 1, "mount": "/data", "machine": b})); got != "move[machine:"+b+",mount:/data]" {
		t.Fatalf("move: %q", got)
	}
	if got := steps(plan(alice, vol, map[string]any{"size_gb": 1, "mount": "/srv", "machine": a})); got != "detach[] attach[machine:"+a+",mount:/srv]" {
		t.Fatalf("a new path: %q", got)
	}
	if r := plan(alice, vol, map[string]any{"size_gb": 1, "mount": "/data"}); steps(r) != "detach[]" || fixed(r) != "" {
		t.Fatalf("detach: %v", r.body)
	}
	// what it names anew is checked as a create's: bob's machine is no
	// machine of alice's
	bm := id(s.create(bob, map[string]any{"type": "machine", "zone": "v", "spec": map[string]any{"image": "debian-13", "kind": "container"}}))
	if r := plan(alice, vol, map[string]any{"size_gb": 1, "mount": "/data", "machine": bm}); r.code != 422 || !strings.Contains(r.str("detail"), "you have no machine "+bm) {
		t.Fatalf("someone else's machine: %d %v", r.code, r.body)
	}

	// a machine: its size through resize, its kind set at its birth
	if got := steps(plan(alice, a, map[string]any{"image": "debian-13", "kind": "container"})); got != "" {
		t.Fatalf("in sync with its defaults: %q", got)
	}
	if got := steps(plan(alice, a, map[string]any{"image": "debian-13", "kind": "container", "type": "t3.medium"})); got != "resize[type:t3.medium]" {
		t.Fatalf("resize: %q", got)
	}
	if got := steps(plan(alice, a, map[string]any{"image": "debian-13", "kind": "container", "cores": 3, "memory_gb": 2})); got != "resize[cores:3,memory_gb:2]" {
		t.Fatalf("resize by cores: %q", got)
	}
	if r := plan(alice, a, map[string]any{"image": "debian-13"}); fixed(r) != "/kind" || !strings.Contains(r.str("fixed", "0", "reason"), "it is a container, and a machine's kind is set at its birth") {
		t.Fatalf("kind: %v", r.body)
	}
	// the spec is checked as a create's
	if r := plan(alice, a, map[string]any{"image": "debian-13", "kind": "container", "colour": "red"}); r.code != 422 || r.str("kind") != "schema" {
		t.Fatalf("schema: %d %v", r.code, r.body)
	}
	// only its owner plans a change; someone else's reads as nothing
	if r := plan(bob, a, map[string]any{"image": "debian-13"}); r.code != 404 {
		t.Fatalf("bob: %d %v", r.code, r.body)
	}
	// a read-only token may ask (it changes nothing)
	if r := plan(s.readOnly("alice", "users"), a, map[string]any{"image": "debian-13", "kind": "container"}); r.code != 200 {
		t.Fatalf("read-only: %d %v", r.code, r.body)
	}
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}
