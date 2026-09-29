package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tomblancdev/hangar/driver"
	"github.com/tomblancdev/hangar/internal/core"
	"github.com/tomblancdev/hangar/internal/identity"
)

// A zone of 10 GB: 2 always the host's own, 5 kept for a priority guest
// (4100) while it runs — so 3 GB can be promised and 5 lent. And a zone that
// sleeps, woken by a webhook.
const roomConfig = `
data_dir: %[1]s
identity:
  oidc: {issuer: %[2]s, audience: hangar}
tiers:
  - name: hooks
    groups: [hooks]
    room: true
    zones: [t]
    limits: {}
  - name: users
    groups: [users]
    zones: [t, w]
    limits: {"machines.*": unlimited}
zones:
  - name: t
    driver: fake
    endpoint: "%[1]s/zone-t.json"
    room:
      memory_gb: 10
      grace: 1s
      reservations:
        - {name: host, memory_gb: 2}
        - {name: priority, memory_gb: 5, while_running: "4100"}
  - name: w
    driver: fake
    endpoint: "%[1]s/zone-w.json"
    wake:
      url: WAKE_URL
      body: '{"wait": true}'
      headers: {Authorization: {env: HANGAR_TEST_WAKE}}
      timeout: 5s
plugins:
  - name: machines
    path: %[3]s
    args: [hangar-test-plugin, machines]
    zones: [t, w]
    settings:
      images:
        debian-13: {vm: debian-13, container: "store:vztmpl/debian-13.tar.zst"}
reconcile:
  every: 1h
`

// engineState is the fake engine's file, as tests edit it.
type engineState struct {
	Seq       int                        `json:"seq"`
	Guests    map[string]*driver.Guest   `json:"guests"`
	Specs     map[string]json.RawMessage `json:"specs,omitempty"`
	Watched   map[string]bool            `json:"watched,omitempty"`
	NodesDown map[string]bool            `json:"nodes_down,omitempty"`
	Asleep    bool                       `json:"asleep,omitempty"`
	WatchFail bool                       `json:"watch_fails,omitempty"`
}

func readEngine(t *testing.T, path string) engineState {
	t.Helper()
	var st engineState
	b, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(b, &st)
	}
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if st.Guests == nil {
		st.Guests = map[string]*driver.Guest{}
	}
	return st
}

func editEngine(t *testing.T, path string, f func(*engineState)) {
	t.Helper()
	st := readEngine(t, path)
	f(&st)
	b, _ := json.Marshal(st)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func newRoomStack(t *testing.T, wakeURL string) *stack {
	t.Helper()
	return newStackWith(t, strings.ReplaceAll(roomConfig, "WAKE_URL", wakeURL), "machines")
}

func (s *stack) roomToken(subject string, groups ...string) string {
	secret, _, err := identity.Mint(context.Background(), s.store, subject, "hook", groups, []string{identity.ScopeRoom}, time.Hour, 24*time.Hour)
	if err != nil {
		s.t.Fatal(err)
	}
	return secret
}

// The pools refuse with the arithmetic; a claim holds what the zone lends —
// spot stopped, a floor shrunk, CPU capped — before it answers; a release
// gives it back, and brings back the spot machines that asked to be.
func TestTheRoomIsHeldAndGivenBack(t *testing.T) {
	s := newRoomStack(t, "http://192.0.2.1/unused")
	alice, hook := s.token("alice", "users"), s.roomToken("hook-t", "hooks")
	engine := s.engine[:strings.LastIndex(s.engine, "/")] + "/zone-t.json"
	machine := func(spec map[string]any) reply {
		t.Helper()
		spec["image"] = "debian-13"
		return s.do("POST", "/v1/resources", alice, map[string]any{"type": "machine", "zone": "t", "spec": spec})
	}
	made := func(r reply) string {
		t.Helper()
		if r.code != 202 || s.done(alice, r.str("operation", "id")).str("state") != "succeeded" {
			t.Fatalf("%d %v", r.code, r.body)
		}
		return r.str("resource", "id")
	}
	g := made(machine(map[string]any{"kind": "container", "class": "guaranteed", "cores": 2, "memory_gb": 1, "cores_beside": 1}))
	gs := made(machine(map[string]any{"kind": "container", "class": "guaranteed+spot", "cores": 2, "memory_gb": 3, "floor_gb": 1, "cores_beside": 1}))
	sp := made(machine(map[string]any{"type": "t3.small"}))
	once := made(machine(map[string]any{"type": "t3.micro", "resume": false}))

	// the arithmetic, both pools
	if r := machine(map[string]any{"kind": "container", "class": "guaranteed", "cores": 1, "memory_gb": 2}); r.code != 409 ||
		!strings.Contains(r.str("detail"), "zone t's guaranteed pool holds 3 GB; 2 GB booked; this asks for 2 GB more — ask for spot, or less") ||
		r.str("room", "pool") != "guaranteed" {
		t.Fatalf("booked beyond the pool: %d %v", r.code, r.body)
	}
	if r := machine(map[string]any{"type": "t3.micro"}); r.code != 409 ||
		!strings.Contains(r.str("detail"), "spot pool holds 5 GB; 5 GB in use; this asks for 1 GB more") {
		t.Fatalf("lent beyond the pool: %d %v", r.code, r.body)
	}
	if r := machine(map[string]any{"kind": "container", "class": "guaranteed+spot", "cores": 1, "memory_gb": 1, "floor_gb": 1}); r.code != 422 ||
		!strings.Contains(r.str("detail"), "floor_gb (1) stays under memory_gb (1)") {
		t.Fatalf("a floor as big as the machine: %d %v", r.code, r.body)
	}
	if r := machine(map[string]any{"type": "t3.micro", "cores_beside": 1}); r.code != 422 || !strings.Contains(r.str("detail"), "spot machine stops") {
		t.Fatalf("a cap on a spot machine: %d %v", r.code, r.body)
	}

	// who may claim: a hook's tier, with a room token — which reads nothing
	if r := s.do("POST", "/v1/zones/t/claim", alice, map[string]any{"guest": "4100"}); r.code != 403 || !strings.Contains(r.str("detail"), "tier users claims no room") {
		t.Fatalf("alice claimed: %d %v", r.code, r.body)
	}
	if r := s.do("GET", "/v1/zones", hook, nil); r.code != 403 {
		t.Fatalf("a room token read: %d %v", r.code, r.body)
	}
	if r := s.do("POST", "/v1/zones/t/claim", hook, map[string]any{"guest": "4001"}); r.code != 404 || !strings.Contains(r.str("detail"), "keeps no room for guest 4001") {
		t.Fatalf("a guest with no reservation: %d %v", r.code, r.body)
	}

	// the priority guest's hook, before it starts
	r := s.do("POST", "/v1/zones/t/claim", hook, map[string]any{"guest": "4100"})
	if r.code != 200 || r.str("reservation") != "priority" || r.str("held_by") != "priority" || len(r.body["resources"].([]any)) != 4 {
		t.Fatalf("the claim: %d %v", r.code, r.body)
	}
	st := readEngine(t, engine)
	for id, want := range map[string]driver.Guest{
		g:    {Running: true, MemoryMB: 1024, CPULimit: 1},
		gs:   {Running: true, MemoryMB: 1024, CPULimit: 1},
		sp:   {Running: false, MemoryMB: 2048},
		once: {Running: false, MemoryMB: 1024},
	} {
		got := st.Guests[id]
		if got.Running != want.Running || got.MemoryMB != want.MemoryMB || got.CPULimit != want.CPULimit || !slices.Equal(got.Holds, []string{"4100"}) {
			t.Errorf("held, %s is %+v", id, got)
		}
	}
	if got := s.do("GET", "/v1/resources/"+once, alice, nil); got.str("spec", "running") != "false" || got.str("hold") != "4100" {
		t.Fatalf("stopped for good: %v", got.body)
	}
	z := s.do("GET", "/v1/zones", alice, nil)
	if z.str("zones", "0", "room", "held_by") != "priority" || z.str("zones", "0", "room", "spot_used_mb") != "0" ||
		z.str("zones", "0", "room", "booked_mb") != "2048" || z.str("zones", "0", "room", "reservations", "1", "claim", "by") != "hook-t" {
		t.Fatalf("the zone: %v", z.body)
	}

	// while held: nothing borrows; a floor is born on its floor
	if r := s.do("POST", "/v1/resources/"+sp+"/actions/start", alice, nil); r.code != 409 || r.str("room", "held_for") != "priority" {
		t.Fatalf("a spot start while held: %d %v", r.code, r.body)
	}
	if r := machine(map[string]any{"type": "t3.micro"}); r.code != 409 || !strings.Contains(r.str("detail"), "held for priority (while guest 4100 runs)") {
		t.Fatalf("a spot create while held: %d %v", r.code, r.body)
	}
	late := made(machine(map[string]any{"kind": "container", "class": "guaranteed+spot", "cores": 1, "memory_gb": 2, "floor_gb": 1}))
	if got := readEngine(t, engine).Guests[late]; got.MemoryMB != 1024 || !got.Running || !slices.Equal(got.Holds, []string{"4100"}) {
		t.Fatalf("born while held: %+v", got)
	}

	// after it stopped: given back
	r = s.do("POST", "/v1/zones/t/release", hook, map[string]any{"guest": "4100"})
	if r.code != 200 || r.str("held_by") != "<nil>" {
		t.Fatalf("the release: %d %v", r.code, r.body)
	}
	st = readEngine(t, engine)
	for id, want := range map[string]driver.Guest{
		g:    {Running: true, MemoryMB: 1024},
		gs:   {Running: true, MemoryMB: 3072},
		sp:   {Running: true, MemoryMB: 2048},
		once: {Running: false, MemoryMB: 1024}, // resume: false
		late: {Running: true, MemoryMB: 2048},
	} {
		got := st.Guests[id]
		if got.Running != want.Running || got.MemoryMB != want.MemoryMB || got.CPULimit != 0 || len(got.Holds) != 0 {
			t.Errorf("released, %s is %+v", id, got)
		}
	}
	if !strings.Contains(s.logs.String(), `"result":"held"`) || !strings.Contains(s.logs.String(), `"result":"released"`) {
		t.Error("no held/released lines in the audit")
	}

	// a container that holds more than its floor: shrunk as far as it
	// allows, and the rest said
	editEngine(t, engine, func(st *engineState) { st.Guests[gs].MemoryUsedMB = 1500 })
	r = s.do("POST", "/v1/zones/t/claim", hook, map[string]any{"guest": "4100"})
	found := false
	for _, x := range r.body["resources"].([]any) {
		m := x.(map[string]any)
		if m["resource"] == gs {
			found = m["result"] == "drifted" && strings.Contains(m["detail"].(string), "it holds 1500 MB, above its floor of 1024 MB: 540 MB could not be given back")
		}
	}
	if !found || readEngine(t, engine).Guests[gs].MemoryMB != 1564 {
		t.Fatalf("a floor it cannot reach: %v", r.body)
	}
	editEngine(t, engine, func(st *engineState) { st.Guests[gs].MemoryUsedMB = 0 })
	_ = s.do("POST", "/v1/zones/t/release", hook, map[string]any{"guest": "4100"})
}

// A claim whose release never comes (the guest's node lost its power) ends
// once its grace is past and its guest is not seen running; a hold the
// engine's node placed alone (the brain was not there) is adopted, then ends
// the same way.
func TestAClaimEndsAndANodesHoldIsAdopted(t *testing.T) {
	s := newRoomStack(t, "http://192.0.2.1/unused")
	alice, hook := s.token("alice", "users"), s.roomToken("hook-t", "hooks")
	engine := s.engine[:strings.LastIndex(s.engine, "/")] + "/zone-t.json"
	ctx := context.Background()
	r := s.do("POST", "/v1/resources", alice, map[string]any{"type": "machine", "zone": "t", "spec": map[string]any{"image": "debian-13", "type": "t3.small"}})
	if r.code != 202 || s.done(alice, r.str("operation", "id")).str("state") != "succeeded" {
		t.Fatalf("%d %v", r.code, r.body)
	}
	sp := r.str("resource", "id")

	// claimed, its guest read not running yet (it is starting), then its
	// power unreadable past the grace: a hiccup of the engine's API is not a
	// guest gone — the claim stands
	if r := s.do("POST", "/v1/zones/t/claim", hook, map[string]any{"guest": "4100"}); r.code != 200 {
		t.Fatalf("%d %v", r.code, r.body)
	}
	s.core.ReconcileOnce(ctx)
	editEngine(t, engine, func(st *engineState) { st.WatchFail = true })
	time.Sleep(1100 * time.Millisecond)
	s.core.ReconcileOnce(ctx)
	if got := readEngine(t, engine).Guests[sp]; got.Running || len(got.Holds) == 0 {
		t.Fatalf("an unreadable condition ended the claim: %+v", got)
	}
	editEngine(t, engine, func(st *engineState) { st.WatchFail = false })

	// claimed; the guest seen running keeps it past its grace
	editEngine(t, engine, func(st *engineState) { st.Watched = map[string]bool{"4100": true} })
	if r := s.do("POST", "/v1/zones/t/claim", hook, map[string]any{"guest": "4100"}); r.code != 200 {
		t.Fatalf("%d %v", r.code, r.body)
	}
	time.Sleep(1100 * time.Millisecond)
	s.core.ReconcileOnce(ctx)
	if got := readEngine(t, engine).Guests[sp]; got.Running {
		t.Fatalf("a claim whose guest runs ended at its grace: %+v", got)
	}
	// its guest gone without a word: the claim ends, the room comes back
	editEngine(t, engine, func(st *engineState) { st.Watched = map[string]bool{"4100": false} })
	s.core.ReconcileOnce(ctx)
	if got := readEngine(t, engine).Guests[sp]; !got.Running || len(got.Holds) != 0 {
		t.Fatalf("a stale claim still holds: %+v", got)
	}
	if !strings.Contains(s.logs.String(), `"result":"claim-expired"`) {
		t.Error("no claim-expired line")
	}

	// the node held it on its own — tag first, then stopped — while the
	// brain could not be reached, and its guest is not running yet
	editEngine(t, engine, func(st *engineState) {
		st.Guests[sp].Holds, st.Guests[sp].Running = []string{"4100"}, false
	})
	s.core.ReconcileOnce(ctx)
	if got := s.do("GET", "/v1/resources/"+sp, alice, nil); got.str("hold") != "4100" {
		t.Fatalf("the node's hold was not adopted: %v", got.body)
	}
	if got := readEngine(t, engine).Guests[sp]; got.Running {
		t.Fatalf("adopted, and started anyway: %+v", got)
	}
	if !strings.Contains(s.logs.String(), `"result":"claim-adopted"`) {
		t.Error("no claim-adopted line")
	}
	time.Sleep(1100 * time.Millisecond)
	s.core.ReconcileOnce(ctx)
	if got := readEngine(t, engine).Guests[sp]; !got.Running || len(got.Holds) != 0 {
		t.Fatalf("the adopted hold never ended: %+v", got)
	}
}

// A zone that sleeps is woken by its webhook before something starts in it —
// never by a reconcile — with the credential read from the core's
// environment.
func TestAZoneIsWokenBeforeAStart(t *testing.T) {
	core.WakePoll = 20 * time.Millisecond
	t.Setenv("HANGAR_TEST_WAKE", "Bearer w4ke")
	var calls atomic.Int32
	var engine string
	wake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer w4ke" || r.Method != "POST" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		calls.Add(1)
		editEngine(t, engine, func(st *engineState) { st.Asleep = false })
	}))
	t.Cleanup(wake.Close)
	s := newRoomStack(t, wake.URL)
	engine = s.engine[:strings.LastIndex(s.engine, "/")] + "/zone-w.json"
	editEngine(t, engine, func(st *engineState) { st.Asleep = true })
	alice := s.token("alice", "users")

	r := s.do("POST", "/v1/resources", alice, map[string]any{"type": "machine", "zone": "w", "spec": map[string]any{"image": "debian-13", "kind": "container"}})
	if r.code != 202 || s.done(alice, r.str("operation", "id")).str("state") != "succeeded" || calls.Load() != 1 {
		t.Fatalf("the create in a sleeping zone: %d %v, %d wake calls\n%s", r.code, r.body, calls.Load(), s.logs.String())
	}
	id := r.str("resource", "id")
	if r := s.do("POST", "/v1/resources/"+id+"/actions/stop", alice, nil); r.code != 202 || s.done(alice, r.str("operation", "id")).str("state") != "succeeded" {
		t.Fatalf("stop: %v", r.body)
	}
	editEngine(t, engine, func(st *engineState) { st.Asleep = true })
	s.core.ReconcileOnce(context.Background())
	if calls.Load() != 1 {
		t.Fatal("a reconcile woke the zone")
	}
	if got := s.do("GET", "/v1/resources/"+id, alice, nil); got.str("state") != "ready" {
		t.Fatalf("a machine of a sleeping zone was judged: %v", got.body)
	}
	if z := s.do("GET", "/v1/zones", alice, nil); z.str("zones", "1", "awake") != "false" {
		t.Fatalf("the sleeping zone reads %v", z.body)
	}
	if r := s.do("POST", "/v1/resources/"+id+"/actions/start", alice, nil); r.code != 202 || s.done(alice, r.str("operation", "id")).str("state") != "succeeded" || calls.Load() != 2 {
		t.Fatalf("a start in a sleeping zone: %v, %d calls", r.body, calls.Load())
	}
	if !readEngine(t, engine).Guests[id].Running {
		t.Fatal("woken, and not started")
	}
	// a wake that fails fails the start, and says why
	wake.Close()
	if r := s.do("POST", "/v1/resources/"+id+"/actions/stop", alice, nil); r.code != 202 || s.done(alice, r.str("operation", "id")).str("state") != "succeeded" {
		t.Fatalf("stop: %v", r.body)
	}
	editEngine(t, engine, func(st *engineState) { st.Asleep = true })
	r = s.do("POST", "/v1/resources/"+id+"/actions/start", alice, nil)
	if op := s.done(alice, r.str("operation", "id")); op.str("state") != "failed" || !strings.Contains(op.str("error"), "zone w's wake did not answer") {
		t.Fatalf("a start whose wake failed: %v", op.body)
	}
}

// A pass that comes while a release is under way waits for it: read on the
// bench, a survey mid-release met the release's own tags not yet taken off,
// took them for a node acting alone, and held everything again for a grace.
func TestAPassWaitsForARelease(t *testing.T) {
	s := newRoomStack(t, "http://192.0.2.1/unused")
	alice, hook := s.token("alice", "users"), s.roomToken("hook-t", "hooks")
	engine := s.engine[:strings.LastIndex(s.engine, "/")] + "/zone-t.json"
	var ids []string
	for range 3 {
		r := s.do("POST", "/v1/resources", alice, map[string]any{"type": "machine", "zone": "t", "spec": map[string]any{"image": "debian-13", "kind": "container", "type": "t3.micro"}})
		if r.code != 202 || s.done(alice, r.str("operation", "id")).str("state") != "succeeded" {
			t.Fatalf("%d %v", r.code, r.body)
		}
		ids = append(ids, r.str("resource", "id"))
	}
	if r := s.do("POST", "/v1/zones/t/claim", hook, map[string]any{"guest": "4100"}); r.code != 200 {
		t.Fatalf("%d %v", r.code, r.body)
	}
	// the release paused between its holds and the engine: a pass fired there
	pass := make(chan struct{})
	core.BetweenHoldsAndEngine = func() {
		go func() { s.core.ReconcileOnce(context.Background()); close(pass) }()
		time.Sleep(300 * time.Millisecond)
	}
	t.Cleanup(func() { core.BetweenHoldsAndEngine = nil })
	if r := s.do("POST", "/v1/zones/t/release", hook, map[string]any{"guest": "4100"}); r.code != 200 {
		t.Fatalf("%d %v", r.code, r.body)
	}
	core.BetweenHoldsAndEngine = nil
	<-pass
	if strings.Contains(s.logs.String(), `"result":"claim-adopted"`) {
		t.Fatal("a pass mid-release adopted the release's own tags")
	}
	for _, id := range ids {
		if got := readEngine(t, engine).Guests[id]; !got.Running || len(got.Holds) != 0 {
			t.Errorf("after the release, %s is %+v", id, got)
		}
	}
}
