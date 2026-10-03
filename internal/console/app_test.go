package console_test

import (
	"encoding/json"
	"fmt"
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
			// the one file here the app did not write (tools/editor) is read
			// like the others, but for the name every SVG is written under —
			// a name, in an image it draws from its own bytes: nothing is fetched
			if strings.HasPrefix(p, "vendor/") {
				line = strings.ReplaceAll(line, "http://www.w3.org/2000/svg", "")
			}
			if m := sinks.FindString(line); m != "" {
				at := strings.Index(line, m)
				t.Errorf("%s:%d holds %q: %s", p, i+1, m, strings.TrimSpace(line[max(0, at-80):min(len(line), at+120)]))
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
	if got.Errors["cores"] != "a whole number" || got.Errors["ratio"] != "a number" || got.Errors["extra"] != "line 1, column 2: it stops too soon: a } is missing" {
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

// JSON, on its own — run in a browser, where it runs. What is typed is read
// with the place it breaks at and what was expected there, and never
// disagrees with the browser's own reader on whether it is JSON. What is
// shown is listed as a person reads it: indented, what is short on one line,
// a text of several lines as its lines, the rest folded past twelve. And the
// box it is typed in — an editor, under the page's own policy: a plain box
// until it is there, the same verdict under both, the line it breaks at lit,
// the console's look.
func TestJSONTypedAndListed(t *testing.T) {
	br := browsertest.Start(t)
	s := stacktest.New(t, tokensOnly, "toy")
	p := br.Page(900, 700, false)
	p.Goto(s.URL + "/console/")
	p.Sees("COME IN")

	// ---- read
	said := map[string]string{
		`{"a": 1}`:   "sound",
		`{"a": 1,}`:  "1:9 nothing follows the last comma: take it out",
		`[1, 2`:      "1:6 it stops too soon: a ] is missing (unfinished)",
		`{"a": [`:    "1:8 it stops too soon: a ] is missing (unfinished)",
		`{"a" 1}`:    "1:6 a : is expected after a name",
		`{'a': 1}`:   `1:2 a text goes in double quotes: "…"`,
		`{"a": tru}`: "1:7 tru is not JSON: a text goes in quotes",
		`[1 2]`:      "1:4 a comma or a ] is expected",
		`{"a": 01}`:  "1:7 01 is not a number as JSON writes one",
		`{}  x`:      "1:5 something is left after the end",
		"[{\"p\": 1},\n {\"p\": 2}\n {\"p\": 3}]": "3:2 a comma or a ] is expected",
		"{\"a\": \"x\ny\"}":                       `1:7 this text is not closed on its line, or holds what JSON does not take (a tab, a lone \)`,
	}
	// sound or not: the same answer as the browser's own reader, whatever is typed
	both := []string{`1`, `-0`, `1e5`, `1E+5`, `-1.5e-3`, `0.5`, `-`, `1.`, `.5`, `+1`, `01`, `1 2`, `"a"`, `"\u00e9\n\"\\\/"`, `"\x"`, "\"a\tb\"", `"a`, `true`, `nul`, `null`,
		`[]`, `{}`, ` [ ] `, `[,]`, `[1,]`, `{,}`, `{"a"}`, `{"a":}`, `{"a":1 "b":2}`, `{"a":{"b":[1,{"c":null}]}}`, `[[[[]]]]`, `[`, `]`, `{"a":1}}`, "[1,\u00a02]", `{"a":1,"a":2}`, "\ufeff{}", ``, ` `}
	cases := make([]string, 0, len(said))
	for c := range said {
		cases = append(cases, c)
	}
	var read struct {
		Said     map[string]string `json:"said"`
		Disagree []string          `json:"disagree"`
		Lines    []int             `json:"lines"`
		Words    []string          `json:"words"`
	}
	cs, _ := json.Marshal(cases)
	bs, _ := json.Marshal(both)
	js := `import('./static/js/json.js').then((m) => {
	  const verdict = (c) => { const g = m.check(c); return g.places ? 'sound' : g.line + ':' + g.column + ' ' + g.message + (g.unfinished ? ' (unfinished)' : ''); };
	  const native = (c) => { try { JSON.parse(c); return true; } catch { return false; } };
	  const doc = '[{"p": 1},\n {"p": 2, "q/r": {"s": 3}},\n {"p": 3}]';
	  return {said: Object.fromEntries(` + string(cs) + `.map((c) => [c, verdict(c)])),
	    disagree: ` + string(bs) + `.concat(` + string(cs) + `).filter((c) => !!m.check(c).places !== native(c)),
	    lines: [m.lineOf(doc, ''), m.lineOf(doc, '/1/p'), m.lineOf(doc, '/1/q~1r/s'), m.lineOf(doc, '/2/none/there'), m.lineOf('[', '/0')],
	    words: [JSON.stringify(m.pairs({cores: 12, keys: ['a', 'b'], on: true})), JSON.stringify(m.pairs({rules: [{port: 22}]})), JSON.stringify(m.pairs(['a'])),
	      [m.nested('a'), m.nested('a\n'), m.nested('a\nb'), m.nested(['a', 1]), m.nested([{}]), m.nested({}), m.nested(null), m.nested(['a\nb'])].join(' ')]};
	})`
	if err := p.Eval(js, &read); err != nil {
		t.Fatal(err)
	}
	for c, want := range said {
		if read.Said[c] != want {
			t.Errorf("%q reads\n  %s\nnot\n  %s", c, read.Said[c], want)
		}
	}
	if len(read.Disagree) != 0 {
		t.Errorf("sound here and not for the browser, or the reverse: %q", read.Disagree)
	}
	if fmt.Sprint(read.Lines) != "[1 2 2 3 0]" {
		t.Errorf("the lines pointers lead to: %v", read.Lines)
	}
	if strings.Join(read.Words, " | ") != `[["cores",12],["keys",["a","b"]],["on",true]] | null | null | false false true false true true false true` {
		t.Errorf("pairs, nested: %v", read.Words)
	}

	// ---- listed
	var shown struct {
		Lines    []string `json:"lines"`
		Folded   []string `json:"folded"`
		Unfolded []string `json:"unfolded"`
		Again    int      `json:"again"`
		Link     string   `json:"link"`
		Raw      bool     `json:"raw"`
		One      []string `json:"one"`
	}
	js = `Promise.all([import('./static/js/code.js'), import('./static/js/dom.js')]).then(([c, d]) => {
	  const lines = (n) => [...n.querySelectorAll('.json-line')].map((l) => l.style.getPropertyValue('--depth') + (l.classList.contains('text') ? 't' : '') + '|' + l.textContent);
	  const keys = (n) => [...n.querySelectorAll('.json-key')].map((k) => k.textContent);
	  const link = (x) => (x.startsWith('box-') ? d.h('a', {href: '#/r/' + x, title: x}, 'named') : null);
	  const small = c.listing({a: {b: 1}, t: 'x\n\ny\n', l: [{c: 'd'}, 2], e: {}, i: ['box-1', 'no'], n: null, f: false}, {link});
	  const script = Array.from({length: 20}, (_, i) => 'line ' + (i + 1)).join('\n');
	  const unfolded = new Set();
	  const big = c.listing({script, after: 1}, {unfolded, key: 'k'});
	  const folded = [String(lines(big).length), keys(big)[0]];
	  big.querySelector('.json-key').click();
	  const a = small.querySelector('a');
	  return {lines: lines(small), folded, unfolded: [String(lines(big).length), keys(big)[0], [...unfolded].join()],
	    again: lines(c.listing({script, after: 1}, {unfolded, key: 'k'})).length,
	    link: a.getAttribute('href') + ' ' + a.textContent, raw: big.textContent.includes('\\n'),
	    one: [lines(c.listing({vm: 'img-1'})).join(), String(keys(c.listing({vm: 'img-1'})).length), lines(c.listing('a\nb')).join()]};
	})`
	if err := p.Eval(js, &shown); err != nil {
		t.Fatal(err)
	}
	want := []string{`0|{`, `1|"a": { "b": 1 },`, `1|"t": text, 3 lines`, `2t|x`, `2t|`, `2t|y`, `1|"l": [`, `2|{ "c": "d" },`, `2|2`, `1|],`, `1|"e": {},`, `1|"i": ["named", "no"],`, `1|"n": null,`, `1|"f": false`, `0|}`}
	if strings.Join(shown.Lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("a value is listed\n%s\nnot\n%s", strings.Join(shown.Lines, "\n"), strings.Join(want, "\n"))
	}
	if strings.Join(shown.Folded, " | ") != "12 | show all 24 lines" || strings.Join(shown.Unfolded, " | ") != "24 | fold | k" || shown.Again != 24 {
		t.Errorf("a long one: folded %v, unfolded %v, drawn again %d lines", shown.Folded, shown.Unfolded, shown.Again)
	}
	if shown.Link != "#/r/box-1 named" || shown.Raw {
		t.Errorf("an id inside leads to %q; a line break written as letters: %v", shown.Link, shown.Raw)
	}
	// what reads on one line has no key under it
	if strings.Join(shown.One, " ; ") != `0|{ "vm": "img-1" } ; 0 ; 0|text, 2 lines,1t|a,1t|b` {
		t.Errorf("short ones: %v", shown.One)
	}

	// ---- typed: a plain box until the editor is there, the same verdict under both
	var typed []string
	js = `import('./static/js/code.js').then(async (c) => {
	  const ed = c.editor({id: 'probe', label: 'probe'});
	  document.body.append(ed.node);
	  const says = ed.node.querySelector('.code-says');
	  const out = [];
	  out.push(says.textContent + '|' + (ed.node.querySelector('textarea') ? 'plain' : 'editor'));
	  ed.value = '[\n  {"port": 22}\n  {"port": 443}\n]';
	  ed.mark('/0/port');
	  out.push(says.textContent + '|' + says.className + '|' + document.getElementById('probe').value.length);
	  out.push('there ' + await ed.ready);
	  const root = ed.node.querySelector('.code-box').shadowRoot;
	  if (!root) return out;
	  const lit = () => [...root.querySelectorAll('.cm-lineNumbers .cm-lit-n')].map((x) => x.textContent).join();
	  const settled = () => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)));
	  await settled();
	  // what was typed in the plain box is in the editor; the plain box is gone
	  out.push([!!root.querySelector('.cm-editor'), !ed.node.querySelector('textarea'), ed.value.length, says.textContent, lit(), [...root.querySelectorAll('.cm-broken')].map((x) => x.textContent).join()].join('|'));
	  ed.value = '[\n  {"port": 22},\n  {"port": 70000}\n]';
	  await settled();
	  out.push(says.textContent + '|' + lit() + '|' + says.className);
	  ed.mark('/1/port');
	  await settled();
	  out.push('marked ' + lit());
	  ed.value = ed.value + ' ';
	  await settled();
	  out.push('typed ' + lit());
	  ed.value = '{"a": [';
	  await settled();
	  out.push(says.textContent + '|' + lit() + '|' + says.className);
	  ed.value = '{"a":[1,{"b":"c"}]}';
	  ed.node.querySelector('.json-key').click();
	  await settled();
	  out.push('tidy ' + JSON.stringify(ed.value) + ' ' + [...root.querySelectorAll('.cm-lineNumbers .cm-gutterElement')].pop().textContent);
	  // its look is the console's, read through the root it sits in: a name
	  // in ash, a number lit — and its styles are sheets that root adopts,
	  // never a style tag on the page (the page's policy would refuse one)
	  const colour = (word) => { const el = [...root.querySelectorAll('.cm-line span')].find((x) => x.textContent === word); return el ? getComputedStyle(el).color : 'no ' + word; };
	  out.push('look ' + colour('"a"') + ' ' + colour('1') + ' ' + getComputedStyle(root.querySelector('.cm-scroller')).fontFamily.split(',')[0]);
	  out.push('styles ' + document.querySelectorAll('style').length + ' ' + (root.adoptedStyleSheets.length > 0));
	  ed.node.remove();
	  return out;
	})`
	if err := p.Eval(js, &typed); err != nil {
		t.Fatal(err)
	}
	want = []string{
		`empty: left out|plain`,
		`line 3, column 3: a comma or a ] is expected|code-says bad|34`,
		`there true`,
		// the line it breaks at lit, the piece that breaks underlined
		`true|true|34|line 3, column 3: a comma or a ] is expected|3|{`,
		`reads as JSON · 4 lines||code-says on`,
		`marked 3`,
		`typed `,
		// what only stops too soon is being typed: said, its line not lit
		`line 1, column 8: it stops too soon: a ] is missing||code-says wait`,
		`tidy "{\n  \"a\": [\n    1,\n    {\n      \"b\": \"c\"\n    }\n  ]\n}" 8`,
		`look rgb(134, 140, 153) rgb(200, 255, 0) "IBM Plex Mono"`,
		`styles 0 true`,
	}
	if strings.Join(typed, "\n") != strings.Join(want, "\n") {
		t.Errorf("the box:\n%s\nnot\n%s", strings.Join(typed, "\n"), strings.Join(want, "\n"))
	}
	p.Quiet()
}
