package core

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/tomblancdev/hangar/internal/audit"
	"github.com/tomblancdev/hangar/internal/identity"
	"github.com/tomblancdev/hangar/internal/plugins"
	"github.com/tomblancdev/hangar/internal/registry"
	"github.com/tomblancdev/hangar/sdk/pluginpb"
)

// ---- A change's plan: what brings a resource to another spec ------------

// Step is one action a change takes, asked afterwards as any action is.
type Step struct {
	Action string          `json:"action"`
	Params json.RawMessage `json:"params,omitempty"`
}

// ChangePlan is what brings a resource to a spec: the steps, in order, or
// the fields no action changes. Neither = it is there.
type ChangePlan struct {
	Resource string              `json:"resource"`
	Steps    []Step              `json:"steps"`
	Fixed    []plugins.Violation `json:"fixed,omitempty"`
	// Resolved: what each "@<schedule>" of the spec stood for.
	Resolved []string `json:"resolved,omitempty"`
}

// PlanChange says what brings a resource to the spec — the steps its plugin
// names, each then asked as any action is (planned, admitted, audited), or
// the fields set at its birth. It changes nothing.
//
// A reference written "@<schedule>" is where the resource already is when
// what it names was made by that schedule: a machine born from last week's
// image is what "@debian-13" asks for (I22) — the latest names what a new
// machine is born from, and never moves one that exists.
func (c *Core) PlanChange(ctx context.Context, who *Caller, id string, spec json.RawMessage) (*ChangePlan, error) {
	if !who.Can(identity.ScopeRead) {
		return nil, c.refused(ctx, problem(403, KindScope, "this token cannot read"))
	}
	r, t, p := c.resourceFor(ctx, who, id, true)
	if p != nil {
		return nil, p
	}
	switch r.State {
	case registry.Ready:
	case registry.Deleted:
		return nil, c.refused(ctx, problem(404, KindNotFound, "%s was deleted at %s", r.ID, r.DeletedAt.Format(time.RFC3339)))
	case registry.Lost:
		return nil, c.refused(ctx, problem(409, KindEngine, "%s is lost: its engine no longer has it — delete it", r.ID))
	default:
		if p := c.busy(ctx, r); p != nil {
			return nil, p
		}
		return nil, c.refused(ctx, problem(409, KindEngine, "%s is %s", r.ID, r.State))
	}
	if len(spec) == 0 {
		spec = json.RawMessage("{}")
	}
	spec, p = c.keepLatest(ctx, t.Refs, spec, r.Spec)
	if p != nil {
		return nil, c.refused(ctx, p)
	}
	var groups []string
	if who.Subject == r.Owner {
		groups = who.Groups
	}
	spec, resolved, p := c.resolve(ctx, r.Owner, groups, r.Zone, t.Refs, spec)
	if p != nil {
		return nil, c.refused(ctx, p)
	}
	if v := t.Validate(spec); len(v) > 0 {
		p := problem(422, KindSchema, "the spec does not fit type %s: %s", t.Name, v[0].Reason)
		p.Violations = v
		return nil, c.refused(ctx, p)
	}
	// only what the spec names anew is checked: what the resource names
	// already stays named, even if it was retired since (a machine keeps
	// the image it was born from)
	if _, p := c.references(ctx, r.Owner, groups, r.Zone, t.Refs, newRefs(t.Refs, spec, r.Spec)); p != nil {
		return nil, c.refused(ctx, p)
	}
	refs, err := c.loadRefs(ctx, t.Refs, spec)
	if err != nil {
		return nil, problem(500, KindInternal, "%v", err)
	}
	client, err := c.host.Plugin(t.Plugin).Client(ctx)
	if err != nil {
		return nil, problem(503, KindDown, "%v", err)
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := client.PlanChange(cctx, &pluginpb.PlanChangeRequest{Current: toProto(r), Spec: spec, Refs: refs})
	if status.Code(err) == codes.Unimplemented {
		return nil, c.refused(ctx, problem(422, KindUnavailable, "type %s changes only through its actions, asked one by one: its plugin plans no change", t.Name))
	}
	if err != nil {
		return nil, c.refused(ctx, fromPlugin(t.Plugin, err))
	}
	out := &ChangePlan{Resource: r.ID, Steps: []Step{}, Resolved: resolved}
	for _, st := range resp.GetSteps() {
		a := t.Action(st.GetAction())
		if a == nil {
			return nil, problem(502, KindDown, "plugin %s planned the action %q, which type %s does not have", t.Plugin, st.GetAction(), t.Name)
		}
		params := json.RawMessage(st.GetParams())
		if len(params) == 0 {
			params = json.RawMessage("{}")
		}
		if v := a.Validate(params); len(v) > 0 {
			return nil, problem(502, KindDown, "plugin %s planned %s with params that do not fit it: %s", t.Plugin, a.Name, v[0].Reason)
		}
		out.Steps = append(out.Steps, Step{Action: a.Name, Params: params})
	}
	for _, f := range resp.GetFixed() {
		out.Fixed = append(out.Fixed, plugins.Violation{Field: f.GetField(), Reason: f.GetReason()})
	}
	names := make([]string, len(out.Steps))
	for i, s := range out.Steps {
		names[i] = s.Action
	}
	audit.From(ctx).Set(func(e *audit.Event) {
		e.Fields = map[string]string{"steps": strings.Join(names, ","), "fixed": fmt.Sprint(len(out.Fixed))}
		if resolved != nil {
			e.Fields["resolved"] = strings.Join(resolved, ",")
		}
	})
	return out, nil
}

// keepLatest writes each "@<schedule>" of a wanted spec as what the
// resource names already in that field, when that schedule made it.
func (c *Core) keepLatest(ctx context.Context, refs []plugins.Ref, want, now json.RawMessage) (json.RawMessage, *Problem) {
	var fields, cur map[string]json.RawMessage
	if len(refs) == 0 || json.Unmarshal(want, &fields) != nil {
		return want, nil
	}
	_ = json.Unmarshal(now, &cur)
	madeBy := func(id, schedule string) bool {
		r, err := c.store.Resource(ctx, id)
		return err == nil && r.Tags[ScheduleTag] == schedule
	}
	changed := false
	for _, ref := range refs {
		raw, ok := fields[ref.Field]
		if !ok {
			continue
		}
		had := refIDs(ref, now)
		if ref.Many {
			var vs []string
			if json.Unmarshal(raw, &vs) != nil {
				continue
			}
			for i, v := range vs {
				name, ok := strings.CutPrefix(v, "@")
				if !ok {
					continue
				}
				for _, id := range had {
					if madeBy(id, name) && !slices.Contains(vs, id) {
						vs[i], changed = id, true
						break
					}
				}
			}
			fields[ref.Field], _ = json.Marshal(vs)
			continue
		}
		var v string
		if json.Unmarshal(raw, &v) != nil {
			continue
		}
		if name, ok := strings.CutPrefix(v, "@"); ok && len(had) == 1 && madeBy(had[0], name) {
			fields[ref.Field], _ = json.Marshal(had[0])
			changed = true
		}
	}
	if !changed {
		return want, nil
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, problem(500, KindInternal, "%v", err)
	}
	return out, nil
}

// newRefs is a wanted spec with only the references it names anew: an id
// the resource names already in the same field is left out.
func newRefs(refs []plugins.Ref, want, now json.RawMessage) json.RawMessage {
	var fields map[string]json.RawMessage
	if json.Unmarshal(want, &fields) != nil {
		return want
	}
	out := map[string]json.RawMessage{}
	for _, ref := range refs {
		had := refIDs(ref, now)
		var fresh []string
		for _, id := range refIDs(ref, want) {
			if !slices.Contains(had, id) {
				fresh = append(fresh, id)
			}
		}
		switch {
		case len(fresh) == 0:
		case ref.Many:
			out[ref.Field], _ = json.Marshal(fresh)
		default:
			out[ref.Field], _ = json.Marshal(fresh[0])
		}
	}
	b, _ := json.Marshal(out)
	return b
}
