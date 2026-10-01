package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"gopkg.in/yaml.v3"
)

// ---- Printing -----------------------------------------------------------------

func table(w io.Writer, header string, rows [][]string) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, header)
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	return tw.Flush()
}

// show prints a value as YAML (for people) or JSON (-o json).
func show(w io.Writer, format string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if format == "json" {
		var out any
		_ = json.Unmarshal(b, &out)
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	var out any
	if err := yaml.Unmarshal(b, &out); err != nil {
		return err
	}
	y, err := yaml.Marshal(out)
	if err != nil {
		return err
	}
	_, err = w.Write(y)
	return err
}

func outputOpt(p *string) *opt {
	return &opt{names: []string{"output", "o"}, value: true, arg: "FORMAT", help: "yaml (default), json, or id", set: func(v string) error {
		if !slices.Contains([]string{"yaml", "json", "id", "table"}, v) {
			return fmt.Errorf("yaml, json or id, not %q", v)
		}
		*p = v
		return nil
	}}
}

// ---- Who, what, where, how much -------------------------------------------------

type whoamiView struct {
	Subject  string   `json:"subject"`
	Name     string   `json:"name"`
	Groups   []string `json:"groups"`
	Tier     string   `json:"tier"`
	Operator bool     `json:"operator"`
	Via      string   `json:"via"`
	Scopes   []string `json:"scopes"`
}

func (w whoamiView) who() string {
	if w.Name != "" && w.Name != w.Subject {
		return fmt.Sprintf("%s (%s)", w.Subject, w.Name)
	}
	return w.Subject
}

func simple(env *Env, args []string, help string) (*client, string, error) {
	format := "table"
	opts := []*opt{outputOpt(&format)}
	pos, err := parse(args, opts)
	if errors.Is(err, errHelp) {
		fmt.Fprintln(env.Stdout, help)
		flagHelp(env.Stdout, opts)
		return nil, "", errHelp
	}
	if err != nil {
		return nil, "", err
	}
	if len(pos) > 0 {
		return nil, "", usagef("no argument here: %s", strings.Join(pos, " "))
	}
	c, err := connect(env)
	return c, format, err
}

func quiet(err error) error {
	if errors.Is(err, errHelp) {
		return nil
	}
	return err
}

func whoami(env *Env, args []string) error {
	c, format, err := simple(env, args, "hangar whoami — who the brain takes you for, and your tier")
	if err != nil {
		return quiet(err)
	}
	var me whoamiView
	if err := c.do("GET", "/v1/whoami", nil, &me); err != nil {
		return err
	}
	if format != "table" {
		return show(env.Stdout, format, me)
	}
	role := ""
	if me.Operator {
		role = " (operator)"
	}
	fmt.Fprintf(env.Stdout, "%s on %s — tier %s%s, via %s, groups: %s\n", me.who(), c.base, me.Tier, role, me.Via, strings.Join(me.Groups, ", "))
	return nil
}

func types(env *Env, args []string) error {
	c, format, err := simple(env, args, "hangar types — every type the brain's plugins offer you, where, and its actions")
	if err != nil {
		return quiet(err)
	}
	ts, err := c.catalogue()
	if err != nil {
		return err
	}
	if format != "table" {
		return show(env.Stdout, format, ts)
	}
	var rows [][]string
	for _, t := range ts {
		acts := []string{}
		for _, a := range t.Actions {
			acts = append(acts, verb(a.Name))
		}
		rows = append(rows, []string{t.Name, t.IDPrefix + "-…", t.Plugin, strings.Join(t.Zones, " "), strings.Join(acts, " ")})
	}
	return table(env.Stdout, "TYPE\tID\tPLUGIN\tZONES\tACTIONS", rows)
}

func zones(env *Env, args []string) error {
	c, format, err := simple(env, args, "hangar zones — the zones open to you, their plugins and room")
	if err != nil {
		return quiet(err)
	}
	var out struct {
		Zones []struct {
			Name    string `json:"name"`
			Driver  string `json:"driver"`
			Awake   *bool  `json:"awake"`
			Plugins map[string]struct {
				Error string `json:"error"`
			} `json:"plugins"`
			Room *struct {
				Guaranteed int    `json:"guaranteed_mb"`
				Spot       int    `json:"spot_mb"`
				Booked     int    `json:"booked_mb"`
				SpotUsed   int    `json:"spot_used_mb"`
				HeldBy     string `json:"held_by"`
			} `json:"room"`
		} `json:"zones"`
	}
	if err := c.do("GET", "/v1/zones", nil, &out); err != nil {
		return err
	}
	if format != "table" {
		return show(env.Stdout, format, out.Zones)
	}
	var rows [][]string
	for _, z := range out.Zones {
		awake, room := "", ""
		if z.Awake != nil {
			awake = map[bool]string{true: "awake", false: "asleep"}[*z.Awake]
		}
		if r := z.Room; r != nil {
			room = fmt.Sprintf("guaranteed %d/%d GB, spot %d/%d GB", r.Booked/1024, r.Guaranteed/1024, r.SpotUsed/1024, r.Spot/1024)
			if r.HeldBy != "" {
				room += ", held by " + r.HeldBy
			}
		}
		plugins := []string{}
		for p, st := range z.Plugins {
			if st.Error != "" {
				p += " (unusable)"
			}
			plugins = append(plugins, p)
		}
		slices.Sort(plugins)
		rows = append(rows, []string{z.Name, z.Driver, awake, room, strings.Join(plugins, " ")})
	}
	return table(env.Stdout, "ZONE\tDRIVER\tAWAKE\tROOM\tPLUGINS", rows)
}

func limitsCmd(env *Env, args []string) error {
	c, format, err := simple(env, args, "hangar limits — your tier's limit on every dimension, and what you hold")
	if err != nil {
		return quiet(err)
	}
	var out struct {
		Tier   string `json:"tier"`
		Limits []struct {
			Name   string    `json:"name"`
			Unit   string    `json:"unit"`
			Limit  any       `json:"limit"`
			Used   float64   `json:"used"`
			Kind   string    `json:"kind"`
			Period string    `json:"period"`
			Resets time.Time `json:"resets"`
		} `json:"limits"`
	}
	if err := c.do("GET", "/v1/limits", nil, &out); err != nil {
		return err
	}
	if format != "table" {
		return show(env.Stdout, format, out)
	}
	var rows [][]string
	for _, l := range out.Limits {
		lim := fmt.Sprint(l.Limit)
		if list, ok := l.Limit.([]any); ok {
			parts := []string{}
			for _, x := range list {
				parts = append(parts, fmt.Sprint(x))
			}
			lim = strings.Join(parts, ", ")
		}
		used := strconv.FormatFloat(l.Used, 'f', -1, 64)
		switch l.Kind {
		case "choice":
			used = ""
		case "meter":
			// consumed, not held: this month's, to a decimal, and when the next begins
			used = strconv.FormatFloat(math.Floor(l.Used*10)/10, 'f', -1, 64)
			lim += " a month"
			if !l.Resets.IsZero() {
				lim += ", back on " + l.Resets.Format("2 January")
			}
		}
		rows = append(rows, []string{l.Name, used, lim, l.Unit})
	}
	fmt.Fprintf(env.Stdout, "tier %s\n", out.Tier)
	return table(env.Stdout, "DIMENSION\tUSED\tLIMIT\tUNIT", rows)
}

func operations(env *Env, args []string) error {
	format, res := "table", ""
	opts := []*opt{strOpt(&res, "ID", "one resource's operations", "resource"), outputOpt(&format)}
	pos, err := parse(args, opts)
	if errors.Is(err, errHelp) {
		fmt.Fprintln(env.Stdout, "hangar operations [--resource ID] — your operations, newest first")
		flagHelp(env.Stdout, opts)
		return nil
	}
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef("no argument here: %s", strings.Join(pos, " "))
	}
	c, err := connect(env)
	if err != nil {
		return err
	}
	path := "/v1/operations?limit=50"
	if res != "" {
		path += "&resource=" + url.QueryEscape(res)
	}
	var out struct {
		Operations []operation `json:"operations"`
	}
	if err := c.do("GET", path, nil, &out); err != nil {
		return err
	}
	if format != "table" {
		return show(env.Stdout, format, out.Operations)
	}
	var rows [][]string
	for _, op := range out.Operations {
		what := op.Kind
		if op.Action != "" {
			what = op.Action
		}
		rows = append(rows, []string{op.ID, op.ResourceID, what, op.State, op.CreatedAt.Local().Format(time.DateTime), op.Error})
	}
	return table(env.Stdout, "OPERATION\tRESOURCE\tWHAT\tSTATE\tASKED\tERROR", rows)
}

func waitCmd(env *Env, args []string) error {
	timeout := 30 * time.Minute
	opts := []*opt{{names: []string{"timeout"}, value: true, arg: "DURATION", help: "how long to wait [default 30m]", set: func(v string) (err error) {
		timeout, err = time.ParseDuration(v)
		return err
	}}}
	pos, err := parse(args, opts)
	if errors.Is(err, errHelp) {
		fmt.Fprintln(env.Stdout, "hangar wait OP — wait for an operation to end; a failed one is an error")
		flagHelp(env.Stdout, opts)
		return nil
	}
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("wait OP — one operation's id")
	}
	c, err := connect(env)
	if err != nil {
		return err
	}
	op, err := c.wait(pos[0], timeout)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stderr, "%s: %s\n", describeOp(op), op.State)
	return nil
}

// ---- A type's commands, drawn from its schema -----------------------------------

// verb is how an action is typed: set_backup → set-backup.
func verb(action string) string { return strings.ReplaceAll(action, "_", "-") }

var builtinVerbs = []string{"create", "list", "get", "delete"}

func typeCmd(env *Env, name string, args []string) error {
	c, err := connect(env)
	if err != nil {
		return err
	}
	ts, err := c.catalogue()
	if err != nil {
		return err
	}
	var t *typeView
	for _, x := range ts {
		if x.Name == name {
			t = x
		}
	}
	if t == nil {
		names := []string{}
		for _, x := range ts {
			names = append(names, x.Name)
		}
		return usagef("no command or type %q here (types: %s; hangar help)", name, strings.Join(names, ", "))
	}
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		typeHelp(env.Stdout, t)
		return nil
	}
	v, rest := args[0], args[1:]
	switch v {
	case "create":
		return createCmd(env, c, t, rest)
	case "list":
		return listCmd(env, c, t, rest)
	case "get":
		return getCmd(env, c, t, rest)
	case "delete":
		return deleteCmd(env, c, t, rest)
	case "act":
		if len(rest) < 2 {
			return usagef("%s act ID ACTION …", t.Name)
		}
		v, rest = rest[1], append([]string{rest[0]}, rest[2:]...)
	}
	a := t.action(v)
	if a == nil {
		return usagef("type %s has no command %q (hangar %s --help)", t.Name, v, t.Name)
	}
	return actCmd(env, c, t, a, rest)
}

func typeHelp(w io.Writer, t *typeView) {
	title := t.Title
	if title == "" {
		title = t.Name
	}
	fmt.Fprintf(w, "hangar %s — %s: %s\n  ids %s-…, from plugin %s, offered in: %s\n\n", t.Name, title, t.Description, t.IDPrefix, t.Plugin, strings.Join(t.Zones, ", "))
	fmt.Fprintf(w, "  hangar %s create [--zone Z] [FIELDS] [-f spec.yaml]\n", t.Name)
	fmt.Fprintf(w, "  hangar %s list | get ID | delete ID\n", t.Name)
	for _, a := range t.Actions {
		fmt.Fprintf(w, "  hangar %s %s ID%s — %s\n", t.Name, verb(a.Name), paramsWords(a.fields), a.Description)
	}
	fmt.Fprintf(w, "\nFields (--help on a command lists its flags):\n")
	for _, f := range t.fields {
		fmt.Fprintf(w, "  %-14s %s\n", f.Name, f.help())
	}
}

func paramsWords(fs []field) string {
	if len(fs) == 0 {
		return ""
	}
	parts := []string{}
	for _, f := range fs {
		parts = append(parts, "--"+strings.ReplaceAll(f.Name, "_", "-"))
	}
	return " [" + strings.Join(parts, " ") + "]"
}

// readDoc reads a YAML (or JSON) document from a file, or stdin for "-".
func readDoc(env *Env, path string) (map[string]any, error) {
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
	doc := map[string]any{}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	return jsonable(doc).(map[string]any), nil
}

// defaultZone: the one named, else $HANGAR_ZONE, else the only zone the type
// is offered in.
func defaultZone(env *Env, t *typeView, zone string) (string, error) {
	if zone == "" {
		zone = env.Getenv("HANGAR_ZONE")
	}
	if zone == "" && len(t.Zones) == 1 {
		zone = t.Zones[0]
	}
	if zone == "" {
		return "", usagef("name a zone: --zone (%s is offered in: %s)", t.Name, strings.Join(t.Zones, ", "))
	}
	return zone, nil
}

func tagsOf(list []string) (map[string]string, error) {
	if len(list) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, t := range list {
		k, v, ok := strings.Cut(t, "=")
		if !ok {
			return nil, usagef("--tag key=value, not %q", t)
		}
		out[k] = v
	}
	return out, nil
}

func createCmd(env *Env, c *client, t *typeView, args []string) error {
	spec := map[string]any{}
	var zone, file, format string
	var tags []string
	noWait := false
	format = "yaml"
	fieldOpts := flags(t.fields, spec)
	own := []*opt{
		strOpt(&zone, "ZONE", "where (default: $HANGAR_ZONE, or the only zone it is offered in)", "zone"),
		listOpt(&tags, "KEY=VALUE", "a tag (repeatable)", "tag"),
		strOpt(&file, "FILE", "the spec as YAML or JSON (- = stdin); flags are written over it", "from-file", "f"),
		boolOpt(&noWait, "answer at once, without waiting for it to be ready", "no-wait"),
		outputOpt(&format),
		setOpt(t.fields, spec),
	}
	// the command's own flags win over a field of the same name (--set reaches it)
	opts := own
	for _, o := range fieldOpts {
		if !slices.ContainsFunc(own, func(x *opt) bool { return slices.Contains(x.names, o.names[0]) }) {
			opts = append(opts, o)
		}
	}
	pos, err := parse(args, opts)
	if errors.Is(err, errHelp) {
		fmt.Fprintf(env.Stdout, "hangar %s create [flags]\n\n", t.Name)
		flagHelp(env.Stdout, opts)
		return nil
	}
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef("create takes flags, not %s", strings.Join(pos, " "))
	}
	if file != "" {
		base, err := readDoc(env, file)
		if err != nil {
			return err
		}
		for k, v := range spec {
			base[k] = v
		}
		spec = base
	}
	if zone, err = defaultZone(env, t, zone); err != nil {
		return err
	}
	tagMap, err := tagsOf(tags)
	if err != nil {
		return err
	}
	var acc accepted
	if err := c.do("POST", "/v1/resources", map[string]any{"type": t.Name, "zone": zone, "spec": spec, "tags": tagMap, "client_token": clientToken()}, &acc); err != nil {
		return err
	}
	return finish(env, c, t, &acc, noWait, format, "created")
}

// finish waits for an accepted request (unless asked not to) and says how it
// ended.
func finish(env *Env, c *client, t *typeView, acc *accepted, noWait bool, format, done string) error {
	id := acc.Resource.ID
	if noWait {
		if format == "id" {
			fmt.Fprintln(env.Stdout, id)
			return nil
		}
		fmt.Fprintf(env.Stderr, "%s: asked (%s) — hangar wait %s\n", id, acc.Operation.ID, acc.Operation.ID)
		return nil
	}
	start := env.Now()
	op, err := c.wait(acc.Operation.ID, 30*time.Minute)
	if err != nil {
		return err
	}
	var r resource
	if err := c.do("GET", "/v1/resources/"+id, nil, &r); err != nil {
		return err
	}
	switch format {
	case "id":
		fmt.Fprintln(env.Stdout, id)
	case "json":
		return show(env.Stdout, "json", map[string]any{"resource": r, "result": op.Result})
	default:
		note := r.State
		if r.Unusable != "" {
			note += ", " + r.Unusable
		}
		fmt.Fprintf(env.Stderr, "%s %s (%s, %s)\n", done, id, note, env.Now().Sub(start).Round(time.Second))
		if len(op.Result) > 0 && string(op.Result) != "null" {
			var res any
			_ = json.Unmarshal(op.Result, &res)
			return show(env.Stdout, "yaml", res)
		}
	}
	return nil
}

func listCmd(env *Env, c *client, t *typeView, args []string) error {
	format, zone, owner := "table", "", ""
	var tags, states []string
	opts := []*opt{
		strOpt(&zone, "ZONE", "in one zone", "zone"),
		listOpt(&tags, "KEY=VALUE", "with this tag (repeatable: every one)", "tag"),
		listOpt(&states, "STATE", "in this state (repeatable; default: every live one)", "state"),
		strOpt(&owner, "SUBJECT", "someone's (operators)", "owner"),
		outputOpt(&format),
	}
	pos, err := parse(args, opts)
	if errors.Is(err, errHelp) {
		fmt.Fprintf(env.Stdout, "hangar %s list — yours, and what is shared with you\n\n", t.Name)
		flagHelp(env.Stdout, opts)
		return nil
	}
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef("list takes flags, not %s", strings.Join(pos, " "))
	}
	q := url.Values{"type": {t.Name}}
	if zone != "" {
		q.Set("zone", zone)
	}
	if owner != "" {
		q.Set("owner", owner)
	}
	q["tag"], q["state"] = tags, states
	rs, err := c.list(q)
	if err != nil {
		return err
	}
	switch format {
	case "json", "yaml":
		return show(env.Stdout, format, rs)
	case "id":
		for _, r := range rs {
			fmt.Fprintln(env.Stdout, r.ID)
		}
		return nil
	}
	var rows [][]string
	for _, r := range rs {
		state := r.State
		if r.Unusable != "" {
			state += " (" + r.Unusable + ")"
		}
		rows = append(rows, []string{r.ID, state, r.Zone, r.Owner, summary(t, r.Spec)})
	}
	return table(env.Stdout, "ID\tSTATE\tZONE\tOWNER\tSPEC", rows)
}

// list reads every page of a listing.
func (c *client) list(q url.Values) ([]resource, error) {
	var all []resource
	q.Set("limit", "1000")
	for {
		var page struct {
			Resources []resource `json:"resources"`
			Next      int64      `json:"next"`
		}
		if err := c.do("GET", "/v1/resources?"+q.Encode(), nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Resources...)
		if page.Next == 0 {
			return all, nil
		}
		q.Set("after", fmt.Sprint(page.Next))
	}
}

func oneID(t *typeView, verb string, pos []string) (string, error) {
	if len(pos) != 1 {
		return "", usagef("%s %s ID — one id", t.Name, verb)
	}
	if !strings.HasPrefix(pos[0], t.IDPrefix+"-") {
		return "", usagef("%s is not a %s's id (%s-…)", pos[0], t.Name, t.IDPrefix)
	}
	return pos[0], nil
}

func getCmd(env *Env, c *client, t *typeView, args []string) error {
	format := "yaml"
	opts := []*opt{outputOpt(&format)}
	pos, err := parse(args, opts)
	if errors.Is(err, errHelp) {
		fmt.Fprintf(env.Stdout, "hangar %s get ID\n\n", t.Name)
		flagHelp(env.Stdout, opts)
		return nil
	}
	if err != nil {
		return err
	}
	id, err := oneID(t, "get", pos)
	if err != nil {
		return err
	}
	var r json.RawMessage
	if err := c.do("GET", "/v1/resources/"+id, nil, &r); err != nil {
		return err
	}
	if format == "id" {
		fmt.Fprintln(env.Stdout, id)
		return nil
	}
	var v any
	_ = json.Unmarshal(r, &v)
	return show(env.Stdout, format, v)
}

func deleteCmd(env *Env, c *client, t *typeView, args []string) error {
	noWait := false
	opts := []*opt{boolOpt(&noWait, "answer at once, without waiting for it to go", "no-wait")}
	pos, err := parse(args, opts)
	if errors.Is(err, errHelp) {
		fmt.Fprintf(env.Stdout, "hangar %s delete ID\n\n", t.Name)
		flagHelp(env.Stdout, opts)
		return nil
	}
	if err != nil {
		return err
	}
	id, err := oneID(t, "delete", pos)
	if err != nil {
		return err
	}
	var acc accepted
	if err := c.do("DELETE", "/v1/resources/"+id+"?client_token="+clientToken(), nil, &acc); err != nil {
		return err
	}
	return finish(env, c, t, &acc, noWait, "yaml", "deleted")
}

func actCmd(env *Env, c *client, t *typeView, a *actionView, args []string) error {
	params := map[string]any{}
	var file string
	noWait, format := false, "yaml"
	own := []*opt{
		strOpt(&file, "FILE", "the params as YAML or JSON (- = stdin); flags are written over it", "from-file", "f"),
		boolOpt(&noWait, "answer at once, without waiting for it to end", "no-wait"),
		outputOpt(&format),
		setOpt(a.fields, params),
	}
	opts := own
	for _, o := range flags(a.fields, params) {
		if !slices.ContainsFunc(own, func(x *opt) bool { return slices.Contains(x.names, o.names[0]) }) {
			opts = append(opts, o)
		}
	}
	pos, err := parse(args, opts)
	if errors.Is(err, errHelp) {
		fmt.Fprintf(env.Stdout, "hangar %s %s ID — %s\n\n", t.Name, verb(a.Name), a.Description)
		flagHelp(env.Stdout, opts)
		return nil
	}
	if err != nil {
		return err
	}
	id, err := oneID(t, verb(a.Name), pos)
	if err != nil {
		return err
	}
	if file != "" {
		base, err := readDoc(env, file)
		if err != nil {
			return err
		}
		for k, v := range params {
			base[k] = v
		}
		params = base
	}
	var acc accepted
	if err := c.do("POST", "/v1/resources/"+id+"/actions/"+a.Name, map[string]any{"params": params, "client_token": clientToken()}, &acc); err != nil {
		return err
	}
	return finish(env, c, t, &acc, noWait, format, verb(a.Name)+":")
}
