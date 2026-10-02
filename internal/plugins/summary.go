package plugins

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// A type says how one of its resources reads, at its schema's root:
//
//	"x-hangar-summary": ["{kind}", "{cores} cores", "{memory_gb} GB[ ({floor_gb} guaranteed)]", "{image|image_id}"]
//	"x-hangar-status":  {"field": "running", "on": "running", "off": "stopped"}
//
// The summary is one sentence: its parts, joined by " · ". In a part a hole
// {field} is that field's value — the spec's, else what was observed —,
// {a|b} the first of them that has one, {field?words} the words when the
// field is set, {field=value?words} the words when it is that value; a
// stretch between [ and ] is left out when one of its holes is empty, and so
// is a whole part. A field that names another resource (x-hangar-ref) reads
// as that resource's name. The core fills the sentence in and serves it, so
// every client prints the same words, and none of them knows a type.
//
// The status is the one word a settled resource wears: `on` while the field
// is set — as observed, else as asked —, `off` otherwise. A type that says
// none is "ready".

// Summary is a type's sentence, compiled.
type Summary struct {
	parts [][]piece
}

// StatusRule is a type's status word.
type StatusRule struct {
	Field string `json:"field"`
	On    string `json:"on"`
	Off   string `json:"off"`
}

type piece struct {
	text  string  // a literal, when hole and group are nil
	hole  *hole   // a hole
	group []piece // an optional stretch
}

type hole struct {
	fields []string // the first that has a value
	equals *string  // {field=value?words}
	words  string   // {field?words}
	asked  bool     // has a "?"
}

// readOf reads a schema root's x-hangar-summary and x-hangar-status.
func readOf(raw []byte) (*Summary, *StatusRule, error) {
	var doc struct {
		Summary []string    `json:"x-hangar-summary"`
		Status  *StatusRule `json:"x-hangar-status"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, nil, err
	}
	var sum *Summary
	if len(doc.Summary) > 0 {
		sum = &Summary{}
		for i, p := range doc.Summary {
			ps, rest, err := parsePieces(p, false)
			if err == nil && rest != "" {
				err = fmt.Errorf("a ] with no [")
			}
			if err != nil {
				return nil, nil, fmt.Errorf("x-hangar-summary part %d (%q): %w", i+1, p, err)
			}
			sum.parts = append(sum.parts, ps)
		}
	}
	if st := doc.Status; st != nil && (st.Field == "" || st.On == "" || st.Off == "") {
		return nil, nil, fmt.Errorf("x-hangar-status names a field, and its word on and off")
	}
	return sum, doc.Status, nil
}

// parsePieces reads a part, or — inGroup — an optional stretch up to its ].
func parsePieces(s string, inGroup bool) ([]piece, string, error) {
	var out []piece
	lit := func(t string) {
		if t != "" {
			out = append(out, piece{text: t})
		}
	}
	for {
		i := strings.IndexAny(s, "{[]")
		if i < 0 {
			if inGroup {
				return nil, "", fmt.Errorf("a [ with no ]")
			}
			lit(s)
			return out, "", nil
		}
		lit(s[:i])
		switch s[i] {
		case ']':
			if !inGroup {
				return out, s[i:], nil
			}
			return out, s[i+1:], nil
		case '[':
			if inGroup {
				return nil, "", fmt.Errorf("a [ inside a [")
			}
			g, rest, err := parsePieces(s[i+1:], true)
			if err != nil {
				return nil, "", err
			}
			out, s = append(out, piece{group: g}), rest
		case '{':
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				return nil, "", fmt.Errorf("a { with no }")
			}
			h, err := parseHole(s[i+1 : i+end])
			if err != nil {
				return nil, "", err
			}
			out, s = append(out, piece{hole: h}), s[i+end+1:]
		}
	}
}

func parseHole(s string) (*hole, error) {
	h := &hole{}
	head, words, asked := strings.Cut(s, "?")
	h.words, h.asked = words, asked
	if f, v, ok := strings.Cut(head, "="); ok {
		if !asked {
			return nil, fmt.Errorf("{%s}: a test says its words after a ?", s)
		}
		head, h.equals = f, &v
	}
	h.fields = strings.Split(head, "|")
	for _, f := range h.fields {
		if f == "" || strings.ContainsAny(f, " {}[]") {
			return nil, fmt.Errorf("{%s}: a hole names a field", s)
		}
	}
	if asked && (len(h.fields) != 1 || words == "") {
		return nil, fmt.Errorf("{%s}: one field, then the words", s)
	}
	return h, nil
}

// Summarize is the type's sentence for a resource; names are what the
// resources it names are called, by id. "" for a type that says none.
func (t *Type) Summarize(spec, observed json.RawMessage, names map[string]string) string {
	if t == nil || t.summary == nil {
		return ""
	}
	look := lookup(spec, observed)
	var parts []string
	for _, p := range t.summary.parts {
		if text, ok := render(p, look, names); ok && strings.TrimSpace(text) != "" {
			parts = append(parts, strings.TrimSpace(text))
		}
	}
	return strings.Join(parts, " · ")
}

// StatusOf is the word a settled resource of the type wears, and whether it
// is lit; ok is false for a type that says none.
func (t *Type) StatusOf(spec, observed json.RawMessage) (word string, on, ok bool) {
	if t == nil || t.status == nil {
		return "", false, false
	}
	if _, set := lookup(observed, spec)(t.status.Field); set {
		return t.status.On, true, true
	}
	return t.status.Off, false, true
}

// lookup reads a field in the first document, else in the second: its value
// as a person reads it, and whether it is set (present and not false, zero,
// empty).
func lookup(first, second json.RawMessage) func(string) (string, bool) {
	var a, b map[string]any
	_ = json.Unmarshal(first, &a)
	_ = json.Unmarshal(second, &b)
	return func(field string) (string, bool) {
		v, ok := a[field]
		if !ok {
			v = b[field]
		}
		switch x := v.(type) {
		case string:
			return x, x != ""
		case bool:
			return strconv.FormatBool(x), x
		case float64:
			return strconv.FormatFloat(x, 'f', -1, 64), x != 0
		case []any:
			var words []string
			for _, e := range x {
				if s, isString := e.(string); isString {
					words = append(words, s)
				}
			}
			if len(words) != len(x) {
				return strconv.Itoa(len(x)), len(x) > 0
			}
			return strings.Join(words, ", "), len(x) > 0
		}
		return "", false
	}
}

// longest is how much of a value a sentence shows.
const longest = 48

func render(ps []piece, look func(string) (string, bool), names map[string]string) (string, bool) {
	var b strings.Builder
	for _, p := range ps {
		switch {
		case p.group != nil:
			if text, ok := render(p.group, look, names); ok {
				b.WriteString(text)
			}
		case p.hole != nil:
			text, ok := fill(p.hole, look, names)
			if !ok {
				return "", false
			}
			b.WriteString(text)
		default:
			b.WriteString(p.text)
		}
	}
	return b.String(), true
}

func fill(h *hole, look func(string) (string, bool), names map[string]string) (string, bool) {
	if h.asked {
		v, set := look(h.fields[0])
		if h.equals != nil {
			return h.words, v == *h.equals
		}
		return h.words, set
	}
	for _, f := range h.fields {
		v, set := look(f)
		if !set {
			continue
		}
		// what it names reads by name: one id, or several
		ids := strings.Split(v, ", ")
		for i, id := range ids {
			if n := names[id]; n != "" {
				ids[i] = n
			}
		}
		v = strings.Join(ids, ", ")
		if r := []rune(v); len(r) > longest {
			v = string(r[:longest-1]) + "…"
		}
		return v, true
	}
	return "", false
}
