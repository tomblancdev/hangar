package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/tomblancdev/hangar/internal/audit"
	"github.com/tomblancdev/hangar/internal/config"
	"github.com/tomblancdev/hangar/internal/identity"
	"github.com/tomblancdev/hangar/internal/ids"
	"github.com/tomblancdev/hangar/internal/plugins"
	"github.com/tomblancdev/hangar/internal/registry"
)

// ScheduleTag marks what a schedule made, with the schedule's name. Keys
// starting with hangar are the core's: nobody else writes one.
const ScheduleTag = "hangar:schedule"

// CheckSchedules holds the operator's schedules against what the plugins
// declared: a type some plugin makes, a spec its schema takes, a retire
// action it has — refused at start, not found out at the first run a week
// later.
func CheckSchedules(cfg *config.Config, host *plugins.Host) error {
	var errs []error
	for _, sc := range cfg.Schedules {
		t := host.Type(sc.Create.Type)
		if t == nil {
			errs = append(errs, fmt.Errorf("schedule %s: no plugin here makes the type %s", sc.Name, sc.Create.Type))
			continue
		}
		spec, err := specOf(sc)
		if err != nil {
			errs = append(errs, fmt.Errorf("schedule %s: create.spec: %v", sc.Name, err))
			continue
		}
		if v := t.Validate(spec); len(v) > 0 {
			errs = append(errs, fmt.Errorf("schedule %s: the spec does not fit type %s: %s %s", sc.Name, t.Name, v[0].Field, v[0].Reason))
		}
		if sc.Retire != "" {
			a := t.Action(sc.Retire)
			if a == nil {
				errs = append(errs, fmt.Errorf("schedule %s: type %s has no action %q to retire with", sc.Name, t.Name, sc.Retire))
			} else if v := a.Validate(json.RawMessage("{}")); len(v) > 0 {
				errs = append(errs, fmt.Errorf("schedule %s: action %s needs params: it cannot retire alone", sc.Name, sc.Retire))
			}
		}
	}
	return errors.Join(errs...)
}

func specOf(sc config.Schedule) (json.RawMessage, error) {
	if len(sc.Create.Spec) == 0 {
		return json.RawMessage("{}"), nil
	}
	return json.Marshal(sc.Create.Spec)
}

// ScheduleOnce runs every schedule whose time came since it last ran — once,
// however many times it came while the brain was away — and lets go of what
// each made beyond what it keeps.
func (c *Core) ScheduleOnce(ctx context.Context) {
	now := c.Now()
	for i := range c.cfg.Schedules {
		if ctx.Err() != nil {
			return
		}
		sc := &c.cfg.Schedules[i]
		c.runSchedule(ctx, sc, now)
		c.keep(ctx, sc)
	}
}

// runSchedule asks for what a schedule makes if its time came.
func (c *Core) runSchedule(ctx context.Context, sc *config.Schedule, now time.Time) {
	st, err := c.store.Schedule(ctx, sc.Name)
	if errors.Is(err, registry.ErrNotFound) {
		// a schedule seen for the first time waits for its first time, as a
		// new Kubernetes CronJob does
		st = &registry.ScheduleState{Name: sc.Name, Since: now}
		if err := c.store.PutSchedule(ctx, st); err != nil {
			c.log.Error("schedule: its clock could not be written", "schedule", sc.Name, "err", err)
			return
		}
		c.audit.Write(audit.Event{Action: "schedule", Actor: "hangar", Via: "schedule:" + sc.Name, Type: sc.Create.Type, Zone: sc.Create.Zone,
			Result: "armed", Detail: "first run " + sc.Line.Next(now, sc.Location).Format(time.RFC3339)})
		return
	}
	if err != nil {
		c.log.Error("schedule: its clock could not be read", "schedule", sc.Name, "err", err)
		return
	}
	due, came := sc.Line.Last(st.Since, now, sc.Location)
	if !came {
		return
	}
	result, detail, id, e := c.fire(ctx, sc, due)
	st.Since, st.LastRun = now, &now
	st.LastResult, st.LastDetail, st.LastResource = result, detail, id
	if err := c.store.PutSchedule(ctx, st); err != nil {
		c.log.Error("schedule: its run could not be written", "schedule", sc.Name, "err", err)
	}
	// a new run: what could not be let go of is tried again
	c.schedMu.Lock()
	delete(c.tried, sc.Name)
	c.schedMu.Unlock()
	c.metrics.Inc("hangar_schedule_runs_total", sc.Name, result)
	e.Action, e.Via, e.Result, e.Detail = "schedule", "schedule:"+sc.Name, result, detail
	if e.Actor == "" {
		e.Actor = sc.As.Subject
	}
	if e.Fields == nil {
		e.Fields = map[string]string{}
	}
	e.Fields["due"] = due.Format(time.RFC3339)
	c.audit.Write(e)
}

// fire asks for a schedule's create in the name it names — one at a time:
// while what it made last is still being made, this run is skipped (a
// Kubernetes CronJob's Forbid). Its client token is the run's own, so a
// brain that died between the ask and writing it down asks the same again
// and gets the first answer.
func (c *Core) fire(ctx context.Context, sc *config.Schedule, due time.Time) (result, detail, id string, e audit.Event) {
	e = audit.Event{Type: sc.Create.Type, Zone: sc.Create.Zone}
	outs, err := c.outputs(ctx, sc)
	if err != nil {
		return "failed", err.Error(), "", e
	}
	for _, r := range outs {
		if r.State == registry.Creating || r.Pending {
			return "skipped", r.ID + " is still being made", r.ID, e
		}
	}
	who, p := c.scheduleCaller(ctx, sc)
	if p != nil {
		return "refused", p.Detail, "", e
	}
	spec, err := specOf(*sc)
	if err != nil {
		return "failed", err.Error(), "", e
	}
	sum := sha256.Sum256([]byte(sc.Name + "@" + due.UTC().Format(time.RFC3339)))
	actx, rec := audit.WithRecord(ctx)
	op, r, replayed, err := c.create(actx, who, sc.Create.Type,
		CreateInput{Zone: sc.Create.Zone, Spec: spec, Tags: sc.Create.Tags, ClientToken: "schedule-" + hex.EncodeToString(sum[:12])},
		map[string]string{ScheduleTag: sc.Name})
	e = rec.Event()
	e.Actor, e.Name, e.Tier = who.Subject, who.Name, who.Tier.Name
	if err != nil {
		var p *Problem
		if errors.As(err, &p) {
			return "refused", p.Detail, "", e
		}
		return "failed", err.Error(), "", e
	}
	detail = "operation " + op.ID
	if replayed {
		detail += " (asked before the brain stopped)"
	}
	return "asked", detail, r.ID, e
}

// scheduleCaller is the one a schedule acts for, with their tier as the file
// says it now.
func (c *Core) scheduleCaller(ctx context.Context, sc *config.Schedule) (*Caller, *Problem) {
	return c.Caller(ctx, &identity.Identity{Subject: sc.As.Subject, Name: "schedule " + sc.Name, Groups: sc.As.Groups,
		Via: "schedule", Scopes: []string{identity.ScopeRead, identity.ScopeWrite}})
}

// outputs lists what a schedule made that still holds something, oldest
// first.
func (c *Core) outputs(ctx context.Context, sc *config.Schedule) ([]*registry.Resource, error) {
	var out []*registry.Resource
	var after int64
	for {
		rs, next, err := c.store.Resources(ctx, registry.Filter{Type: sc.Create.Type, Tags: map[string]string{ScheduleTag: sc.Name},
			States: registry.Live, After: after, Limit: 500})
		if err != nil {
			return nil, err
		}
		out = append(out, rs...)
		if next == 0 {
			return out, nil
		}
		after = next
	}
}

// keep lets go of what a schedule made beyond what it keeps, newest first:
// the Keep newest usable ones stay as they are; an older usable one is given
// the schedule's retire action — only once newer ones are usable, never
// before (AWS Image Builder's lifecycle: deprecate, then delete); one that
// is not usable (retired, failed) or that its engine lost is deleted once a
// newer one is usable and nothing names it any more. One still being made,
// or with an operation running, is left alone. What could not be let go of
// is tried again at the schedule's next run.
func (c *Core) keep(ctx context.Context, sc *config.Schedule) {
	outs, err := c.outputs(ctx, sc)
	if err != nil {
		c.log.Error("schedule: what it made could not be listed", "schedule", sc.Name, "err", err)
		return
	}
	newer := 0 // usable ones newer than the one looked at
	for i := len(outs) - 1; i >= 0; i-- {
		r := outs[i]
		switch {
		case slices.Contains(registry.Busy, r.State) || r.Pending:
		case r.State == registry.Ready && r.Unusable == "":
			if newer >= sc.Keep {
				if sc.Retire != "" {
					c.letGo(ctx, sc, r, sc.Retire)
				} else {
					c.letGoUnnamed(ctx, sc, r)
				}
			}
			newer++
		case newer > 0:
			c.letGoUnnamed(ctx, sc, r)
		}
	}
}

// letGoUnnamed deletes what a schedule made once nothing names it any more
// (a machine born from an image names it while it lives).
func (c *Core) letGoUnnamed(ctx context.Context, sc *config.Schedule, r *registry.Resource) {
	var named []string
	err := c.store.Tx(ctx, func(tx *registry.Tx) error {
		froms, err := tx.Referrers(r.ID)
		for _, f := range froms {
			if holding(f.State) && !slices.Contains(named, f.ID) {
				named = append(named, f.ID)
			}
		}
		return err
	})
	if err != nil || len(named) > 0 {
		return
	}
	c.letGo(ctx, sc, r, "")
}

// letGo asks, in the schedule's name, for an action on what it made — or
// for its delete (action "") — once per run of the schedule.
func (c *Core) letGo(ctx context.Context, sc *config.Schedule, r *registry.Resource, action string) {
	key := r.ID + "/" + action
	c.schedMu.Lock()
	if c.tried[sc.Name] == nil {
		c.tried[sc.Name] = map[string]bool{}
	}
	if c.tried[sc.Name][key] {
		c.schedMu.Unlock()
		return
	}
	c.tried[sc.Name][key] = true
	c.schedMu.Unlock()
	who, p := c.scheduleCaller(ctx, sc)
	actx, rec := audit.WithRecord(ctx)
	verb := "delete"
	if p == nil {
		if action == "" {
			_, _, _, err := c.Delete(actx, who, r.ID, "")
			p = asProblem(err)
		} else {
			verb = action
			_, _, _, err := c.Act(actx, who, r.ID, action, ActInput{})
			p = asProblem(err)
		}
	}
	e := rec.Event()
	e.Action, e.Actor, e.Via, e.Resource, e.Type, e.Zone = "schedule", sc.As.Subject, "schedule:"+sc.Name, r.ID, r.Type, r.Zone
	e.Fields = map[string]string{"let_go": verb}
	if p != nil {
		e.Result, e.Detail = "refused", p.Detail
	} else if e.Result == "" {
		e.Result = "accepted"
	}
	c.audit.Write(e)
}

func asProblem(err error) *Problem {
	if err == nil {
		return nil
	}
	var p *Problem
	if errors.As(err, &p) {
		return p
	}
	return problem(500, KindInternal, "%v", err)
}

// ---- "@<schedule>": the newest usable one it made ----------------------

// resolve writes, in a spec or an action's params, every reference given as
// "@<schedule>" as the id of the newest usable resource that schedule made
// which the owner may name — AWS's resolve:ssm, Google's image families. The
// latest is the publisher's pointer, never a search by name that anyone
// could answer with a look-alike. The request keeps the id: a machine says
// what it was born from, and never moves under it.
//
// And every reference given as a name, as the id of what its owner calls so
// (names.go): a name goes wherever an id goes, and the registry keeps ids.
func (c *Core) resolve(ctx context.Context, owner string, groups []string, zone string, refs []plugins.Ref, doc json.RawMessage) (json.RawMessage, []string, *Problem) {
	var fields map[string]json.RawMessage
	if len(refs) == 0 || json.Unmarshal(doc, &fields) != nil {
		return doc, nil, nil
	}
	var resolved []string
	var bad []plugins.Violation
	one := func(ref plugins.Ref, field, v string) string {
		var id, why string
		switch {
		case v == "" || ids.Valid(v):
			return v
		case strings.HasPrefix(v, "@"):
			id, why = c.latest(ctx, owner, groups, zone, ref, strings.TrimPrefix(v, "@"))
		default:
			// a name where an id goes: what its owner calls so
			id, why = c.named(ctx, owner, ref, v)
		}
		if why != "" {
			bad = append(bad, plugins.Violation{Field: field, Reason: v + ": " + why})
			return v
		}
		resolved = append(resolved, v+"="+id)
		return id
	}
	changed := false
	for _, ref := range refs {
		raw, ok := fields[ref.Field]
		if !ok {
			continue
		}
		if ref.Many {
			var vs []string
			if json.Unmarshal(raw, &vs) != nil {
				continue
			}
			out := make([]string, len(vs))
			for i, v := range vs {
				out[i] = one(ref, fmt.Sprintf("/%s/%d", ref.Field, i), v)
			}
			if !slices.Equal(out, vs) {
				fields[ref.Field], _ = json.Marshal(out)
				changed = true
			}
			continue
		}
		var v string
		if json.Unmarshal(raw, &v) != nil {
			continue
		}
		if id := one(ref, "/"+ref.Field, v); id != v {
			fields[ref.Field], _ = json.Marshal(id)
			changed = true
		}
	}
	if len(bad) > 0 {
		p := problem(422, KindSchema, "%s: %s", bad[0].Field, bad[0].Reason)
		p.Violations = bad
		return nil, nil, p
	}
	if !changed {
		return doc, nil, nil
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, nil, problem(500, KindInternal, "%v", err)
	}
	return out, resolved, nil
}

// latest is the newest usable resource a schedule made that the owner may
// name there, or why there is none.
func (c *Core) latest(ctx context.Context, owner string, groups []string, zone string, ref plugins.Ref, name string) (string, string) {
	sc, ok := c.cfg.Schedule(name)
	switch {
	case !ok:
		return "", "no schedule " + name + " here"
	case sc.Create.Type != ref.Type:
		return "", fmt.Sprintf("schedule %s makes %ss, not %ss", name, sc.Create.Type, ref.Type)
	case sc.Create.Zone != zone:
		return "", fmt.Sprintf("schedule %s makes them in zone %s, not %s", name, sc.Create.Zone, zone)
	}
	outs, err := c.outputs(ctx, sc)
	if err != nil {
		return "", "the registry cannot be read"
	}
	for i := len(outs) - 1; i >= 0; i-- {
		r := outs[i]
		mine := r.Owner == owner || !ref.Attached && sharedWith(r, groups)
		if r.State == registry.Ready && r.Unusable == "" && mine {
			return r.ID, ""
		}
	}
	return "", fmt.Sprintf("schedule %s has made no usable %s you may name yet", name, ref.Type)
}

// withTags returns the tags a request asked for, with the core's own.
func withTags(asked, core map[string]string) map[string]string {
	if len(core) == 0 {
		return asked
	}
	out := maps.Clone(asked)
	if out == nil {
		out = map[string]string{}
	}
	maps.Copy(out, core)
	return out
}
