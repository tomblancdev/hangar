package machines

// A machine's end — what the brain knows of it, beside its owner's delete:
//
//   - on_poweroff: what its own power-off means. A guest that stops while its
//     spec says it runs, with nothing of the brain's to explain it (no hold,
//     no lock of its engine's own work, seen running at the last look), is
//     started again (restart, as ever), left stopped (stop: its spec says so
//     from then on), or deleted (delete). EC2's
//     InstanceInitiatedShutdownBehavior, in the product's words. Stop and
//     delete are applied only when it is found so at two looks at least
//     confirmAfter apart: the first leaves it stopped and says so — a guest
//     stopped for a moment of its own (a container rebooting itself) is
//     running again by the next.
//   - until and at_end: when the brain ends it at the latest, and how — stopped
//     (stop) or deleted (delete), running or not. Compute Engine's
//     terminationTime and instanceTerminationAction. for, at its birth or by
//     set_end, is an until written as a time from now.
//
// Its own power-off cannot be told, from what the brain reads, from a stop
// below it — its node's, or a hand's on the engine: Proxmox VE runs neither
// through anything the product's token sees (docs/proxmox.md, « A machine's
// end »). So any stop nobody asked here is its own, and its event says
// which it could have been.
//
// A delete is the core's to carry out (ReconcileResponse.end). Until the
// machine is gone its spec says so (Ending), and every look hands its end
// over again — a delete refused while a volume is attached to it is done
// once the volume goes; one its engine failed, at the next look.

import (
	"cmp"
	"errors"
	"fmt"
	"time"

	"github.com/tomblancdev/hangar/driver"
	"github.com/tomblancdev/hangar/sdk"
	"github.com/tomblancdev/hangar/sdk/pluginpb"
)

// What a machine's own power-off means (Spec.OnPoweroff); restart is
// written "" in a spec.
const (
	OnPoweroffRestart = "restart"
	OnPoweroffStop    = "stop"
	OnPoweroffDelete  = "delete"
)

// What its until does (Spec.AtEnd); stop is written "" in a spec.
const (
	AtEndStop   = "stop"
	AtEndDelete = "delete"
)

// maxFor is the longest for: a year.
const maxFor = 365 * 24 * time.Hour

// confirmAfter: how long a machine is found stopped, nobody having asked it
// here, before its on_poweroff stop or delete is applied.
const confirmAfter = 30 * time.Second

// errPassed: an until that is no time to come.
var errPassed = errors.New("has passed: a time to come")

const setEndSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "minProperties": 1,
  "properties": {
    "on_poweroff": { "type": "string", "enum": ["restart", "stop", "delete"],
                     "description": "What its own power-off means: restart, it is started again; stop, it stays stopped; delete, it is deleted, its disk with it." },
    "until":       { "type": "string", "maxLength": 40,
                     "description": "When it ends at the latest — a time, 2026-10-20T18:30:00Z; never: no end. Or for." },
    "for":         { "type": "string", "maxLength": 16,
                     "description": "Its until, as a time from now (2h, 90m — 1m to a year)." },
    "at_end":      { "type": "string", "enum": ["stop", "delete"],
                     "description": "What its until does: stop, it is stopped and stays so; delete, it is deleted, its disk with it." }
  }
}`

// endParams say a machine's end: a create's fields, or set_end's params. A
// field left out leaves what the spec has.
type endParams struct {
	OnPoweroff *string `json:"on_poweroff"`
	Until      *string `json:"until"`
	For        *string `json:"for"`
	AtEnd      *string `json:"at_end"`
}

// setEnd writes on a spec what end params name, or says why not.
func setEnd(s *Spec, e endParams, now time.Time) []*pluginpb.Refusal {
	var out []*pluginpb.Refusal
	if e.OnPoweroff != nil {
		switch v := *e.OnPoweroff; v {
		case "", OnPoweroffRestart:
			s.OnPoweroff = ""
		case OnPoweroffStop, OnPoweroffDelete:
			s.OnPoweroff = v
		default:
			out = append(out, &pluginpb.Refusal{Field: "/on_poweroff", Reason: fmt.Sprintf("no on_poweroff %q: restart, stop or delete", v)})
		}
	}
	if e.AtEnd != nil {
		switch v := *e.AtEnd; v {
		case "", AtEndStop:
			s.AtEnd = ""
		case AtEndDelete:
			s.AtEnd = v
		default:
			out = append(out, &pluginpb.Refusal{Field: "/at_end", Reason: fmt.Sprintf("no at_end %q: stop or delete", v)})
		}
	}
	switch {
	case e.Until != nil && e.For != nil:
		out = append(out, &pluginpb.Refusal{Field: "/for", Reason: "until, or for — not both"})
	case e.For != nil:
		d, err := time.ParseDuration(*e.For)
		switch {
		case err != nil:
			out = append(out, &pluginpb.Refusal{Field: "/for", Reason: fmt.Sprintf("%q is no duration: 2h, 90m", *e.For)})
		case d < time.Minute || d > maxFor:
			out = append(out, &pluginpb.Refusal{Field: "/for", Reason: fmt.Sprintf("%s: between 1m and a year", *e.For)})
		default:
			s.Until = now.Add(d).UTC().Truncate(time.Second).Format(time.RFC3339)
		}
	case e.Until != nil:
		u, err := parseUntil(*e.Until, now)
		if err != nil {
			out = append(out, &pluginpb.Refusal{Field: "/until", Reason: err.Error()})
		} else {
			s.Until = u
		}
	}
	return out
}

// parseUntil reads an until as a spec keeps it — a time to come, in UTC to
// the second — or none ("" or never).
func parseUntil(v string, now time.Time) (string, error) {
	if v == "" || v == "never" {
		return "", nil
	}
	t, err := time.Parse(time.RFC3339, v)
	switch {
	case err != nil:
		return "", fmt.Errorf("%q is no time: 2026-10-20T18:30:00Z — or never", v)
	case !t.After(now):
		return "", fmt.Errorf("%s %w", v, errPassed)
	}
	return t.UTC().Truncate(time.Second).Format(time.RFC3339), nil
}

// end is a machine's end as a look found it.
type end struct {
	why    string
	event  *pluginpb.Event
	delete bool // and not only stopped
	// again: its end came at an earlier look, said then; the core is handed
	// its delete again
	again bool
	// unasked: since when it is found stopped, nobody having asked it here,
	// its on_poweroff not applied yet — left stopped meanwhile
	unasked *time.Time
}

// ending says whether a machine came to its end at this look — its until
// passed, or its own power-off that its on_poweroff does not undo — and
// writes on its spec what that changes: its until cleared, its Ending. last
// is the machine as the look before saw it; asked, that the brain asked it a
// stop or a reboot that did not end well since it was last seen running —
// a stop of its then is the brain's. Nothing ends a guest its engine's own
// work holds: its end waits for that work to be done.
func ending(s *Spec, guest driver.Guest, hold string, last Observed, asked bool) end {
	if guest.Lock != "" {
		return end{}
	}
	if s.Ending != "" && !s.Running {
		return end{why: s.Ending, delete: true, again: true}
	}
	now := readAt(guest)
	if u, err := time.Parse(time.RFC3339, s.Until); err == nil && !now.Before(u) {
		at := cmp.Or(s.AtEnd, AtEndStop)
		s.Until = ""
		e := end{delete: at == AtEndDelete, why: fmt.Sprintf("its until, %s, has come: it is stopped", u.UTC().Format(time.RFC3339))}
		if e.delete {
			e.why = fmt.Sprintf("its until, %s, has come: it is deleted", u.UTC().Format(time.RFC3339))
			s.Ending = e.why
		}
		e.event = sdk.Event("machine.ended", e.why, map[string]string{"by": "until", "at_end": at})
		return e
	}
	// stopped, and nothing of the brain's says why: not a hold — the brain's,
	// or one its node wrote alone before the brain knew —, not a stop it
	// asked (the spec would say so, or it asked one that went wrong); and it
	// ran when last looked at — not paused under its engine's work —, or was
	// found so then already
	own := s.Running && !guest.Running && len(guest.Holds) == 0 && wanted(*s, hold).running && !asked
	switch {
	case !own || s.OnPoweroff == "":
		return end{}
	case last.Unasked == nil && last.Running && last.Lock == "":
		return end{unasked: &now}
	case last.Unasked == nil:
		return end{} // nothing says it ran: started, as ever
	case now.Sub(*last.Unasked) < confirmAfter:
		return end{unasked: last.Unasked}
	case s.OnPoweroff == OnPoweroffStop:
		why := "it stopped, and nobody asked it here (its own power-off, or its node's): its on_poweroff says stop, and it stays stopped"
		return end{why: why, event: sdk.Event("machine.stopped_itself", why, nil)}
	}
	why := "it stopped, and nobody asked it here (its own power-off, or its node's): its on_poweroff says delete"
	s.Ending = why
	return end{why: why, delete: true, event: sdk.Event("machine.ended", why, map[string]string{"by": "poweroff", "at_end": AtEndDelete})}
}

// askedDown marks a machine the brain asked a stop or a reboot of that did
// not end well: at the next look, a stop of it is the brain's.
// Kept in this process alone — a brain started anew does not know it.
func (p *Plugin) askedDown(id string) {
	p.amu.Lock()
	defer p.amu.Unlock()
	if p.asked == nil {
		p.asked = map[string]bool{}
	}
	p.asked[id] = true
}

// tookAskedDown says whether a machine is so marked, for this look alone:
// the look puts it back to what its spec says, and says what it found.
func (p *Plugin) tookAskedDown(id string) bool {
	p.amu.Lock()
	defer p.amu.Unlock()
	was := p.asked[id]
	delete(p.asked, id)
	return was
}
