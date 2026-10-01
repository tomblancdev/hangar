package machines

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/tomblancdev/hangar/driver"
	"github.com/tomblancdev/hangar/driver/fake"
	"github.com/tomblancdev/hangar/sdk/pluginpb"
)

var t0 = time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)

// held is a machine as the core holds it: what each answer of the plugin
// writes back (spec, observed) and adds up (the hours).
type held struct {
	t      *testing.T
	p      *Plugin
	e      *fake.Engine
	r      *pluginpb.Resource
	hours  float64
	events []string
}

// born makes a machine on the fake engine, its clock frozen at t0.
func born(t *testing.T, spec string) *held {
	t.Helper()
	p := configured(t, "fake", "")
	e := p.Driver("z").(*fake.Engine)
	e.SetNow(t0)
	pr, _ := plan(t, p, spec)
	if len(pr.GetRefusals()) > 0 {
		t.Fatalf("%s: %s", spec, reasons(pr))
	}
	m := &held{t: t, p: p, e: e, r: &pluginpb.Resource{Id: "m-0123456789abcdef0", Type: "machine", Zone: "z", Owner: "alice", Spec: pr.GetSpec()}}
	cr, err := p.Create(context.Background(), &pluginpb.CreateRequest{Resource: m.r})
	if err != nil {
		t.Fatal(err)
	}
	m.r.Observed = cr.GetObserved()
	return m
}

func (m *held) spec() Spec         { var s Spec; _ = jsonInto(m.r.GetSpec(), &s); return s }
func (m *held) observed() Observed { return observedOf(m.r) }

// pass moves the engine's clock and reconciles, as the core's pass does.
func (m *held) pass(after time.Duration) *pluginpb.ReconcileResponse {
	m.t.Helper()
	m.e.SetNow(m.e.Now().Add(after))
	resp, err := m.p.Reconcile(context.Background(), &pluginpb.ReconcileRequest{Resource: m.r})
	if err != nil {
		m.t.Fatal(err)
	}
	m.write(resp.GetSpec(), resp.GetObserved(), resp.GetConsumed(), resp.GetEvents())
	return resp
}

func (m *held) write(spec, observed []byte, c *pluginpb.Consumed, evs []*pluginpb.Event) {
	if len(spec) > 0 {
		m.r.Spec = spec
	}
	if len(observed) > 0 {
		m.r.Observed = observed
	}
	m.hours += c.GetAmounts()[VCPUHours]
	for _, ev := range evs {
		m.events = append(m.events, ev.GetName())
	}
}

// act asks an action as the core does: planned first when it changes usage,
// its planned spec written before the plugin acts.
func (m *held) act(action, params string) string {
	m.t.Helper()
	ctx := context.Background()
	if action != "let_sleep" && action != "reboot" {
		pr, err := m.p.Plan(ctx, &pluginpb.PlanRequest{Type: "machine", Zone: "z", Action: action, Params: []byte(params), Current: m.r})
		if err != nil {
			m.t.Fatal(err)
		}
		if len(pr.GetRefusals()) > 0 {
			return reasons(pr)
		}
		m.r.Spec = pr.GetSpec()
	}
	resp, err := m.p.Act(ctx, &pluginpb.ActRequest{Resource: m.r, Action: action, Params: []byte(params)})
	if err != nil {
		return err.Error()
	}
	m.write(resp.GetSpec(), resp.GetObserved(), resp.GetConsumed(), resp.GetEvents())
	return ""
}

func (m *held) running() bool {
	m.t.Helper()
	g, err := m.e.Guest(context.Background(), m.r.GetId())
	if err != nil {
		m.t.Fatal(err)
	}
	return g.Running
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

const small = `{"image":"debian-13","kind":"container","cores":2,"memory_gb":1`

// The hours a machine runs are counted from its engine's own clock, once per
// core, at every look — and at every action, as it was before the action.
func TestTheHoursAreCountedOncePerCore(t *testing.T) {
	m := born(t, small+`}`)
	if resp := m.pass(0); resp.GetConsumed() != nil {
		t.Fatalf("nothing ran yet: %v", resp.GetConsumed())
	}
	m.pass(90 * time.Minute)
	if !near(m.hours, 3) {
		t.Fatalf("2 cores for 1h30: %v vCPU-hours", m.hours)
	}
	// the same look answered twice (the core could not write the first):
	// counted from the same point, never twice
	before := m.r.GetObserved()
	m.e.SetNow(m.e.Now().Add(30 * time.Minute))
	a, _ := m.p.Reconcile(context.Background(), &pluginpb.ReconcileRequest{Resource: m.r})
	b, _ := m.p.Reconcile(context.Background(), &pluginpb.ReconcileRequest{Resource: m.r})
	if !near(a.GetConsumed().GetAmounts()[VCPUHours], 1) || !near(b.GetConsumed().GetAmounts()[VCPUHours], 1) || string(m.r.GetObserved()) != string(before) {
		t.Fatalf("an answer not written is counted again from the same point: %v then %v", a.GetConsumed(), b.GetConsumed())
	}
	m.write(nil, b.GetObserved(), b.GetConsumed(), nil)
	if resp := m.pass(0); resp.GetConsumed() != nil || !near(m.hours, 4) {
		t.Fatalf("written, it is not counted again: %v, %v in all", resp.GetConsumed(), m.hours)
	}

	// a resize of a running machine: the time up to it counts with the cores
	// it had, the time after with the new ones
	m.e.SetNow(m.e.Now().Add(time.Hour))
	if why := m.act("resize", `{"cores":4,"memory_gb":1}`); why != "" {
		t.Fatal(why)
	}
	if !near(m.hours, 6) {
		t.Fatalf("an hour at 2 cores before the resize: %v", m.hours)
	}
	m.pass(time.Hour)
	if !near(m.hours, 10) {
		t.Fatalf("an hour at 4 cores after it: %v", m.hours)
	}

	// stopped, it consumes nothing; the stop counts what ran up to it
	m.e.SetNow(m.e.Now().Add(15 * time.Minute))
	if why := m.act("stop", ""); why != "" {
		t.Fatal(why)
	}
	if !near(m.hours, 11) || m.running() {
		t.Fatalf("a quarter at 4 cores, then stopped: %v", m.hours)
	}
	m.pass(5 * time.Hour)
	if !near(m.hours, 11) {
		t.Fatalf("a stopped machine consumed: %v", m.hours)
	}
	// started again: counted from its start, not from the last look
	if why := m.act("start", ""); why != "" {
		t.Fatal(why)
	}
	m.pass(30 * time.Minute)
	if !near(m.hours, 13) {
		t.Fatalf("half an hour at 4 cores since its start: %v", m.hours)
	}
	// stopped and started behind the brain's back between two looks: only
	// the time since its last start is counted
	m.e.SetNow(m.e.Now().Add(2 * time.Hour))
	ctx := context.Background()
	_, _ = m.e.SetPower(ctx, m.r.GetId(), false)
	m.e.SetNow(m.e.Now().Add(2 * time.Hour))
	_, _ = m.e.SetPower(ctx, m.r.GetId(), true)
	m.pass(15 * time.Minute)
	if !near(m.hours, 14) {
		t.Fatalf("a quarter at 4 cores since it last started: %v", m.hours)
	}
	// its delete counts its last stretch
	m.e.SetNow(m.e.Now().Add(45 * time.Minute))
	del, err := m.p.Delete(ctx, &pluginpb.DeleteRequest{Resource: m.r})
	if err != nil || !near(del.GetConsumed().GetAmounts()[VCPUHours], 3) {
		t.Fatalf("its last three quarters at 4 cores: %v %v", del.GetConsumed(), err)
	}
}

// A machine with an idle_after is stopped once its engine saw it quiet that
// long — not before, not while kept awake, not on a history that cannot be
// read, not while its room is held — and stays stopped.
func TestAnIdleMachineIsStopped(t *testing.T) {
	m := born(t, small+`,"idle_after":"30m"}`)
	if s := m.spec(); s.IdleAfter != "30m" || s.Awake != "" {
		t.Fatalf("%+v", s)
	}
	m.pass(20 * time.Minute)
	if !m.running() || m.observed().QuietFor != "20m" {
		t.Fatalf("quiet for 20m of 30: running %v, %+v", m.running(), m.observed())
	}
	// busy at the 25th minute: its quiet begins anew
	m.e.SetBusy(m.r.GetId(), m.e.Now().Add(5*time.Minute))
	m.pass(15 * time.Minute)
	if !m.running() || m.observed().QuietFor != "10m" {
		t.Fatalf("busy 10m ago: running %v, %+v", m.running(), m.observed())
	}
	// the control beside the stop: the same quiet, unreadable
	m.e.FailActivity(true)
	if m.pass(time.Hour); !m.running() || m.spec().Running != true {
		t.Fatal("a history that cannot be read was taken for idleness")
	}
	m.e.FailActivity(false)
	// kept awake for 2h: not stopped, however quiet
	if why := m.act("keep_awake", `{"for":"2h"}`); why != "" {
		t.Fatal(why)
	}
	until := m.e.Now().Add(2 * time.Hour).Format(time.RFC3339)
	if m.spec().Awake != until {
		t.Fatalf("kept awake until %s: %+v", until, m.spec())
	}
	if m.pass(119 * time.Minute); !m.running() {
		t.Fatal("stopped while kept awake")
	}
	// it ends by itself: quiet all along, it is stopped at the next look
	resp := m.pass(time.Minute)
	if m.running() || m.spec().Running || m.spec().Awake != "" || resp.GetRoom() == nil || resp.GetRoom().GetRunning() {
		t.Fatalf("kept awake past its end: running %v, %+v, room %v", m.running(), m.spec(), resp.GetRoom())
	}
	if !contains(m.events, "machine.idle") {
		t.Fatalf("it said nothing of its stop: %v", m.events)
	}
	// it stays stopped: nothing starts it again but its owner
	if m.pass(3 * time.Hour); m.running() {
		t.Fatal("it came back by itself")
	}
	if why := m.act("start", ""); why != "" {
		t.Fatal(why)
	}
	// just started, its quiet begins at its start
	if m.pass(29 * time.Minute); !m.running() {
		t.Fatal("stopped before its idle_after since its start")
	}
	// kept awake until let_sleep: a day later it still runs
	if why := m.act("keep_awake", ``); why != "" || m.spec().Awake != "always" {
		t.Fatalf("%s %+v", why, m.spec())
	}
	if m.pass(11 * time.Hour); !m.running() {
		t.Fatal("stopped while kept awake until let_sleep")
	}
	if why := m.act("let_sleep", ""); why != "" || m.spec().Awake != "" {
		t.Fatalf("%s %+v", why, m.spec())
	}
	if m.pass(0); m.running() {
		t.Fatal("let sleep, quiet for hours, and still running")
	}
	if why := m.act("keep_awake", ``); !strings.Contains(why, "it is stopped") {
		t.Fatalf("a stopped machine kept awake: %q", why)
	}

	// never: no idle stop any more
	_ = m.act("start", "")
	if why := m.act("set_idle_after", `{"idle_after":"never"}`); why != "" || m.spec().IdleAfter != "" {
		t.Fatalf("%s %+v", why, m.spec())
	}
	if m.pass(11 * time.Hour); !m.running() || m.observed().QuietFor != "" {
		t.Fatalf("a machine with no idle_after: running %v, %+v", m.running(), m.observed())
	}
	if why := m.act("keep_awake", `{"for":"1h"}`); !strings.Contains(why, "no idle_after") {
		t.Fatalf("nothing to hold off: %q", why)
	}
	if why := m.act("set_idle_after", `{"idle_after":"90m"}`); why != "" || m.spec().IdleAfter != "1h30m" {
		t.Fatalf("%s %+v", why, m.spec())
	}

	// a machine whose room is held is not judged: a hold stops it, and it
	// comes back with the room (it never became "stopped for idleness")
	if m.pass(2 * time.Hour); m.running() {
		t.Fatal("quiet for 2h with an idle_after of 1h30m")
	}
	_ = m.act("start", "")
	m.r.Hold = "4100"
	if m.pass(2 * time.Hour); m.running() || !m.spec().Running {
		t.Fatalf("held, a spot machine stops and stays meant to run: running %v, %+v", m.running(), m.spec())
	}
	m.r.Hold = ""
	if m.pass(0); !m.running() {
		t.Fatal("the room is back and it is not")
	}
}

// remembers is an engine whose history is older than its guests: it answers
// quiet for as long as it is asked, whatever the guest's age — what Proxmox
// VE's did for a guest made on the number of one deleted a minute before.
type remembers struct{ *fake.Engine }

func (remembers) QuietFor(_ context.Context, _ string, window time.Duration, _ driver.Quiet) (time.Duration, error) {
	return window, nil
}

func init() {
	driver.Register("remembers", func(ctx context.Context, p driver.Params) (driver.Driver, error) {
		d, err := fake.Open(ctx, p)
		if err != nil {
			return nil, err
		}
		return remembers{d.(*fake.Engine)}, nil
	})
}

// A machine is never judged idle for longer than it has run, whatever its
// engine's history says of its past.
func TestAMachineIsNotIdleBeforeItHasRunItsIdleAfter(t *testing.T) {
	p := configured(t, "remembers", "")
	e := p.Driver("z").(remembers).Engine
	e.SetNow(t0)
	pr, _ := plan(t, p, small+`,"idle_after":"30m"}`)
	m := &held{t: t, p: p, e: e, r: &pluginpb.Resource{Id: "m-0123456789abcdef0", Type: "machine", Zone: "z", Owner: "alice", Spec: pr.GetSpec()}}
	cr, err := p.Create(context.Background(), &pluginpb.CreateRequest{Resource: m.r})
	if err != nil {
		t.Fatal(err)
	}
	m.r.Observed = cr.GetObserved()
	if m.pass(22 * time.Second); !m.running() || m.observed().QuietFor != "0m" {
		t.Fatalf("22 s old, on an engine that says it was quiet for 30m: running %v, %+v", m.running(), m.observed())
	}
	if m.pass(29 * time.Minute); !m.running() || m.observed().QuietFor != "29m" {
		t.Fatalf("29m old: running %v, %+v", m.running(), m.observed())
	}
	if m.pass(time.Minute); m.running() {
		t.Fatal("30m old and quiet all along, it runs on")
	}
	// started again: its past is not its present
	_ = m.act("start", "")
	if m.pass(time.Minute); !m.running() {
		t.Fatal("stopped a minute after its start, on its last run's quiet")
	}
}

func TestWhatAnIdleAfterAndAKeepAwakeMayBe(t *testing.T) {
	p := configured(t, "fake", "")
	for spec, want := range map[string]string{
		small + `,"idle_after":"3m"}`:     "between 5m and 12h",
		small + `,"idle_after":"13h"}`:    "between 5m and 12h",
		small + `,"idle_after":"soon"}`:   "no duration",
		small + `,"idle_after":"90s"}`:    "whole minutes",
		small + `,"idle_after":"never"}`:  "",
		small + `,"idle_after":"1h30m"}`:  "",
		small + `,"idle_after":"90m"}`:    "",
		small + `,"idle_after":"12h"}`:    "",
		small + `,"idle_after":"5m0s"}`:   "",
		small + `,"idle_after":"0h30m"}`:  "",
		small + `,"idle_after":"-30m"}`:   "between 5m and 12h",
		small + `,"idle_after":"1.5h"}`:   "",
		small + `,"idle_after":"30m "}`:   "no duration",
		small + `,"idle_after":"30min"}`:  "no duration",
		small + `,"idle_after":"2h30m"}`:  "",
		small + `,"idle_after":"12h1m"}`:  "between 5m and 12h",
		small + `,"idle_after":"4m59s"}`:  "whole minutes",
		small + `,"idle_after":"300s"}`:   "",
		small + `,"idle_after":"0"}`:      "between 5m and 12h",
		small + `,"idle_after":"0h5m"}`:   "",
		small + `,"idle_after":"1h0m0s"}`: "",
	} {
		r, _ := plan(t, p, spec)
		if got := reasons(r); (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("%s: %q, want %q", spec, got, want)
		}
	}
	for in, want := range map[string]string{"never": "", "90m": "1h30m", "1.5h": "1h30m", "300s": "5m", "2h": "2h", "1h0m0s": "1h"} {
		if _, s := plan(t, p, small+`,"idle_after":"`+in+`"}`); s.IdleAfter != want {
			t.Errorf("%s is written %q, want %q", in, s.IdleAfter, want)
		}
	}
	// a running machine draws on its hours; a stopped one on nothing
	r, _ := plan(t, p, small+`}`)
	if m := r.GetMeters(); len(m) != 1 || m[0] != VCPUHours {
		t.Fatalf("a machine born running draws on %v", m)
	}
	cur := &pluginpb.Resource{Spec: r.GetSpec()}
	if stop, _ := p.Plan(context.Background(), &pluginpb.PlanRequest{Type: "machine", Zone: "z", Action: "stop", Current: cur}); len(stop.GetMeters()) != 0 {
		t.Fatalf("a stop draws on %v", stop.GetMeters())
	}
	// a zone whose engine keeps no history: no idle_after there
	bare := configured(t, "fake", "kind.container,kind.vm,guest.tags,fence.pool")
	if r, _ := plan(t, bare, small+`,"idle_after":"30m"}`); !strings.Contains(reasons(r), "keeps no history") {
		t.Errorf("an idle_after where nothing reads idleness: %q", reasons(r))
	}
	if r, _ := plan(t, bare, small+`}`); reasons(r) != "" {
		t.Errorf("no idle_after asks nothing of the zone: %q", reasons(r))
	}
	// keep_awake's time
	m := born(t, small+`,"idle_after":"30m"}`)
	for params, want := range map[string]string{`{"for":"30s"}`: "between 1m and 168h", `{"for":"200h"}`: "between 1m and 168h", `{"for":"later"}`: "no duration"} {
		if why := m.act("keep_awake", params); !strings.Contains(why, want) {
			t.Errorf("keep_awake %s: %q, want %q", params, why, want)
		}
	}
	// the operator's thresholds
	if q := p.quiet(); q != (driver.Quiet{CPU: 0.05, SentBps: 20}) {
		t.Fatalf("the defaults: %+v", q)
	}
	tuned := New()
	if _, err := tuned.Configure(context.Background(), &pluginpb.ConfigureRequest{Settings: []byte(`{"idle":{"cpu":0.2,"sent_bps":500}}`)}); err != nil || tuned.quiet() != (driver.Quiet{CPU: 0.2, SentBps: 500}) {
		t.Fatalf("the operator's: %+v %v", tuned.quiet(), err)
	}
	if _, err := New().Configure(context.Background(), &pluginpb.ConfigureRequest{Settings: []byte(`{"idle":{"cpu":0}}`)}); err == nil {
		t.Fatal("a threshold of nothing was taken")
	}
}

// A machine whose owner's month is spent is stopped at the next look, and a
// start is refused while it is.
func TestAMachineWhoseMonthIsSpentIsStopped(t *testing.T) {
	m := born(t, small+`,"idle_after":"30m"}`)
	_ = m.act("keep_awake", ``)
	m.r.Spent = []string{VCPUHours}
	resp := m.pass(10 * time.Minute)
	if m.running() || m.spec().Running || m.spec().Awake != "" || !contains(m.events, "machine.spent") {
		t.Fatalf("spent, and running %v: %+v %v", m.running(), m.spec(), m.events)
	}
	if !near(resp.GetConsumed().GetAmounts()[VCPUHours], 2.0/6) {
		t.Fatalf("its last ten minutes are counted before it stops: %v", resp.GetConsumed())
	}
	if why := m.act("start", ""); !strings.Contains(why, "vCPU-hours for the month are used") || m.running() {
		t.Fatalf("started on a spent month: %q", why)
	}
	m.r.Spent = nil
	if why := m.act("start", ""); why != "" || !m.running() {
		t.Fatalf("the month is back: %q", why)
	}
}

// What apply asks to bring a machine to its file: an idle_after is changed
// by its action, never set at birth.
func TestAnIdleAfterChangesThroughItsAction(t *testing.T) {
	m := born(t, small+`,"idle_after":"30m"}`)
	for want, step := range map[string]string{
		small + `,"idle_after":"30m"}`:   "",
		small + `,"idle_after":"0h30m"}`: "",
		small + `,"idle_after":"2h"}`:    `set_idle_after {"idle_after":"2h"}`,
		small + `}`:                      `set_idle_after {"idle_after":"never"}`,
	} {
		resp, err := m.p.PlanChange(context.Background(), &pluginpb.PlanChangeRequest{Current: m.r, Spec: []byte(want)})
		if err != nil || len(resp.GetFixed()) > 0 {
			t.Fatalf("%s: %v %v", want, resp.GetFixed(), err)
		}
		got := ""
		for _, s := range resp.GetSteps() {
			got += s.GetAction() + " " + string(s.GetParams())
		}
		if got != step {
			t.Errorf("%s: steps %q, want %q", want, got, step)
		}
	}
}

func jsonInto(b []byte, v any) error { return json.Unmarshal(b, v) }
