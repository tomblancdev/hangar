// terminal.js — a resource's stream, drawn as a terminal: a machine's own
// screen and keyboard. The screen is xterm.js (the second file here the app
// did not write: ../vendor/xterm.js, tools/terminal); the socket is the
// console's own, api/v1/resources/<id>/streams/<stream>, which passes it on
// to the brain with the person's token. What is typed goes as bytes, a
// window that changed as {"size":[cols,rows]}; what the machine says comes
// as bytes, the console's own words as text ({"opened":true} once the
// brain's stream is open, {"refused":…} when it is not), and why it ended
// in the socket's last words.
//
// Nothing here knows what a machine is: any type's stream opens the same.
//
// A machine keeps no picture of its screen, and neither does anything on the
// way: opened again, a shell that was left there is there, and says nothing
// until it is spoken to. Two things here answer that. Between its two pages —
// under its machine's keys, the whole window — a terminal is not opened again
// at all: it is carried there as it is, the same screen on the same socket
// (pass). And one that is opened again and hears nothing says so over its
// screen, with a key that asks the machine to draw (quiet).

import {h, clear} from './dom.js';

let fetched = null;
const library = () => (fetched = fetched || sheets().then(() => import('../vendor/xterm.js')));

// The page's policy lets no script write a <style> (style-src 'self'), and
// xterm.js writes three: its colours, its cells' size, its scrollbar's. Each
// becomes a sheet the document adopts instead — a constructed sheet is the
// page's own object, not text the policy has to judge. For the life of the
// page, and for <style> alone: nothing else here makes one.
let shimmed = false;
async function sheets() {
  if (shimmed) return;
  shimmed = true;
  const make = document.createElement;
  document.createElement = function (name, ...rest) {
    if (String(name).toLowerCase() !== 'style') return make.call(document, name, ...rest);
    const sheet = new CSSStyleSheet();
    document.adoptedStyleSheets = [...document.adoptedStyleSheets, sheet];
    const el = make.call(document, 'x-style');
    Object.defineProperty(el, 'textContent', {get: () => '', set: (v) => sheet.replaceSync(String(v))});
    const remove = el.remove.bind(el);
    el.remove = () => { document.adoptedStyleSheets = document.adoptedStyleSheets.filter((s) => s !== sheet); remove(); };
    return el;
  };
}

// its colours are the theme's: a console that is re-themed re-themes this
function colours() {
  const css = getComputedStyle(document.documentElement);
  const v = (name, or) => css.getPropertyValue(name).trim() || or;
  return {
    background: v('--glass', '#06070a'), foreground: v('--chalk', '#e9e6dc'), cursor: v('--toxic', '#c8ff00'), cursorAccent: v('--glass', '#06070a'),
    selectionBackground: v('--tape', 'rgba(200, 255, 0, .55)'), selectionForeground: v('--tar', '#0a0b0e'),
    black: '#1b1f26', red: v('--hazard', '#ff3c5a'), green: '#7ec699', yellow: '#e6c26b', blue: '#6fa8dc', magenta: '#c594c5', cyan: v('--signal', '#19e6ff'), white: '#c9c6bd',
    brightBlack: v('--ash', '#868c99'), brightRed: '#ff7088', brightGreen: v('--toxic', '#c8ff00'), brightYellow: '#ffd76e', brightBlue: '#8fc1f2', brightMagenta: '#e0b0e0',
    brightCyan: '#7af0ff', brightWhite: v('--chalk', '#e9e6dc'),
  };
}

// why a socket closed, in words: the brain's own where it gave some
function ending(ev, refused) {
  if (refused) return {word: 'REFUSED', why: refused.detail || 'the brain refused it', again: 'TRY AGAIN'};
  if (ev.code === 4001) return {word: 'TAKEN', why: (ev.reason || 'opened elsewhere') + '.', again: 'OPEN IT HERE'};
  if (ev.code === 4002) return {word: 'SIGNED OUT', why: 'Your sign-in ended: sign in again.', again: ''};
  if (ev.code === 4000) return {word: 'ENDED', why: cap(ev.reason || 'it was closed') + '.', again: 'OPEN AGAIN'};
  if (ev.code === 1000 || ev.code === 1001) return {word: 'CLOSED', why: 'Closed.', again: 'OPEN AGAIN'};
  return {word: 'CUT', why: 'The connection was lost.', again: 'OPEN AGAIN'};
}
const cap = (s) => s.charAt(0).toUpperCase() + s.slice(1);

// piece: the most bytes one message carries of what is typed or pasted
const piece = 16 * 1024;

// A terminal on its way from one of its pages to the other, by "<id>/<stream>":
// kept as it is for the page the same navigation shows. That page is built
// in the turn the terminal was passed in; one that did not ask for it then
// never will, and the terminal is closed when the turn ends.
const carried = new Map();

// carriedTo names the stream of a resource that is on its way here, if one
// is: its page takes it at once, before it asks the brain anything.
export function carriedTo(id) {
  for (const key of carried.keys()) if (key.startsWith(id + '/')) return key.slice(id.length + 1);
  return '';
}

// quietAfter: how long an open terminal hears nothing before it says so,
// counted from the word that the brain's own stream is open. A machine about
// to sign someone in asks a terminal its size every two seconds, and the
// port's last legs (the engine's proxy, a cluster's hop) take up to another:
// past four, whatever is at the other end waits to be spoken to.
const quietAfter = 4000;

// forget: what a screen kept across an ending is told before its machine is
// opened again — a program that asked to be told of the window's focus may
// be gone, and the page taking the keyboard would type a report at whatever
// reads it now. (DECRST 1004, written to the screen, never to the machine.)
const forget = '\x1b[?1004l';

// fewest: the smallest grid a screen is fitted to. Under it there is no room
// for a terminal, only a window passing through: the grid stays as it was.
const fewest = {cols: 16, rows: 4};

// redraw: what asks a shell, an editor, a pager to draw its screen again —
// Ctrl-L, as a person types it.
const redraw = '\x0c';

// the keys a phone's keyboard has none of
const keys = [['Esc', '\x1b'], ['Tab', '\t'], ['Ctrl', null], ['↑', 'A'], ['↓', 'B'], ['←', 'D'], ['→', 'C']];

// terminal makes one: {node, open(), close(), pass(), focus()}. Nothing is
// asked of the brain until open(); close() lets go of it (and of the screen);
// pass() keeps it for its other page. lead, controls: what the caller puts at
// the start and the end of its bar (whose it is; a switch, a way out).
//
// One that was passed a moment ago is the one handed back: the same node,
// the same screen, the same socket, in the bar of the page that asks.
export function terminal({id, stream, lead = null, controls = []}) {
  const key = id + '/' + stream;
  const was = carried.get(key);
  if (was) {
    carried.delete(key);
    clearTimeout(was.timer);
    // out of the page it leaves: it settles where it is laid next
    was.t.node.remove();
    was.dress(lead, controls);
    return was.t;
  }
  const state = h('span', {class: 'term-state', 'aria-live': 'polite'}, 'not open');
  const grid = h('span', {class: 'term-grid'});
  const screen = h('div', {class: 'term-screen'});
  const hush = h('div', {class: 'term-quiet'});
  const note = h('div', {class: 'term-note'});
  const pad = h('div', {class: 'term-keys', role: 'group', 'aria-label': 'Keys a phone has none of'});
  const bar = h('div', {class: 'term-bar'});
  const node = h('section', {class: 'term', 'data-state': 'closed', 'aria-label': `The ${stream} of ${id}`},
    bar, h('div', {class: 'term-view'}, screen, hush), pad, note);
  const dress = (first, last) => clear(bar, first, h('span', {class: 'lbl'}, stream), state, grid, h('span', {class: 'grow'}), last);
  dress(lead, controls);

  let term = null;
  let fit = null;
  let ws = null;
  let watch = null;
  let ctrl = false;
  let wanted = false; // open() was called, and close() not since
  let took = ''; // the grid the machine was told at its opening
  let passed = false; // on its way to its other page
  let arriving = false; // asked for there, and not laid out there yet
  let hushing = null; // the wait before it says the machine is quiet
  const enc = new TextEncoder();

  function say(word, cls) {
    node.dataset.state = word;
    state.textContent = word;
    state.className = 'term-state ' + (cls || '');
  }

  function send(text) {
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    if (ctrl && text.length === 1) {
      // Ctrl and a letter: the letter's place among the control characters
      const c = text.toUpperCase().charCodeAt(0);
      if (c >= 63 && c <= 95) text = String.fromCharCode(c === 63 ? 127 : c & 31);
      setCtrl(false);
    }
    // a paste goes in pieces: no message here is ever a large one
    const bytes = enc.encode(text);
    for (let at = 0; at < bytes.length; at += piece) ws.send(bytes.subarray(at, at + piece));
  }

  function setCtrl(on) {
    ctrl = on;
    const b = pad.querySelector('[data-key="Ctrl"]');
    if (b) { b.classList.toggle('chosen', on); b.setAttribute('aria-pressed', String(on)); }
  }

  clear(pad, keys.map(([name, seq]) => h('button', {type: 'button', class: 'btn small', 'data-key': name, 'aria-pressed': name === 'Ctrl' ? 'false' : null,
    // the keyboard stays up: the press never takes the focus from the screen
    onpointerdown: (e) => e.preventDefault(),
    onclick: () => {
      if (!term) return;
      if (name === 'Ctrl') setCtrl(!ctrl);
      // an arrow says what the program on the screen asked arrows to say
      else if (seq.length === 1 && 'ABCD'.includes(seq)) send((term.modes.applicationCursorKeysMode ? '\x1bO' : '\x1b[') + seq);
      else send(seq);
      term.focus();
    }}, name)));

  // quiet: open, and not a word from the machine — as a shell left there
  // says none. Said over the screen, with the key that asks it to draw;
  // nothing is typed for anyone. Gone at the machine's first word, or at the
  // person's first key.
  function quiet() {
    hushing = null;
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    node.dataset.quiet = 'yes';
    clear(hush, h('div', {class: 'slip', role: 'status'},
      h('div', {class: 'slip-title'}, 'It is where it was left'),
      h('p', {}, 'A machine says nothing by itself, and keeps no picture of its screen. Redraw asks what holds it to draw again: it types Ctrl-L. At a login, press Enter.'),
      h('div', {class: 'slip-end'}, h('button', {type: 'button', class: 'btn small',
        // the keyboard stays up: the press never takes the focus from the screen
        onpointerdown: (e) => e.preventDefault(),
        onclick: () => {
          if (ws && ws.readyState === WebSocket.OPEN) ws.send(enc.encode(redraw));
          if (term) term.focus();
        }}, 'Redraw'))));
  }

  function spoke() {
    if (hushing) { clearTimeout(hushing); hushing = null; }
    if (!node.dataset.quiet) return;
    delete node.dataset.quiet;
    clear(hush);
  }

  function sized() {
    if (!term) return;
    const now = `${term.cols} × ${term.rows}`;
    clear(grid, now, took && took !== now ? h('span', {class: 'term-hint', title: 'A machine takes its terminal\'s size when you sign in to it.'}, ' · resized: exit signs you in at this size') : null);
  }

  function refit() {
    if (!term || !fit || !screen.clientWidth) return;
    try {
      // fitted to the room its screen has — when it has some. A window on
      // its way to another size passes through ones nothing fits in, and a
      // grid of two columns would push a screenful into the scrollback
      const room = fit.proposeDimensions();
      if (!room || !(room.cols >= fewest.cols) || !(room.rows >= fewest.rows)) return;
      fit.fit();
    } catch { /* not laid out yet */ }
    sized();
  }

  async function open() {
    if (passed) {
      // come from its other page as it was: nothing is asked again
      passed = false;
      settle();
      if (arriving) requestAnimationFrame(() => { if (arriving) settle(); });
      return;
    }
    wanted = true;
    clear(note);
    say('opening…', 'busy');
    let lib;
    try {
      lib = await library();
      // its cells are measured in the console's own face: wait for it
      if (document.fonts && document.fonts.load) await Promise.all([document.fonts.load('14px "IBM Plex Mono"'), document.fonts.load('600 14px "IBM Plex Mono"')]).catch(() => {});
    } catch (e) {
      say('failed', 'bad');
      clear(note, flyer('FAILED', 'The terminal could not be loaded: ' + (e.message || e), 'TRY AGAIN'));
      return;
    }
    if (!wanted) return;
    if (!term) {
      const css = getComputedStyle(document.documentElement);
      term = new lib.Terminal({
        fontFamily: css.getPropertyValue('--mono').trim() || 'monospace', fontSize: 14, lineHeight: 1.15, cursorBlink: true, scrollback: 5000,
        theme: colours(),
        // « report your size, in characters »: how a machine learns the
        // window it is opened in — its serial port carries none
        windowOptions: {getWinSizeChars: true},
        // and none tells a shell its window changed, so no shell draws its
        // line again after one: the line the cursor is on is wrapped and
        // unwrapped with the others, not cut at a narrower window's edge
        reflowCursorLine: true,
      });
      fit = new lib.FitAddon();
      term.loadAddon(fit);
      term.open(screen);
      try {
        // drawn on a canvas where the browser can: every colour, and fast
        const gl = new lib.WebglAddon();
        gl.onContextLoss(() => gl.dispose());
        term.loadAddon(gl);
        node.dataset.drawn = 'webgl';
      } catch {
        node.dataset.drawn = 'dom';
      }
      term.onData((d) => { spoke(); send(d); });
      // a machine takes its terminal's size when it asks for it — at a
      // sign-in — and at no other time: the bar's « resized » is measured
      // from that question, and leaves at the next one (the library answers
      // it after this: nothing is taken from it)
      term.parser.registerCsiHandler({final: 't'}, (params) => {
        if (params[0] === 18) { took = `${term.cols} × ${term.rows}`; sized(); }
        return false;
      });
      term.onBinary((d) => {
        if (!ws || ws.readyState !== WebSocket.OPEN) return;
        const bytes = Uint8Array.from(d, (c) => c.charCodeAt(0));
        for (let at = 0; at < bytes.length; at += piece) ws.send(bytes.subarray(at, at + piece));
      });
      term.onResize(({cols, rows}) => { if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({size: [cols, rows]})); });
      watch = new ResizeObserver(() => { if (arriving) settle(); else refit(); });
      watch.observe(screen);
    }
    refit();
    connect();
  }

  // settle: carried to another page, it takes that page's size, draws there
  // what it holds and takes the keyboard — once it is laid out there, which
  // may be a moment after it is asked for.
  function settle() {
    arriving = !screen.clientWidth;
    if (arriving || !term) return;
    refit();
    term.refresh(0, term.rows - 1);
    term.focus();
  }

  function connect() {
    if (ws) { ws.onclose = null; ws.close(1000); }
    spoke();
    term.write(forget);
    const u = new URL(`api/v1/resources/${encodeURIComponent(id)}/streams/${encodeURIComponent(stream)}`, document.baseURI);
    u.protocol = u.protocol === 'https:' ? 'wss:' : 'ws:';
    u.search = `?cols=${term.cols}&rows=${term.rows}`;
    took = `${term.cols} × ${term.rows}`;
    let refused = null;
    const sock = new WebSocket(u);
    ws = sock;
    sock.binaryType = 'arraybuffer';
    // the console takes this socket before it asks the brain for the
    // stream: « open » is true once it says the brain's own is — and a
    // machine's silence is counted from there
    const opened = () => {
      say('open', 'on');
      sized();
      term.focus();
      hushing = setTimeout(quiet, quietAfter);
    };
    sock.onmessage = (ev) => {
      if (ws !== sock) return;
      if (typeof ev.data !== 'string') { spoke(); term.write(new Uint8Array(ev.data)); return; }
      // words from the console itself: the brain's stream is open, or why
      // the brain refused it
      try {
        const m = JSON.parse(ev.data);
        if (m && m.refused) refused = m.refused;
        else if (m && m.opened) opened();
      } catch { /* not for this page */ }
    };
    sock.onclose = (ev) => {
      if (ws !== sock) return;
      ws = null;
      setCtrl(false);
      spoke();
      const end = ending(ev, refused);
      say(end.word.toLowerCase(), end.word === 'CLOSED' ? '' : 'bad');
      node.dataset.why = end.why;
      if (wanted) clear(note, flyer(end.word, end.why, end.again));
    };
  }

  function flyer(word, why, again) {
    return h('div', {class: 'flyer', role: 'alert'}, h('div', {class: 'flyer-title'}, word), h('p', {class: 'flyer-detail'}, why),
      again ? h('div', {class: 'flyer-end'}, h('button', {type: 'button', class: 'btn solid', onclick: () => open()}, again)) : null);
  }

  // pass: its page is left for its other page. It stays as it is — its
  // screen, its socket — for the terminal() asked there, in this same turn;
  // not asked for by the turn's end, it is closed.
  function pass() {
    if (!term || !wanted) { close(); return; }
    passed = true;
    carried.set(key, {t, dress, timer: setTimeout(() => { carried.delete(key); close(); }, 0)});
  }

  function close() {
    wanted = false;
    passed = false;
    arriving = false;
    spoke();
    if (ws) { const s = ws; ws = null; s.onclose = null; s.close(1000); }
    if (watch) { watch.disconnect(); watch = null; }
    if (term) { term.dispose(); term = null; fit = null; }
    clear(screen);
    clear(note);
    clear(grid);
    say('closed');
  }

  // what is on its screen, as text: the screen may be drawn on a canvas,
  // where nothing reads it
  node.read = () => {
    if (!term) return '';
    const b = term.buffer.active;
    const lines = [];
    for (let i = 0; i < b.length; i++) lines.push(b.getLine(i).translateToString(true));
    return lines.join('\n').trimEnd();
  };
  const t = {node, open, close, pass, focus: () => term && term.focus(), isOpen: () => !!ws};
  return t;
}
