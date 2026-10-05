// terminal.js — a resource's stream, drawn as a terminal: a machine's own
// screen and keyboard. The screen is xterm.js (the second file here the app
// did not write: ../vendor/xterm.js, tools/terminal); the socket is the
// console's own, api/v1/resources/<id>/streams/<stream>, which passes it on
// to the brain with the person's token. What is typed goes as bytes, a
// window that changed as {"size":[cols,rows]}; what the machine says comes
// as bytes, and why it ended in the socket's last words.
//
// Nothing here knows what a machine is: any type's stream opens the same.

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

// the keys a phone's keyboard has none of
const keys = [['Esc', '\x1b'], ['Tab', '\t'], ['Ctrl', null], ['↑', 'A'], ['↓', 'B'], ['←', 'D'], ['→', 'C']];

// terminal makes one: {node, open(), close(), focus()}. Nothing is asked of
// the brain until open(); close() lets go of it (and of the screen).
// controls: what the caller puts in its bar (a switch, a way out).
export function terminal({id, stream, controls = []}) {
  const state = h('span', {class: 'term-state', 'aria-live': 'polite'}, 'not open');
  const grid = h('span', {class: 'term-grid'});
  const screen = h('div', {class: 'term-screen'});
  const note = h('div', {class: 'term-note'});
  const pad = h('div', {class: 'term-keys', role: 'group', 'aria-label': 'Keys a phone has none of'});
  const node = h('section', {class: 'term', 'data-state': 'closed', 'aria-label': `The ${stream} of ${id}`},
    h('div', {class: 'term-bar'}, h('span', {class: 'lbl'}, stream), state, grid, h('span', {class: 'grow'}), controls), screen, pad, note);

  let term = null;
  let fit = null;
  let ws = null;
  let watch = null;
  let ctrl = false;
  let wanted = false; // open() was called, and close() not since
  let took = ''; // the grid the machine was told at its opening
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

  function sized() {
    if (!term) return;
    const now = `${term.cols} × ${term.rows}`;
    clear(grid, now, took && took !== now ? h('span', {class: 'term-hint', title: 'A machine takes its terminal\'s size when you sign in to it.'}, ' · resized: exit signs you in at this size') : null);
  }

  function refit() {
    if (!term || !fit || !screen.clientWidth) return;
    try { fit.fit(); } catch { /* not laid out yet */ }
    sized();
  }

  async function open() {
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
      term.onData(send);
      term.onBinary((d) => {
        if (!ws || ws.readyState !== WebSocket.OPEN) return;
        const bytes = Uint8Array.from(d, (c) => c.charCodeAt(0));
        for (let at = 0; at < bytes.length; at += piece) ws.send(bytes.subarray(at, at + piece));
      });
      term.onResize(({cols, rows}) => { if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({size: [cols, rows]})); });
      watch = new ResizeObserver(() => refit());
      watch.observe(screen);
    }
    refit();
    connect();
  }

  function connect() {
    if (ws) { ws.onclose = null; ws.close(1000); }
    const u = new URL(`api/v1/resources/${encodeURIComponent(id)}/streams/${encodeURIComponent(stream)}`, document.baseURI);
    u.protocol = u.protocol === 'https:' ? 'wss:' : 'ws:';
    u.search = `?cols=${term.cols}&rows=${term.rows}`;
    took = `${term.cols} × ${term.rows}`;
    let refused = null;
    const sock = new WebSocket(u);
    ws = sock;
    sock.binaryType = 'arraybuffer';
    sock.onopen = () => { if (ws === sock) { say('open', 'on'); sized(); term.focus(); } };
    sock.onmessage = (ev) => {
      if (ws !== sock) return;
      if (typeof ev.data !== 'string') { term.write(new Uint8Array(ev.data)); return; }
      // words from the console itself: why the brain refused the opening
      try { const m = JSON.parse(ev.data); if (m && m.refused) refused = m.refused; } catch { /* not for this page */ }
    };
    sock.onclose = (ev) => {
      if (ws !== sock) return;
      ws = null;
      setCtrl(false);
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

  function close() {
    wanted = false;
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
  return {node, open, close, focus: () => term && term.focus(), isOpen: () => !!ws};
}
