package server

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tomblancdev/hangar/internal/core"
)

const scheduleConfig = `
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
  - name: recipes
    groups: [recipes]
    zones: [t]
    limits: {images.count: 3, images.size_gb: 100, images.source: [recipe], images.visibility: [public]}
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
zones:
  - name: t
    driver: fake
    endpoint: "%[1]s/zone-t.json"
plugins:
  - name: machines
    path: %[3]s
    args: [hangar-test-plugin, machines]
    zones: [t]
    settings:
      images:
        debian-13: {vm: debian-13}
  - name: images
    path: %[3]s
    args: [hangar-test-plugin, images]
    zones: [t]
    settings:
      recipes:
        debian:
          base: {vm: "store:import/debian-13.qcow2"}
          disk_gb: 10
          user_data: "#cloud-config\npackages: [qemu-guest-agent]\n"
schedules:
  - name: debian
    cron: "0 3 * * sun"
    time_zone: Europe/Paris
    as: {subject: recipes, groups: [recipes]}
    create: {type: image, zone: t, spec: {recipe: debian, shared_with: ["*"]}}
    keep: 2
    retire: retire
reconcile:
  every: 1h
`

// A recipe baked again by the brain's own clock, on the fake engine: every
// Sunday at 03:00 in Paris, in the name the schedule names and within its
// tier, shared with everyone. "@debian" names the newest usable one — a
// machine keeps the one it was born from; the older ones are retired once
// two newer are usable, never before, and deleted once no machine is born
// from them; a failed bake goes once a newer one works. A run while the
// last bake is still being made is skipped; runs the brain missed come
// once; an image baked by hand from the same recipe is never touched.
func TestARecipeBakedAgainByItself(t *testing.T) {
	var mu sync.Mutex
	clock := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC) // a Wednesday
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	s := newStackClock(t, scheduleConfig, now, "machines", "images")
	ctx := context.Background()
	root := s.token("root", "ops")
	bob := s.token("bob", "users")
	run := func(at time.Time) {
		t.Helper()
		mu.Lock()
		clock = at
		mu.Unlock()
		s.core.ScheduleOnce(ctx)
		s.core.Wait()
		// what it asked to let go of is done: look again, as the next tick would
		s.core.ScheduleOnce(ctx)
		s.core.Wait()
	}
	bake := func() { t.Helper(); s.core.ReconcileOnce(ctx); s.core.Wait() }
	get := func(id string) reply { t.Helper(); return s.do("GET", "/v1/resources/"+id, root, nil) }
	// what the schedule made and still holds something, oldest first, with
	// why each is not usable ("" = usable)
	made := func() (ids, words []string) {
		t.Helper()
		r := s.do("GET", "/v1/resources?type=image&tag="+core.ScheduleTag+"=debian", root, nil)
		for _, x := range r.body["resources"].([]any) {
			m := x.(map[string]any)
			w, _ := m["unusable"].(string)
			ids, words = append(ids, m["id"].(string)), append(words, w)
		}
		return
	}
	lastRun := func() (string, string) {
		t.Helper()
		st, err := s.store.Schedule(ctx, "debian")
		if err != nil {
			t.Fatal(err)
		}
		return st.LastResult, st.LastDetail
	}
	machine := func(who string, image string) reply {
		t.Helper()
		return s.do("POST", "/v1/resources", who, map[string]any{"type": "machine", "zone": "t", "spec": map[string]any{"image_id": image}})
	}
	sunday := func(day int, month time.Month) time.Time {
		// 03:00 in Paris, and 30 s: summer time until the 25th of October
		h := 1
		if month > time.October || month == time.October && day >= 25 {
			h = 2
		}
		return time.Date(2026, month, day, h, 0, 30, 0, time.UTC)
	}

	// an image baked by hand from the same recipe: the schedule's never
	hand := s.create(root, map[string]any{"type": "image", "zone": "t", "spec": map[string]any{"recipe": "debian", "shared_with": []string{"*"}}}).str("resource", "id")
	bake()

	// first sight: armed, nothing asked; the latest names nothing yet
	run(clock)
	if ids, _ := made(); len(ids) != 0 {
		t.Fatalf("a schedule seen for the first time waits for its time: %v", ids)
	}
	if r := machine(bob, "@debian"); r.code != 422 || !strings.Contains(r.str("detail"), "schedule debian has made no usable image you may name yet") {
		t.Fatalf("nothing made yet: %d %v", r.code, r.body)
	}
	for image, words := range map[string]string{"@nope": "no schedule nope here"} {
		if r := machine(bob, image); r.code != 422 || !strings.Contains(r.str("detail"), words) {
			t.Fatalf("%s: %d %v", image, r.code, r.body)
		}
	}
	if r := s.do("POST", "/v1/resources", bob, map[string]any{"type": "machine", "zone": "t",
		"spec": map[string]any{"image": "debian-13", "key_pairs": []string{"@debian"}}}); r.code != 422 || !strings.Contains(r.str("detail"), "makes images, not keypairs") {
		t.Fatalf("a schedule names only what it makes: %d %v", r.code, r.body)
	}
	run(sunday(3, time.October).Add(-time.Minute))
	if ids, _ := made(); len(ids) != 0 {
		t.Fatal("Saturday: nothing")
	}

	// Sunday 03:00: asked once, in the schedule's name, shared with everyone
	run(sunday(4, time.October))
	run(sunday(4, time.October).Add(time.Minute))
	ids, words := made()
	if len(ids) != 1 || words[0] != "pending" {
		t.Fatalf("one bake asked, pending: %v %v", ids, words)
	}
	img1 := ids[0]
	if r := get(img1); r.str("owner") != "recipes" || r.str("shared_with", "0") != "*" || r.str("tags", core.ScheduleTag) != "debian" {
		t.Fatalf("the schedule's, everyone's, tagged: %v", r.body)
	}
	if res, detail := lastRun(); res != "asked" || !strings.HasPrefix(detail, "operation op-") {
		t.Fatalf("its run: %s %s", res, detail)
	}
	if r := machine(bob, "@debian"); r.code != 422 {
		t.Fatalf("still baking, it names nothing: %d %v", r.code, r.body)
	}
	bake()
	// bob asks for the latest: born from img1, and says so
	r := machine(bob, "@debian")
	if r.code != 202 {
		t.Fatalf("born from the latest: %d %v", r.code, r.body)
	}
	bm := r.str("resource", "id")
	s.core.Wait()
	if r = get(bm); r.str("spec", "image_id") != img1 || bornFrom(t, s, bm) != "fake-img-"+img1 {
		t.Fatalf("a machine keeps the image it was born from: %v", r.body["spec"])
	}

	// the next Sunday: a second one; a Sunday while it still bakes: skipped
	run(sunday(11, time.October))
	if ids, _ = made(); len(ids) != 2 {
		t.Fatalf("the second Sunday: %v", ids)
	}
	img2 := ids[1]
	run(sunday(18, time.October))
	if ids, _ = made(); len(ids) != 2 {
		t.Fatalf("a run while the last still bakes asks nothing: %v", ids)
	}
	if res, detail := lastRun(); res != "skipped" || detail != img2+" is still being made" {
		t.Fatalf("skipped, and why: %s %s", res, detail)
	}
	bake()
	if r = machine(bob, "@debian"); r.code != 202 {
		t.Fatalf("%d %v", r.code, r.body)
	} else if s.core.Wait(); get(r.str("resource", "id")).str("spec", "image_id") != img2 {
		t.Fatal("the latest is now the second")
	} else {
		s.do("DELETE", "/v1/resources/"+r.str("resource", "id"), bob, nil)
		s.core.Wait()
	}
	if ids, words = made(); len(ids) != 2 || words[0] != "" || words[1] != "" {
		t.Fatalf("two usable, both kept: %v %v", ids, words)
	}

	// the day the clocks go back, the third: two newer usable, the first is
	// retired — and stays while bob's machine is born from it
	run(sunday(25, time.October))
	img3 := func() string { ids, _ := made(); return ids[2] }()
	if r = get(img1); word(r) != "" {
		t.Fatalf("the third still bakes: the first is not retired yet: %v", r.body)
	}
	bake()
	run(sunday(25, time.October).Add(time.Minute))
	if r = get(img1); word(r) != "retired" || r.str("state") != "ready" {
		t.Fatalf("two newer usable: the first retired, kept while a machine is born from it: %v", r.body)
	}
	if r = machine(bob, img1); r.code != 422 || !strings.Contains(r.str("detail"), img1+" is retired") {
		t.Fatalf("no machine is born from a retired image: %d %v", r.code, r.body)
	}
	if r = get(bm); r.str("observed", "running") != "true" {
		t.Fatalf("bob's machine runs on: %v", r.body)
	}
	// bob's machine brought to a spec: "@debian" is where it is — born from
	// what the schedule made, retired since — and never moves it to the
	// newest; the image it names already needs no longer be usable; another
	// image is set at its birth
	plan := func(spec map[string]any) reply {
		return s.do("POST", "/v1/resources/"+bm+"/plan", bob, map[string]any{"spec": spec})
	}
	if r = plan(map[string]any{"image_id": "@debian"}); r.code != 200 || r.str("steps") != "[]" || r.body["fixed"] != nil {
		t.Fatalf("\"@debian\" keeps a machine where it is: %d %v", r.code, r.body)
	}
	if r = plan(map[string]any{"image_id": img1}); r.code != 200 || r.str("steps") != "[]" || r.body["fixed"] != nil {
		t.Fatalf("the retired image it names already: %d %v", r.code, r.body)
	}
	if r = plan(map[string]any{"image_id": img3}); r.code != 200 || r.str("fixed", "0", "field") != "/image_id" {
		t.Fatalf("another image is set at its birth: %d %v", r.code, r.body)
	}
	if r = get(hand); word(r) != "" || r.str("state") != "ready" {
		t.Fatalf("the image baked by hand is never touched: %v", r.body)
	}

	// within its tier: three images held (the retired one too), a fourth refused
	run(sunday(1, time.November))
	if res, detail := lastRun(); res != "refused" || !strings.Contains(detail, "images.count") {
		t.Fatalf("the schedule stays within its tier, with the numbers: %s %s", res, detail)
	}
	// bob's machine gone, nothing is born from the first: deleted
	s.do("DELETE", "/v1/resources/"+bm, bob, nil)
	s.core.Wait()
	run(sunday(1, time.November).Add(time.Minute))
	if r = get(img1); r.str("state") != "deleted" {
		t.Fatalf("retired and named by nothing: deleted: %v", r.body)
	}

	// a mirror down one Sunday: the bake fails, is kept, and goes once a
	// newer one works — the brain away three Sundays after: one run
	setBuildersFail(t, s, true)
	run(sunday(8, time.November))
	bake()
	ids, words = made()
	if len(ids) != 3 || ids[2] == img3 || words[2] != "failed" {
		t.Fatalf("a failed bake: %v %v", ids, words)
	}
	failed := ids[2]
	if u, _ := get(failed).body["usage"].(map[string]any); len(u) != 0 {
		t.Fatalf("a failed bake holds nothing, as a create that did not happen: %v", u)
	}
	run(sunday(8, time.November).Add(time.Minute))
	if r = get(failed); r.str("state") != "ready" {
		t.Fatalf("a failed bake with no newer usable one stays: %v", r.body)
	}
	setBuildersFail(t, s, false)
	run(sunday(29, time.November).Add(5 * time.Minute))
	ids, _ = made()
	if len(ids) != 4 {
		t.Fatalf("three Sundays missed: one run: %v", ids)
	}
	img5 := ids[3]
	bake()
	run(sunday(29, time.November).Add(6 * time.Minute))
	run(sunday(29, time.November).Add(7 * time.Minute)) // the retired one, deleted
	if r = get(failed); r.str("state") != "deleted" {
		t.Fatalf("a newer one works: the failed one goes: %v", r.body)
	}
	if r = get(img2); r.str("state") != "deleted" {
		t.Fatalf("two newer usable, nothing born from it: the second goes: %v", r.body)
	}
	if ids, words = made(); len(ids) != 2 || ids[0] != img3 || ids[1] != img5 || words[0] != "" || words[1] != "" {
		t.Fatalf("the two newest usable, kept: %v %v", ids, words)
	}

	// a bad new image retired by hand: the latest is the one before
	if r = s.do("POST", "/v1/resources/"+img5+"/actions/retire", root, map[string]any{}); r.code != 202 {
		t.Fatalf("%d %v", r.code, r.body)
	}
	s.core.Wait()
	if r = machine(bob, "@debian"); r.code != 202 {
		t.Fatalf("%d %v", r.code, r.body)
	}
	s.core.Wait()
	if got := get(r.str("resource", "id")).str("spec", "image_id"); got != img3 {
		t.Fatalf("the newest retired by hand, the latest is the one before: %s", got)
	}
	if r = get(hand); word(r) != "" || r.str("state") != "ready" {
		t.Fatalf("the image baked by hand, never touched: %v", r.body)
	}
}

// word is why an image may not be named, "" when it may.
func word(r reply) string { w, _ := r.body["unusable"].(string); return w }

// setBuildersFail writes whether every builder fails in the fake engine,
// keeping the rest of its file as it is.
func setBuildersFail(t *testing.T, s *stack, on bool) {
	t.Helper()
	path := s.cfg.Zones[0].Endpoint
	st := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &st); err != nil {
			t.Fatal(err)
		}
	}
	st["builders_fail"] = on
	b, _ := json.Marshal(st)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}
