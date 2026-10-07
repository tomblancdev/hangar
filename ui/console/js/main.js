// main.js — the console: sign-in, the frame around the pages, the routes.
// Everything it shows comes from the brain's API through the console's own
// server (api.js); what may be asked for comes from the catalogue.

import {api, loadSession, reach, session, signInWithToken, signOut, whenAnswered, whenSignedOut, whenTurnedBack, Problem} from './api.js';
import {h, clear} from './dom.js';
import {fieldsOf, plural} from './schema.js';
import {holding} from './terminal.js';
import * as views from './views.js';

const app = document.getElementById('app');
const state = {me: null, types: [], byName: new Map(), page: null};

// ---- A page's life --------------------------------------------------------------

// A page is looked at again while it is shown, and never after: each route
// gets a life of its own, ended when the next begins.
function life() {
  const l = {alive: true, left: []};
  const notice = () => document.getElementById('notice');
  async function run(f) {
    try {
      const out = await f();
      if (l.alive && notice()) clear(notice());
      return out;
    } catch (p) {
      if (!l.alive || (p instanceof Problem && (p.status === 401 || p.kind === 'gateway'))) return false;
      if (notice()) clear(notice(), h('div', {class: 'banner', role: 'alert'}, p.message || String(p)));
      if (!(p instanceof Problem)) console.error(p);
      return false;
    }
  }
  return {
    l,
    ctx: {
      get me() { return state.me; },
      get types() { return state.types; },
      type: (name) => state.byName.get(name),
      go: (hash) => { location.hash = hash; },
      alive: () => l.alive,
      // onLeave: what a page holds open (a terminal) is let go of when it is left
      onLeave: (f) => { l.left.push(f); },
      now: run,
      // every: look now, then again — soon while something is moving (f
      // answers true), seldom otherwise, and not at all in a hidden tab
      every(ms, f, slow = ms) {
        (async () => {
          for (let first = true; l.alive; first = false) {
            const moving = first || document.visibilityState === 'visible' ? await run(f) : false;
            await new Promise((r) => setTimeout(r, moving ? ms : slow));
          }
        })();
      },
    },
  };
}

// ---- Behind a gateway -----------------------------------------------------------

// A front that locks the console behind a verdict of its own lets a person
// through for a time, then turns the page's calls back to its sign-in
// (api.js). A page loaded anew goes through it — nothing asked, where the
// gateway still knows the person — and comes back where it was: so the page
// reloads itself. Unless it holds what loading it anew would lose — a
// terminal, a form someone has used: then it says so, and waits for them.
// And only after a minute in which every answer was the console's: a page
// turned back as soon as it is loaded is not cured by loading it again, nor
// is one whose front turns some of its looks back, time after time — the
// longest wait between two looks is half that minute.
const steady = 60000;
let since = null; // since when every answer is the console's; null while one was not
let stopped = null; // the gateway is in the way: {steady, asked} — whether the page had worked before, and the calls asked when it last turned one back
let typed = false; // someone has used a form of the page that is shown
let leaving = false;

const holds = () => holding() || (typed && document.forms.length > 0);

for (const kind of ['input', 'keydown']) {
  app.addEventListener(kind, (e) => { if (e.composedPath().some((n) => n.tagName === 'FORM')) typed = true; }, true);
}

whenTurnedBack((asked) => {
  if (!stopped) stopped = {steady: since !== null && performance.now() - since >= steady};
  stopped.asked = asked;
  since = null;
  if (leaving) return;
  if (stopped.steady && !holds()) {
    leaving = true;
    // still here in a while, it was not loaded anew (a load someone stopped):
    // it says so, and tries no more by itself
    setTimeout(() => { leaving = false; if (stopped) { stopped.steady = false; timedOut(); } }, 10000);
    location.reload();
    return;
  }
  timedOut();
});

whenAnswered((n) => {
  // an answer to what was asked before the gateway last turned a call back
  // says nothing of the gateway now
  if (stopped && n <= stopped.asked) return;
  if (since === null) since = performance.now();
  if (!stopped) return;
  // let through again — another tab of the console went through: carry on
  stopped = null;
  const slot = document.getElementById('gate');
  if (slot) { clear(slot); delete slot.dataset.holds; }
});

// timedOut says it on the page, where the page has a place for it — once: a
// notice someone is reading is not drawn again under them.
function timedOut() {
  const slot = document.getElementById('gate');
  const held = String(holds());
  if (!slot || slot.dataset.holds === held) return;
  slot.dataset.holds = held;
  clear(slot, h('div', {class: 'flyer', role: 'alert'}, h('div', {class: 'flyer-title'}, 'TIMED OUT'),
    h('p', {class: 'flyer-detail'}, 'What stands in front of this console asks who you are again, as it does from time to time. Reload the page to go through.'),
    held === 'true' ? h('p', {class: 'flyer-detail'}, 'What is open here goes with it. To keep it, open the console in ',
      h('a', {href: './', target: '_blank', rel: 'noopener'}, 'another tab'), ' first, then come back: this page carries on.') : null,
    h('div', {class: 'flyer-end'}, h('button', {type: 'button', class: 'btn solid', onclick: () => location.reload()}, 'RELOAD'))));
}

// a tab someone comes back to asks at once: turned back, it is healed before
// they press anything; let through again, its notice leaves
document.addEventListener('visibilitychange', () => { if (document.visibilityState === 'visible' && state.me) reach(); });

// ---- The frame ------------------------------------------------------------------

function frame(crumbs) {
  const here = location.hash || '#/';
  const link = (href, words) => h('a', {href, 'aria-current': here === href || (href !== '#/' && here.startsWith(href + '/')) ? 'page' : null}, words);
  const out = async () => { await signOut(); state.me = null; showSignIn({}); };
  return [
    h('div', {class: 'hazard', 'aria-hidden': 'true'}),
    h('div', {class: 'shell'},
      // the side: what may be asked for, as the catalogue lists it
      h('aside', {class: 'side'}, h('div', {class: 'side-in'},
        h('a', {class: 'brand', href: '#/', 'aria-label': 'Home'}, h('img', {class: 'lockup', src: 'mark.svg', alt: 'Le Hangar', width: 640, height: 200})),
        h('nav', {class: 'nav', 'aria-label': 'Pages'},
          link('#/', 'Home'),
          h('div', {class: 'nav-label', 'aria-hidden': 'true'}, '// resources'),
          state.types.map((t) => link('#/t/' + t.name, plural(t.title))),
          h('div', {class: 'nav-label', 'aria-hidden': 'true'}, '// account'),
          link('#/operations', 'Operations'), link('#/tokens', 'Tokens')),
        h('div', {class: 'me'}, h('div', {}, h('b', {}, state.me.name || state.me.subject), h('div', {class: 'tiny muted'}, 'tier ' + state.me.tier + (state.me.operator ? ' · operator' : ''))),
          h('button', {type: 'button', class: 'btn key small', onclick: out}, 'Sign out')))),
      h('div', {class: 'main'},
        // the bar: where you are, as a prompt says it; the zones' lamps
        h('header', {class: 'topbar'},
          h('div', {class: 'crumbs', 'aria-label': 'Where you are'}, h('span', {class: 'prompt', 'aria-hidden': 'true'}, '>'),
            crumbs.map((c, i) => [i ? h('span', {class: 'sep', 'aria-hidden': 'true'}, '/') : null, c.href ? h('a', {href: c.href}, c.words) : h('span', {id: c.id || null}, c.words)]),
            h('span', {class: 'cursor', 'aria-hidden': 'true'})),
          h('div', {id: 'zones', class: 'lamps'})),
        h('div', {id: 'gate'}),
        h('div', {id: 'notice'}),
        h('main', {id: 'view'}),
        statusLine([state.me.name || state.me.subject, 'tier ' + state.me.tier, 'signed in ' + (session.via === 'token' ? 'with a token' : 'at the provider')], 'the command line does the same: hangar --help'))),
  ];
}

// statusLine: the last line of the screen, as a terminal keeps one.
function statusLine(parts, hint) {
  return h('footer', {class: 'status'}, h('span', {class: 'seg on'}, 'LE HANGAR'), parts.map((p) => h('span', {class: 'seg'}, p)),
    h('span', {class: 'grow'}), h('span', {class: 'seg dim'}, hint), h('span', {class: 'seg'}, session.version));
}

// lamps: each zone open to you, lit while it is awake.
async function lamps() {
  const into = document.getElementById('zones');
  if (!into) return false;
  const res = await api('GET', '/v1/zones');
  clear(into, res.zones.map((z) => h('span', {class: 'lamp-line', title: z.awake === false ? 'asleep — woken when something starts' : 'awake'},
    h('span', {class: 'lamp ' + (z.awake === false ? 'off' : 'on')}), 'zone ' + z.name)));
  return false;
}

// ---- The routes -----------------------------------------------------------------

// streamOf: a resource's type, said by its id's prefix, has a stream of that name.
function streamOf(id, name) {
  const t = state.types.find((x) => id.startsWith(x.id_prefix + '-'));
  return t && (t.streams || []).find((s) => s.name === name);
}

// leave ends the page that is shown: nothing of it looks again, and what it
// held open is let go of.
function leave() {
  if (!state.page) return;
  state.page.l.alive = false;
  for (const f of state.page.l.left.splice(0)) { try { f(); } catch (e) { console.error(e); } }
}

function route() {
  if (!state.me) return;
  leave();
  const parts = (location.hash.replace(/^#\/?/, '').split('?')[0]).split('/').filter(Boolean).map(decodeURIComponent);
  const page = life();
  state.page = page;
  typed = false;
  let node;
  let name = 'Home';
  const crumbs = [{href: '#/', words: 'hangar'}];
  const typeCrumb = (n) => { const t = state.byName.get(n); crumbs.push({href: '#/t/' + n, words: (t ? plural(t.title) : n).toLowerCase()}); };
  if (parts.length === 0 || parts[0] === 'signin') node = views.home(page.ctx);
  else if (parts[0] === 't' && parts.length === 2) { node = views.list(page.ctx, parts[1]); name = parts[1]; typeCrumb(parts[1]); }
  else if (parts[0] === 't' && parts.length === 3 && parts[2] === 'new') { node = views.create(page.ctx, parts[1]); name = 'New ' + parts[1]; typeCrumb(parts[1]); crumbs.push({words: 'new'}); }
  else if (parts[0] === 'r' && parts.length === 2) {
    node = views.resource(page.ctx, parts[1]); name = parts[1];
    // an id says its type by its prefix
    const t = state.types.find((x) => parts[1].startsWith(x.id_prefix + '-'));
    if (t) typeCrumb(t.name);
    // its page writes what it is called there, once read
    crumbs.push({words: parts[1], id: 'here'});
  } else if (parts[0] === 'r' && parts.length === 3 && streamOf(parts[1], parts[2])) {
    // one of its streams — its terminal — is a page of its own: the screen,
    // and nothing of the console around it
    document.title = parts[1] + ' — Le Hangar';
    clear(app, h('div', {class: 'hazard', 'aria-hidden': 'true'}), views.stream(page.ctx, parts[1], parts[2]),
      statusLine([state.me.name || state.me.subject, 'tier ' + state.me.tier], parts[2]));
    return;
  } else if (parts[0] === 'operations') { node = views.operations(page.ctx); name = 'Operations'; crumbs.push({words: 'operations'}); }
  else if (parts[0] === 'tokens') { node = views.tokens(page.ctx); name = 'Tokens'; crumbs.push({words: 'tokens'}); }
  else node = h('div', {class: 'page'}, h('p', {class: 'muted'}, 'No such page.'), h('p', {}, h('a', {href: '#/'}, 'Home')));
  document.title = name + ' — Le Hangar';
  clear(app, frame(crumbs));
  clear(document.getElementById('view'), node);
  if (stopped) timedOut();
  page.ctx.every(30000, lamps);
  window.scrollTo(0, 0);
}

// ---- Signing in -----------------------------------------------------------------

function showSignIn({error = '', detail = ''}) {
  leave();
  document.title = 'Sign in — Le Hangar';
  const words = {
    refused: 'The sign-in was refused.', expired: 'That sign-in took too long, or was not begun here.',
    'no-tier': 'You signed in, but your groups reach no tier here: the operator adds you to one.',
    provider: 'The identity provider cannot be reached.', brain: 'The brain cannot be reached.', ended: 'Your sign-in ended.',
    busy: 'Too many sign-ins are under way.',
  };
  const said = error ? h('div', {class: 'banner', role: 'alert'}, words[error] || 'The sign-in failed.', detail && detail !== words[error] ? h('div', {class: 'tiny'}, detail) : null) : null;
  const next = location.hash.startsWith('#/') && !location.hash.startsWith('#/signin') ? location.hash : '';

  let way;
  if (session.how === 'provider') {
    way = [
      h('p', {}, 'You sign in at ', h('b', {}, session.provider || 'your identity provider'), '. Your password, your second factor and your passkey stay on its page: this one never sees them.'),
      h('a', {class: 'btn solid big', href: 'signin?next=' + encodeURIComponent(next)}, 'SIGN IN'),
      h('p', {class: 'tiny'}, 'Not in a group yet? The operator adds you — nothing is asked for here.'),
    ];
  } else {
    const tok = h('input', {id: 'token', class: 'input paper', type: 'password', autocomplete: 'off', spellcheck: 'false', placeholder: 'hgr_…'});
    const err = h('div', {class: 'field-err', role: 'alert'});
    way = h('form', {class: 'token-form', onsubmit: async (e) => {
      e.preventDefault();
      clear(err);
      try {
        await signInWithToken(tok.value.trim());
        await boot();
      } catch (p) {
        clear(err, p.message || 'refused');
      }
    }},
      h('p', {}, 'This hangar has no identity provider: you sign in with an API token, made on the brain\'s host by ', h('code', {}, 'hangar token create'), '.'),
      h('label', {class: 'lbl', for: 'token'}, 'API token'), tok, err,
      h('button', {type: 'submit', class: 'btn solid big'}, 'SIGN IN'));
  }
  clear(app, h('div', {class: 'hazard', 'aria-hidden': 'true'}), h('main', {class: 'signin'},
    h('div', {class: 'signin-mark'}, h('img', {class: 'lockup', src: 'mark.svg', alt: 'Le Hangar', width: 640, height: 200}),
      h('p', {class: 'muted'}, 'What your group is allowed, asked for and made: machines and whatever else this hangar offers.')),
    h('div', {class: 'signin-way'}, said, h('div', {class: 'flyer tilt'}, h('div', {class: 'flyer-title'}, 'COME IN'), way))),
    statusLine(['not signed in'], 'the command line signs in the same way: hangar login'));
  const first = app.querySelector('input, a.btn');
  if (first) first.focus();
}

// ---- Boot -----------------------------------------------------------------------

async function boot() {
  try {
    await loadSession();
    if (!session.signedIn) {
      const q = new URLSearchParams((location.hash.split('?')[1]) || '');
      showSignIn({error: location.hash.startsWith('#/signin') ? q.get('error') || '' : '', detail: q.get('detail') || ''});
      return;
    }
    const [me, cat] = await Promise.all([api('GET', '/v1/whoami'), api('GET', '/v1/types')]);
    state.me = me;
    state.types = cat.types.map((t) => ({
      ...t, title: t.title || t.name, fields: fieldsOf(t.schema),
      actions: (t.actions || []).map((a) => ({...a, fields: fieldsOf(a.params_schema)})),
    }));
    state.byName = new Map(state.types.map((t) => [t.name, t]));
    if (location.hash.startsWith('#/signin')) history.replaceState(null, '', location.pathname + '#/');
    route();
  } catch (p) {
    if (p instanceof Problem && p.status === 401) return; // whenSignedOut shows the way in
    clear(app, h('div', {class: 'hazard', 'aria-hidden': 'true'}), h('main', {class: 'signin'}, h('div', {class: 'banner', role: 'alert'}, p.message || String(p)),
      h('p', {}, h('button', {type: 'button', class: 'btn', onclick: () => location.reload()}, 'Try again'))));
  }
}

whenSignedOut(() => {
  state.me = null;
  loadSession().catch(() => {}).then(() => showSignIn({error: 'ended'}));
});
window.addEventListener('hashchange', () => { if (state.me) route(); });
boot();
