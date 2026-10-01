// main.js — the console: sign-in, the frame around the pages, the routes.
// Everything it shows comes from the brain's API through the console's own
// server (api.js); what may be asked for comes from the catalogue.

import {api, loadSession, session, signInWithToken, signOut, whenSignedOut, Problem} from './api.js';
import {h, clear} from './dom.js';
import {fieldsOf, plural} from './schema.js';
import * as views from './views.js';

const app = document.getElementById('app');
const state = {me: null, types: [], byName: new Map(), page: null};

// ---- A page's life --------------------------------------------------------------

// A page is looked at again while it is shown, and never after: each route
// gets a life of its own, ended when the next begins.
function life() {
  const l = {alive: true};
  const notice = () => document.getElementById('notice');
  async function run(f) {
    try {
      const out = await f();
      if (l.alive && notice()) clear(notice());
      return out;
    } catch (p) {
      if (!l.alive || (p instanceof Problem && p.status === 401)) return false;
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
            crumbs.map((c, i) => [i ? h('span', {class: 'sep', 'aria-hidden': 'true'}, '/') : null, c.href ? h('a', {href: c.href}, c.words) : h('span', {}, c.words)]),
            h('span', {class: 'cursor', 'aria-hidden': 'true'})),
          h('div', {id: 'zones', class: 'lamps'})),
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

function route() {
  if (!state.me) return;
  if (state.page) state.page.l.alive = false;
  const parts = (location.hash.replace(/^#\/?/, '').split('?')[0]).split('/').filter(Boolean).map(decodeURIComponent);
  const page = life();
  state.page = page;
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
    crumbs.push({words: parts[1]});
  } else if (parts[0] === 'operations') { node = views.operations(page.ctx); name = 'Operations'; crumbs.push({words: 'operations'}); }
  else if (parts[0] === 'tokens') { node = views.tokens(page.ctx); name = 'Tokens'; crumbs.push({words: 'tokens'}); }
  else node = h('div', {class: 'page'}, h('p', {class: 'muted'}, 'No such page.'), h('p', {}, h('a', {href: '#/'}, 'Home')));
  document.title = name + ' — Le Hangar';
  clear(app, frame(crumbs));
  clear(document.getElementById('view'), node);
  page.ctx.every(30000, lamps);
  window.scrollTo(0, 0);
}

// ---- Signing in -----------------------------------------------------------------

function showSignIn({error = '', detail = ''}) {
  if (state.page) state.page.l.alive = false;
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
