package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tomblancdev/hangar/internal/testoidc"
)

// engineOf reads a fake engine's file: its guests and volumes, by id.
func engineOf(t *testing.T, s *stack) (guests, volumes map[string]map[string]any) {
	t.Helper()
	var eng struct {
		Guests  map[string]map[string]any `json:"guests"`
		Volumes map[string]map[string]any `json:"volumes"`
	}
	if err := json.Unmarshal([]byte(s.engineFile()), &eng); err != nil {
		t.Fatal(err)
	}
	return eng.Guests, eng.Volumes
}

// until waits for something the brain does behind an answer.
func until(t *testing.T, what string, f func() bool) {
	t.Helper()
	for range 100 {
		if f() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("never: %s", what)
}

// A resource has a name and a description of its owner's, whatever its type:
// served with what a person reads (its owner by name, the word it wears, its
// type's sentence), one thing among its owner's of that type, written where
// the engine shows it — and renamed without anything else of it moving.
func TestAResourceIsCalledByItsName(t *testing.T) {
	s := newStack(t)
	// alice signs in at the provider: the brain learns what she is called
	alice := s.iss.Token(t, testoidc.Claims{Subject: "sub-alice", Audience: "hangar", Groups: []string{"users"}, Name: "alice"})
	bob := s.token("sub-bob", "users")
	root := s.token("root", "ops")
	box := func(who, name string) reply {
		t.Helper()
		return s.create(who, map[string]any{"type": "box", "zone": "z", "name": name, "description": "the build box", "spec": map[string]any{"cores": 2}})
	}

	made := box(alice, "dev")
	if made.code != 202 {
		t.Fatalf("%d %v", made.code, made.body)
	}
	id := made.str("resource", "id")
	r := s.do("GET", "/v1/resources/"+id, alice, nil)
	for path, want := range map[string]string{
		"name": "dev", "description": "the build box", "owner": "sub-alice", "owner_name": "alice",
		"status": "running", "light": "on", "summary": "container · 2 cores · 1 GB",
	} {
		if got := r.str(path); got != want {
			t.Errorf("%s is %q, want %q", path, got, want)
		}
	}
	if _, in := r.body["spec"].(map[string]any)["name"]; in {
		t.Errorf("its spec names it: %v", r.body["spec"])
	}
	guests, _ := engineOf(t, s)
	if g := guests[id]; g["name"] != "dev" || g["label"] != "dev · box of alice — the build box" {
		t.Fatalf("on the engine: %v", g)
	}

	// a name is one thing among its owner's of the type; another owner's is theirs
	if r := box(alice, "dev"); r.code != 409 || r.str("kind") != "conflict" || !strings.Contains(r.str("detail"), id) {
		t.Fatalf("a second dev: %d %v", r.code, r.body)
	}
	theirs := box(bob, "dev")
	if theirs.code != 202 {
		t.Fatalf("bob's dev: %d %v", theirs.code, theirs.body)
	}
	// bob never signed in at the provider: his is shown by subject, and says so on the engine
	if r := s.do("GET", "/v1/resources/"+theirs.str("resource", "id"), bob, nil); r.body["owner_name"] != nil {
		t.Fatalf("a token taught a name: %v", r.body)
	}
	for _, bad := range []string{"Dev", "-dev", "dev-", "my dev", "box-0123456789abcdef0", strings.Repeat("a", 64)} {
		if r := box(alice, bad); r.code != 422 {
			t.Errorf("the name %q: %d %v", bad, r.code, r.body)
		}
	}
	if r := s.do("POST", "/v1/resources", alice, map[string]any{"type": "box", "zone": "z", "description": "two\nlines"}); r.code != 422 {
		t.Fatalf("a description of two lines: %d %v", r.code, r.body)
	}

	// found by name; an operator finds a person's by the name they sign in under
	if l := s.do("GET", "/v1/resources?type=box&name=dev", alice, nil); len(l.body["resources"].([]any)) != 1 || l.str("resources", "0", "id") != id {
		t.Fatalf("by name: %v", l.body)
	}
	if l := s.do("GET", "/v1/resources?owner=alice", root, nil); len(l.body["resources"].([]any)) != 1 || l.str("resources", "0", "id") != id {
		t.Fatalf("an operator, by the owner's name: %v", l.body)
	}
	if l := s.do("GET", "/v1/operations?resource="+id, alice, nil); l.str("operations", "0", "resource_name") != "dev" || l.str("operations", "0", "owner_name") != "alice" {
		t.Fatalf("its operations: %v", l.body)
	}

	// renamed: its id stays, the engine's own screen follows, the host it was born as does not move
	patch := func(who, id string, body map[string]any) reply {
		t.Helper()
		return s.do("PATCH", "/v1/resources/"+id, who, body)
	}
	if r := patch(alice, id, map[string]any{"name": "build", "description": ""}); r.code != 200 || r.str("name") != "build" || r.body["description"] != nil || r.str("id") != id {
		t.Fatalf("renamed: %d %v", r.code, r.body)
	}
	until(t, "the engine says build", func() bool {
		guests, _ := engineOf(t, s)
		return guests[id]["label"] == "build · box of alice" && guests[id]["name"] == "dev"
	})
	if !strings.Contains(s.logs.String(), `"was":"dev"`) {
		t.Error("a rename writes what it was called in the audit")
	}
	// the name it left is free; one that is held is refused; nothing asked is refused; only its owner renames
	again := box(alice, "dev")
	if again.code != 202 {
		t.Fatalf("dev again: %d %v", again.code, again.body)
	}
	if r := patch(alice, id, map[string]any{"name": "dev"}); r.code != 409 || !strings.Contains(r.str("detail"), again.str("resource", "id")) {
		t.Fatalf("renamed to a name that is held: %d %v", r.code, r.body)
	}
	if r := patch(alice, id, map[string]any{"name": "build"}); r.code != 200 {
		t.Fatalf("renamed to its own name: %d %v", r.code, r.body)
	}
	if r := patch(alice, id, map[string]any{}); r.code != 422 {
		t.Fatalf("nothing asked: %d %v", r.code, r.body)
	}
	if r := patch(alice, id, map[string]any{"name": "Build"}); r.code != 422 {
		t.Fatalf("not a name: %d %v", r.code, r.body)
	}
	if r := patch(bob, id, map[string]any{"name": "mine"}); r.code != 404 {
		t.Fatalf("bob renames alice's: %d %v", r.code, r.body)
	}
	if r := patch(s.readOnly("sub-alice", "users"), id, map[string]any{"name": "ro"}); r.code != 403 {
		t.Fatalf("a read-only token renames: %d %v", r.code, r.body)
	}
	// unnamed: it is shown by its id, and several may be
	if r := patch(alice, id, map[string]any{"name": ""}); r.code != 200 || r.body["name"] != nil {
		t.Fatalf("unnamed: %d %v", r.code, r.body)
	}
	until(t, "the engine drops the name", func() bool {
		guests, _ := engineOf(t, s)
		return guests[id]["label"] == "box of alice"
	})
	// a deleted one gives its name back
	gone := again.str("resource", "id")
	if d := s.do("DELETE", "/v1/resources/"+gone, alice, nil); d.code != 202 || s.done(alice, d.str("operation", "id")).str("state") != "succeeded" {
		t.Fatalf("delete: %v", d.body)
	}
	if r := patch(alice, gone, map[string]any{"name": "ghost"}); r.code != 404 {
		t.Fatalf("a deleted one renamed: %d %v", r.code, r.body)
	}
	if r := patch(alice, id, map[string]any{"name": "dev"}); r.code != 200 {
		t.Fatalf("the name a deleted one held: %d %v", r.code, r.body)
	}
}

// A name goes where an id goes: a spec and an action's params take what
// their owner calls a resource, the registry keeps the id, and a resource
// reads with the names of what it names.
func TestANameGoesWhereAnIdGoes(t *testing.T) {
	s := newStackWith(t, volumesConfig, "machines", "volumes")
	s.engine = strings.TrimSuffix(s.engine, "zone-z.json") + "zone-v.json"
	alice := s.token("alice", "users")
	bob := s.token("bob", "users")
	ok := func(r reply) reply {
		t.Helper()
		if r.code != 202 {
			t.Fatalf("%d %v", r.code, r.body)
		}
		if op := s.done(alice, r.str("operation", "id")); op.str("state") != "succeeded" {
			t.Fatalf("%v", op.body)
		}
		return r
	}
	machine := func(who, name string) reply {
		return s.do("POST", "/v1/resources", who, map[string]any{"type": "machine", "zone": "v", "name": name,
			"spec": map[string]any{"kind": "container", "image": "debian-13"}})
	}
	dev := ok(machine(alice, "dev")).str("resource", "id")
	if r := s.do("GET", "/v1/resources/"+dev, alice, nil); r.str("summary") != "container · 2 cores · 1 GB · spot · debian-13 · 203.0.113.1 · walled" || r.str("status") != "running" {
		t.Fatalf("a machine reads: %q, %q", r.str("summary"), r.str("status"))
	}
	guests, _ := engineOf(t, s)
	if g := guests[dev]; g["name"] != "dev" || g["label"] != "dev · machine" {
		t.Fatalf("on the engine: %v", g)
	}

	// a volume names its machine as its owner calls it
	home := ok(s.do("POST", "/v1/resources", alice, map[string]any{"type": "volume", "zone": "v", "name": "home",
		"spec": map[string]any{"size_gb": 1, "machine": "dev", "mount": "/home", "backup": true}})).str("resource", "id")
	r := s.do("GET", "/v1/resources/"+home, alice, nil)
	if r.str("spec", "machine") != dev {
		t.Fatalf("the registry keeps the id: %v", r.body["spec"])
	}
	if r.str("summary") != "1 GB · on dev at /home · backed up" || r.str("status") != "attached" || r.str("names", dev) != "dev" {
		t.Fatalf("a volume reads: %q, %q, %v", r.str("summary"), r.str("status"), r.body["names"])
	}
	if !strings.Contains(s.logs.String(), `dev=`+dev) {
		t.Error("the audit says what a name stood for")
	}
	_, volumes := engineOf(t, s)
	if volumes[home]["label"] != "home" {
		t.Fatalf("on the engine: %v", volumes[home])
	}

	// an action's params too; a parked one says so
	scratch := ok(s.do("POST", "/v1/resources", alice, map[string]any{"type": "volume", "zone": "v", "name": "scratch",
		"spec": map[string]any{"size_gb": 1, "content": "filesystem"}})).str("resource", "id")
	if r := s.do("GET", "/v1/resources/"+scratch, alice, nil); r.str("summary") != "1 GB" || r.str("status") != "parked" || r.str("light") != "off" {
		t.Fatalf("parked: %q %q", r.str("summary"), r.str("status"))
	}
	ok(s.do("POST", "/v1/resources/"+dev+"/actions/stop", alice, nil))
	ok(s.do("POST", "/v1/resources/"+scratch+"/actions/attach", alice, map[string]any{"params": map[string]any{"machine": "dev", "mount": "/scratch"}}))
	if r := s.do("GET", "/v1/resources/"+scratch, alice, nil); r.str("spec", "machine") != dev || r.str("summary") != "1 GB · on dev at /scratch" {
		t.Fatalf("attached by name: %v", r.body)
	}
	if r := s.do("GET", "/v1/resources/"+dev, alice, nil); r.str("status") != "stopped" || r.str("light") != "off" {
		t.Fatalf("a stopped machine wears %q", r.str("status"))
	}

	// a name nobody holds, one that is no name, another's: each says so, and none is found
	for name, why := range map[string]string{"nope": "you have no machine named nope", "No Name": "neither an id (m-…) nor a name"} {
		r := s.do("POST", "/v1/resources", alice, map[string]any{"type": "volume", "zone": "v", "spec": map[string]any{"size_gb": 1, "machine": name}})
		if r.code != 422 || !strings.Contains(r.str("detail"), why) {
			t.Errorf("machine %q: %d %v", name, r.code, r.body)
		}
	}
	ok2 := machine(bob, "theirs")
	if ok2.code != 202 || s.done(bob, ok2.str("operation", "id")).str("state") != "succeeded" {
		t.Fatalf("%v", ok2.body)
	}
	if r := s.do("POST", "/v1/resources", alice, map[string]any{"type": "volume", "zone": "v", "spec": map[string]any{"size_gb": 1, "machine": "theirs"}}); r.code != 422 || !strings.Contains(r.str("detail"), "you have no machine named theirs") {
		t.Fatalf("another's machine by name: %d %v", r.code, r.body)
	}

	// a rename is read at once where the name shows, and reaches the engine at the next look
	if r := s.do("PATCH", "/v1/resources/"+dev, alice, map[string]any{"name": "box"}); r.code != 200 {
		t.Fatalf("%d %v", r.code, r.body)
	}
	if r := s.do("GET", "/v1/resources/"+home, alice, nil); r.str("summary") != "1 GB · on box at /home · backed up" {
		t.Fatalf("after its machine's rename: %q", r.str("summary"))
	}
	if r := s.do("PATCH", "/v1/resources/"+home, alice, map[string]any{"name": "maison"}); r.code != 200 {
		t.Fatalf("%d %v", r.code, r.body)
	}
	until(t, "the engine follows both renames", func() bool {
		guests, volumes := engineOf(t, s)
		return guests[dev]["label"] == "box · machine" && volumes[home]["label"] == "maison"
	})
}
