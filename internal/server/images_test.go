package server

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/tomblancdev/hangar/driver/fake"
)

const imagesConfig = `
data_dir: %[1]s
identity:
  oidc: {issuer: %[2]s, audience: hangar}
tiers:
  - name: ops
    groups: [ops]
    operator: true
    zones: [t]
    limits: {"*": unlimited, "images.source": [recipe, machine], "images.visibility": [private, shared, public],
             "machines.kind": [vm, container], "machines.class": [spot, guaranteed]}
  - name: users
    groups: [users]
    zones: [t]
    limits:
      machines.count: 4
      machines.vcpu_hours: 1000
      machines.vcpu: 8
      machines.memory_gb: 16
      machines.disk_gb: 64
      machines.kind: [vm, container]
      machines.class: [spot]
      volumes.count: 1
      volumes.size_gb: 4
      images.count: 2
      images.size_gb: 20
      images.source: [machine]
      images.visibility: [private, shared]
zones:
  - name: t
    driver: fake
    endpoint: "%[1]s/zone-t.json"
    room:
      memory_gb: 12
      grace: 1s
      reservations:
        - {name: priority, memory_gb: 6, while_running: "4100"}
plugins:
  - name: machines
    path: %[3]s
    args: [hangar-test-plugin, machines]
    zones: [t]
    settings:
      images:
        debian-13: {vm: debian-13, container: "store:vztmpl/debian-13.tar.zst"}
  - name: volumes
    path: %[3]s
    args: [hangar-test-plugin, volumes]
    zones: [t]
  - name: images
    path: %[3]s
    args: [hangar-test-plugin, images]
    zones: [t]
    settings:
      recipes:
        debian:
          base: {vm: "store:import/debian-13.qcow2"}
          disk_gb: 10
          memory_mb: 2048
          user_data: "#cloud-config\npackages: [qemu-guest-agent]\n"
        broken:
          base: {vm: "store:import/debian-13.qcow2"}
          disk_gb: 4
          user_data: "#!/bin/sh\n# hangar-fake: fail this bake\n"
reconcile:
  every: 1h
`

// setWatched writes whether the zone's priority guest runs, keeping the
// rest of the fake engine's file as it is.
func setWatched(t *testing.T, path string, on bool) {
	t.Helper()
	st := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &st); err != nil {
			t.Fatal(err)
		}
	}
	st["watched"] = map[string]bool{"4100": on}
	b, _ := json.Marshal(st)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// An image through the whole brain, on the fake engine: baked by an operator
// (a user's tier may not), pending until a reconcile finishes it, borrowing
// its builder's room meanwhile; private, then shared with everyone —
// others see it and have machines born from it, never change or delete it;
// a person's own saved from a stopped machine and shared with their own
// group only; deleted only once no machine shares its disk; retired.
func TestAnImagesLife(t *testing.T) {
	s := newStackWith(t, imagesConfig, "machines", "volumes", "images")
	ctx := context.Background()
	root := s.token("root", "ops")
	alice := s.token("alice", "users", "family")
	bob := s.token("bob", "users", "family")
	carol := s.token("carol", "users")
	refused := func(r reply, code int, words string) {
		t.Helper()
		if r.code != code || !strings.Contains(r.str("detail"), words) {
			t.Fatalf("want %d %q: %d %v", code, words, r.code, r.body)
		}
	}
	get := func(who, id string) reply {
		t.Helper()
		return s.do("GET", "/v1/resources/"+id, who, nil)
	}
	act := func(who, id, action string, params map[string]any) reply {
		t.Helper()
		r := s.do("POST", "/v1/resources/"+id+"/actions/"+action, who, map[string]any{"params": params})
		if r.code == 202 {
			s.done(who, r.str("operation", "id"))
		}
		return r
	}
	opEnds := func(who string, r reply, state string) reply {
		t.Helper()
		if r.code != 202 {
			t.Fatalf("%d %v", r.code, r.body)
		}
		op := s.done(who, r.str("operation", "id"))
		if op.str("state") != state {
			t.Fatalf("want %s: %v", state, op.body)
		}
		return op
	}
	machine := func(who string, spec map[string]any) string {
		t.Helper()
		return s.create(who, map[string]any{"type": "machine", "zone": "t", "spec": spec}).str("resource", "id")
	}

	// a user's tier makes no image from a recipe; an operator's does
	refused(s.do("POST", "/v1/resources", alice, map[string]any{"type": "image", "zone": "t", "spec": map[string]any{"recipe": "debian"}}),
		403, "images.source")
	refused(s.do("POST", "/v1/resources", root, map[string]any{"type": "image", "zone": "t", "spec": map[string]any{"recipe": "nope"}}),
		422, `no recipe "nope" here (there are: broken, debian)`)
	img := s.create(root, map[string]any{"type": "image", "zone": "t", "name": "debian", "spec": map[string]any{"recipe": "debian"}}).str("resource", "id")
	r := get(root, img)
	if r.str("observed", "state") != "pending" || r.str("room", "spot_mb") != "2048" || r.str("room", "running") != "true" {
		t.Fatalf("a bake begun is pending and borrows its builder's room: %v", r.body)
	}
	if r.str("spec", "from", "recipe") != "debian" || r.str("spec", "size_gb") != "10" || r.str("spec", "attempt") != "1" {
		t.Fatalf("its recipe is written in it: %v", r.body["spec"])
	}
	refused(machine2(s, root, img), 422, img+" is pending")
	if r.str("unusable") != "pending" || r.str("pending") != "true" {
		t.Fatalf("the brain is told it is still being made: %v", r.body)
	}
	s.core.ReconcileOnce(ctx)
	r = get(root, img)
	if r.str("observed", "state") != "available" || r.str("room", "spot_mb") != "0" || r.str("room", "running") != "false" {
		t.Fatalf("a finished bake is available and gives its room back: %v", r.body)
	}
	if r.str("observed", "forms", "vm") != "fake-img-"+img || r.str("observed", "made_at") == "<nil>" {
		t.Fatalf("its engine form and its birth: %v", r.body["observed"])
	}

	// private: bob neither sees nor names it
	refused(get(bob, img), 404, "no resource "+img)
	refused(s.do("POST", "/v1/resources", bob, map[string]any{"type": "machine", "zone": "t", "spec": map[string]any{"image_id": img}}),
		422, "you have no image "+img)

	// shared with everyone: seen, listed, named — not changed, not deleted
	act(root, img, "share", map[string]any{"shared_with": []string{"*"}})
	if r = get(bob, img); r.code != 200 || r.str("shared_with", "0") != "*" {
		t.Fatalf("bob sees what is everyone's: %d %v", r.code, r.body)
	}
	refused(s.do("DELETE", "/v1/resources/"+img, bob, nil), 403, "only its owner changes it")
	refused(s.do("POST", "/v1/resources/"+img+"/actions/retire", bob, map[string]any{}), 403, "only its owner changes it")
	refused(s.do("POST", "/v1/resources", bob, map[string]any{"type": "machine", "zone": "t", "spec": map[string]any{"image_id": img, "disk_gb": 4}}),
		422, img+"'s disk is 10 GB: disk_gb is at least that")
	refused(s.do("POST", "/v1/resources", bob, map[string]any{"type": "machine", "zone": "t", "spec": map[string]any{"image_id": img, "kind": "container"}}),
		422, "is an image for a vm, not for a container")
	refused(s.do("POST", "/v1/resources", bob, map[string]any{"type": "machine", "zone": "t", "spec": map[string]any{"image_id": img, "image": "debian-13"}}),
		422, "not both")
	bm := machine(bob, map[string]any{"image_id": img})
	if r = get(bob, bm); r.str("spec", "disk_gb") != "10" {
		t.Fatalf("born with the image's disk: %v", r.body["spec"])
	}
	if got := bornFrom(t, s, bm); got != "fake-img-"+img {
		t.Fatalf("bob's machine was not born from the image: %q", got)
	}
	if rels, _ := s.store.Relations(ctx, bm); !slices.Contains(rels, [2]string{"image_id", img}) {
		t.Fatalf("the machine names its image: %v", rels)
	}

	// the image's disk is shared by what was born from it: no delete then
	opEnds(root, s.do("DELETE", "/v1/resources/"+img, root, nil), "failed")
	if r = get(root, img); r.str("state") != "ready" {
		t.Fatalf("a refused delete leaves it as it was: %v", r.body)
	}
	// retired: no machine born from it any more; bob's runs on
	act(root, img, "retire", nil)
	refused(machine2(s, bob, img), 422, "is retired")
	if r = get(bob, bm); r.str("observed", "running") != "true" {
		t.Fatalf("a machine born from a retired image runs on: %v", r.body)
	}

	// a person's own, saved from their stopped machine — never its volumes
	am := machine(alice, map[string]any{"image": "debian-13", "disk_gb": 6})
	refused(s.do("POST", "/v1/resources", alice, map[string]any{"type": "image", "zone": "t", "spec": map[string]any{"machine": am}}),
		422, "runs: stop it first")
	act(alice, am, "stop", nil)
	vol := s.create(alice, map[string]any{"type": "volume", "zone": "t", "spec": map[string]any{"size_gb": 1, "machine": am}}).str("resource", "id")
	op := opEnds(alice, s.do("POST", "/v1/resources", alice, map[string]any{"type": "image", "zone": "t", "spec": map[string]any{"machine": am}}), "failed")
	if !strings.Contains(op.str("error"), "an image is its system disk alone — detach them first") {
		t.Fatalf("a machine with a volume is not saved: %v", op.body)
	}
	act(alice, vol, "detach", nil)
	mine := s.create(alice, map[string]any{"type": "image", "zone": "t", "spec": map[string]any{"machine": am}}).str("resource", "id")
	if r = get(alice, mine); r.str("observed", "state") != "available" || r.str("spec", "size_gb") != "6" || r.str("choices", "images.source") != "machine" {
		t.Fatalf("a save is available at once, the machine's disk its size: %v", r.body)
	}
	// shared with her own group — not one she is not in, not everyone
	refused(s.do("POST", "/v1/resources/"+mine+"/actions/share", alice, map[string]any{"params": map[string]any{"shared_with": []string{"ops"}}}),
		403, "you share only with groups you are in (users, family), or with everyone (*): not ops")
	refused(s.do("POST", "/v1/resources/"+mine+"/actions/share", alice, map[string]any{"params": map[string]any{"shared_with": []string{"*"}}}),
		403, "images.visibility")
	act(alice, mine, "share", map[string]any{"shared_with": []string{"family"}})
	if get(bob, mine).code != 200 {
		t.Fatal("bob, in family, sees alice's image")
	}
	refused(get(carol, mine), 404, "no resource")
	refused(s.do("POST", "/v1/resources", carol, map[string]any{"type": "machine", "zone": "t", "spec": map[string]any{"image_id": mine}}),
		422, "you have no image "+mine)
	list := s.do("GET", "/v1/resources?type=image", bob, nil)
	if n := len(list.body["resources"].([]any)); n != 2 {
		t.Fatalf("bob lists root's public image and alice's family one: %v", list.body)
	}
	if n := len(s.do("GET", "/v1/resources?type=image", carol, nil).body["resources"].([]any)); n != 1 {
		t.Fatalf("carol lists what is everyone's alone: %d", n)
	}
	// what a resource names is called by name for its owner alone: bob, who
	// only shares the image, reads the id of the machine it was saved from
	for id, name := range map[string]string{am: "forge", mine: "base"} {
		if r := s.do("PATCH", "/v1/resources/"+id, alice, map[string]any{"name": name}); r.code != 200 {
			t.Fatalf("%d %v", r.code, r.body)
		}
	}
	if r = get(alice, mine); r.str("summary") != "vm · 6 GB · saved from forge · shared with family" || r.str("names", am) != "forge" || r.str("status") != "available" {
		t.Fatalf("alice reads her image: %q %v", r.str("summary"), r.body["names"])
	}
	if r = get(bob, mine); r.str("summary") != "vm · 6 GB · saved from "+am+" · shared with family" || r.body["names"] != nil || r.str("name") != "base" {
		t.Fatalf("bob reads alice's image: %q %v", r.str("summary"), r.body["names"])
	}
	// a name is its owner's word for their own: what is shared is named by
	// its id — anyone may call theirs base, and a request by name would be
	// handed a look-alike. Alice's own machine is born from her base
	refused(s.do("POST", "/v1/resources", bob, map[string]any{"type": "machine", "zone": "t", "spec": map[string]any{"image_id": "base"}}),
		422, "you have no image named base (one shared with you is named by its id)")
	own := s.do("POST", "/v1/resources", alice, map[string]any{"type": "machine", "zone": "t", "spec": map[string]any{"image_id": "base"}})
	if own.code != 202 || own.str("resource", "spec", "image_id") != mine {
		t.Fatalf("alice's machine from her base: %d %v", own.code, own.body)
	}
	opEnds(alice, own, "succeeded")
	opEnds(alice, s.do("DELETE", "/v1/resources/"+own.str("resource", "id"), alice, nil), "succeeded")
	bm2 := machine(bob, map[string]any{"image_id": mine})
	if got := bornFrom(t, s, bm2); got != "fake-img-"+mine {
		t.Fatalf("bob's second machine is born from alice's image: %q", got)
	}

	// the image goes once nothing shares its disk
	opEnds(bob, s.do("DELETE", "/v1/resources/"+bm, bob, nil), "succeeded")
	opEnds(root, s.do("DELETE", "/v1/resources/"+img, root, nil), "succeeded")
	if b, _ := os.ReadFile(s.cfg.Zones[0].Endpoint); strings.Contains(string(b), `"fake-img-`+img+`"`) {
		t.Fatalf("the engine still has the image: %s", b)
	}
}

// bornFrom reads, in the fake engine's file, what a guest was made from.
func bornFrom(t *testing.T, s *stack, id string) string {
	t.Helper()
	var st struct {
		Specs map[string]struct {
			Image string `json:"Image"`
		} `json:"specs"`
	}
	b, _ := os.ReadFile(s.cfg.Zones[0].Endpoint)
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	return st.Specs[id].Image
}

// machine2 asks for a VM born from an image, and returns the answer.
func machine2(s *stack, who, img string) reply {
	return s.do("POST", "/v1/resources", who, map[string]any{"type": "machine", "zone": "t", "spec": map[string]any{"image_id": img}})
}

// A bake meets the zone's priority guest: refused while the room is held,
// let go when the room is needed mid-bake, and started over by itself once
// it is back — its room borrowed all along, never counted as failed. A
// recipe that fails is failed with its own words, and stays failed until
// its owner bakes it again.
func TestABakeGivesWayAndARecipeFails(t *testing.T) {
	s := newStackWith(t, imagesConfig, "machines", "volumes", "images")
	ctx := context.Background()
	root := s.token("root", "ops")
	file := s.cfg.Zones[0].Endpoint
	get := func(id string) reply { t.Helper(); return s.do("GET", "/v1/resources/"+id, root, nil) }
	state := func(id string) string { t.Helper(); return get(id).str("observed", "state") }
	bake := func(recipe string) string {
		t.Helper()
		r := s.create(root, map[string]any{"type": "image", "zone": "t", "spec": map[string]any{"recipe": recipe}})
		if r.code != 202 {
			t.Fatalf("bake: %d %v", r.code, r.body)
		}
		return r.str("resource", "id")
	}

	// mid-bake, the priority guest starts: the builder is let go
	img := bake("debian")
	setWatched(t, file, true)
	s.core.ReconcileOnce(ctx)
	if r := get(img); r.str("observed", "state") != "waiting" || r.str("hold") != "4100" {
		t.Fatalf("held mid-bake, it waits: %v", r.body)
	}
	// a bake asked while the room is held is refused, as a spot machine is
	if r := s.do("POST", "/v1/resources", root, map[string]any{"type": "image", "zone": "t", "spec": map[string]any{"recipe": "debian"}}); r.code != 409 ||
		!strings.Contains(r.str("detail"), "borrowed room is held for priority") {
		t.Fatalf("a bake asked while the room is held: %d %v", r.code, r.body)
	}
	s.core.ReconcileOnce(ctx)
	if state(img) != "waiting" {
		t.Fatal("still held, it still waits")
	}
	// the room is back: both start over, then finish
	setWatched(t, file, false)
	s.core.ReconcileOnce(ctx)
	if state(img) != "pending" {
		t.Fatalf("the room back, the bake starts over: %s", state(img))
	}
	s.core.ReconcileOnce(ctx)
	if r := get(img); r.str("observed", "state") != "available" || r.str("room", "spot_mb") != "0" || r.str("observed", "restarts") != "<nil>" {
		t.Fatalf("finished after the hold, no restart counted: %v", r.body)
	}

	// a recipe that fails: its words, its room given back, never retried
	bad := bake("broken")
	s.core.ReconcileOnce(ctx)
	r := get(bad)
	if r.str("observed", "state") != "failed" || !strings.Contains(r.str("observed", "detail"), fake.FailBake) || r.str("room", "spot_mb") != "0" {
		t.Fatalf("a failed bake says why and gives its room back: %v", r.body)
	}
	if u, _ := r.body["usage"].(map[string]any); len(u) != 0 || r.str("unusable") != "failed" {
		t.Fatalf("a failed bake holds nothing, and no machine is born from it: %v", r.body)
	}
	s.core.ReconcileOnce(ctx)
	if r = get(bad); r.str("observed", "state") != "failed" || r.str("observed", "attempt") != "1" {
		t.Fatalf("a failed bake is not retried by itself: %v", r.body)
	}
	if r := s.do("POST", "/v1/resources/"+img+"/actions/rebake", root, map[string]any{}); r.code != 422 || !strings.Contains(r.str("detail"), "only a failed bake") {
		t.Fatalf("an available image is not baked again: %d %v", r.code, r.body)
	}
	if r := s.do("POST", "/v1/resources/"+bad+"/actions/rebake", root, map[string]any{}); r.code != 202 {
		t.Fatalf("rebake: %d %v", r.code, r.body)
	} else {
		s.done(root, r.str("operation", "id"))
	}
	if r = get(bad); r.str("observed", "state") != "pending" || r.str("spec", "attempt") != "2" || r.str("room", "spot_mb") != "2048" {
		t.Fatalf("baked again: attempt 2, pending, its room borrowed: %v", r.body)
	}
	if r.str("usage", "images.count") != "1" || r.str("usage", "images.size_gb") != "4" {
		t.Fatalf("baked again, it is admitted anew: %v", r.body["usage"])
	}
	s.core.ReconcileOnce(ctx)
	if r = get(bad); r.str("observed", "state") != "failed" || r.str("observed", "attempt") != "2" {
		t.Fatalf("the same recipe fails again, as attempt 2: %v", r.body)
	}
}
