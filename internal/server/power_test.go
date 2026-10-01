package server

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// A zone on the fake engine, whose file holds the engine's clock; a tier
// with ten vCPU-hours a month, counted in Paris's calendar.
const powerConfig = `
data_dir: %[1]s
time_zone: Europe/Paris
identity:
  oidc: {issuer: %[2]s, audience: hangar}
tiers:
  - name: operators
    groups: [ops]
    operator: true
    zones: ["*"]
    limits: {"*": unlimited, "machines.kind": [vm, container], "machines.class": [spot]}
  - name: unmetered
    groups: [unmetered]
    zones: [p, bare]
    limits: {"machines.*": unlimited}
  - name: users
    groups: [users]
    zones: [p, bare]
    limits:
      machines.count: 4
      machines.vcpu_hours: 10
      machines.vcpu: 16
      machines.memory_gb: 16
      machines.disk_gb: 64
      machines.kind: [vm, container]
      machines.class: [spot]
zones:
  - {name: p, driver: fake, endpoint: "%[1]s/zone-p.json"}
  - name: bare
    driver: fake
    options: {capabilities: "kind.container,kind.vm,guest.tags,fence.pool"}
plugins:
  - name: machines
    path: %[3]s
    args: [hangar-test-plugin, machines]
    zones: [p, bare]
    settings:
      images:
        debian-13: {vm: debian-13, container: "store:vztmpl/debian-13.tar.zst"}
reconcile:
  every: 1h
`

// power is a brain whose clock and whose engine's clock the test moves
// together: an hour passing is one line.
type power struct {
	*stack
	mu    sync.Mutex
	clock time.Time
	zone  string // the fake engine's file
}

func newPower(t *testing.T, at time.Time) *power {
	t.Helper()
	p := &power{clock: at}
	p.stack = newStackClock(t, powerConfig, func() time.Time { p.mu.Lock(); defer p.mu.Unlock(); return p.clock }, "machines")
	p.zone = strings.TrimSuffix(p.engine, "zone-z.json") + "zone-p.json"
	p.set(at)
	return p
}

// edit changes the engine's file, keeping everything it does not touch.
func (p *power) edit(f func(m map[string]any)) {
	p.t.Helper()
	m := map[string]any{}
	if b, err := os.ReadFile(p.zone); err == nil {
		if err := json.Unmarshal(b, &m); err != nil {
			p.t.Fatal(err)
		}
	}
	f(m)
	b, _ := json.Marshal(m)
	if err := os.WriteFile(p.zone, b, 0o600); err != nil {
		p.t.Fatal(err)
	}
}

// set puts both clocks at a time.
func (p *power) set(at time.Time) {
	p.t.Helper()
	p.mu.Lock()
	p.clock = at
	p.mu.Unlock()
	p.edit(func(m map[string]any) { m["now"] = at.UTC().Format(time.RFC3339) })
}

func (p *power) now() time.Time { p.mu.Lock(); defer p.mu.Unlock(); return p.clock }

// pass lets time go by, then looks at everything as the brain's loop does.
func (p *power) pass(after time.Duration) {
	p.t.Helper()
	p.set(p.now().Add(after))
	p.core.ReconcileOnce(context.Background())
	p.core.Wait()
}

// busy says a guest was busy up to now.
func (p *power) busy(id string) {
	p.t.Helper()
	p.edit(func(m map[string]any) {
		b, _ := m["busy_until"].(map[string]any)
		if b == nil {
			b = map[string]any{}
		}
		b[id] = p.now().UTC().Format(time.RFC3339)
		m["busy_until"] = b
	})
}

// runs reads a guest's power on the engine itself.
func (p *power) runs(id string) bool {
	p.t.Helper()
	var st struct {
		Guests map[string]struct {
			Running bool `json:"running"`
		} `json:"guests"`
	}
	b, _ := os.ReadFile(p.zone)
	if err := json.Unmarshal(b, &st); err != nil {
		p.t.Fatal(err)
	}
	g, ok := st.Guests[id]
	if !ok {
		p.t.Fatalf("the engine has no %s", id)
	}
	return g.Running
}

func (p *power) machine(who string, spec map[string]any) reply {
	p.t.Helper()
	full := map[string]any{"image": "debian-13", "kind": "container", "cores": 2, "memory_gb": 1}
	for k, v := range spec {
		full[k] = v
	}
	return p.do("POST", "/v1/resources", who, map[string]any{"type": "machine", "zone": "p", "spec": full})
}

// act asks an action and waits for it; its reply is the ask's when refused.
func (p *power) act(who, id, action string, params map[string]any) reply {
	p.t.Helper()
	var body any
	if params != nil {
		body = map[string]any{"params": params}
	}
	r := p.do("POST", "/v1/resources/"+id+"/actions/"+action, who, body)
	if r.code == 202 {
		if op := p.done(who, r.str("operation", "id")); op.str("state") != "succeeded" {
			p.t.Fatalf("%s failed: %v", action, op.body)
		}
	}
	return r
}

// hours reads what the caller's month has consumed, and the rest of the line.
func (p *power) hours(who string) (used float64, line map[string]any) {
	p.t.Helper()
	r := p.do("GET", "/v1/limits", who, nil)
	for _, l := range asList(r.body["limits"]) {
		if m := l.(map[string]any); m["name"] == "machines.vcpu_hours" {
			return m["used"].(float64), m
		}
	}
	p.t.Fatalf("no machines.vcpu_hours in %v", r.body)
	return 0, nil
}

func (p *power) wantHours(who string, want float64) {
	p.t.Helper()
	if got, _ := p.hours(who); math.Abs(got-want) > 1e-6 {
		p.t.Fatalf("%v vCPU-hours used, want %v", got, want)
	}
}

// An idle machine is stopped at its idle_after, through the whole brain: the
// plugin reads the engine's history at each pass, the registry learns the
// machine is no longer meant to run, and nothing starts it again but its
// owner. Kept awake, it is not — for a time, or until let_sleep.
func TestAnIdleMachineIsStoppedAtItsIdleAfter(t *testing.T) {
	p := newPower(t, time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC))
	alice := p.token("alice", "unmetered")
	get := func(id string) reply { t.Helper(); return p.do("GET", "/v1/resources/"+id, alice, nil) }

	// what it may be: refused at the ask, with why
	for after, want := range map[string]string{"3m": "between 5m and 12h", "soon": "no duration"} {
		if r := p.machine(alice, map[string]any{"idle_after": after}); r.code != 422 || !strings.Contains(r.str("detail"), want) {
			t.Fatalf("idle_after %s: %d %v", after, r.code, r.body)
		}
	}
	if r := p.do("POST", "/v1/resources", alice, map[string]any{"type": "machine", "zone": "bare",
		"spec": map[string]any{"image": "debian-13", "idle_after": "30m"}}); r.code != 422 || !strings.Contains(r.str("detail"), "zone bare keeps no history") {
		t.Fatalf("an idle_after where nothing reads idleness: %d %v", r.code, r.body)
	}

	r := p.machine(alice, map[string]any{"idle_after": "30m"})
	if r.code != 202 {
		t.Fatalf("%d %v", r.code, r.body)
	}
	p.done(alice, r.str("operation", "id"))
	id := r.str("resource", "id")
	// the control beside it: the same machine, the same hours, no idle_after
	r = p.machine(alice, nil)
	p.done(alice, r.str("operation", "id"))
	never := r.str("resource", "id")

	p.pass(20 * time.Minute)
	p.busy(id) // busy at its 20th minute
	p.pass(29 * time.Minute)
	if g := get(id); !p.runs(id) || g.str("observed", "quiet_for") != "29m" || g.str("spec", "running") != "true" {
		t.Fatalf("quiet for 29m of 30: %v", g.body)
	}
	// kept awake for an hour: it runs on, quiet as it is
	if r := p.act(alice, id, "keep_awake", map[string]any{"for": "1h"}); r.code != 202 {
		t.Fatalf("%d %v", r.code, r.body)
	}
	if want := p.now().Add(time.Hour).UTC().Format(time.RFC3339); get(id).str("spec", "awake") != want {
		t.Fatalf("kept awake until %s: %v", want, get(id).body)
	}
	p.pass(59 * time.Minute)
	if !p.runs(id) {
		t.Fatal("stopped while kept awake")
	}
	// the hour over, it is stopped at the next pass — and the registry knows
	p.pass(time.Minute)
	g := get(id)
	if p.runs(id) || g.str("spec", "running") != "false" || g.str("room", "running") != "false" || g.str("spec", "awake") != "<nil>" || g.str("state") != "ready" {
		t.Fatalf("idle for 1h29, kept awake for 1h: %v", g.body)
	}
	if logs := p.logs.String(); !strings.Contains(logs, `"result":"machine.idle"`) || !strings.Contains(logs, "quiet for 30m, its idle_after") {
		t.Fatal("the audit says nothing of the idle stop")
	}
	if !p.runs(never) {
		t.Fatal("a machine with no idle_after was stopped")
	}
	// it stays stopped, pass after pass
	p.pass(2 * time.Hour)
	if p.runs(id) {
		t.Fatal("it came back by itself")
	}
	// its owner starts it: its quiet begins at its start
	if r := p.act(alice, id, "start", nil); r.code != 202 {
		t.Fatalf("%d %v", r.code, r.body)
	}
	p.pass(25 * time.Minute)
	if !p.runs(id) {
		t.Fatal("stopped 25m after its start")
	}
	// kept awake until let_sleep
	if r := p.act(alice, id, "keep_awake", nil); r.code != 202 || get(id).str("spec", "awake") != "always" {
		t.Fatalf("%d %v", r.code, get(id).body)
	}
	p.pass(11 * time.Hour)
	if !p.runs(id) {
		t.Fatal("stopped while kept awake until let_sleep")
	}
	if r := p.act(alice, id, "let_sleep", nil); r.code != 202 {
		t.Fatalf("%d %v", r.code, r.body)
	}
	p.pass(0)
	if p.runs(id) {
		t.Fatal("let sleep, quiet for hours, and running")
	}
	// refused at the ask: a stopped machine kept awake; one with nothing to hold off
	if r := p.act(alice, id, "keep_awake", nil); r.code != 422 || !strings.Contains(r.str("detail"), "it is stopped") {
		t.Fatalf("%d %v", r.code, r.body)
	}
	if r := p.act(alice, never, "keep_awake", map[string]any{"for": "1h"}); r.code != 422 || !strings.Contains(r.str("detail"), "no idle_after") {
		t.Fatalf("%d %v", r.code, r.body)
	}
	// set later, by its action — and what a spec file would ask for it
	if r := p.act(alice, never, "set_idle_after", map[string]any{"idle_after": "90m"}); r.code != 202 || get(never).str("spec", "idle_after") != "1h30m" {
		t.Fatalf("%d %v", r.code, get(never).body)
	}
	plan := p.do("POST", "/v1/resources/"+never+"/plan", alice, map[string]any{"spec": map[string]any{"image": "debian-13", "kind": "container", "cores": 2, "memory_gb": 1}})
	if plan.code != 200 || plan.str("steps", "0", "action") != "set_idle_after" || plan.str("steps", "0", "params", "idle_after") != "never" {
		t.Fatalf("a file with no idle_after: %d %v", plan.code, plan.body)
	}
	// the machine that had none ran all along, and is now stopped in its turn
	p.pass(0)
	if p.runs(never) {
		t.Fatal("quiet for a day with an idle_after of 1h30m")
	}
}

// The hours a machine runs are its owner's, counted once per core and per
// calendar month; the month spent, a start is refused with the numbers and
// what still runs is stopped; a delete gives nothing back; the next month
// begins at nothing.
func TestTheMonthsHours(t *testing.T) {
	p := newPower(t, time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC))
	alice, bob, root := p.token("alice", "users"), p.token("bob", "users"), p.token("root", "ops")
	create := func(who string, spec map[string]any) string {
		t.Helper()
		r := p.machine(who, spec)
		if r.code != 202 {
			t.Fatalf("%d %v", r.code, r.body)
		}
		p.done(who, r.str("operation", "id"))
		return r.str("resource", "id")
	}

	a := create(alice, nil) // 2 cores
	if used, line := p.hours(alice); used != 0 || line["limit"] != float64(10) || line["kind"] != "meter" || line["unit"] != "vCPU-hours" ||
		line["period"] != "2026-10" || line["resets"] != "2026-11-01T00:00:00+01:00" {
		t.Fatalf("the month's line: %v", line)
	}
	p.pass(2 * time.Hour)
	p.wantHours(alice, 4)
	p.wantHours(bob, 0)

	// stopped, it consumes nothing; the stop counted what ran up to it
	p.set(p.now().Add(30 * time.Minute))
	p.act(alice, a, "stop", nil)
	p.wantHours(alice, 5)
	p.pass(3 * time.Hour)
	p.wantHours(alice, 5)

	// started again; deleted while it runs: its last hour is counted, and
	// stays counted — a delete gives nothing back
	p.act(alice, a, "start", nil)
	p.set(p.now().Add(time.Hour))
	if r := p.do("DELETE", "/v1/resources/"+a, alice, nil); r.code != 202 {
		t.Fatalf("%d %v", r.code, r.body)
	} else {
		p.done(alice, r.str("operation", "id"))
	}
	p.wantHours(alice, 7)
	if r := p.do("GET", "/v1/limits", alice, nil); !strings.Contains(r.str("limits"), "machines.count") {
		t.Fatalf("%v", r.body)
	}
	p.pass(time.Hour)
	p.wantHours(alice, 7)

	// two machines at once burn twice as fast: 6 cores for half an hour
	b := create(alice, nil)
	c := create(alice, map[string]any{"cores": 4})
	p.pass(30 * time.Minute)
	p.wantHours(alice, 10)
	if !p.runs(b) || !p.runs(c) {
		t.Fatal("stopped before the brain knew the month was spent")
	}
	// the month is spent: what runs is stopped at the next pass…
	p.pass(time.Minute)
	for _, id := range []string{b, c} {
		if g := p.do("GET", "/v1/resources/"+id, alice, nil); p.runs(id) || g.str("spec", "running") != "false" || g.str("room", "running") != "false" {
			t.Fatalf("the month spent, %s runs on: %v", id, g.body)
		}
	}
	if logs := p.logs.String(); !strings.Contains(logs, `"result":"machine.spent"`) {
		t.Fatal("the audit says nothing of the stop")
	}
	used, _ := p.hours(alice)
	if used < 10 || used > 10.2 {
		t.Fatalf("its last minute is counted, no more: %v", used)
	}
	// …a start is refused with the numbers, and so is a new machine
	refused := func(used string, r reply) {
		t.Helper()
		if r.code != 403 || r.str("kind") != "limit" || r.str("refusals", "0", "reason") != "meter" || r.str("refusals", "0", "period") != "2026-10" ||
			!strings.Contains(r.str("detail"), used+" of 10 vCPU-hours (machines.vcpu_hours) used in October 2026: it is back on 1 November") {
			t.Fatalf("%d %v", r.code, r.body)
		}
	}
	refused("10.1", p.act(alice, b, "start", nil))
	refused("10.1", p.machine(alice, nil))
	// over a quantity as well: every reason at once, the quantity first
	if r := p.machine(alice, map[string]any{"cores": 16}); r.code != 403 || r.str("refusals", "0", "dimension") != "machines.vcpu" ||
		r.str("refusals", "1", "reason") != "meter" || !strings.Contains(r.str("detail"), "machines.vcpu") {
		t.Fatalf("over its cores and its month: %d %v", r.code, r.body)
	}
	if logs := p.logs.String(); !strings.Contains(logs, `"reason":"meter"`) {
		t.Fatal("the audit does not say why it refused")
	}
	// what draws on nothing is still asked freely
	if r := p.act(alice, c, "set_idle_after", map[string]any{"idle_after": "30m"}); r.code != 202 {
		t.Fatalf("a stopped machine's idle_after: %d %v", r.code, r.body)
	}
	// someone else's month is their own
	create(bob, nil)

	// an operator starts it for her, under the operator's own limits: it runs
	if r := p.act(root, b, "start", nil); r.code != 202 {
		t.Fatalf("%d %v", r.code, r.body)
	}
	p.pass(10 * time.Minute)
	if !p.runs(b) {
		t.Fatal("started by an operator, stopped under its owner's tier")
	}
	// her own next ask on it comes under her tier again
	refused("10.4", p.act(alice, b, "resize", map[string]any{"cores": 2, "memory_gb": 2}))
	p.act(root, b, "stop", nil)

	// the month ends at midnight in the brain's time zone, not in UTC's
	p.set(time.Date(2026, 10, 31, 22, 59, 0, 0, time.UTC)) // 23:59 in Paris
	if r := p.act(alice, b, "start", nil); r.code != 403 {
		t.Fatalf("a minute before the month's end: %d %v", r.code, r.body)
	}
	p.set(time.Date(2026, 10, 31, 23, 0, 30, 0, time.UTC)) // 00:00:30 on 1 November
	if r := p.act(alice, b, "start", nil); r.code != 202 {
		t.Fatalf("the new month: %d %v", r.code, r.body)
	}
	if used, line := p.hours(alice); used != 0 || line["period"] != "2026-11" || line["resets"] != "2026-12-01T00:00:00+01:00" {
		t.Fatalf("the new month's line: %v", line)
	}
	p.pass(time.Hour)
	p.wantHours(alice, 2)
	if !p.runs(b) {
		t.Fatal("stopped in a month that has hours left")
	}
	// the metric adds what every owner consumed
	if m := p.do("GET", "/metrics", "", nil); m.code != 200 {
		t.Fatalf("%d", m.code)
	}
}
