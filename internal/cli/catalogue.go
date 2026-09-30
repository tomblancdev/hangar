package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ---- The catalogue: what the brain says may be asked for --------------------

type typeView struct {
	Name        string          `json:"name"`
	IDPrefix    string          `json:"id_prefix"`
	Plugin      string          `json:"plugin"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
	Zones       []string        `json:"zones"`
	Actions     []actionView    `json:"actions"`

	fields []field
}

type actionView struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	ParamsSchema json.RawMessage `json:"params_schema"`
	ChangesUsage bool            `json:"changes_usage"`
	Zones        []string        `json:"zones"`

	fields []field
}

func (c *client) catalogue() ([]*typeView, error) {
	var out struct {
		Types []*typeView `json:"types"`
	}
	if err := c.do("GET", "/v1/types", nil, &out); err != nil {
		return nil, err
	}
	for _, t := range out.Types {
		var err error
		if t.fields, err = fieldsOf(t.Schema); err != nil {
			return nil, fmt.Errorf("type %s's schema: %v", t.Name, err)
		}
		for i := range t.Actions {
			if t.Actions[i].fields, err = fieldsOf(t.Actions[i].ParamsSchema); err != nil {
				return nil, fmt.Errorf("type %s's action %s: %v", t.Name, t.Actions[i].Name, err)
			}
		}
	}
	return out.Types, nil
}

func (t *typeView) action(name string) *actionView {
	name = strings.ReplaceAll(name, "-", "_")
	for i := range t.Actions {
		if strings.ReplaceAll(t.Actions[i].Name, "-", "_") == name {
			return &t.Actions[i]
		}
	}
	return nil
}

// field is a top-level property of a schema: a flag, a spec file's key.
type field struct {
	Name     string
	Kind     string // string, integer, number, boolean, array, object, or "" (any)
	Items    string // an array's items' kind
	Enum     []string
	Default  any
	Desc     string
	Ref      string // x-hangar-ref: the type it names by id
	Attached bool   // x-hangar-attached
	Required bool
}

// fieldsOf reads a schema's top-level properties in the order it writes them.
func fieldsOf(schema json.RawMessage) ([]field, error) {
	if len(schema) == 0 {
		return nil, nil
	}
	var top struct {
		Properties json.RawMessage `json:"properties"`
		Required   []string        `json:"required"`
	}
	if err := json.Unmarshal(schema, &top); err != nil {
		return nil, err
	}
	names, err := keysInOrder(top.Properties)
	if err != nil {
		return nil, err
	}
	var props map[string]struct {
		Type     any    `json:"type"`
		Enum     []any  `json:"enum"`
		Default  any    `json:"default"`
		Desc     string `json:"description"`
		Ref      string `json:"x-hangar-ref"`
		Attached bool   `json:"x-hangar-attached"`
		Items    *struct {
			Type any    `json:"type"`
			Ref  string `json:"x-hangar-ref"`
		} `json:"items"`
	}
	if len(top.Properties) > 0 {
		if err := json.Unmarshal(top.Properties, &props); err != nil {
			return nil, err
		}
	}
	var out []field
	for _, n := range names {
		p := props[n]
		f := field{Name: n, Kind: kindOf(p.Type), Default: p.Default, Desc: p.Desc, Ref: p.Ref, Attached: p.Attached}
		for _, e := range p.Enum {
			f.Enum = append(f.Enum, fmt.Sprint(e))
		}
		if p.Items != nil {
			f.Items = kindOf(p.Items.Type)
			if p.Items.Ref != "" {
				f.Ref = p.Items.Ref
			}
		}
		for _, r := range top.Required {
			if r == n {
				f.Required = true
			}
		}
		out = append(out, f)
	}
	return out, nil
}

func kindOf(t any) string {
	switch v := t.(type) {
	case string:
		return v
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok && s != "null" {
				return s
			}
		}
	}
	return ""
}

// keysInOrder lists a JSON object's keys as written.
func keysInOrder(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, fmt.Errorf("properties is not an object")
	}
	var out []string
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		out = append(out, t.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// idPattern is the shape of every id the brain mints.
var idPattern = regexp.MustCompile(`^[a-z][a-z0-9]{0,7}-[0-9a-f]{17}$`)

// ---- Flags drawn from fields ----------------------------------------------------

// valueOf reads a flag's text as its field's kind.
func (f field) valueOf(v string) (any, error) {
	switch f.Kind {
	case "integer":
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("a whole number, not %q", v)
		}
		return n, nil
	case "number":
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, fmt.Errorf("a number, not %q", v)
		}
		return n, nil
	case "boolean":
		switch strings.ToLower(v) {
		case "true", "1", "yes":
			return true, nil
		case "false", "0", "no":
			return false, nil
		}
		return nil, fmt.Errorf("true or false, not %q", v)
	case "string":
		return v, nil
	}
	// an object, or anything: YAML (so JSON too)
	var out any
	if err := yaml.Unmarshal([]byte(v), &out); err != nil {
		return nil, fmt.Errorf("not YAML nor JSON: %v", err)
	}
	return jsonable(out), nil
}

// flags turns fields into options writing into a document.
func flags(fs []field, doc map[string]any) []*opt {
	var out []*opt
	for _, f := range fs {
		f := f
		o := &opt{names: []string{f.Name}, value: f.Kind != "boolean", help: f.help(), arg: f.arg()}
		switch {
		case f.Kind == "array":
			item := field{Kind: f.Items}
			o.set = func(v string) error {
				list, _ := doc[f.Name].([]any)
				for _, part := range strings.Split(v, ",") {
					x, err := item.valueOf(strings.TrimSpace(part))
					if err != nil {
						return err
					}
					list = append(list, x)
				}
				doc[f.Name] = list
				return nil
			}
		default:
			o.set = func(v string) error {
				x, err := f.valueOf(v)
				if err != nil {
					return err
				}
				doc[f.Name] = x
				return nil
			}
		}
		out = append(out, o)
	}
	return out
}

func (f field) arg() string {
	switch {
	case f.Ref != "":
		return "ID"
	case f.Kind == "integer" || f.Kind == "number":
		return "N"
	case len(f.Enum) > 0:
		return strings.Join(f.Enum, "|")
	case f.Kind == "array":
		return "A,B"
	case f.Kind == "object" || f.Kind == "":
		return "YAML"
	}
	return "TEXT"
}

func (f field) help() string {
	h := f.Desc
	if f.Ref != "" {
		many := "a"
		if f.Kind == "array" {
			many = "each"
		}
		h += fmt.Sprintf(" (%s %s: an id, or @<schedule> for the newest it made)", many, f.Ref)
	}
	if f.Default != nil {
		h += fmt.Sprintf(" [default %v]", f.Default)
	}
	if f.Required {
		h += " [required]"
	}
	return strings.TrimSpace(h)
}

// setOpt is --set key=value: any field, its value YAML — for objects, and for
// a field whose name one of the command's own flags already takes.
func setOpt(fs []field, doc map[string]any) *opt {
	return &opt{names: []string{"set"}, value: true, arg: "KEY=YAML", help: "any field, its value read as YAML (repeatable)", set: func(v string) error {
		k, val, ok := strings.Cut(v, "=")
		if !ok {
			return fmt.Errorf("key=value, not %q", v)
		}
		f := field{Name: k}
		for _, x := range fs {
			if x.Name == k {
				f = x
			}
		}
		if f.Kind == "string" {
			doc[k] = val
			return nil
		}
		f.Kind = ""
		x, err := f.valueOf(val)
		if err != nil {
			return err
		}
		doc[k] = x
		return nil
	}}
}

// jsonable turns what YAML decodes into what JSON encodes.
func jsonable(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = jsonable(e)
		}
		return x
	case map[any]any:
		m := map[string]any{}
		for k, e := range x {
			m[fmt.Sprint(k)] = jsonable(e)
		}
		return m
	case []any:
		for i, e := range x {
			x[i] = jsonable(e)
		}
		return x
	}
	return v
}

// summary is a spec's scalar fields, in its schema's order, short.
func summary(t *typeView, spec json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(spec, &m) != nil {
		return ""
	}
	var parts []string
	seen := map[string]bool{}
	add := func(k string) {
		v, ok := m[k]
		if !ok || seen[k] {
			return
		}
		seen[k] = true
		switch x := v.(type) {
		case string:
			if len(x) > 32 {
				x = x[:29] + "…"
			}
			if x != "" {
				parts = append(parts, k+"="+x)
			}
		case float64, bool:
			parts = append(parts, fmt.Sprintf("%s=%v", k, x))
		case []any:
			if len(x) > 0 {
				parts = append(parts, fmt.Sprintf("%s=%d", k, len(x)))
			}
		}
	}
	if t != nil {
		for _, f := range t.fields {
			add(f.Name)
		}
	} else {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			add(k)
		}
	}
	return strings.Join(parts, " ")
}
