// json.js — JSON as a person types it and reads it. Pure: no page here, so
// it is tested on its own. What is typed is read with the place it breaks
// at, in words — the browser's own reader says whether, seldom where, and
// never the same from one browser to the next; what is shown is told apart:
// a value that reads on one line, and one that needs a listing of its own.

const pieces = /([ \t\n\r]+)|("(?:[^"\\\u0000-\u001f]|\\(?:["\\/bfnrt]|u[0-9a-fA-F]{4}))*")|(-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?(?![\w.+-]))|((?:true|false|null)(?!\w))|([{}\[\],:])|("[^\n]*|[^ \t\n\r{}\[\],:"]+)/y;

// lex cuts a text into its pieces: {kind, text, at}. kind: space, string,
// number, word (true, false, null), punct — or bad: what JSON has no place for.
function lex(text) {
  const out = [];
  pieces.lastIndex = 0;
  let m;
  while (pieces.lastIndex < text.length && (m = pieces.exec(text))) {
    const kind = m[1] !== undefined ? 'space' : m[2] !== undefined ? 'string' : m[3] !== undefined ? 'number' : m[4] !== undefined ? 'word' : m[5] !== undefined ? 'punct' : 'bad';
    out.push({kind, text: m[0], at: m.index});
  }
  return out;
}

function bad(p) {
  if (p.text.startsWith('"')) return 'this text is not closed on its line, or holds what JSON does not take (a tab, a lone \\)';
  if (p.text.startsWith("'")) return 'a text goes in double quotes: "…"';
  const said = p.text.length > 24 ? p.text.slice(0, 23) + '…' : p.text;
  return /^-?[\d.]/.test(p.text) ? said + ' is not a number as JSON writes one' : said + ' is not JSON: a text goes in quotes';
}

// check reads a text as JSON. Sound: {value, places} — places says where
// each thing in it begins, by its pointer (/1/port). Not sound: {message,
// at, to, line, column, unfinished} — the first place it breaks (the piece
// from at to to), what was expected there, and whether it only stops too
// soon.
export function check(text) {
  const toks = lex(text).filter((p) => p.kind !== 'space');
  const places = new Map();
  let i = 0;
  const broke = (p, message) => Object.assign(new Error(message), {at: p ? p.at : text.length, to: p ? p.at + p.text.length : text.length, unfinished: !p});
  const step = (s) => String(s).replace(/~/g, '~0').replace(/\//g, '~1');
  function value(path) {
    const p = toks[i];
    if (!p) throw broke(null, 'it stops too soon: a value is missing');
    places.set(path, p.at);
    if (p.kind === 'string' || p.kind === 'number' || p.kind === 'word') { i++; return; }
    if (p.text === '{' || p.text === '[') {
      const list = p.text === '[';
      const close = list ? ']' : '}';
      i++;
      if (toks[i] && toks[i].text === close) { i++; return; }
      for (let n = 0; ; n++) {
        let name = n;
        const k = toks[i];
        if (!k) throw broke(null, `it stops too soon: a ${close} is missing`);
        if (k.text === close) throw broke(k, 'nothing follows the last comma: take it out');
        if (!list) {
          if (k.kind !== 'string') throw broke(k, k.kind === 'bad' ? bad(k) : 'a name in quotes is expected');
          name = JSON.parse(k.text);
          i++;
          if (!toks[i] || toks[i].text !== ':') throw broke(toks[i], 'a : is expected after a name');
          i++;
        }
        value(path + '/' + step(name));
        const after = toks[i];
        if (after && after.text === ',') { i++; continue; }
        if (after && after.text === close) { i++; return; }
        throw broke(after, after ? `a comma or a ${close} is expected` : `it stops too soon: a ${close} is missing`);
      }
    }
    throw broke(p, p.kind === 'bad' ? bad(p) : 'a value is expected');
  }
  try {
    value('');
    if (toks[i]) throw broke(toks[i], 'something is left after the end');
    return {value: JSON.parse(text), places};
  } catch (e) {
    const at = typeof e.at === 'number' ? e.at : 0;
    const before = text.slice(0, at);
    return {message: typeof e.at === 'number' ? e.message : 'not JSON', at, to: typeof e.to === 'number' ? e.to : at, unfinished: e.unfinished === true,
      line: before.split('\n').length, column: at - before.lastIndexOf('\n')};
  }
}

// lineOf says which line of a text a pointer's value begins on (/1/port) —
// or the nearest thing above it that is there; 0 when the text is not JSON.
export function lineOf(text, pointer) {
  const got = check(text);
  if (!got.places) return 0;
  let p = pointer || '';
  while (p && !got.places.has(p)) p = p.slice(0, p.lastIndexOf('/'));
  return text.slice(0, got.places.get(p)).split('\n').length;
}

// isText: a text of several lines (a first-boot script).
export function isText(v) {
  return typeof v === 'string' && v.replace(/\n+$/, '').includes('\n');
}

// nested: a value that does not read on one line — an object, a list that
// holds one, a text of several lines.
export function nested(v) {
  if (isText(v)) return true;
  if (Array.isArray(v)) return v.some((x) => nested(x));
  return v !== null && typeof v === 'object';
}

// pairs: an object's pairs when each of its values reads on one line — the
// command line writes them cores=4 · memory_gb=8; null when one does not.
export function pairs(o) {
  if (o === null || typeof o !== 'object' || Array.isArray(o)) return null;
  const out = Object.entries(o);
  return out.some(([, v]) => nested(v)) ? null : out;
}
