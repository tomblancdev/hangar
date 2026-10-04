package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tomblancdev/hangar/internal/audit"
	"github.com/tomblancdev/hangar/internal/plugins"
	"github.com/tomblancdev/hangar/internal/registry"
)

// ---- A reference's default: what a create names by itself ----------------
//
// A reference its schema marks "x-hangar-default": "<name>" that a create
// leaves out is given the owner's own resource of that type called <name> —
// AWS's default VPC: a household never learns the word network, and a
// person's machines see each other and nobody else's. It is made first when
// they have none: an ordinary create, in their name, within their tier,
// audited as theirs, tagged as the core's doing. Where no plugin makes that
// type in the zone, the field stays out and the resource is made as it
// always was.
//
// The default is what a NEW resource is given: a change's plan leaves an
// existing one where it stands (keepDefaults), as "@<schedule>" does.

// DefaultTag is the tag the core writes on what it made as a reference's
// default: its value the type and field it was made for.
const DefaultTag = "hangar:default"

// defaultWait is how long a create waits for the default it names to be
// made, before asking to be asked again.
const defaultWait = 75 * time.Second

// defaults writes into a create's spec the id of each default it leaves out,
// making what is not there. It returns the spec and what each stood for.
func (c *Core) defaults(ctx context.Context, who *Caller, t *plugins.Type, zone string, spec json.RawMessage) (json.RawMessage, []string, *Problem) {
	var fields map[string]json.RawMessage
	var given []string
	for _, ref := range t.Refs {
		if ref.Default == "" {
			continue
		}
		if fields == nil && json.Unmarshal(spec, &fields) != nil {
			return spec, nil, nil // not an object: the schema says so next
		}
		if _, said := fields[ref.Field]; said {
			continue
		}
		rt := c.host.Type(ref.Type)
		if rt == nil {
			continue
		}
		if ok, _ := c.host.Available(rt, zone); !ok {
			continue
		}
		id, p := c.defaultOf(ctx, who, t, ref, zone)
		if p != nil {
			return nil, nil, p
		}
		if fields == nil {
			fields = map[string]json.RawMessage{}
		}
		fields[ref.Field], _ = json.Marshal(id)
		given = append(given, ref.Default+"="+id)
	}
	if given == nil {
		return spec, nil, nil
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, nil, problem(500, KindInternal, "%v", err)
	}
	return out, given, nil
}

// defaultOf finds the owner's default for a reference in a zone — ready — or
// makes it, and waits for it.
func (c *Core) defaultOf(ctx context.Context, who *Caller, t *plugins.Type, ref plugins.Ref, zone string) (string, *Problem) {
	field := "/" + ref.Field
	deadline := time.Now().Add(defaultWait)
	var made bool
	for {
		id, err := c.store.Named(ctx, who.Subject, ref.Type, ref.Default)
		if err != nil {
			return "", problem(500, KindInternal, "%v", err)
		}
		if id == "" {
			if made {
				return "", about(problem(409, KindEngine, "your %s %s could not be made: name one, or ask again", ref.Type, ref.Default), field)
			}
			made = true
			if p := c.makeDefault(ctx, who, t, ref, zone, deadline); p != nil {
				return "", about(p, field)
			}
			continue
		}
		r, err := c.store.Resource(ctx, id)
		if err != nil {
			return "", problem(500, KindInternal, "%v", err)
		}
		switch {
		case r.Zone != zone:
			return "", about(problem(422, KindSchema, "your %s %s is in zone %s: name a %s of zone %s", ref.Type, ref.Default, r.Zone, ref.Type, zone), field)
		case r.State == registry.Ready:
			return id, nil
		case r.State != registry.Creating && r.State != registry.Updating:
			return "", about(problem(409, KindEngine, "your %s %s (%s) is %s: name another", ref.Type, ref.Default, id, r.State), field)
		case !time.Now().Before(deadline):
			return "", about(problem(409, KindBusy, "your %s %s (%s) is still being made: ask again in a moment", ref.Type, ref.Default, id), field)
		}
		select {
		case <-ctx.Done():
			return "", problem(499, KindBusy, "the request ended while your %s %s was being made", ref.Type, ref.Default)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// makeDefault asks for the owner's default, as the owner would, and waits
// for it to be made. A name taken meanwhile is another request making the
// same one: nothing to say.
func (c *Core) makeDefault(ctx context.Context, who *Caller, t *plugins.Type, ref plugins.Ref, zone string, deadline time.Time) *Problem {
	actx, rec := audit.WithRecord(ctx)
	op, r, _, err := c.create(actx, who, ref.Type,
		CreateInput{Zone: zone, Name: ref.Default, Spec: json.RawMessage("{}")},
		map[string]string{DefaultTag: t.Name + "." + ref.Field})
	e := rec.Event()
	e.Action, e.Actor, e.Name, e.Tier, e.Via = "default", who.Subject, who.Name, who.Tier.Name, who.Via
	if e.Result == "" {
		e.Result = "refused"
	}
	var p *Problem
	if errors.As(err, &p) {
		e.Detail = p.Detail
	}
	c.audit.Write(e)
	switch {
	case err == nil:
	case p != nil && p.Kind == KindConflict:
		return nil // someone made it at the same moment: found at the next look
	case p != nil:
		refused := *p
		refused.Detail = fmt.Sprintf("it names no %s, and your %s called %s could not be made: %s", ref.Type, ref.Type, ref.Default, p.Detail)
		return &refused
	default:
		return problem(500, KindInternal, "%v", err)
	}
	done, err := c.Operation(ctx, who, op.ID, time.Until(deadline))
	if err != nil {
		return problem(500, KindInternal, "%v", err)
	}
	switch done.State {
	case registry.OpSucceeded:
		return nil
	case registry.OpRunning:
		return problem(409, KindBusy, "your %s %s (%s) is still being made: ask again in a moment", ref.Type, ref.Default, r.ID)
	}
	return problem(409, KindEngine, "it names no %s, and your %s called %s could not be made: %s", ref.Type, ref.Type, ref.Default, done.Error)
}

// keepDefaults leaves an existing resource where it stands: a reference with
// a default that the wanted spec leaves out is wanted as the resource has it
// — named, or not at all. The default is a new resource's.
func keepDefaults(refs []plugins.Ref, want, now json.RawMessage) json.RawMessage {
	var fields, cur map[string]json.RawMessage
	if json.Unmarshal(want, &fields) != nil {
		return want
	}
	_ = json.Unmarshal(now, &cur)
	changed := false
	for _, ref := range refs {
		if ref.Default == "" {
			continue
		}
		if _, said := fields[ref.Field]; said {
			continue
		}
		if has, ok := cur[ref.Field]; ok {
			fields[ref.Field], changed = has, true
		}
	}
	if !changed {
		return want
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return want
	}
	return out
}
