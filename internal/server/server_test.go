package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tomblancdev/hangar/api"
	"github.com/tomblancdev/hangar/internal/audit"
	"github.com/tomblancdev/hangar/internal/config"
	"github.com/tomblancdev/hangar/internal/core"
	"github.com/tomblancdev/hangar/internal/identity"
	"github.com/tomblancdev/hangar/internal/limits"
	"github.com/tomblancdev/hangar/internal/metrics"
	"github.com/tomblancdev/hangar/internal/plugins"
	"github.com/tomblancdev/hangar/internal/registry"
	"github.com/tomblancdev/hangar/internal/testoidc"
	"github.com/tomblancdev/hangar/plugins/images"
	"github.com/tomblancdev/hangar/plugins/machines"
	"github.com/tomblancdev/hangar/plugins/toy"
	"github.com/tomblancdev/hangar/plugins/volumes"
	"github.com/tomblancdev/hangar/sdk"
)

// The test binary is also the toy plugin's process: the core starts it as
// "<test> hangar-test-plugin toy", exactly as it starts "hangar plugin toy".
func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "hangar-test-plugin" {
		switch os.Args[2] {
		case "toy":
			sdk.Serve(toy.New())
		case "machines":
			sdk.Serve(machines.New())
		case "volumes":
			sdk.Serve(volumes.New())
		case "images":
			sdk.Serve(images.New())
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// ---- The stack: the whole brain, one real plugin process, a fake engine ----

type stack struct {
	t      *testing.T
	url    string
	cfg    *config.Config
	store  *registry.Store
	host   *plugins.Host
	core   *core.Core
	iss    *testoidc.Issuer
	engine string // the fake engine's file for zone "z"
	logs   *syncBuf
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

const stackConfig = `
data_dir: %[1]s
identity:
  oidc: {issuer: %[2]s, audience: hangar}
tiers:
  - name: operators
    groups: [ops]
    operator: true
    zones: ["*"]
    limits: {"*": unlimited}
  - name: users
    groups: [users]
    zones: [z]
    limits:
      toy.boxes: 2
      toy.cores: 4
      toy.memory_gb: 8
      toy.kind: [container]
zones:
  - name: z
    driver: fake
    endpoint: %[1]s/zone-z.json
    options: {capabilities: "kind.container,kind.vm,guest.tags"}
  - name: y
    driver: fake
plugins:
  - name: toy
    path: %[3]s
    args: [hangar-test-plugin, toy]
    zones: [z, y]
reconcile:
  every: 1h
`

func newStack(t *testing.T) *stack { t.Helper(); return newStackWith(t, stackConfig, "toy") }

// newStackWith runs the brain on another config (%[1]s the data directory,
// %[2]s the identity provider, %[3]s this test binary, the plugins' program).
func newStackWith(t *testing.T, cfgText string, enabled ...string) *stack {
	t.Helper()
	return newStackClock(t, cfgText, nil, enabled...)
}

// newStackClock is a stack whose schedules run on the clock now, turned by
// the test alone (ScheduleOnce), never by the brain's own ticker.
func newStackClock(t *testing.T, cfgText string, now func() time.Time, enabled ...string) *stack {
	t.Helper()
	dir, err := os.MkdirTemp("", "h") // short: the plugin's socket lives under it
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	s := &stack{t: t, iss: testoidc.New(t), engine: dir + "/zone-z.json", logs: &syncBuf{}}
	s.cfg, err = config.Parse(fmt.Appendf(nil, cfgText, dir, s.iss.URL, os.Args[0]))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewJSONHandler(s.logs, nil))
	if s.store, err = registry.Open(dir + "/hangar.db"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if s.host, err = plugins.Start(ctx, s.cfg, plugins.Options{DataDir: dir, Logs: io.Discard}, log); err != nil {
		t.Fatal(err)
	}
	if err := limits.Check(s.cfg.Tiers, s.host.Dimensions(), enabled); err != nil {
		t.Fatal(err)
	}
	if err := core.CheckSchedules(s.cfg, s.host); err != nil {
		t.Fatal(err)
	}
	m := metrics.New()
	a := audit.New(log)
	s.core = core.New(ctx, s.cfg, s.store, s.host, a, log, m)
	s.core.Retry = nil
	if now != nil {
		s.core.Now, s.core.ScheduleEvery = now, 0
	}
	srv, err := New(s.cfg, s.core, identity.New(s.cfg.Identity, s.store, nil), s.store, s.host, a, m, log, "test")
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv.Handler())
	s.url = hs.URL
	done := make(chan struct{})
	go func() { _ = s.core.Run(ctx); close(done) }()
	t.Cleanup(func() {
		hs.Close()
		cancel()
		<-done
		s.host.Close()
		_ = s.store.Close()
	})
	return s
}

// token makes an API token for a subject in some groups.
func (s *stack) token(subject string, groups ...string) string {
	secret, _, err := identity.Mint(context.Background(), s.store, subject, "test", groups, nil, time.Hour, 24*time.Hour)
	if err != nil {
		s.t.Fatal(err)
	}
	return secret
}

func (s *stack) readOnly(subject string, groups ...string) string {
	secret, _, err := identity.Mint(context.Background(), s.store, subject, "test", groups, []string{identity.ScopeRead}, time.Hour, 24*time.Hour)
	if err != nil {
		s.t.Fatal(err)
	}
	return secret
}

type reply struct {
	code int
	body map[string]any
	hdr  http.Header
}

func (r reply) str(path ...string) string {
	var v any = r.body
	for _, p := range path {
		switch m := v.(type) {
		case map[string]any:
			v = m[p]
		case []any:
			var i int
			_, _ = fmt.Sscan(p, &i)
			if i < len(m) {
				v = m[i]
			} else {
				v = nil
			}
		}
	}
	return fmt.Sprint(v)
}

func (s *stack) do(method, path, bearer string, body any) reply {
	s.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, s.url+path, rd)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := reply{code: resp.StatusCode, hdr: resp.Header, body: map[string]any{}}
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &out.body)
	return out
}

// done waits for an operation and returns it.
func (s *stack) done(bearer, op string) reply {
	s.t.Helper()
	r := s.do("GET", "/v1/operations/"+op+"?wait=20", bearer, nil)
	if r.str("state") == "running" {
		s.t.Fatalf("operation %s still running", op)
	}
	return r
}

func (s *stack) create(bearer string, body map[string]any) reply {
	s.t.Helper()
	r := s.do("POST", "/v1/resources", bearer, body)
	if r.code == 202 {
		if op := s.done(bearer, r.str("operation", "id")); op.str("state") != "succeeded" {
			s.t.Fatalf("create failed: %v", op.body)
		}
	}
	return r
}

func (s *stack) engineFile() string {
	b, _ := os.ReadFile(s.engine)
	return string(b)
}

// ---- The contract ----------------------------------------------------------

func TestTheRoutesAreTheContract(t *testing.T) {
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(api.OpenAPI, &doc); err != nil {
		t.Fatal(err)
	}
	var spec []string
	for path, ops := range doc.Paths {
		for method := range ops {
			if method != "parameters" {
				spec = append(spec, strings.ToUpper(method)+" "+path)
			}
		}
	}
	var served []string
	for _, r := range Routes() {
		served = append(served, strings.Replace(r, "/{$}", "/", 1))
	}
	slices.Sort(spec)
	slices.Sort(served)
	for _, r := range served {
		if !slices.Contains(spec, r) {
			t.Errorf("served but not in the contract: %s", r)
		}
	}
	for _, r := range spec {
		if !slices.Contains(served, r) {
			t.Errorf("in the contract but not served: %s", r)
		}
	}
	if _, err := SpecJSON("x"); err != nil {
		t.Fatal(err)
	}
}

// ---- The ask, end to end ----------------------------------------------------

func TestTheAsk(t *testing.T) {
	s := newStack(t)
	alice := s.token("alice", "users")

	// signed in?
	r := s.do("GET", "/v1/whoami", "", nil)
	if r.code != 401 || r.str("kind") != "sign-in" || r.hdr.Get("WWW-Authenticate") == "" {
		t.Fatalf("no token: %d %v", r.code, r.body)
	}
	// in a tier?
	stranger := s.iss.Token(t, testoidc.Claims{Subject: "eve", Audience: "hangar", Groups: []string{"visitors"}})
	if r = s.do("GET", "/v1/whoami", stranger, nil); r.code != 403 || r.str("kind") != "no-tier" {
		t.Fatalf("no tier: %d %v", r.code, r.body)
	}
	if r = s.do("GET", "/v1/whoami", alice, nil); r.code != 200 || r.str("tier") != "users" || r.str("via") == "oidc" {
		t.Fatalf("whoami: %v", r.body)
	}

	// the catalogue: the box, in zone z only (y is not open to users), and
	// no suspend there (z's engine cannot suspend to disk)
	r = s.do("GET", "/v1/types", alice, nil)
	if r.str("types", "0", "name") != "box" || r.str("types", "0", "zones") != "[z]" {
		t.Fatalf("catalogue: %v", r.body)
	}
	for _, a := range r.body["types"].([]any)[0].(map[string]any)["actions"].([]any) {
		a := a.(map[string]any)
		if a["name"] == "suspend" && len(a["zones"].([]any)) != 0 {
			t.Fatal("suspend offered in a zone without guest.suspend_to_disk")
		}
	}

	// a box: accepted, carried out by the plugin's process, on the engine
	box := map[string]any{"type": "box", "zone": "z", "spec": map[string]any{"cores": 2}, "client_token": "first", "tags": map[string]string{"team": "a"}}
	r = s.create(alice, box)
	if r.code != 202 {
		t.Fatalf("create: %d %v", r.code, r.body)
	}
	id := r.str("resource", "id")
	if !strings.HasPrefix(id, "box-") || len(id) != len("box-")+17 {
		t.Fatalf("id %q", id)
	}
	got := s.do("GET", "/v1/resources/"+id, alice, nil)
	if got.str("state") != "ready" || got.str("observed", "engine_ref") == "" || got.str("usage", "toy.cores") != "2" {
		t.Fatalf("the box: %v", got.body)
	}
	if !strings.Contains(s.engineFile(), id) {
		t.Fatal("the engine has no such guest")
	}

	// a retry is not a second request; a reused token is not a new one
	if r = s.do("POST", "/v1/resources", alice, box); r.code != 200 || r.str("resource", "id") != id {
		t.Fatalf("replay: %d %v", r.code, r.body)
	}
	box["spec"] = map[string]any{"cores": 3}
	if r = s.do("POST", "/v1/resources", alice, box); r.code != 409 || r.str("kind") != "conflict" {
		t.Fatalf("token reused: %d %v", r.code, r.body)
	}

	// the schema, the zone, the choice
	if r = s.do("POST", "/v1/resources", alice, map[string]any{"type": "box", "zone": "z", "spec": map[string]any{"cores": 100}}); r.code != 422 ||
		r.str("kind") != "schema" || r.str("violations", "0", "field") != "/cores" {
		t.Fatalf("schema: %d %v", r.code, r.body)
	}
	if r = s.do("POST", "/v1/resources", alice, map[string]any{"type": "box", "zone": "y"}); r.code != 403 || r.str("kind") != "zone" {
		t.Fatalf("zone: %d %v", r.code, r.body)
	}
	if r = s.do("POST", "/v1/resources", alice, map[string]any{"type": "box", "zone": "z", "spec": map[string]any{"kind": "vm"}}); r.code != 403 ||
		r.str("refusals", "0", "reason") != "choice" {
		t.Fatalf("choice: %d %v", r.code, r.body)
	}
	if r = s.do("POST", "/v1/resources", alice, map[string]any{"type": "box", "zone": "z", "colour": "red"}); r.code != 400 {
		t.Fatalf("an unknown field is a typo: %d %v", r.code, r.body)
	}

	// the limits, with the numbers
	second := s.create(alice, map[string]any{"type": "box", "zone": "z", "spec": map[string]any{"cores": 2}}).str("resource", "id")
	r = s.do("POST", "/v1/resources", alice, map[string]any{"type": "box", "zone": "z"})
	if r.code != 403 || r.str("kind") != "limit" || r.str("detail") != "2 of 2 toy.boxes used; this asks for 1 more" {
		t.Fatalf("over the limit: %d %v", r.code, r.body)
	}
	r = s.do("POST", "/v1/resources/"+id+"/actions/resize", alice, map[string]any{"params": map[string]any{"cores": 3}})
	if r.code != 403 || r.str("detail") != "4 of 4 vCPU (toy.cores) used; this asks for 1 more" {
		t.Fatalf("resize over the limit: %d %v", r.code, r.body)
	}
	if r = s.do("POST", "/v1/resources/"+id+"/actions/suspend", alice, nil); r.code != 422 || r.str("kind") != "unavailable-here" {
		t.Fatalf("suspend: %d %v", r.code, r.body)
	}

	// actions: stop, then grow the memory inside the limit
	r = s.do("POST", "/v1/resources/"+id+"/actions/stop", alice, nil)
	if r.code != 202 || s.done(alice, r.str("operation", "id")).str("state") != "succeeded" {
		t.Fatalf("stop: %d %v", r.code, r.body)
	}
	r = s.do("POST", "/v1/resources/"+id+"/actions/resize", alice, map[string]any{"params": map[string]any{"memory_gb": 3}})
	if r.code != 202 || s.done(alice, r.str("operation", "id")).str("state") != "succeeded" {
		t.Fatalf("resize: %d %v", r.code, r.body)
	}
	got = s.do("GET", "/v1/resources/"+id, alice, nil)
	if got.str("spec", "running") != "false" || got.str("spec", "memory_gb") != "3" || got.str("observed", "memory_gb") != "3" ||
		got.str("usage", "toy.memory_gb") != "3" {
		t.Fatalf("after stop + resize: %v", got.body)
	}

	// who sees what
	bob := s.token("bob", "users")
	if r = s.do("GET", "/v1/resources/"+id, bob, nil); r.code != 404 {
		t.Fatalf("bob sees alice's box: %d", r.code)
	}
	if r = s.do("GET", "/v1/resources", bob, nil); len(r.body["resources"].([]any)) != 0 {
		t.Fatal("bob lists alice's boxes")
	}
	root := s.token("root", "ops")
	if r = s.do("GET", "/v1/resources?tag=team=a", root, nil); len(r.body["resources"].([]any)) != 1 {
		t.Fatalf("the operator's tag filter: %v", r.body)
	}

	// scopes: a read-only token cannot ask, a token cannot make tokens,
	// a person signed in can
	ro := s.readOnly("alice", "users")
	if r = s.do("POST", "/v1/resources", ro, map[string]any{"type": "box", "zone": "z"}); r.code != 403 || r.str("kind") != "scope" {
		t.Fatalf("read-only: %d %v", r.code, r.body)
	}
	if r = s.do("POST", "/v1/tokens", alice, map[string]any{"name": "x", "expires_in": 3600}); r.code != 403 {
		t.Fatalf("a token made a token: %d", r.code)
	}
	person := s.iss.Token(t, testoidc.Claims{Subject: "alice", Audience: "hangar", Groups: []string{"users"}})
	r = s.do("POST", "/v1/tokens", person, map[string]any{"name": "ci", "expires_in": 3600, "scopes": []string{"read"}})
	if r.code != 201 || !strings.HasPrefix(r.str("secret"), "hgr_") {
		t.Fatalf("mint: %d %v", r.code, r.body)
	}
	if w := s.do("GET", "/v1/resources/"+id, r.str("secret"), nil); w.code != 200 {
		t.Fatalf("the minted token reads: %d", w.code)
	}

	// delete gives the room back
	r = s.do("DELETE", "/v1/resources/"+second, alice, nil)
	if r.code != 202 || s.done(alice, r.str("operation", "id")).str("state") != "succeeded" {
		t.Fatalf("delete: %d %v", r.code, r.body)
	}
	if strings.Contains(s.engineFile(), second) {
		t.Fatal("the engine still has the deleted box")
	}
	r = s.do("GET", "/v1/limits", alice, nil)
	for _, l := range r.body["limits"].([]any) {
		l := l.(map[string]any)
		if l["name"] == "toy.boxes" && l["used"] != float64(1) {
			t.Fatalf("toy.boxes used after the delete: %v", l)
		}
	}
	if r = s.create(alice, map[string]any{"type": "box", "zone": "z"}); r.code != 202 {
		t.Fatalf("the room came back: %d %v", r.code, r.body)
	}

	// every call wrote its line; a refusal says why
	var refusal map[string]any
	for _, line := range strings.Split(s.logs.String(), "\n") {
		if strings.Contains(line, `"kind":"audit"`) && strings.Contains(line, `"reason":"limit"`) {
			_ = json.Unmarshal([]byte(line), &refusal)
			break
		}
	}
	if refusal == nil || refusal["actor"] != "alice" || refusal["status"] != float64(403) || refusal["detail"] != "2 of 2 toy.boxes used; this asks for 1 more" {
		t.Fatalf("the refusal's audit line: %v", refusal)
	}
}

// ---- Reconcile: the engine moved behind the brain's back --------------------

func TestReconcile(t *testing.T) {
	s := newStack(t)
	alice := s.token("alice", "users")
	id := s.create(alice, map[string]any{"type": "box", "zone": "z", "spec": map[string]any{"cores": 2}}).str("resource", "id")

	// someone changes the guest on the engine: reconcile puts it back
	var eng struct {
		Seq    int                       `json:"seq"`
		Guests map[string]map[string]any `json:"guests"`
	}
	edit := func(f func()) {
		_ = json.Unmarshal([]byte(s.engineFile()), &eng)
		f()
		b, _ := json.Marshal(eng)
		if err := os.WriteFile(s.engine, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	edit(func() { eng.Guests[id]["cores"] = 9 })
	s.core.ReconcileOnce(context.Background())
	_ = json.Unmarshal([]byte(s.engineFile()), &eng)
	if eng.Guests[id]["cores"] != float64(2) {
		t.Fatalf("not repaired: %v", eng.Guests[id])
	}
	if !strings.Contains(s.logs.String(), `"action":"reconcile"`) {
		t.Fatal("a repair writes an audit line")
	}

	// someone deletes it on the engine: lost, and says so
	edit(func() { delete(eng.Guests, id) })
	s.core.ReconcileOnce(context.Background())
	if r := s.do("GET", "/v1/resources/"+id, alice, nil); r.str("state") != "lost" || r.str("drift") == "" {
		t.Fatalf("not lost: %v", r.body)
	}
	if r := s.do("POST", "/v1/resources/"+id+"/actions/start", alice, nil); r.code != 409 || !strings.Contains(r.str("detail"), "lost") {
		t.Fatalf("an action on a lost box: %d %v", r.code, r.body)
	}
	// a lost box still counts (it holds its owner's room until deleted), and
	// deleting it works: the engine's "already gone" is a success
	r := s.do("DELETE", "/v1/resources/"+id, alice, nil)
	if r.code != 202 || s.done(alice, r.str("operation", "id")).str("state") != "succeeded" {
		t.Fatalf("delete a lost box: %d %v", r.code, r.body)
	}
}

// ---- An operation the brain died during is carried out at the next start ---

func TestAnInterruptedOperationResumes(t *testing.T) {
	s := newStack(t)
	// the registry as a brain leaves it when it dies mid-create: the row
	// written, the operation running, the engine never called
	id, opID := "box-0123456789abcdef0", "op-0123456789abcdef0"
	err := s.store.Tx(context.Background(), func(tx *registry.Tx) error {
		if err := tx.InsertResource(&registry.Resource{ID: id, Type: "box", Plugin: "toy", Owner: "alice", Zone: "z",
			State: registry.Creating, Spec: json.RawMessage(`{"kind":"container","cores":1,"memory_gb":1,"running":true}`),
			Usage: map[string]int64{"toy.boxes": 1, "toy.cores": 1, "toy.memory_gb": 1}}); err != nil {
			return err
		}
		return tx.InsertOperation(&registry.Operation{ID: opID, Owner: "alice", ResourceID: id, Kind: registry.OpCreate})
	})
	if err != nil {
		t.Fatal(err)
	}
	// a second brain starts on the same registry
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	again := core.New(ctx, s.cfg, s.store, s.host, audit.New(log), log, metrics.New())
	go func() { _ = again.Run(ctx) }()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if op, _ := s.store.Operation(ctx, opID); op != nil && op.State != registry.OpRunning {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	r, _ := s.store.Resource(ctx, id)
	if r.State != registry.Ready || !strings.Contains(s.engineFile(), id) {
		t.Fatalf("not resumed: %s", r.State)
	}
	cancel()
	again.Wait()
}

// ---- The house contract -----------------------------------------------------

func TestHealthAndMetrics(t *testing.T) {
	s := newStack(t)
	alice := s.token("alice", "users")
	s.create(alice, map[string]any{"type": "box", "zone": "z"})
	if r := s.do("GET", "/healthz", "", nil); r.code != 200 || r.str("status") != "ok" {
		t.Fatalf("%d %v", r.code, r.body)
	}
	resp, err := http.Get(s.url + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{
		`hangar_plugin_up{plugin="toy"} 1`,
		`hangar_resources{type="box",state="ready"} 1`,
		`hangar_operations_total{kind="create",result="succeeded"} 1`,
		`hangar_http_requests_total{route="POST /v1/resources",code="202"} 1`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("metrics lack %s", want)
		}
	}
	resp, err = http.Get(s.url + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	// with the console on (the default), the brain's address is the console's
	if resp.Request.URL.Path != "/console/" || !strings.Contains(string(b), "static/js/main.js") {
		t.Fatalf("the front page: at %s", resp.Request.URL.Path)
	}
}
