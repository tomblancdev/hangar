// code.js — JSON on the page: a listing of a value that does not read on one
// line, and a box to type one in. The listing is JSON as it is typed —
// indented, one thing a line, what is short kept on one — but for a text of
// several lines, shown as its lines. The box is an editor — CodeMirror 6,
// the one file here the app did not write (../vendor/, tools/editor) —
// dressed and judged by the console: its look, and under it whether what is
// typed reads, and where it does not.

import {h, clear} from './dom.js';
import {check, lineOf, isText} from './json.js';

const span = (cls, ...children) => h('span', {class: cls}, children);
const dim = (s) => span('j-dim', s);
const plain = (v) => v === null || typeof v !== 'object';

// atom: a plain value as JSON writes it; a text that names a resource leads to it.
function atom(v, link) {
  if (typeof v !== 'string') return span('j-num', JSON.stringify(v));
  const to = link ? link(v) : null;
  return to ? span('j-str', '"', to, '"') : span('j-str', JSON.stringify(v));
}

// short: a list or an object that reads on one line.
function short(v) {
  return Object.values(v).every((x) => plain(x) && !isText(x)) && JSON.stringify(v).length <= 56;
}

// rows writes a value's lines: {depth, text, parts}.
function rows(v, depth, lead, trail, out, link) {
  if (isText(v)) {
    const text = v.replace(/\n+$/, '').split('\n');
    // no comma after it: its lines end it
    out.push({depth, parts: [lead, span('j-note', `text, ${text.length} lines`)]});
    for (const line of text) out.push({depth: depth + 1, text: true, parts: [line]});
    return;
  }
  if (plain(v)) { out.push({depth, parts: [lead, atom(v, link), trail]}); return; }
  const list = Array.isArray(v);
  const entries = list ? v.map((x) => [null, x]) : Object.entries(v);
  const [open, close] = list ? ['[', ']'] : ['{', '}'];
  const name = (k) => (k === null ? null : [span('j-key', JSON.stringify(k)), dim(': ')]);
  if (!entries.length) { out.push({depth, parts: [lead, dim(open + close), trail]}); return; }
  if (short(v)) {
    const pad = list ? '' : ' ';
    out.push({depth, parts: [lead, dim(open + pad), entries.map(([k, x], i) => [i ? dim(', ') : null, name(k), atom(x, link)]), dim(pad + close), trail]});
    return;
  }
  out.push({depth, parts: [lead, dim(open)]});
  entries.forEach(([k, x], i) => rows(x, depth + 1, name(k), i < entries.length - 1 ? dim(',') : null, out, link));
  out.push({depth, parts: [dim(close), trail]});
}

// listing draws a value as JSON a person reads.
//   link      (text) → a node, for a text that names a resource
//   fold      past this many lines the rest waits behind a key
//   unfolded  a Set kept by the page, and this listing's key in it: what a
//             person unfolded stays so when the page is drawn again
export function listing(v, {link = null, fold = 12, unfolded = null, key = ''} = {}) {
  const out = [];
  rows(v, 0, null, null, out, link);
  const long = out.length > fold + 3;
  let open = !long || (unfolded !== null && unfolded.has(key));
  const node = h('div', {class: 'json'});
  const line = (r) => {
    const el = h('div', {class: 'json-line' + (r.text ? ' text' : '')}, r.parts);
    // through the style object, not an attribute: the page's policy allows no inline style
    el.style.setProperty('--depth', String(r.depth));
    return el;
  };
  function draw() {
    clear(node, h('div', {class: 'json-lines'}, (open ? out : out.slice(0, fold)).map(line)),
      out.length > 1 ? h('div', {class: 'json-keys'},
        long ? h('button', {type: 'button', class: 'json-key', 'aria-expanded': String(open), onclick: () => {
          open = !open;
          if (unfolded) unfolded[open ? 'add' : 'delete'](key);
          draw();
        }}, open ? 'fold' : `show all ${out.length} lines`) : null,
        copyKey(v)) : null);
  }
  draw();
  return node;
}

// copyKey: the value as JSON, whole, on the clipboard — where the browser has one.
function copyKey(v) {
  if (!navigator.clipboard) return null;
  const key = h('button', {type: 'button', class: 'json-key', title: 'Copy it, as JSON'}, 'copy');
  key.addEventListener('click', async () => {
    try { await navigator.clipboard.writeText(JSON.stringify(v, null, 2)); key.textContent = 'copied'; } catch { key.textContent = 'not copied'; }
    setTimeout(() => { key.textContent = 'copy'; }, 2000);
  });
  return key;
}

// library: the editor's own code, fetched when a form first has a JSON field
// and never before — the pages that have none pay nothing for it.
let fetched = null;
const library = () => (fetched = fetched || import('../vendor/codemirror.js'));

// editor makes the box JSON is typed in: {node, value, mark(pointer),
// focus(), ready}. A plain box holds what is typed until the editor is
// there, and stays if it never comes (ready answers which). Under either,
// the verdict: it reads, or the line and column it breaks at and what was
// expected there. mark lights the line a refusal points at, until something
// is typed.
//
// node is the field itself to whoever looks for it by its id: it has a
// value, and takes the focus.
export function editor({id, label = '', rows: least = 6, placeholder = 'JSON'}) {
  const plain = h('textarea', {class: 'code-plain', rows: least, wrap: 'off', spellcheck: 'false', autocomplete: 'off', autocapitalize: 'none', placeholder, 'aria-label': label || null, 'aria-describedby': id + '-says'});
  const box = h('div', {class: 'code-box', hidden: true});
  const says = h('span', {id: id + '-says', class: 'code-says', role: 'status'});
  const tidy = h('button', {type: 'button', class: 'json-key', title: 'Write it again, indented', onclick: () => {
    const got = check(text());
    if (got.places) write(JSON.stringify(got.value, null, 2));
  }}, 'tidy');
  const node = h('div', {id, class: 'code'}, plain, box,
    h('div', {class: 'code-foot'}, says, h('span', {class: 'grow'}), h('span', {class: 'code-hint'}, 'Tab indents · Esc, then Tab, moves on'), tidy));
  let view = null;   // the editor, once it is there
  let light = null;  // its way to light a line
  let marked = '';   // a pointer to light once it is there

  const text = () => (view ? view.state.doc.toString() : plain.value);
  function write(v) {
    marked = '';
    if (view) view.dispatch({changes: {from: 0, to: view.state.doc.length, insert: v}});
    else { plain.value = v; said(); }
  }

  // said: the verdict under the box, whichever holds the text
  function said() {
    const now = text();
    const count = now.split('\n').length;
    const got = now.trim() === '' ? null : check(now);
    const broken = got && got.message ? got : null;
    // what only stops too soon is being typed: said, not shouted
    says.className = 'code-says' + (!got ? '' : !broken ? ' on' : broken.unfinished ? ' wait' : ' bad');
    says.textContent = !got ? 'empty: left out' : broken ? `line ${broken.line}, column ${broken.column}: ${broken.message}` : `reads as JSON · ${count} ${count === 1 ? 'line' : 'lines'}`;
    tidy.disabled = !got || !!broken;
  }
  plain.addEventListener('input', () => { marked = ''; said(); });
  said();

  Object.defineProperty(node, 'value', {get: text, set: write});
  node.focus = () => (view ? view.focus() : plain.focus());

  const ready = library().then((cm) => {
    const made = dress(cm, {doc: plain.value, label, placeholder, onChange: said});
    // in a root of its own: its styles are sheets the root adopts, where a
    // page's would be a style tag — which the page's policy refuses
    const root = box.attachShadow({mode: 'open'});
    const had = document.activeElement === plain;
    view = new cm.EditorView({state: made.state, parent: root, root});
    light = made.light;
    plain.remove();
    box.hidden = false;
    if (marked) view.dispatch({effects: light.of(lineOf(text(), marked))});
    if (had) view.focus();
    said();
    return true;
  }).catch((e) => {
    // the plain box stays, and what went wrong is said where a person who looks will find it
    console.error(e);
    return false;
  });

  return {
    node,
    ready,
    get value() { return text(); },
    set value(v) { write(v); },
    mark(pointer) {
      marked = pointer;
      if (view) view.dispatch({effects: light.of(lineOf(text(), pointer))});
    },
    focus() { node.focus(); },
  };
}

// dress puts the editor together: what it does (numbered lines, its history,
// pairs, folds, a search, several cursors), how it looks — the console's
// tokens, read through the root it sits in —, and what it lights: the line
// it breaks at, the stretch that breaks, the line a refusal points at.
function dress(cm, {doc, label, placeholder, onChange}) {
  const {EditorState, EditorView, StateField, StateEffect, RangeSet, Decoration, GutterMarker, gutterLineClass, HighlightStyle, tags, keymap} = cm;

  // judged: what the text reads as, and the line a refusal marked (gone
  // with the first thing typed)
  const light = StateEffect.define();
  const read = (state, marked) => {
    const now = state.doc.toString();
    const got = now.trim() === '' ? null : check(now);
    const broken = got && got.message && !got.unfinished ? got : null;
    const lines = new Set();
    if (broken) lines.add(Math.min(broken.line, state.doc.lines));
    if (marked) lines.add(Math.min(marked, state.doc.lines));
    return {marked, broken, lines: [...lines].sort((a, b) => a - b)};
  };
  const judged = StateField.define({
    create: (state) => read(state, 0),
    update(was, tr) {
      let marked = tr.docChanged ? 0 : was.marked;
      for (const e of tr.effects) if (e.is(light)) marked = e.value;
      return tr.docChanged || marked !== was.marked ? read(tr.state, marked) : was;
    },
  });
  const litLine = Decoration.line({class: 'cm-lit'});
  const stretch = Decoration.mark({class: 'cm-broken'});
  const litNumber = new (class extends GutterMarker { elementClass = 'cm-lit-n'; })();
  const lit = [
    judged,
    EditorView.decorations.compute([judged], (state) => {
      const {broken, lines} = state.field(judged);
      const ranges = lines.map((n) => litLine.range(state.doc.line(n).from));
      if (broken && broken.to > broken.at) ranges.push(stretch.range(broken.at, Math.min(broken.to, state.doc.length)));
      return Decoration.set(ranges, true);
    }),
    gutterLineClass.compute([judged], (state) => RangeSet.of(state.field(judged).lines.map((n) => litNumber.range(state.doc.line(n).from)))),
    EditorView.updateListener.of((u) => { if (u.docChanged || u.state.field(judged) !== u.startState.field(judged)) onChange(); }),
  ];

  const colours = HighlightStyle.define([
    {tag: tags.propertyName, color: 'var(--ash)'},
    {tag: tags.string, color: 'var(--chalk)'},
    {tag: [tags.number, tags.bool, tags.null], color: 'var(--toxic)'},
    {tag: [tags.separator, tags.brace, tags.squareBracket], color: 'var(--ash)', opacity: '.7'},
    {tag: tags.invalid, color: 'var(--hazard)'},
  ]);
  const key = {font: '600 11px/1 var(--mono)', letterSpacing: '.1em', textTransform: 'uppercase', color: 'var(--chalk)', background: 'var(--key)',
    border: '1px solid var(--rebar)', borderRadius: '0', padding: '6px 10px', cursor: 'pointer'};
  const look = EditorView.theme({
    '&': {color: 'var(--chalk)', backgroundColor: 'transparent', fontSize: '14px', maxHeight: '34em'},
    '&.cm-focused': {outline: 'none'},
    '.cm-scroller': {fontFamily: 'var(--mono)', lineHeight: '1.5', overflow: 'auto'},
    '.cm-content': {padding: '11px 0', minHeight: '9em', caretColor: 'var(--toxic)'},
    '.cm-line': {padding: '0 12px'},
    '.cm-cursor, .cm-dropCursor': {borderLeftColor: 'var(--toxic)', borderLeftWidth: '2px'},
    '&.cm-focused > .cm-scroller > .cm-selectionLayer .cm-selectionBackground, .cm-selectionBackground': {backgroundColor: 'rgba(200, 255, 0, .22)'},
    '.cm-selectionMatch': {backgroundColor: 'rgba(25, 230, 255, .16)'},
    '.cm-gutters': {backgroundColor: 'transparent', color: 'var(--ash)', border: '0', borderRight: '1px solid var(--rebar)'},
    '.cm-lineNumbers .cm-gutterElement': {minWidth: '3.4em', padding: '0 10px 0 12px', opacity: '.7'},
    '.cm-foldGutter .cm-gutterElement': {padding: '0 4px', cursor: 'pointer'},
    '.cm-activeLine': {backgroundColor: 'rgba(255, 255, 255, .03)'},
    '.cm-activeLineGutter': {backgroundColor: 'transparent', color: 'var(--chalk)'},
    // a line that breaks, or that the brain refused: its number lit, a wash across it
    '.cm-lit': {backgroundColor: 'rgba(255, 60, 90, .09)'},
    '.cm-lineNumbers .cm-gutterElement.cm-lit-n': {opacity: '1', color: 'var(--tar)', backgroundColor: 'var(--hazard)', fontWeight: '600'},
    '.cm-broken': {textDecoration: 'underline wavy var(--hazard)', textUnderlineOffset: '3px'},
    '&.cm-focused .cm-matchingBracket, .cm-matchingBracket': {backgroundColor: 'transparent', outline: '1px solid var(--toxic)'},
    '&.cm-focused .cm-nonmatchingBracket, .cm-nonmatchingBracket': {backgroundColor: 'transparent', outline: '1px solid var(--hazard)'},
    '.cm-foldPlaceholder': {color: 'var(--toxic)', backgroundColor: 'transparent', border: '1px solid var(--rebar)', padding: '0 6px'},
    '.cm-placeholder': {color: 'var(--ash)'},
    '.cm-panels': {color: 'var(--chalk)', backgroundColor: 'var(--steel)'},
    '.cm-panels.cm-panels-bottom': {borderTop: '1px solid var(--rebar)'},
    '.cm-panel.cm-search': {padding: '8px 12px', fontFamily: 'var(--mono)', fontSize: '12px'},
    '.cm-panel.cm-search label': {fontSize: '12px', color: 'var(--ash)'},
    '.cm-panel.cm-search [name=close]': {color: 'var(--chalk)', fontSize: '18px', cursor: 'pointer'},
    '.cm-textfield': {font: '400 13px/1.3 var(--mono)', color: 'var(--chalk)', backgroundColor: 'var(--glass)', border: '1px solid var(--rebar)', borderRadius: '0', padding: '5px 8px'},
    '.cm-textfield:focus': {borderColor: 'var(--toxic)', outline: 'none'},
    '.cm-button': {...key, backgroundImage: 'none'},
    '.cm-button:hover': {borderColor: 'var(--toxic)', color: 'var(--toxic)'},
    '.cm-button:active': {backgroundImage: 'none'},
    '.cm-searchMatch': {backgroundColor: 'rgba(25, 230, 255, .2)'},
    '.cm-searchMatch.cm-searchMatch-selected': {backgroundColor: 'rgba(200, 255, 0, .3)'},
  }, {dark: true});

  const state = EditorState.create({doc, extensions: [
    cm.lineNumbers(), cm.highlightActiveLineGutter(), cm.highlightSpecialChars(), cm.history(), cm.foldGutter(), cm.drawSelection(), cm.dropCursor(),
    EditorState.allowMultipleSelections.of(true), EditorState.tabSize.of(2), cm.indentUnit.of('  '), cm.indentOnInput(),
    cm.bracketMatching(), cm.closeBrackets(), cm.rectangularSelection(), cm.crosshairCursor(), cm.highlightActiveLine(), cm.highlightSelectionMatches(),
    cm.search(),
    keymap.of([...cm.closeBracketsKeymap, ...cm.defaultKeymap, ...cm.searchKeymap, ...cm.historyKeymap, ...cm.foldKeymap, cm.indentWithTab]),
    cm.json(), cm.syntaxHighlighting(colours), look, cm.placeholder(placeholder),
    EditorView.contentAttributes.of({'aria-label': label || 'JSON', spellcheck: 'false', autocapitalize: 'none'}),
    lit,
  ]});
  return {state, light};
}
