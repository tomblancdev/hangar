// dom.js — the page is built from nodes, never from markup: what the brain
// (or anyone through it) says is always text, never code. No raw-markup sink
// exists in this app, and a test holds it to that.

// h makes an element: h('a', {class: 'btn', href: '#/'}, 'Home').
export function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  let value;
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === null || v === undefined || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k === 'value') value = v;
    else if (k === 'href') el.setAttribute('href', internal(v));
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (v === true) el.setAttribute(k, '');
    else el.setAttribute(k, String(v));
  }
  add(el, children);
  // after its children: a select's value names one of its options
  if (value !== undefined) el.value = value;
  return el;
}

// internal: a link leads to a place in the app, or to the console's own
// sign-in — nowhere else, whatever a field says.
function internal(href) {
  if (href.startsWith('#/') || href === '#' || href.startsWith('signin?') || href === 'signin') return href;
  throw new Error('not a link inside the console: ' + href);
}

export function add(el, children) {
  for (const c of children.flat(Infinity)) {
    if (c === null || c === undefined || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

export function clear(el, ...children) {
  el.replaceChildren();
  return add(el, children);
}

// bar is a gauge: how much of a limit is used.
export function bar(used, max, cls) {
  const fill = h('div', {class: 'bar-fill'});
  const pct = max > 0 ? Math.min(100, Math.max(0, (used / max) * 100)) : 0;
  // through the style object, not an attribute: the page's policy allows no inline style
  fill.style.width = pct + '%';
  return h('div', {class: 'bar ' + (cls || ''), role: 'img', 'aria-label': `${fmtNumber(used)} of ${fmtNumber(max)}`}, fill);
}

export function fmtNumber(n) {
  if (typeof n !== 'number') return String(n);
  return Number.isInteger(n) ? String(n) : n.toFixed(1);
}

// ago says how long ago, shortly: "now", "4 min", "2 h", "3 d", then a date.
export function ago(iso, now = Date.now()) {
  if (!iso) return '';
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return '';
  const s = Math.max(0, Math.round((now - t) / 1000));
  if (s < 45) return 'now';
  if (s < 3600) return Math.round(s / 60) + ' min ago';
  if (s < 86400) return Math.round(s / 3600) + ' h ago';
  if (s < 7 * 86400) return Math.round(s / 86400) + ' d ago';
  return day(iso);
}

export function day(iso) {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleDateString(undefined, {day: 'numeric', month: 'long', year: 'numeric'});
}

export function took(from, to) {
  const s = Math.max(0, Math.round((Date.parse(to) - Date.parse(from)) / 1000));
  if (Number.isNaN(s)) return '';
  return s < 90 ? s + ' s' : Math.round(s / 60) + ' min';
}
