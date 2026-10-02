package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// ---- A resource as a person reads it ------------------------------------------
//
// The brain serves what a person reads on each resource — the word it wears
// (status), its type's own sentence (summary), its owner's name, the names of
// what it names — and the command line prints that: it knows no type. A name
// goes wherever an id goes: `hangar machine start dev`.

// called is what a resource answers to: its name, or its id when unnamed.
func called(r *resource) string {
	if r.Name != "" {
		return r.Name
	}
	return r.ID
}

// wears is the one word a resource wears; an older brain says only its state.
func wears(r *resource) string {
	switch {
	case r.Status != "":
		return r.Status
	case r.Unusable != "":
		return r.State + " (" + r.Unusable + ")"
	}
	return r.State
}

// reads is what a resource is, in its type's own sentence; an older brain's
// is drawn here from the spec's fields.
func reads(t *typeView, r *resource) string {
	if r.Summary != "" {
		return r.Summary
	}
	return summary(t, r.Spec)
}

// whose is a resource's owner as a person reads it: the name they sign in
// under, else the subject.
func whose(r *resource) string {
	if r.OwnerName != "" {
		return r.OwnerName
	}
	return r.Owner
}

// ago is an age, short: 45s, 12m, 3h, 5d.
func ago(now, then time.Time) string {
	if then.IsZero() {
		return "-"
	}
	d := now.Sub(then)
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// place is where its engine keeps it, in the engine's own words, when its
// plugin says (observed.engine_ref).
func place(r *resource) string {
	var o struct {
		EngineRef string `json:"engine_ref"`
	}
	_ = json.Unmarshal(r.Observed, &o)
	return o.EngineRef
}

// me asks who the caller is, once.
func (c *client) me() (*whoamiView, error) {
	if c.who == nil {
		var w whoamiView
		if err := c.do("GET", "/v1/whoami", nil, &w); err != nil {
			return nil, err
		}
		c.who = &w
	}
	return c.who, nil
}

// ---- A name where an id goes --------------------------------------------------

// idOf reads the one resource a command is about: its id, or what its owner
// calls it.
func (c *client) idOf(t *typeView, verb string, pos []string) (string, error) {
	if len(pos) != 1 {
		return "", usagef("%s %s NAME|ID — one %s", t.Name, verb, t.Name)
	}
	return c.find(t, pos[0])
}

func (c *client) find(t *typeView, arg string) (string, error) {
	if idPattern.MatchString(arg) {
		if !strings.HasPrefix(arg, t.IDPrefix+"-") {
			return "", usagef("%s is not a %s's id (%s-…)", arg, t.Name, t.IDPrefix)
		}
		return arg, nil
	}
	if !namePattern.MatchString(arg) {
		return "", usagef("%q is neither a %s's id (%s-…) nor a name", arg, t.Name, t.IDPrefix)
	}
	all, err := c.list(url.Values{"type": {t.Name}, "name": {arg}})
	if err != nil {
		return "", err
	}
	var rs []resource
	for _, r := range all { // an older brain does not filter by name
		if r.Name == arg {
			rs = append(rs, r)
		}
	}
	if len(rs) > 1 {
		// yours first: what is shared with you, or — for an operator —
		// someone else's, is named by its id
		who, err := c.me()
		if err != nil {
			return "", err
		}
		var mine []resource
		for _, r := range rs {
			if r.Owner == who.Subject {
				mine = append(mine, r)
			}
		}
		if len(mine) == 1 {
			rs = mine
		}
	}
	switch len(rs) {
	case 0:
		return "", fmt.Errorf("no %s named %s (hangar %s list)", t.Name, arg, t.Name)
	case 1:
		return rs[0].ID, nil
	}
	var which []string
	for _, r := range rs {
		which = append(which, fmt.Sprintf("%s (%s's)", r.ID, whose(&r)))
	}
	return "", fmt.Errorf("%d %ss are named %s: %s — say which by its id", len(rs), t.Name, arg, strings.Join(which, ", "))
}

// ---- A type's list ------------------------------------------------------------

// listRows prints resources of one type: what each is called, the word it
// wears, its sentence, where and since when — and whose, when one is not the
// caller's.
func listRows(env *Env, c *client, t *typeView, rs []resource, wide bool) error {
	others := false
	if len(rs) > 0 {
		who, err := c.me()
		if err != nil {
			return err
		}
		others = slices.ContainsFunc(rs, func(r resource) bool { return r.Owner != who.Subject })
	}
	header := "NAME\tSTATE\tWHAT\tZONE\tAGE\tID"
	if others || wide {
		header += "\tOWNER"
	}
	if wide {
		header += "\tSET\tPLACE\tDESCRIPTION"
	}
	var rows [][]string
	for i := range rs {
		r := &rs[i]
		row := []string{called(r), wears(r), dash(reads(t, r)), r.Zone, ago(env.Now(), r.CreatedAt), r.ID}
		if others || wide {
			row = append(row, whose(r))
		}
		if wide {
			row = append(row, dash(r.Tags[TagSet]), dash(place(r)), dash(r.Description))
		}
		rows = append(rows, row)
	}
	return table(env.Stdout, header, rows)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ---- One resource, as a card ----------------------------------------------------

// card prints one resource for a person: what it is called and wears, its
// sentence, where it is and whose, what it uses and what uses it, then what
// was asked and what was seen, field by field.
func card(env *Env, c *client, ts []*typeView, t *typeView, r *resource) error {
	w := env.Stdout
	who, err := c.me()
	if err != nil {
		return err
	}
	title := strings.ToLower(orName(t))
	head := fmt.Sprintf("%s  %s · %s", called(r), title, wears(r))
	if r.Name != "" {
		head += "  " + r.ID
	}
	fmt.Fprintln(w, head)
	if s := reads(t, r); s != "" {
		fmt.Fprintf(w, "  %s\n", s)
	}
	if r.Description != "" {
		fmt.Fprintf(w, "  « %s »\n", r.Description)
	}
	fmt.Fprintln(w)
	row := func(k, v string) {
		if v != "" {
			fmt.Fprintf(w, "  %-9s %s\n", k, v)
		}
	}
	where := "zone " + r.Zone
	if p := place(r); p != "" {
		where += " · " + p
	}
	row("where", where)
	owner := whose(r)
	switch {
	case r.Owner != who.Subject:
	case r.OwnerName != "":
		owner += " (you)"
	default:
		owner = "you" // never seen at the provider: a subject says nothing to a person
	}
	if set := r.Tags[TagSet]; set != "" {
		owner += " · set " + set
	}
	if len(r.SharedWith) > 0 {
		with := make([]string, len(r.SharedWith))
		for i, g := range r.SharedWith {
			with[i] = g
			if g == "*" {
				with[i] = "everyone"
			}
		}
		owner += " · shared with " + strings.Join(with, ", ")
	}
	row("whose", owner)
	row("made", r.CreatedAt.Local().Format("2 Jan 2006 15:04")+" · "+ago(env.Now(), r.CreatedAt)+" ago")
	var room []string
	if r.Room.GuaranteedMB > 0 {
		room = append(room, fmt.Sprintf("%s GB guaranteed", gb(r.Room.GuaranteedMB)))
	}
	if r.Room.SpotMB > 0 {
		room = append(room, fmt.Sprintf("%s GB borrowed while it runs", gb(r.Room.SpotMB)))
	}
	row("room", strings.Join(room, " · "))
	if r.Hold != "" {
		row("held", "its room is held for "+r.Hold)
	}
	row("drift", r.Drift)

	// what it names, and what names it — read from the schemas' references
	byName := map[string]*typeView{}
	for _, x := range ts {
		byName[x.Name] = x
	}
	var spec map[string]any
	_ = json.Unmarshal(r.Spec, &spec)
	var uses []string
	for _, f := range refFields(t) {
		for _, id := range named(spec, f) {
			word := f.Ref
			if rt := byName[f.Ref]; rt != nil {
				word = strings.ToLower(orName(rt))
			}
			uses = append(uses, word+" "+orID(r.Names[id], id))
		}
	}
	row("uses", strings.Join(uses, " · "))
	var usedBy []string
	for _, o := range ts {
		var fs []field
		for _, f := range refFields(o) {
			if f.Ref == t.Name {
				fs = append(fs, f)
			}
		}
		if len(fs) == 0 {
			continue
		}
		others, err := c.list(url.Values{"type": {o.Name}, "zone": {r.Zone}})
		if err != nil {
			return err
		}
		for i := range others {
			var os map[string]any
			_ = json.Unmarshal(others[i].Spec, &os)
			if slices.ContainsFunc(fs, func(f field) bool { return slices.Contains(named(os, f), r.ID) }) {
				usedBy = append(usedBy, strings.ToLower(orName(o))+" "+called(&others[i]))
			}
		}
	}
	row("used by", strings.Join(usedBy, " · "))

	pairs := func(title string, raw json.RawMessage, order []string) {
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil || len(m) == 0 {
			return
		}
		keys := slices.Clone(order)
		rest := []string{}
		for k := range m {
			if !slices.Contains(keys, k) {
				rest = append(rest, k)
			}
		}
		sort.Strings(rest)
		fmt.Fprintf(w, "\n  %s\n", title)
		for _, k := range append(keys, rest...) {
			v, ok := m[k]
			if !ok {
				continue
			}
			text := wordsOf(v, r.Names)
			if text == "" {
				continue
			}
			fmt.Fprintf(w, "    %-14s %s\n", strings.ReplaceAll(k, "_", " "), text)
		}
	}
	var order []string
	for _, f := range t.fields {
		order = append(order, f.Name)
	}
	pairs("as asked", r.Spec, order)
	pairs("as seen", r.Observed, nil)
	if len(r.Tags) > 0 {
		keys := make([]string, 0, len(r.Tags))
		for k := range r.Tags {
			keys = append(keys, k+"="+r.Tags[k])
		}
		sort.Strings(keys)
		fmt.Fprintf(w, "\n  tags      %s\n", strings.Join(keys, ", "))
	}
	fmt.Fprintf(w, "\n  the whole record: hangar %s get %s -o yaml\n", t.Name, called(r))
	return nil
}

func orName(t *typeView) string {
	if t.Title != "" {
		return t.Title
	}
	return t.Name
}

func orID(name, id string) string {
	if name != "" {
		return name
	}
	return id
}

func gb(mb int64) string {
	s := fmt.Sprintf("%.1f", float64(mb)/1024)
	return strings.TrimSuffix(s, ".0")
}

// wordsOf is a value as a person reads it; an id reads as what it names.
func wordsOf(v any, names map[string]string) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		if n := names[x]; n != "" {
			return n
		}
		if utf8.RuneCountInString(x) > 72 {
			return string([]rune(x)[:71]) + "…"
		}
		return x
	case bool:
		if x {
			return "yes"
		}
		return "no"
	case float64:
		return strings.TrimSuffix(fmt.Sprintf("%.3f", x), ".000")
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			parts = append(parts, wordsOf(e, names))
		}
		return strings.Join(parts, ", ")
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// ---- rename, describe -----------------------------------------------------------

func renameCmd(env *Env, c *client, t *typeView, args []string) error {
	pos, err := parse(args, nil)
	if errors.Is(err, errHelp) {
		fmt.Fprintf(env.Stdout, "hangar %s rename NAME|ID NEW — what you call it; \"\" unnames it. Its id stays, and so does the host name a machine was born with.\n", t.Name)
		return nil
	}
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usagef("%s rename NAME|ID NEW — the one to rename, then its new name", t.Name)
	}
	id, err := c.find(t, pos[0])
	if err != nil {
		return err
	}
	var r resource
	if err := c.do("PATCH", "/v1/resources/"+id, map[string]any{"name": pos[1]}, &r); err != nil {
		return err
	}
	if r.Name == "" {
		fmt.Fprintf(env.Stderr, "%s is unnamed\n", id)
		return nil
	}
	fmt.Fprintf(env.Stderr, "%s is now %s\n", id, r.Name)
	return nil
}

func describeCmd(env *Env, c *client, t *typeView, args []string) error {
	pos, err := parse(args, nil)
	if errors.Is(err, errHelp) {
		fmt.Fprintf(env.Stdout, "hangar %s describe NAME|ID WORDS — one line about it; \"\" takes it off\n", t.Name)
		return nil
	}
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return usagef("%s describe NAME|ID WORDS — the one to describe, then a line about it", t.Name)
	}
	id, err := c.find(t, pos[0])
	if err != nil {
		return err
	}
	var r resource
	if err := c.do("PATCH", "/v1/resources/"+id, map[string]any{"description": strings.Join(pos[1:], " ")}, &r); err != nil {
		return err
	}
	fmt.Fprintf(env.Stderr, "%s: described\n", called(&r))
	return nil
}

// ---- hangar list: everything you hold, on one screen ----------------------------

// A holder is a type others attach to (a schema's x-hangar-attached names
// it): a machine, with the volumes plugged into it. Each of the caller's
// holders is shown with what hangs on it — what names it, and what it names
// —, then what hangs on nothing.
func listAll(env *Env, args []string) error {
	format, zone := "table", ""
	opts := []*opt{strOpt(&zone, "ZONE", "in one zone", "zone"), outputOpt(&format)}
	pos, err := parse(args, opts)
	if errors.Is(err, errHelp) {
		fmt.Fprintln(env.Stdout, "hangar list — everything you hold, each machine with what hangs on it")
		flagHelp(env.Stdout, opts)
		return nil
	}
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef("list takes flags, not %s (one type's: hangar <type> list)", strings.Join(pos, " "))
	}
	c, err := connect(env)
	if err != nil {
		return err
	}
	ts, err := c.catalogue()
	if err != nil {
		return err
	}
	who, err := c.me()
	if err != nil {
		return err
	}
	q := url.Values{}
	if zone != "" {
		q.Set("zone", zone)
	}
	all, err := c.list(q) // the caller's own, and what is shared with them
	if err != nil {
		return err
	}
	switch format {
	case "json", "yaml":
		return show(env.Stdout, format, all)
	case "id":
		for _, r := range all {
			if r.Owner == who.Subject {
				fmt.Fprintln(env.Stdout, r.ID)
			}
		}
		return nil
	}
	return tree(env.Stdout, ts, who.Subject, all)
}

type treeLine struct {
	prefix string
	r      *resource
}

func tree(w io.Writer, ts []*typeView, me string, all []resource) error {
	types := map[string]*typeView{}
	holder := map[string]bool{}
	for _, t := range ts {
		types[t.Name] = t
		for _, f := range t.fields {
			if f.Attached && f.Ref != "" {
				holder[f.Ref] = true
			}
		}
	}
	byID := map[string]*resource{}
	for i := range all {
		byID[all[i].ID] = &all[i]
	}
	refs := func(r *resource) []string { return specRefs(r, types) }
	// under each holder of the caller's: what names it, then what it names
	hangs := map[string][]*resource{}
	hung := map[string]bool{}
	for i := range all {
		r := &all[i]
		if r.Owner != me || !holder[r.Type] {
			continue
		}
		for j := range all {
			if o := &all[j]; o.ID != r.ID && slices.Contains(refs(o), r.ID) {
				hangs[r.ID] = append(hangs[r.ID], o)
				hung[o.ID] = true
			}
		}
		for _, id := range refs(r) {
			if o := byID[id]; o != nil && !slices.Contains(hangs[r.ID], o) {
				hangs[r.ID] = append(hangs[r.ID], o)
				hung[o.ID] = true
			}
		}
	}
	// the groups: a spec file's set, in a zone
	type group struct {
		set, zone string
		lines     []treeLine
	}
	var groups []*group
	groupOf := func(r *resource) *group {
		set := r.Tags[TagSet]
		for _, g := range groups {
			if g.set == set && g.zone == r.Zone {
				return g
			}
		}
		g := &group{set: set, zone: r.Zone}
		groups = append(groups, g)
		return g
	}
	for i := range all {
		r := &all[i]
		if r.Owner != me || !holder[r.Type] {
			continue
		}
		g := groupOf(r)
		g.lines = append(g.lines, treeLine{"", r})
		kids := hangs[r.ID]
		for k, o := range kids {
			mark := "├ "
			if k == len(kids)-1 {
				mark = "└ "
			}
			g.lines = append(g.lines, treeLine{mark, o})
		}
	}
	for i := range all {
		if r := &all[i]; r.Owner == me && !holder[r.Type] && !hung[r.ID] {
			g := groupOf(r)
			g.lines = append(g.lines, treeLine{"", r})
		}
	}
	if len(groups) == 0 {
		fmt.Fprintln(w, "You hold nothing yet (hangar types says what may be asked for).")
		return nil
	}
	sort.SliceStable(groups, func(i, j int) bool {
		a, b := groups[i], groups[j]
		if (a.set == "") != (b.set == "") {
			return a.set != "" // what a spec file made first
		}
		if a.set != b.set {
			return a.set < b.set
		}
		return a.zone < b.zone
	})
	// one width for every group: the columns line up down the screen
	var wn, wt, ws int
	word := func(r *resource) string {
		if t := types[r.Type]; t != nil {
			return strings.ToLower(orName(t))
		}
		return r.Type
	}
	for _, g := range groups {
		for _, l := range g.lines {
			wn = max(wn, utf8.RuneCountInString(l.prefix+called(l.r)))
			wt = max(wt, utf8.RuneCountInString(word(l.r)))
			ws = max(ws, utf8.RuneCountInString(wears(l.r)))
		}
	}
	pad := func(s string, n int) string { return s + strings.Repeat(" ", max(0, n-utf8.RuneCountInString(s))) }
	for i, g := range groups {
		if i > 0 {
			fmt.Fprintln(w)
		}
		head := "zone " + g.zone
		if g.set != "" {
			head = "set " + g.set + " · " + head
		}
		fmt.Fprintln(w, head)
		for _, l := range g.lines {
			what := reads(types[l.r.Type], l.r)
			if l.r.Owner != me {
				what = strings.TrimPrefix(what+" · "+whose(l.r)+"'s", " · ")
			}
			line := pad(l.prefix+called(l.r), wn) + "  " + pad(word(l.r), wt) + "  " + pad(wears(l.r), ws) + "  " + what
			fmt.Fprintln(w, strings.TrimRight(line, " "))
		}
	}
	return nil
}
