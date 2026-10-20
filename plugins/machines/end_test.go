package machines

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tomblancdev/hangar/driver"
	"github.com/tomblancdev/hangar/sdk/pluginpb"
)

// offBehind powers a machine off on its engine, as its own poweroff — or
// its node — would: nothing the brain asked.
func (m *held) offBehind() {
	m.t.Helper()
	m.e.Tamper(m.r.GetId(), func(g *driver.Guest) { g.Running, g.StartedAt = false, nil })
}

// A machine's own power-off is what its on_poweroff says: started again (as
// ever, and for one that says nothing), left stopped, or its end — the core
// told to delete it. Never on what the brain asked, nor on what the brain
// knows of: a hold, its engine's work, a machine not seen running.
func TestAMachinesOwnPowerOff(t *testing.T) {
	// the control: one that says nothing is started again, as ever
	m := born(t, small+`}`)
	m.pass(time.Minute)
	m.offBehind()
	if resp := m.pass(time.Minute); !m.running() || resp.GetEnd() != "" || resp.GetDetail() != "put back: [power]" || !m.spec().Running {
		t.Fatalf("one that says nothing is put back: %v %v", m.running(), resp)
	}
	if r, s := plan(t, m.p, small+`,"on_poweroff":"restart"}`); len(r.GetRefusals()) > 0 || s.OnPoweroff != "" {
		t.Fatalf("restart is written as nothing: %+v %s", s, reasons(r))
	}

	// stop: found so at two looks in a row, it stays stopped, and its spec
	// says so — the first look leaves it stopped and says why
	m = born(t, small+`,"on_poweroff":"stop"}`)
	if s := m.spec(); s.OnPoweroff != OnPoweroffStop {
		t.Fatalf("%+v", s)
	}
	m.pass(time.Minute)
	m.offBehind()
	resp := m.pass(time.Minute)
	if m.running() || !m.spec().Running || resp.GetDrift() != pluginpb.Drift_DRIFT_DRIFTED || m.observed().Unasked == nil ||
		!strings.Contains(resp.GetDetail(), "if it is still stopped 30s after, its on_poweroff (stop) is applied") {
		t.Fatalf("the first look: %v %+v %v", m.running(), m.spec(), resp)
	}
	// a look too soon after — a claim's, a short reconcile — waits on
	if resp := m.pass(time.Second); m.running() || !m.spec().Running || resp.GetDrift() != pluginpb.Drift_DRIFT_DRIFTED {
		t.Fatalf("a second look a second later: %v", resp)
	}
	resp = m.pass(29 * time.Second)
	if m.running() || m.spec().Running || resp.GetEnd() != "" || resp.GetDrift() != pluginpb.Drift_DRIFT_REPAIRED ||
		!strings.Contains(resp.GetDetail(), "its on_poweroff says stop") || resp.GetRoom().GetRunning() || !slices.Contains(m.events, "machine.stopped_itself") {
		t.Fatalf("stop: %v %+v %v", m.running(), m.spec(), resp)
	}
	if resp := m.pass(time.Hour); m.running() || resp.GetDrift() != pluginpb.Drift_DRIFT_IN_SYNC {
		t.Fatalf("it came back by itself: %v", resp)
	}
	// started by its owner, it runs — and its next power-off is its own again
	if why := m.act("start", ""); why != "" || !m.running() {
		t.Fatalf("%s", why)
	}
	m.pass(time.Minute)
	if !m.running() {
		t.Fatal("stopped again at the look after its owner's start")
	}

	// delete: its end, the core's to carry out — stopped meanwhile
	m = born(t, small+`,"on_poweroff":"delete"}`)
	m.pass(time.Minute)
	m.offBehind()
	if resp := m.pass(time.Minute); resp.GetEnd() != "" || m.running() {
		t.Fatalf("the first look ends nothing: %v", resp)
	}
	// its owner acts between the two looks: the next does not forget it
	if why := m.act("set_end", `{"at_end":"delete"}`); why != "" || m.observed().Unasked == nil {
		t.Fatalf("an action between the looks: %s %+v", why, m.observed())
	}
	resp = m.pass(30 * time.Second)
	if m.running() || m.spec().Running || !strings.Contains(resp.GetEnd(), "its on_poweroff says delete") ||
		resp.GetDrift() != pluginpb.Drift_DRIFT_REPAIRED || !slices.Contains(m.events, "machine.ended") {
		t.Fatalf("delete: %+v %v", m.spec(), resp)
	}
	var fields map[string]string
	for _, ev := range resp.GetEvents() {
		if ev.GetName() == "machine.ended" {
			fields = ev.GetFields()
		}
	}
	if fields["by"] != "poweroff" || fields["at_end"] != "delete" || !strings.Contains(m.spec().Ending, "its on_poweroff says delete") {
		t.Fatalf("its event says what ended it: %v %+v", fields, m.spec())
	}
	// not deleted yet (a volume on it, its engine down): its end is handed
	// over again at every look, said once
	if resp := m.pass(time.Minute); resp.GetEnd() == "" || len(resp.GetEvents()) != 0 || resp.GetDrift() != pluginpb.Drift_DRIFT_DRIFTED ||
		!strings.Contains(resp.GetDetail(), "its delete is the brain's, not done yet") {
		t.Fatalf("its end again: %v", resp)
	}
	// unless its owner starts it: then it is theirs again
	if why := m.act("start", ""); why != "" || m.spec().Ending != "" || !m.running() {
		t.Fatalf("started by its owner: %s %+v", why, m.spec())
	}
	if resp := m.pass(time.Minute); resp.GetEnd() != "" {
		t.Fatalf("started, its end is undone: %v", resp)
	}

	// stopped for a moment of its own — a container rebooting itself —, it
	// is running again at the next look: nothing ends, and nothing is said
	// of it after
	m = born(t, small+`,"on_poweroff":"delete"}`)
	m.pass(time.Minute)
	m.offBehind()
	m.pass(time.Minute)
	at := m.e.Now()
	m.e.Tamper(m.r.GetId(), func(g *driver.Guest) { g.Running, g.StartedAt = true, &at })
	if resp := m.pass(30 * time.Second); resp.GetEnd() != "" || !m.running() || m.observed().Unasked != nil || resp.GetDrift() != pluginpb.Drift_DRIFT_IN_SYNC {
		t.Fatalf("running again: %v", resp)
	}
	m.offBehind()
	if resp := m.pass(time.Minute); resp.GetEnd() != "" {
		t.Fatalf("a power-off after it is seen anew, at two looks: %v", resp)
	}

	// a stop the brain asked that went wrong — the guest may be down all the
	// same —: its stop is the brain's, never its own
	m = born(t, small+`,"on_poweroff":"delete"}`)
	m.pass(time.Minute)
	m.e.Tamper(m.r.GetId(), func(g *driver.Guest) { g.Lock = "snapshot" })
	was := m.r.Spec
	if why := m.act("stop", ""); !strings.Contains(why, "locked (snapshot)") {
		t.Fatalf("a stop refused: %q", why)
	}
	m.r.Spec = was // as the core puts it back
	m.e.Tamper(m.r.GetId(), func(g *driver.Guest) { g.Lock, g.Running, g.StartedAt = "", false, nil })
	if resp := m.pass(time.Minute); resp.GetEnd() != "" || m.observed().Unasked != nil || !m.running() {
		t.Fatalf("a stop asked that went wrong: %v", resp)
	}
	m.offBehind() // seen running since: its next stop is its own again
	if resp := m.pass(time.Minute); m.observed().Unasked == nil {
		t.Fatalf("its own, after: %v", resp)
	}

	// a look that saw it running under its engine's work (a backup runs a
	// stopped VM paused) does not count as seen running
	m = born(t, small+`,"on_poweroff":"delete"}`)
	if why := m.act("stop", ""); why != "" {
		t.Fatal(why)
	}
	m.r.Spec = []byte(strings.Replace(string(m.r.Spec), `"running":false`, `"running":true`, 1)) // a start the engine refused
	m.e.Tamper(m.r.GetId(), func(g *driver.Guest) { at := t0; g.Running, g.StartedAt, g.Lock = true, &at, "backup" })
	if m.pass(time.Minute); m.observed().Lock != "backup" {
		t.Fatalf("the lock observed: %+v", m.observed())
	}
	m.e.Tamper(m.r.GetId(), func(g *driver.Guest) { g.Running, g.StartedAt, g.Lock = false, nil, "" })
	if resp := m.pass(time.Minute); m.observed().Unasked != nil || resp.GetEnd() != "" {
		t.Fatalf("seen running only under a lock: %v", resp)
	}

	// what the brain asked is never its own: a stop, a reboot
	m = born(t, small+`,"on_poweroff":"delete"}`)
	if why := m.act("stop", ""); why != "" {
		t.Fatal(why)
	}
	if resp := m.pass(time.Minute); resp.GetEnd() != "" || resp.GetDrift() != pluginpb.Drift_DRIFT_IN_SYNC {
		t.Fatalf("its owner's stop ended it: %v", resp)
	}
	if why := m.act("start", ""); why != "" {
		t.Fatal(why)
	}
	if why := m.act("reboot", ""); why != "" {
		t.Fatal(why)
	}
	if resp := m.pass(time.Minute); resp.GetEnd() != "" || !m.running() {
		t.Fatalf("a reboot ended it: %v", resp)
	}

	// nor is a hold's stop — the brain's, or one its node wrote alone
	m = born(t, small+`,"on_poweroff":"delete"}`)
	m.pass(time.Minute)
	m.r.Hold = "4100"
	if resp := m.pass(time.Minute); m.running() || resp.GetEnd() != "" {
		t.Fatalf("held: %v %v", m.running(), resp)
	}
	m.pass(time.Minute)
	m.r.Hold = ""
	if resp := m.pass(time.Minute); !m.running() || resp.GetEnd() != "" {
		t.Fatalf("the room back, it is started again: %v %v", m.running(), resp)
	}
	m.e.Tamper(m.r.GetId(), func(g *driver.Guest) { g.Running, g.StartedAt, g.Holds = false, nil, []string{"4100"} })
	if resp := m.pass(time.Minute); resp.GetEnd() != "" || m.observed().Unasked != nil {
		t.Fatalf("stopped by its node for a hold the brain did not know yet: %v", resp)
	}
	// and a hold the brain knows before its guest wears it
	m.pass(time.Minute)
	m.offBehind()
	m.r.Hold = "4100"
	if resp := m.pass(time.Minute); resp.GetEnd() != "" || m.observed().Unasked != nil {
		t.Fatalf("stopped under a hold the brain knows: %v", resp)
	}

	// nor its engine's own work (a backup, a migration): put back once done
	m = born(t, small+`,"on_poweroff":"delete"}`)
	m.pass(time.Minute)
	m.e.Tamper(m.r.GetId(), func(g *driver.Guest) { g.Running, g.StartedAt, g.Lock = false, nil, "backup" })
	for range 2 {
		if resp := m.pass(time.Minute); resp.GetEnd() != "" || resp.GetDrift() != pluginpb.Drift_DRIFT_DRIFTED || !strings.Contains(resp.GetDetail(), "locked (backup)") {
			t.Fatalf("locked: %v", resp)
		}
	}
	m.e.Tamper(m.r.GetId(), func(g *driver.Guest) { g.Lock = "" })
	if resp := m.pass(time.Minute); resp.GetEnd() != "" || !m.running() {
		t.Fatalf("its engine's work done, it is put back: %v %v", m.running(), resp)
	}

	// nor one the last look did not see running: nothing says it ran
	m = born(t, small+`,"on_poweroff":"delete"}`)
	m.offBehind()
	m.r.Observed = []byte(`{"engine_ref":"x","kind":"container","cores":2,"memory_mb":1024,"running":false}`)
	if resp := m.pass(time.Minute); resp.GetEnd() != "" || !m.running() {
		t.Fatalf("not seen running: %v", resp)
	}
}

// until ends a machine at a time — stopped, or deleted — running or not;
// for is an until from now. Its engine's work postpones it; it is cleared
// when it comes, so a start after it is not ended again.
func TestAMachinesUntil(t *testing.T) {
	m := born(t, small+`,"for":"2h"}`)
	if s := m.spec(); s.Until != t0.Add(2*time.Hour).Format(time.RFC3339) || s.AtEnd != "" {
		t.Fatalf("for, at its birth: %+v", s)
	}
	m.pass(119 * time.Minute)
	if !m.running() {
		t.Fatal("stopped before its until")
	}
	resp := m.pass(time.Minute)
	if m.running() || m.spec().Running || m.spec().Until != "" || resp.GetEnd() != "" ||
		!strings.Contains(resp.GetDetail(), "its until, 2026-10-10T10:00:00Z, has come: it is stopped") {
		t.Fatalf("its until come, at_end stop: %v %+v %v", m.running(), m.spec(), resp)
	}
	if why := m.act("start", ""); why != "" {
		t.Fatal(why)
	}
	if m.pass(time.Hour); !m.running() {
		t.Fatal("an until that came ended it again")
	}
	// a start past an until that has not been looked at yet is refused
	m = born(t, small+`,"for":"1h"}`)
	if why := m.act("stop", ""); why != "" {
		t.Fatal(why)
	}
	m.e.SetNow(m.e.Now().Add(2 * time.Hour))
	if why := m.act("start", ""); !strings.Contains(why, "/until its until, 2026-10-10T09:00:00Z, has passed") {
		t.Fatalf("a start past its until: %q", why)
	}

	// at_end delete — and one that is stopped already is deleted all the same
	m = born(t, small+`,"until":"2026-10-10T10:30:00+02:00","at_end":"delete","on_poweroff":"stop"}`)
	if s := m.spec(); s.Until != "2026-10-10T08:30:00Z" || s.OnPoweroff != OnPoweroffStop {
		t.Fatalf("written in UTC: %+v", s)
	}
	if resp := m.pass(30 * time.Minute); resp.GetEnd() == "" || m.running() {
		t.Fatalf("running, its until come: %v", resp)
	}
	m = born(t, small+`,"for":"30m","at_end":"delete"}`)
	if why := m.act("stop", ""); why != "" {
		t.Fatal(why)
	}
	m.pass(29 * time.Minute)
	resp = m.pass(time.Minute)
	if !strings.Contains(resp.GetEnd(), "has come: it is deleted") || resp.GetDrift() != pluginpb.Drift_DRIFT_REPAIRED {
		t.Fatalf("stopped, its until come: %v", resp)
	}
	for _, ev := range resp.GetEvents() {
		if ev.GetName() == "machine.ended" && (ev.GetFields()["by"] != "until" || ev.GetFields()["at_end"] != "delete") {
			t.Fatalf("its event: %v", ev)
		}
	}

	// its engine's work holds it: its end waits for that
	m = born(t, small+`,"for":"1h","at_end":"delete"}`)
	m.e.Tamper(m.r.GetId(), func(g *driver.Guest) { g.Lock = "migrate" })
	if resp := m.pass(2 * time.Hour); resp.GetEnd() != "" || m.spec().Until == "" {
		t.Fatalf("ended under a lock: %v", resp)
	}
	m.e.Tamper(m.r.GetId(), func(g *driver.Guest) { g.Lock = "" })
	if resp := m.pass(0); resp.GetEnd() == "" {
		t.Fatalf("the lock gone, its end comes: %v", resp)
	}

	// what is refused at the ask, with why
	p := configured(t, "fake", "")
	p.Driver("z").(interface{ SetNow(time.Time) }).SetNow(t0)
	for spec, want := range map[string]string{
		`,"until":"2026-10-10T07:59:59Z"}`:            "/until 2026-10-10T07:59:59Z has passed",
		`,"until":"tomorrow"}`:                        `/until "tomorrow" is no time`,
		`,"for":"soon"}`:                              `/for "soon" is no duration`,
		`,"for":"30s"}`:                               "/for 30s: between 1m and a year",
		`,"for":"8761h"}`:                             "/for 8761h: between 1m and a year",
		`,"for":"1h","until":"2026-10-11T00:00:00Z"}`: "/for until, or for — not both",
	} {
		if r, _ := plan(t, p, small+spec); !strings.HasPrefix(reasons(r), want) {
			t.Fatalf("%s: %q, want %q", spec, reasons(r), want)
		}
	}
}

// set_end changes a machine's end after its birth, one field at a time; a
// spec file asks for it as a step — an until only when it names one.
func TestSetEnd(t *testing.T) {
	m := born(t, small+`}`)
	if why := m.act("set_end", `{"on_poweroff":"delete","for":"90m","at_end":"delete"}`); why != "" {
		t.Fatal(why)
	}
	if s := m.spec(); s.OnPoweroff != OnPoweroffDelete || s.AtEnd != AtEndDelete || s.Until != t0.Add(90*time.Minute).Format(time.RFC3339) {
		t.Fatalf("%+v", s)
	}
	if !slices.Contains(m.events, "machine.end_set") {
		t.Fatalf("%v", m.events)
	}
	if why := m.act("set_end", `{"until":"never"}`); why != "" || m.spec().Until != "" || m.spec().OnPoweroff != OnPoweroffDelete {
		t.Fatalf("never, the rest kept: %s %+v", why, m.spec())
	}
	if why := m.act("set_end", `{"on_poweroff":"restart","at_end":"stop"}`); why != "" || m.spec().OnPoweroff != "" || m.spec().AtEnd != "" {
		t.Fatalf("back to the defaults: %s %+v", why, m.spec())
	}
	if why := m.act("set_end", `{"until":"2026-10-10T07:00:00Z"}`); !strings.Contains(why, "has passed") {
		t.Fatalf("an until gone by: %q", why)
	}

	// a spec file
	m = born(t, small+`,"on_poweroff":"delete","for":"2h"}`)
	change := func(spec string) *pluginpb.PlanChangeResponse {
		t.Helper()
		resp, err := m.p.PlanChange(context.Background(), &pluginpb.PlanChangeRequest{Current: m.r, Spec: []byte(small + spec)})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := change(`,"on_poweroff":"delete"}`); len(resp.GetSteps()) != 0 || len(resp.GetFixed()) != 0 {
		t.Fatalf("a file that names no until leaves the one an asker wrote: %v", resp)
	}
	if resp := change(`,"on_poweroff":"delete","for":"5h"}`); len(resp.GetSteps()) != 0 {
		t.Fatalf("for is a birth's: %v", resp)
	}
	if resp := change(`,"on_poweroff":"delete","until":"2026-10-09T00:00:00Z"}`); len(resp.GetSteps()) != 0 {
		t.Fatalf("an until that has passed asks nothing: %v", resp)
	}
	resp := change(`,"at_end":"delete","until":"2026-10-11T00:00:00Z"}`)
	if len(resp.GetSteps()) != 1 || resp.GetSteps()[0].GetAction() != "set_end" ||
		string(resp.GetSteps()[0].GetParams()) != `{"at_end":"delete","on_poweroff":"restart","until":"2026-10-11T00:00:00Z"}` {
		t.Fatalf("one step, what differs: %v", resp)
	}
}
