package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ---- apply: make a spec file true -------------------------------------------
//
// A spec file names resources, each in its type's own words:
//
//	set: dev-box            # what apply keeps these under
//	zone: lab               # every resource's, unless one says otherwise
//	resources:
//	  me:   {type: keypair, spec: {public_key: "ssh-ed25519 AAAA…"}}
//	  box:  {type: machine, spec: {kind: container, cores: 2, memory_gb: 4, image: debian-13, key_pairs: [me]}}
//	  home: {type: volume,  spec: {size_gb: 16, mount: /home, machine: box}}
//
// A reference (a field its schema marks x-hangar-ref) names an id,
// "@<schedule>", or another entry of the file — apply makes them in that
// order. The brain is the state: each resource carries the tags
// apply:set=<set> and apply:name=<entry>, and only the caller's own count
// (someone else's shared with the same tags is never theirs to change).
//
// What is missing is created; what differs is brought to the file by the
// steps its plugin names (POST /v1/resources/{id}/plan), each asked as an
// action; what left the file is deleted — after asking. A field set at a
// resource's birth that differs stops everything before anything changes.

// The tags apply keeps a set under.
const (
	TagSet  = "apply:set"
	TagName = "apply:name"
)

var namePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

type specFile struct {
	Set     string
	Zone    string
	Entries []*fileEntry
}

type fileEntry struct {
	Name string
	Type string
	Zone string
	Spec map[string]any
	Tags map[string]string
	Line int

	t    *typeView
	deps []string // the entries it names
}

func readSpecFile(env *Env, path string) (*specFile, error) {
	var b []byte
	var err error
	if path == "-" {
		b, err = io.ReadAll(env.Stdin)
	} else {
		b, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(b, &root); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: a spec file is a mapping: set, zone, resources", path)
	}
	bad := func(n *yaml.Node, format string, a ...any) error {
		return fmt.Errorf("%s:%d: %s", path, n.Line, fmt.Sprintf(format, a...))
	}
	f := &specFile{}
	top := root.Content[0]
	for i := 0; i < len(top.Content); i += 2 {
		k, v := top.Content[i], top.Content[i+1]
		switch k.Value {
		case "set":
			f.Set = v.Value
		case "zone":
			f.Zone = v.Value
		case "resources":
			if v.Kind != yaml.MappingNode {
				return nil, bad(v, "resources is a mapping: a name, then its type and spec")
			}
			for j := 0; j < len(v.Content); j += 2 {
				nk, nv := v.Content[j], v.Content[j+1]
				e := &fileEntry{Name: nk.Value, Line: nk.Line, Spec: map[string]any{}}
				if !namePattern.MatchString(e.Name) || idPattern.MatchString(e.Name) {
					return nil, bad(nk, "%q: a name is lowercase letters, digits and dashes, and not shaped like an id", e.Name)
				}
				if nv.Kind != yaml.MappingNode {
					return nil, bad(nv, "%s: type, spec, and zone or tags if need be", e.Name)
				}
				for m := 0; m < len(nv.Content); m += 2 {
					ek, ev := nv.Content[m], nv.Content[m+1]
					switch ek.Value {
					case "type":
						e.Type = ev.Value
					case "zone":
						e.Zone = ev.Value
					case "spec":
						var spec map[string]any
						if err := ev.Decode(&spec); err != nil {
							return nil, bad(ev, "%s's spec: %v", e.Name, err)
						}
						if spec != nil {
							e.Spec = jsonable(spec).(map[string]any)
						}
					case "tags":
						if err := ev.Decode(&e.Tags); err != nil {
							return nil, bad(ev, "%s's tags: %v", e.Name, err)
						}
					default:
						return nil, bad(ek, "%s: no key %q (type, zone, spec, tags)", e.Name, ek.Value)
					}
				}
				if e.Type == "" {
					return nil, bad(nk, "%s names no type", e.Name)
				}
				if e.Zone == "" {
					e.Zone = f.Zone
				}
				f.Entries = append(f.Entries, e)
			}
		default:
			return nil, bad(k, "no key %q (set, zone, resources)", k.Value)
		}
	}
	if !namePattern.MatchString(f.Set) {
		return nil, fmt.Errorf("%s: set names what apply keeps these resources under — lowercase letters, digits and dashes", path)
	}
	return f, nil
}

// ---- The plan ---------------------------------------------------------------------

type change struct {
	kind  string // create, steps, keep, delete, later (its plan once what it names exists)
	e     *fileEntry
	r     *resource // what exists
	steps []planStep
	fixed []string
}

type planStep struct {
	Action string          `json:"action"`
	Params json.RawMessage `json:"params"`
}

type changePlan struct {
	Steps []planStep `json:"steps"`
	Fixed []struct {
		Field  string `json:"field"`
		Reason string `json:"reason"`
	} `json:"fixed"`
	Resolved []string `json:"resolved"`
}

type applier struct {
	env     *Env
	c       *client
	f       *specFile
	me      whoamiView
	types   map[string]*typeView
	byName  map[string]*fileEntry
	have    map[string]*resource // by entry name: what exists
	ids     map[string]string    // by entry name: its id, once known
	timeout time.Duration
}

func (a *applier) say(format string, v ...any) { fmt.Fprintf(a.env.Stderr, format+"\n", v...) }

// refs are an entry's reference fields and what each names.
func refFields(t *typeView) []field {
	var out []field
	for _, f := range t.fields {
		if f.Ref != "" {
			out = append(out, f)
		}
	}
	return out
}

// named lists the values of a reference field in a spec.
func named(spec map[string]any, f field) []string {
	switch v := spec[f.Name].(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// entryName: a reference's value that names an entry of the file (not an id,
// not "@<schedule>").
func entryName(v string) bool { return !idPattern.MatchString(v) && !strings.HasPrefix(v, "@") }

// link finds what each entry names, and orders the entries so that each
// comes after what it names.
func (a *applier) link() ([]*fileEntry, error) {
	for _, e := range a.f.Entries {
		t := a.types[e.Type]
		if t == nil {
			return nil, fmt.Errorf("%s: the brain offers no type %q", e.Name, e.Type)
		}
		e.t = t
		if e.Zone == "" {
			if len(t.Zones) != 1 {
				return nil, fmt.Errorf("%s: name a zone (in the file, or for this entry): %s is offered in %s", e.Name, t.Name, strings.Join(t.Zones, ", "))
			}
			e.Zone = t.Zones[0]
		}
		for _, f := range refFields(t) {
			for _, v := range named(e.Spec, f) {
				if !entryName(v) {
					continue
				}
				other := a.byName[v]
				switch {
				case other == nil:
					return nil, fmt.Errorf("%s: %s names %q, which the file does not declare", e.Name, f.Name, v)
				case other.Type != f.Ref:
					return nil, fmt.Errorf("%s: %s names %s, a %s — it takes a %s", e.Name, f.Name, v, other.Type, f.Ref)
				case other.Zone != "" && other.Zone != e.Zone:
					return nil, fmt.Errorf("%s: %s names %s, in zone %s — not %s", e.Name, f.Name, v, other.Zone, e.Zone)
				}
				if !slices.Contains(e.deps, v) {
					e.deps = append(e.deps, v)
				}
			}
		}
	}
	// the file's order, each after what it names
	var order []*fileEntry
	state := map[string]int{} // 1 visiting, 2 done
	var visit func(e *fileEntry, path []string) error
	visit = func(e *fileEntry, path []string) error {
		switch state[e.Name] {
		case 2:
			return nil
		case 1:
			return fmt.Errorf("the entries name each other in a circle: %s", strings.Join(append(path, e.Name), " → "))
		}
		state[e.Name] = 1
		for _, d := range e.deps {
			if err := visit(a.byName[d], append(path, e.Name)); err != nil {
				return err
			}
		}
		state[e.Name] = 2
		order = append(order, e)
		return nil
	}
	for _, e := range a.f.Entries {
		if err := visit(e, nil); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// resolved is an entry's spec with every entry it names written as its id;
// ok=false while one of them has none yet.
func (a *applier) resolved(e *fileEntry) (map[string]any, bool) {
	spec := maps.Clone(e.Spec)
	for _, f := range refFields(e.t) {
		switch v := spec[f.Name].(type) {
		case string:
			if entryName(v) {
				id, ok := a.ids[v]
				if !ok {
					return nil, false
				}
				spec[f.Name] = id
			}
		case []any:
			out := make([]any, len(v))
			for i, x := range v {
				s, _ := x.(string)
				if entryName(s) {
					id, ok := a.ids[s]
					if !ok {
						return nil, false
					}
					out[i] = id
				} else {
					out[i] = x
				}
			}
			spec[f.Name] = out
		}
	}
	return spec, true
}

func (a *applier) planOne(e *fileEntry, r *resource) (*change, error) {
	ch := &change{e: e, r: r}
	if r.Type != e.Type {
		ch.fixed = append(ch.fixed, fmt.Sprintf("it is a %s (%s), and a resource's type is set at its birth", r.Type, r.ID))
		return ch, nil
	}
	if r.State == "lost" {
		ch.fixed = append(ch.fixed, fmt.Sprintf("%s is lost: its engine no longer has it — hangar %s delete %s, then apply again", r.ID, r.Type, r.ID))
		return ch, nil
	}
	if r.Zone != e.Zone {
		ch.fixed = append(ch.fixed, fmt.Sprintf("/zone: it is in %s, and a resource's zone is set at its birth", r.Zone))
	}
	own := maps.Clone(r.Tags)
	delete(own, TagSet)
	delete(own, TagName)
	for k := range own {
		if strings.HasPrefix(k, "hangar") {
			delete(own, k)
		}
	}
	if !maps.Equal(own, e.Tags) && !(len(own) == 0 && len(e.Tags) == 0) {
		ch.fixed = append(ch.fixed, "/tags: a resource's tags are set at its birth")
	}
	spec, ok := a.resolved(e)
	if !ok {
		ch.kind = "later"
		return ch, nil
	}
	var p changePlan
	if err := a.c.do("POST", "/v1/resources/"+r.ID+"/plan", map[string]any{"spec": spec}, &p); err != nil {
		return nil, fmt.Errorf("%s (%s): %w", e.Name, r.ID, err)
	}
	for _, f := range p.Fixed {
		ch.fixed = append(ch.fixed, f.Field+": "+f.Reason)
	}
	ch.steps = p.Steps
	ch.kind = "keep"
	if len(ch.steps) > 0 {
		ch.kind = "steps"
	}
	return ch, nil
}

func stepWords(steps []planStep) string {
	var parts []string
	for _, s := range steps {
		w := verb(s.Action)
		if len(s.Params) > 0 && string(s.Params) != "{}" {
			w += " " + compactJSON(s.Params)
		}
		parts = append(parts, w)
	}
	return strings.Join(parts, ", ")
}

func compactJSON(raw json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return string(raw)
	}
	keys := slices.Sorted(maps.Keys(m))
	parts := make([]string, len(keys))
	for i, k := range keys {
		b, _ := json.Marshal(m[k])
		parts[i] = k + ": " + strings.Trim(string(b), `"`)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// ---- The command ---------------------------------------------------------------------

func apply(env *Env, args []string) error {
	var planOnly, yes bool
	timeout := 30 * time.Minute
	opts := []*opt{
		boolOpt(&planOnly, "print the plan and change nothing", "plan"),
		boolOpt(&yes, "delete what left the file without asking (scripts)", "yes", "y"),
		{names: []string{"timeout"}, value: true, arg: "DURATION", help: "how long one step may take [default 30m]", set: func(v string) (err error) {
			timeout, err = time.ParseDuration(v)
			return err
		}},
	}
	pos, err := parse(args, opts)
	if errors.Is(err, errHelp) {
		fmt.Fprintln(env.Stdout, `hangar apply FILE — make a spec file true

Creates what is missing, brings what differs to the file (the actions its plugin
names), deletes what left the file — after asking. A field set at a resource's
birth that differs stops everything before anything changes. FILE - = stdin.`)
		flagHelp(env.Stdout, opts)
		return nil
	}
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("apply FILE — one spec file")
	}
	f, err := readSpecFile(env, pos[0])
	if err != nil {
		return err
	}
	c, err := connect(env)
	if err != nil {
		return err
	}
	a := &applier{env: env, c: c, f: f, types: map[string]*typeView{}, byName: map[string]*fileEntry{},
		have: map[string]*resource{}, ids: map[string]string{}, timeout: timeout}
	if err := c.do("GET", "/v1/whoami", nil, &a.me); err != nil {
		return err
	}
	ts, err := c.catalogue()
	if err != nil {
		return err
	}
	for _, t := range ts {
		a.types[t.Name] = t
	}
	for _, e := range f.Entries {
		a.byName[e.Name] = e
	}
	order, err := a.link()
	if err != nil {
		return err
	}

	// what exists: the caller's own, under the set
	rs, err := c.list(url.Values{"tag": {TagSet + "=" + f.Set}})
	if err != nil {
		return err
	}
	var gone []*resource // under the set, no longer in the file
	for i := range rs {
		r := &rs[i]
		if r.Owner != a.me.Subject {
			continue
		}
		if r.State == "creating" || r.State == "updating" || r.State == "deleting" {
			if r, err = c.settle(r.ID, timeout, func(s string) { a.say("  %s", s) }); err != nil {
				return err
			}
			if r.State == "deleted" || r.State == "failed" {
				continue
			}
		}
		name := r.Tags[TagName]
		if prev := a.have[name]; prev != nil {
			return fmt.Errorf("two resources are %s in set %s: %s and %s — delete one by hand", name, f.Set, prev.ID, r.ID)
		}
		if a.byName[name] == nil {
			gone = append(gone, r)
			continue
		}
		a.have[name] = r
		a.ids[name] = r.ID
	}

	fmt.Fprintf(env.Stderr, "apply %s: set %s on %s, as %s (tier %s)\n\n", pos[0], f.Set, c.base, a.me.who(), a.me.Tier)
	var changes []*change
	blocked := 0
	for _, e := range order {
		r := a.have[e.Name]
		if r == nil {
			changes = append(changes, &change{kind: "create", e: e})
			continue
		}
		ch, err := a.planOne(e, r)
		if err != nil {
			return err
		}
		if len(ch.fixed) > 0 {
			blocked++
		}
		changes = append(changes, ch)
	}
	// deleted after everything else, what names another first
	slices.SortStableFunc(gone, func(x, y *resource) int { return strings.Compare(x.Tags[TagName], y.Tags[TagName]) })
	gone = deleteOrder(gone, a.types)
	for _, r := range gone {
		changes = append(changes, &change{kind: "delete", r: r})
	}

	counts := map[string]int{}
	for _, ch := range changes {
		counts[ch.kind]++
		name, typ, id := "", "", ""
		if ch.e != nil {
			name, typ = ch.e.Name, ch.e.Type
		}
		if ch.r != nil {
			id = ch.r.ID
			if name == "" {
				name, typ = ch.r.Tags[TagName], ch.r.Type
			}
		}
		line := func(mark, what string) {
			fmt.Fprintf(env.Stderr, "  %s %-12s %-9s %-22s %s\n", mark, name, typ, id, what)
		}
		switch {
		case len(ch.fixed) > 0:
			for _, fx := range ch.fixed {
				line("!", fx)
			}
		case ch.kind == "create":
			what := "create"
			if len(ch.e.deps) > 0 {
				what += " (after " + strings.Join(ch.e.deps, ", ") + ")"
			}
			line("+", what)
		case ch.kind == "steps":
			line("~", stepWords(ch.steps))
		case ch.kind == "later":
			line("~", "its plan once "+strings.Join(ch.e.deps, ", ")+" exists")
		case ch.kind == "keep":
			line("=", "in sync")
		case ch.kind == "delete":
			line("-", "delete — with what it holds: "+summary(a.types[ch.r.Type], ch.r.Spec))
		}
	}
	fmt.Fprintln(env.Stderr)
	if blocked > 0 {
		return fmt.Errorf("nothing changed: %d resource(s) differ in what is set at their birth — put the file back, or delete the resource and apply again", blocked)
	}
	todo := counts["create"] + counts["steps"] + counts["later"] + counts["delete"]
	if todo == 0 {
		fmt.Fprintln(env.Stderr, "Nothing to do: every resource is as the file says.")
		return nil
	}
	if planOnly {
		fmt.Fprintf(env.Stderr, "Plan: %d to create, %d to change, %d to delete — nothing changed (--plan).\n", counts["create"], counts["steps"]+counts["later"], counts["delete"])
		return nil
	}
	if n := counts["delete"]; n > 0 && !yes {
		if !env.Terminal {
			return fmt.Errorf("%d resource(s) left the file and would be deleted: run again with --yes to delete them", n)
		}
		if !confirm(env, fmt.Sprintf("Delete %d resource(s) that left the file, with what they hold?", n)) {
			return errors.New("nothing changed: the deletes were not confirmed")
		}
	}
	start := env.Now()
	done := map[string]int{}
	for _, ch := range changes {
		if err := a.do(ch); err != nil {
			return fmt.Errorf("stopped: %w — what came before it is done; apply again once it is fixed", err)
		}
		done[ch.kind]++
	}
	fmt.Fprintf(env.Stderr, "\nApplied in %s: %d created, %d changed, %d deleted, %d in sync.\n", env.Now().Sub(start).Round(time.Second),
		done["create"], done["steps"]+done["later"], done["delete"], done["keep"])
	return nil
}

// deleteOrder puts a resource that names another of the doomed before it.
func deleteOrder(rs []*resource, types map[string]*typeView) []*resource {
	ids := map[string]bool{}
	for _, r := range rs {
		ids[r.ID] = true
	}
	var out []*resource
	placed := map[string]bool{}
	var place func(r *resource)
	place = func(r *resource) {
		if placed[r.ID] {
			return
		}
		placed[r.ID] = true
		// what names it goes first
		for _, o := range rs {
			if placed[o.ID] {
				continue
			}
			for _, n := range specRefs(o, types) {
				if n == r.ID {
					place(o)
				}
			}
		}
		out = append(out, r)
	}
	for _, r := range rs {
		place(r)
	}
	return out
}

func specRefs(r *resource, types map[string]*typeView) []string {
	t := types[r.Type]
	if t == nil {
		return nil
	}
	var spec map[string]any
	_ = json.Unmarshal(r.Spec, &spec)
	var out []string
	for _, f := range refFields(t) {
		out = append(out, named(spec, f)...)
	}
	return out
}

// ---- Doing it ------------------------------------------------------------------------

func (a *applier) do(ch *change) error {
	switch ch.kind {
	case "keep":
		return nil
	case "create":
		spec, ok := a.resolved(ch.e)
		if !ok {
			return fmt.Errorf("%s: what it names was not made", ch.e.Name)
		}
		tags := maps.Clone(ch.e.Tags)
		if tags == nil {
			tags = map[string]string{}
		}
		tags[TagSet], tags[TagName] = a.f.Set, ch.e.Name
		var acc accepted
		body := map[string]any{"type": ch.e.Type, "zone": ch.e.Zone, "spec": spec, "tags": tags, "client_token": clientToken()}
		if err := a.c.do("POST", "/v1/resources", body, &acc); err != nil {
			return fmt.Errorf("%s: %w", ch.e.Name, err)
		}
		a.say("  + %s: %s asked", ch.e.Name, acc.Resource.ID)
		if err := a.until(ch.e.Name, acc.Operation.ID); err != nil {
			return err
		}
		// named by what comes after: it must be usable, not only made (an
		// image is made before it is baked)
		r, err := a.usable(acc.Resource.ID)
		if err != nil {
			return fmt.Errorf("%s: %w", ch.e.Name, err)
		}
		a.ids[ch.e.Name] = r.ID
		a.say("  + %s: %s %s", ch.e.Name, r.ID, r.State)
		return nil
	case "later":
		re, err := a.planOne(ch.e, ch.r)
		if err != nil {
			return err
		}
		if len(re.fixed) > 0 {
			return fmt.Errorf("%s: %s", ch.e.Name, strings.Join(re.fixed, "; "))
		}
		ch.steps = re.steps
		return a.steps(ch)
	case "steps":
		return a.steps(ch)
	case "delete":
		return a.remove(ch.r)
	}
	return nil
}

func (a *applier) steps(ch *change) error {
	for _, s := range ch.steps {
		var acc accepted
		body := map[string]any{"params": s.Params, "client_token": clientToken()}
		if err := a.c.do("POST", "/v1/resources/"+ch.r.ID+"/actions/"+s.Action, body, &acc); err != nil {
			return fmt.Errorf("%s: %s: %w", ch.e.Name, verb(s.Action), err)
		}
		if err := a.until(ch.e.Name, acc.Operation.ID); err != nil {
			return err
		}
		a.say("  ~ %s: %s done", ch.e.Name, stepWords([]planStep{s}))
	}
	return nil
}

// remove deletes a resource that left the file — unplugged first from what it
// is attached to (its plugin's own steps), since neither end of an attachment
// is deleted while it holds.
func (a *applier) remove(r *resource) error {
	name := r.Tags[TagName]
	if t := a.types[r.Type]; t != nil {
		var spec map[string]any
		_ = json.Unmarshal(r.Spec, &spec)
		free := false
		for _, f := range t.fields {
			if f.Attached && spec[f.Name] != nil {
				delete(spec, f.Name)
				free = true
			}
		}
		if free {
			var p changePlan
			if err := a.c.do("POST", "/v1/resources/"+r.ID+"/plan", map[string]any{"spec": spec}, &p); err != nil {
				return fmt.Errorf("%s (%s): %w", name, r.ID, err)
			}
			if err := a.steps(&change{e: &fileEntry{Name: name}, r: r, steps: p.Steps}); err != nil {
				return err
			}
		}
	}
	var acc accepted
	if err := a.c.do("DELETE", "/v1/resources/"+r.ID+"?client_token="+clientToken(), nil, &acc); err != nil {
		return fmt.Errorf("%s (%s): %w", name, r.ID, err)
	}
	if err := a.until(name, acc.Operation.ID); err != nil {
		return err
	}
	a.say("  - %s: %s deleted", name, r.ID)
	return nil
}

func (a *applier) until(name, op string) error {
	if _, err := a.c.wait(op, a.timeout); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// usable waits for a resource still being made by itself (an image baking)
// to be one a request may name, or to fail.
func (a *applier) usable(id string) (*resource, error) {
	end := a.env.Now().Add(a.timeout)
	said := false
	for {
		var r resource
		if err := a.c.do("GET", "/v1/resources/"+id, nil, &r); err != nil {
			return nil, err
		}
		switch {
		case r.State == "failed":
			return nil, fmt.Errorf("%s failed", id)
		case r.Unusable == "":
			return &r, nil
		case !r.Pending:
			return nil, fmt.Errorf("%s is %s", id, r.Unusable)
		case a.env.Now().After(end):
			return nil, fmt.Errorf("%s is still %s after %s", id, r.Unusable, a.timeout)
		}
		if !said {
			a.say("  … %s is %s: waiting", id, r.Unusable)
			said = true
		}
		a.env.Sleep(a.env.Poll)
	}
}
