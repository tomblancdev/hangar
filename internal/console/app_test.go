package console_test

import (
	"encoding/json"
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/tomblancdev/hangar/internal/browsertest"
	"github.com/tomblancdev/hangar/internal/stacktest"
	"github.com/tomblancdev/hangar/ui"
)

// The app builds its pages from nodes: what the brain says — a name, a
// description, an error, anything a person typed — is text, never markup.
// No file of it may hold a way to turn a string into code or markup, load
// anything from elsewhere, or keep a credential in the browser.
func TestTheAppHoldsNoWayIn(t *testing.T) {
	sinks := regexp.MustCompile(`innerHTML|outerHTML|insertAdjacentHTML|document\.write|\beval\(|new Function|setAttribute\(\s*['"]on|srcdoc|javascript:|localStorage|sessionStorage|document\.cookie|https?://`)
	var files int
	err := fs.WalkDir(ui.Console(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !(strings.HasSuffix(p, ".js") || strings.HasSuffix(p, ".html") || strings.HasSuffix(p, ".css")) {
			return err
		}
		files++
		b, err := fs.ReadFile(ui.Console(), p)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(b), "\n") {
			if m := sinks.FindString(line); m != "" {
				t.Errorf("%s:%d holds %q: %s", p, i+1, m, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 8 {
		t.Fatalf("read %d of the app's files: the walk no longer finds them", files)
	}
}

// The schema reader, on its own: which control a field gets, and what JSON
// what was typed becomes — run in a browser, where it runs.
func TestTheSchemaReader(t *testing.T) {
	br := browsertest.Start(t)
	s := stacktest.New(t, tokensOnly, "toy")
	p := br.Page(800, 600, false)
	p.Goto(s.URL + "/console/")
	p.Sees("COME IN")
	const schema = `{
	  "type": "object", "required": ["must", "flag"],
	  "properties": {
	    "name":    {"type": "string", "description": "A name."},
	    "kind":    {"type": "string", "enum": ["vm", "container"], "default": "vm"},
	    "content": {"type": "string", "enum": ["block", "filesystem"]},
	    "cores":   {"type": "integer", "minimum": 1, "maximum": 8, "default": 1},
	    "ratio":   {"type": "number"},
	    "resume":  {"type": "boolean", "default": true},
	    "flag":    {"type": "boolean"},
	    "maybe":   {"type": "boolean"},
	    "image":   {"type": "string", "x-hangar-ref": "image"},
	    "machine": {"type": "string", "x-hangar-ref": "machine", "x-hangar-attached": true},
	    "keys":    {"type": "array", "items": {"type": "string", "x-hangar-ref": "keypair"}},
	    "shared":  {"type": "array", "x-hangar-share": true, "items": {"type": "string"}},
	    "must":    {"type": "array", "items": {"type": "string"}},
	    "notes":   {"type": "array", "items": {"type": "string"}},
	    "script":  {"type": "string", "maxLength": 16384},
	    "extra":   {"type": "object"},
	    "either":  {"type": ["string", "null"]}
	  }
	}`
	var got struct {
		Order   []string          `json:"order"`
		Widgets map[string]string `json:"widgets"`
		Doc     map[string]any    `json:"doc"`
		Errors  map[string]string `json:"errors"`
		Empty   map[string]any    `json:"empty"`
		Summary string            `json:"summary"`
		Words   []string          `json:"words"`
		Field   []string          `json:"field"`
	}
	js := `import('./static/js/schema.js').then((m) => {
	  const fields = m.fieldsOf(` + schema + `);
	  const widgets = Object.fromEntries(fields.map((f) => [f.name, m.widget(f) + (f.ref ? ':' + f.ref : '') + (f.attached ? ':attached' : '') + (f.required ? ':required' : '')]));
	  const typed = {name: '  box  ', kind: 'container', content: '', cores: '4', ratio: '0.5', resume: false, flag: false, maybe: 'no', image: '@debian', machine: '',
	    keys: ['kp-1', 'kp-2'], shared: [], must: '', notes: ' a \n\n b ', script: '#!/bin/sh\n  indented\n', extra: '{"a": 1}', either: ''};
	  const {doc, errors} = m.collect(fields, typed);
	  const bad = m.collect(fields, {cores: '1.5', ratio: 'x', extra: '{'});
	  const empty = m.collect(fields, {name: '', content: '', cores: '', maybe: '', image: '', keys: [], shared: [], notes: '', script: '  ', extra: ''}).doc;
	  return {order: fields.map((f) => f.name), widgets, doc, errors: bad.errors, empty,
	    summary: m.summary(fields, {name: 'box', kind: 'vm', cores: 2, resume: true, keys: ['a', 'b'], script: 'x'.repeat(40), extra: {a: 1}}, ['name']),
	    words: [m.label('memory_gb'), m.plural('Box'), m.plural('Key pair'), m.plural('Policy'), m.show(true), m.show(['a', 'b'])],
	    field: [m.fieldOf('/key_pairs/0'), m.fieldOf('/name'), m.fieldOf('')]};
	})`
	if err := p.Eval(js, &got); err != nil {
		t.Fatal(err)
	}
	// every field, in the order the schema writes them
	if strings.Join(got.Order, " ") != "name kind content cores ratio resume flag maybe image machine keys shared must notes script extra either" {
		t.Errorf("the fields' order: %v", got.Order)
	}
	for field, want := range map[string]string{
		"name": "text", "kind": "chips", "content": "select", "cores": "number", "ratio": "number", "resume": "check", "flag": "check:required", "maybe": "tristate",
		"image": "ref:image", "machine": "ref:machine:attached", "keys": "refs:keypair", "shared": "share", "must": "lines:required", "notes": "lines",
		"script": "textarea", "extra": "json", "either": "text",
	} {
		if got.Widgets[field] != want {
			t.Errorf("the control of %s: %s, not %s", field, got.Widgets[field], want)
		}
	}
	// what was typed, as JSON: trimmed, of its kind, a script kept as written,
	// what must be said said even when empty, what was left empty left out
	want := `{"cores":4,"extra":{"a":1},"flag":false,"image":"@debian","keys":["kp-1","kp-2"],"kind":"container","maybe":false,"must":[],"name":"box","notes":["a","b"],"ratio":0.5,"resume":false,"script":"#!/bin/sh\n  indented\n"}`
	if b, _ := json.Marshal(got.Doc); string(b) != want {
		t.Errorf("what was typed became\n%s\nnot\n%s", b, want)
	}
	if len(got.Empty) != 0 {
		t.Errorf("fields left empty were sent: %v", got.Empty)
	}
	if got.Errors["cores"] != "a whole number" || got.Errors["ratio"] != "a number" || got.Errors["extra"] != "not JSON" {
		t.Errorf("what could not be read: %v", got.Errors)
	}
	if got.Summary != "kind=vm · cores=2 · resume=true · keys=2 · script=xxxxxxxxxxxxxxxxxxxxxxxxxxxxx…" {
		t.Errorf("the summary: %s", got.Summary)
	}
	if strings.Join(got.Words, "|") != "memory gb|Boxes|Key pairs|Policies|yes|a, b" || strings.Join(got.Field, "|") != "key_pairs|name|" {
		t.Errorf("words: %v %v", got.Words, got.Field)
	}
	// a link leads inside the console, whatever it is given
	var links []string
	if err := p.Eval(`import('./static/js/dom.js').then((m) => ['#/r/x', 'signin?next=', 'https://elsewhere.example.org/', 'javascript:alert(1)', '//elsewhere.example.org', '/v1/tokens'].map((href) => {
	  try { m.h('a', {href}, 'x'); return 'made'; } catch { return 'refused'; }
	}))`, &links); err != nil {
		t.Fatal(err)
	}
	if strings.Join(links, " ") != "made made refused refused refused refused" {
		t.Errorf("links: %v", links)
	}
	p.Quiet()
}
