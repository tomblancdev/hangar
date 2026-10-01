package machines

import (
	"fmt"
	"slices"
	"time"

	"github.com/tomblancdev/hangar/driver"
	"github.com/tomblancdev/hangar/sdk/pluginpb"
)

// ---- Power: the hours a machine runs, and its idle stop (§6) ----------------
//
// The hours are read from the engine: since when a guest runs, and when it
// was read. Each look counts the time it ran since the last one, once per
// core, and keeps how far it has counted in the observed state — which the
// core writes in the same transaction as the amount, so an answer it could
// not write is counted again from the same point. What ran while nobody
// looked is counted at the next look, as long as the guest still runs.
//
// Idleness is read from the engine too (driver.Activity): a machine with an
// idle_after is stopped once its CPU and what it sends stayed quiet that
// long — and stays stopped: its owner starts it. Not while its room is held,
// not while it is kept awake, and never on a history that cannot be read.

// VCPUHours is the meter a running machine draws on: its cores, per hour.
const VCPUHours = "machines.vcpu_hours"

// What an idle_after and a keep_awake may be.
const (
	minIdleAfter = 5 * time.Minute
	maxIdleAfter = 12 * time.Hour // the engines keep a guest's fine history for a day
	maxKeepAwake = 168 * time.Hour
	always       = "always"
)

// The thresholds a machine stays under to count as idle, unless the operator
// says otherwise — read on a Debian 13 guest of Proxmox VE: an idle VM sits
// at 0.008 cores and sends under 3 bytes a second; 20 s of one busy core
// reads 0.33 in its minute; one small packet a second, 50 to 100.
const (
	defaultIdleCPU     = 0.05
	defaultIdleSentBps = 20
)

// parseIdleAfter reads an idle_after: "" and "never" are none (0).
func parseIdleAfter(v string) (time.Duration, error) {
	if v == "" || v == "never" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	switch {
	case err != nil:
		return 0, fmt.Errorf("%q is no duration: 30m, 2h, 1h30m — or never", v)
	case d%time.Minute != 0:
		return 0, fmt.Errorf("%s: whole minutes", v)
	case d < minIdleAfter || d > maxIdleAfter:
		return 0, fmt.Errorf("%s: between %s and %s — the engine's history is read a minute at a time, and kept fine for a day",
			v, fmtIdleAfter(minIdleAfter), fmtIdleAfter(maxIdleAfter))
	}
	return d, nil
}

// fmtIdleAfter writes a duration of whole minutes the short way: 30m, 2h,
// 1h30m; none is "".
func fmtIdleAfter(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
	switch {
	case m == 0:
		return fmt.Sprintf("%dh", h)
	case h == 0:
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}

// setIdleAfter writes an idle_after on a spec, in its short form, or says
// why not: no duration, or a zone whose engine keeps no history to read.
func (p *Plugin) setIdleAfter(s *Spec, v, zone string, caps []driver.Capability) *pluginpb.Refusal {
	d, err := parseIdleAfter(v)
	if err != nil {
		return &pluginpb.Refusal{Field: "/idle_after", Reason: err.Error()}
	}
	if _, reads := p.Driver(zone).(driver.Activity); d > 0 && (!reads || !slices.Contains(caps, driver.GuestActivity)) {
		return &pluginpb.Refusal{Field: "/idle_after", Reason: fmt.Sprintf("zone %s keeps no history of its guests' CPU and network: nothing there says a machine is idle", zone)}
	}
	s.IdleAfter = fmtIdleAfter(d)
	return nil
}

// keepAwake is what a keep_awake writes on a machine: "always", or the time
// it ends — or why not.
func keepAwake(s Spec, forHow string, now time.Time) (string, *pluginpb.Refusal) {
	switch {
	case !s.Running:
		return "", &pluginpb.Refusal{Reason: "it is stopped: start it, then keep it awake"}
	case s.IdleAfter == "":
		return "", &pluginpb.Refusal{Reason: "it has no idle_after: nothing stops it for idleness, and nothing needs holding off"}
	case forHow == "":
		return always, nil
	}
	d, err := time.ParseDuration(forHow)
	switch {
	case err != nil:
		return "", &pluginpb.Refusal{Field: "/for", Reason: fmt.Sprintf("%q is no duration: 8h, 90m — or none, until let_sleep", forHow)}
	case d < time.Minute || d > maxKeepAwake:
		return "", &pluginpb.Refusal{Field: "/for", Reason: fmt.Sprintf("%s: between 1m and %s — or none, until let_sleep", forHow, fmtIdleAfter(maxKeepAwake))}
	}
	return now.Add(d).UTC().Truncate(time.Second).Format(time.RFC3339), nil
}

// keptAwake says whether a machine is kept from its idle stop at a time.
func keptAwake(s Spec, at time.Time) bool {
	if s.Awake == "" {
		return false
	}
	if s.Awake == always {
		return true
	}
	until, err := time.Parse(time.RFC3339, s.Awake)
	return err == nil && at.Before(until)
}

// now is the clock a zone's times are read by: its engine's where it has one
// of its own (the fake engine's, which tests move), this process's otherwise.
func (p *Plugin) now(zone string) time.Time {
	if c, ok := p.Driver(zone).(interface{ Now() time.Time }); ok {
		return c.Now()
	}
	return time.Now().UTC()
}

// quiet is under what a machine counts as idle here.
func (p *Plugin) quiet() driver.Quiet {
	p.mu.RLock()
	defer p.mu.RUnlock()
	q := driver.Quiet{CPU: defaultIdleCPU, SentBps: defaultIdleSentBps}
	if v := p.settings.Idle.CPU; v != nil {
		q.CPU = *v
	}
	if v := p.settings.Idle.SentBps; v != nil {
		q.SentBps = *v
	}
	return q
}

// readAt is when a guest was read.
func readAt(g driver.Guest) time.Time {
	if g.At.IsZero() {
		return time.Now().UTC()
	}
	return g.At.UTC()
}

// count says what a machine consumed up to a reading of its guest, since
// the plugin last counted (prev.CountedAt): the time it ran, once per core.
// A guest that does not run consumed nothing; one whose start the engine
// does not say is counted from the last look.
func count(prev Observed, g driver.Guest) (*pluginpb.Consumed, time.Time) {
	at := readAt(g)
	if !g.Running {
		return nil, at
	}
	from := prev.CountedAt
	if g.StartedAt != nil && (from == nil || g.StartedAt.After(*from)) {
		from = g.StartedAt
	}
	if from == nil || !at.After(*from) {
		return nil, at
	}
	return &pluginpb.Consumed{Amounts: map[string]float64{VCPUHours: at.Sub(*from).Hours() * float64(g.Cores)}}, at
}

// fmtQuiet writes how long a machine has been quiet, in whole minutes.
func fmtQuiet(d time.Duration) string {
	if d < time.Minute {
		return "0m"
	}
	return fmtIdleAfter(d.Truncate(time.Minute))
}
