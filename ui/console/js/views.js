// views.js — the pages. Nothing here knows a machine from a volume: every
// page is drawn from the catalogue the brain serves (GET /v1/types) and from
// the resources themselves. A new plugin's types appear with no change here.
//
// A view is view(ctx, …) → node. It may ask ctx.every(ms, f) to be looked at
// again while it is the page shown; what a person is typing is never redrawn.

import {api, Problem, clientToken, settled, session} from './api.js';
import {h, clear, bar, ago, day, took, fmtNumber} from './dom.js';
import {summary, label, plural, show, fieldsOf} from './schema.js';
import {nested, pairs} from './json.js';
import {listing} from './code.js';
import {buildForm, refusal} from './form.js';
import {terminal} from './terminal.js';

const idShape = /^[a-z][a-z0-9]{0,7}-[0-9a-f]{17}$/;
const moving = ['creating', 'updating', 'deleting'];

// What a resource is called is the brain's own, on every type: the one
// control here no catalogue declares.
const renameWord = 'rename';
const calledFields = fieldsOf({type: 'object', properties: {
  name: {type: 'string', maxLength: 63, description: 'a-z, 0-9 and -. One thing among yours of this type; it goes wherever its id goes. Empty: unnamed, shown by its id.'},
  description: {type: 'string', maxLength: 256, description: 'One line about it.'},
}});

// ---- What a resource reads as -------------------------------------------------

// nameOf: what its owner calls it; its id when unnamed.
export function nameOf(r) {
  return typeof r.name === 'string' && r.name ? r.name : r.id;
}

// ownerOf: its owner by the name they sign in under; the subject when the
// brain never saw them.
function ownerOf(r) {
  return r.owner_name || r.owner;
}

// whatOf: what it is, in its type's own sentence — the brain's; an older
// brain's is drawn here from the spec's fields.
function whatOf(ctx, r) {
  if (typeof r.summary === 'string') return r.summary;
  const t = ctx.type(r.type);
  return t ? summary(t.fields, r.spec) : '';
}

// stampOf: the one word a resource wears, and its colour — the brain's say
// (status, light: its state while something moves, its plugin's word, its
// type's own: running or stopped, attached or parked). An older brain says
// only the state: then the core's state first; then whether it may be named;
// then — for what reports a `running` — whether it runs.
export function stampOf(r) {
  if (r.status && r.light) return [r.status, r.light];
  if (moving.includes(r.state)) return [r.state, 'busy'];
  if (r.state === 'failed' || r.state === 'lost') return [r.state, 'bad'];
  if (r.state === 'deleted') return ['deleted', 'off'];
  if (r.pending) return [r.unusable || 'pending', 'busy'];
  if (r.unusable) return [r.unusable, r.unusable === 'failed' ? 'bad' : 'off'];
  if (r.observed && typeof r.observed.running === 'boolean') return r.observed.running ? ['running', 'on'] : ['stopped', 'off'];
  return ['ready', 'on'];
}

function stamp(r) {
  const [word, cls] = stampOf(r);
  return h('span', {class: 'stamp ' + cls}, word);
}

// lamp: a state as a console shows it in a list — a light, and its word.
function lamp(word, cls) {
  return h('span', {class: 'state ' + cls}, h('span', {class: 'lamp ' + cls, 'aria-hidden': 'true'}), word);
}

// rowsHead names a list's columns.
function rowsHead(...names) {
  return h('div', {class: 'row head', 'aria-hidden': 'true'}, names.map((n) => h('div', {}, n)));
}

// row: one resource in a list — what it is called, the word it wears, its
// sentence and its owner's line about it, where and since when. hangs: it is
// drawn under what it hangs on ('mid', or 'last' of them).
function row(ctx, r, {owner = false, hangs = ''} = {}) {
  const t = ctx.type(r.type);
  const what = whatOf(ctx, r);
  const notes = [];
  if (r.hold) notes.push('its room is held for ' + r.hold);
  if (r.drift) notes.push(r.drift);
  if (r.owner !== ctx.me.subject) notes.push(owner ? 'owner ' + ownerOf(r) : 'shared with you by ' + ownerOf(r));
  return h('a', {class: 'row' + (hangs ? ' hangs ' + hangs : ''), href: '#/r/' + r.id},
    h('div', {class: 'row-name'}, h('div', {class: 'name'}, nameOf(r)), h('div', {class: 'tiny muted'}, (t ? t.title : r.type) + (r.name ? ' · ' + r.id : ''))),
    h('div', {class: 'row-stamp'}, lamp(...stampOf(r))),
    h('div', {class: 'row-what'}, what || h('span', {class: 'muted'}, '—'),
      r.description ? h('div', {class: 'tiny muted said-of'}, r.description) : null,
      notes.length ? h('div', {class: 'tiny muted'}, notes.join(' · ')) : null),
    h('div', {class: 'row-when tiny muted'}, r.zone + ' · ' + ago(r.created_at)));
}

// refsOf: the ids a resource's spec names, by its type's references.
function refsOf(ctx, r) {
  const t = ctx.type(r.type);
  const out = [];
  for (const f of t ? t.fields.filter((f) => f.ref) : []) {
    const v = (r.spec || {})[f.name];
    for (const x of Array.isArray(v) ? v : v ? [v] : []) if (typeof x === 'string') out.push(x);
  }
  return out;
}

// held: everything a person holds, as the command line's `hangar list`
// draws it — each set of a spec file in its zone, each holder (a type
// others attach to: a machine) with what hangs on it — what names it, then
// what it names —, then what hangs on nothing.
function held(ctx, all) {
  const holder = new Set();
  for (const t of ctx.types) for (const f of t.fields) if (f.attached && f.ref) holder.add(f.ref);
  const mine = (r) => r.owner === ctx.me.subject;
  const byId = new Map(all.map((r) => [r.id, r]));
  const hangs = new Map();
  const hung = new Set();
  for (const r of all) {
    if (!mine(r) || !holder.has(r.type)) continue;
    const kids = all.filter((o) => o.id !== r.id && refsOf(ctx, o).includes(r.id));
    for (const id of refsOf(ctx, r)) { const o = byId.get(id); if (o && !kids.includes(o)) kids.push(o); }
    hangs.set(r.id, kids);
    for (const o of kids) hung.add(o.id);
  }
  const groups = [];
  const groupOf = (r) => {
    const set = (r.tags || {})['apply:set'] || '';
    let g = groups.find((x) => x.set === set && x.zone === r.zone);
    if (!g) { g = {set, zone: r.zone, rows: []}; groups.push(g); }
    return g;
  };
  for (const r of all) {
    if (!mine(r) || !holder.has(r.type)) continue;
    const g = groupOf(r);
    g.rows.push(row(ctx, r));
    const kids = hangs.get(r.id);
    kids.forEach((o, i) => g.rows.push(row(ctx, o, {hangs: i === kids.length - 1 ? 'last' : 'mid'})));
  }
  for (const r of all) if (mine(r) && !holder.has(r.type) && !hung.has(r.id)) groupOf(r).rows.push(row(ctx, r));
  groups.sort((a, b) => (a.set === '') - (b.set === '') || a.set.localeCompare(b.set) || a.zone.localeCompare(b.zone));
  return groups.map((g) => [
    groups.length > 1 || g.set ? h('div', {class: 'row set-head'}, (g.set ? 'set ' + g.set + ' · ' : '') + 'zone ' + g.zone) : null,
    g.rows]);
}

function opLine(op, ctx) {
  const what = op.kind === 'action' ? label(op.action) : op.kind;
  // what an action was asked with, as the command line writes it (cores=4 ·
  // memory_gb=8) — listed under the line when a param does not read on one;
  // a create's and a delete's are the core's own notes
  const asked = op.kind === 'action' && op.params && Object.keys(op.params).length ? op.params : null;
  const flat = asked ? pairs(asked) : null;
  const leads = (x) => (typeof x === 'string' && idShape.test(x) ? h('a', {href: '#/r/' + x}, x) : null);
  const res = op.state === 'running' ? h('span', {class: 'busy'}, 'running…')
    : op.state === 'failed' ? h('span', {class: 'bad'}, 'failed: ' + (op.error || ''))
    : h('span', {class: 'muted'}, 'done in ' + took(op.created_at, op.finished_at || op.updated_at));
  return h('div', {class: 'op'},
    h('div', {class: 'muted'}, ago(op.created_at)),
    h('div', {}, h('b', {}, what), ' ', h('a', {href: '#/r/' + op.resource_id, title: op.resource_id}, op.resource_name || op.resource_id), ctx.me.operator && op.owner !== ctx.me.subject ? h('span', {class: 'muted'}, ' · ' + (op.owner_name || op.owner)) : null,
      flat ? h('span', {class: 'muted'}, ' ', flat.map(([k, v], i) => [i ? ' · ' : '', k + '=', leads(v) || (Array.isArray(v) ? v.join(',') : String(v))])) : null),
    h('div', {}, res),
    asked && !flat ? listing(asked, {link: leads}) : null);
}

function problemNode(p) {
  if (p instanceof Problem) return refusal(p);
  return h('div', {class: 'flyer', role: 'alert'}, h('div', {class: 'flyer-title'}, 'BROKEN'), h('p', {}, String(p && p.message || p)));
}

function title(eyebrow, words, accent, sprayed) {
  return h('div', {class: 'title'},
    eyebrow ? h('div', {class: 'eyebrow'}, eyebrow) : null,
    h('h1', {tabindex: '-1'}, words, accent ? [' ', h('span', {class: 'accent'}, accent)] : null,
      sprayed ? h('span', {class: 'spray', 'aria-hidden': 'true'}, sprayed) : null));
}

// ---- Home: what you hold ------------------------------------------------------

export function home(ctx) {
  const limits = h('div', {class: 'gauges'});
  const choices = h('div', {class: 'chips'});
  const zones = h('div', {class: 'stack'});
  const yours = h('div', {class: 'rows'});
  const lately = h('div', {class: 'ops'});
  const head = h('div', {});

  async function look() {
    const [lim, res, ops, zs] = await Promise.all([
      api('GET', '/v1/limits'), api('GET', '/v1/resources?limit=200' + mine(ctx)), api('GET', '/v1/operations?limit=6'), api('GET', '/v1/zones'),
    ]);
    const mineOnly = res.resources.filter((r) => r.owner === ctx.me.subject);
    clear(head, title(`tier ${lim.tier}` + (ctx.me.operator ? ' · operator' : ''), 'WHAT YOU', 'HOLD', mineOnly.length ? String(mineOnly.length) + (mineOnly.length === 1 ? ' thing' : ' things') : ''));

    clear(limits, lim.limits.filter((l) => l.kind !== 'choice' && (l.limit !== 0 || l.used > 0)).map(gauge));
    if (!limits.childNodes.length) limits.append(h('p', {class: 'muted'}, 'Your tier names no limit: nothing may be asked for.'));
    const allowed = lim.limits.filter((l) => l.kind === 'choice' && Array.isArray(l.limit) && l.limit.length);
    clear(choices, allowed.length ? h('span', {class: 'muted'}, 'you may ask for') : null,
      allowed.map((l) => h('span', {class: 'chip', title: l.description || l.name}, l.name.replace(/^.*\./, '') + ': ' + l.limit.join(', '))));

    clear(zones, zs.zones.map(zone));
    clear(yours, mineOnly.length ? held(ctx, res.resources) : h('p', {class: 'muted empty'}, 'Nothing yet. Ask for something above.'));
    clear(lately, ops.operations.length ? ops.operations.map((o) => opLine(o, ctx)) : h('p', {class: 'muted empty'}, 'No operation yet.'));
  }

  const asks = ctx.types.filter((t) => t.zones.length).map((t) => h('a', {class: 'btn primary', href: `#/t/${t.name}/new`}, '+ ' + t.title));
  const node = h('div', {class: 'page'},
    h('div', {class: 'page-head'}, head, h('div', {class: 'asks'}, asks)),
    section('YOUR LIMITS', 'what your tier allows — a refusal always comes with these numbers', limits, choices),
    h('div', {class: 'two'},
      h('div', {}, section('YOURS', '', yours)),
      h('div', {}, section('THE ZONES', '', zones), section('LATELY', '', lately))));
  ctx.every(8000, look);
  return node;
}

// mine: an operator's listing is everyone's — home shows their own.
function mine(ctx) {
  return ctx.me.operator ? '&owner=' + encodeURIComponent(ctx.me.subject) : '';
}

function section(name, hint, ...body) {
  return h('section', {class: 'section'}, h('div', {class: 'section-head'}, h('h2', {}, name), hint ? h('div', {class: 'tiny muted'}, hint) : null), body);
}

function gauge(l) {
  const unlimited = l.limit === 'unlimited';
  const max = unlimited ? 0 : Number(l.limit);
  const near = !unlimited && max > 0 && l.used / max >= 0.85;
  let note = '';
  if (l.kind === 'meter') note = (l.period ? 'this month' : '') + (l.resets ? ' — back on ' + day(l.resets) : '');
  return h('div', {class: 'gauge screen' + (near ? ' near' : '')},
    h('div', {class: 'gauge-head'}, h('div', {class: 'lbl'}, label(l.name.replace('.', ' ')))),
    h('div', {class: 'gauge-num'}, h('span', {class: 'num'}, fmtNumber(l.used)), h('span', {class: 'muted'}, unlimited ? ' — no limit' : ` of ${fmtNumber(max)} ${l.unit || ''}`)),
    unlimited ? null : bar(l.used, max),
    h('div', {class: 'tiny muted gauge-note'}, note ? h('div', {class: near ? 'bad' : ''}, note) : null, h('div', {class: 'clamp', title: l.description || null}, l.description)));
}

function zone(z) {
  const gb = (mb) => fmtNumber(Math.round(mb / 102.4) / 10);
  const broken = Object.entries(z.plugins || {}).filter(([, st]) => st.error).map(([p, st]) => h('div', {class: 'tiny bad'}, `${p}: ${st.error}`));
  const room = z.room;
  return h('div', {class: 'screen zone'},
    h('div', {class: 'zone-head'}, h('div', {class: 'name'}, z.name),
      z.awake === undefined ? null : lamp(z.awake ? 'awake' : 'asleep — woken when something starts', z.awake ? 'on' : 'off')),
    room ? [
      h('div', {class: 'meter-line'}, h('span', {class: 'muted'}, 'guaranteed — yours whatever happens'), h('span', {}, `${gb(room.booked_mb)} of ${gb(room.guaranteed_mb)} GB booked`)),
      bar(room.booked_mb, room.guaranteed_mb),
      h('div', {class: 'meter-line'}, h('span', {class: 'muted'}, 'spot — lent, taken back when needed'), h('span', {}, `${gb(room.spot_used_mb)} of ${gb(room.spot_mb)} GB lent`)),
      bar(room.spot_used_mb, room.spot_mb, 'striped'),
      room.reservations.length ? h('div', {class: 'tiny muted keeps'}, room.reservations.map((rv) =>
        h('div', {}, 'kept for ', h('b', {}, rv.name), `: ${gb(rv.memory_mb)} GB ${rv.condition || 'always'}` + (rv.condition ? (rv.in_force ? ' — in force now' : ' — not in force: it is lent') : '')))) : null,
    ] : h('div', {class: 'tiny muted'}, 'This zone counts no room: what your tier allows fits.'),
    broken);
}

// ---- A type's list --------------------------------------------------------------

export function list(ctx, typeName) {
  const t = ctx.type(typeName);
  if (!t) return missing(`No plugin here makes the type ${typeName}.`);
  const rows = h('div', {class: 'rows'});
  const head = h('div', {});
  const filter = {state: '', zone: ''};
  const states = ['', 'ready', 'creating', 'failed', 'deleted'];

  async function look() {
    let q = `/v1/resources?type=${encodeURIComponent(t.name)}&limit=500`;
    if (filter.state) q += '&state=' + filter.state;
    if (filter.zone) q += '&zone=' + encodeURIComponent(filter.zone);
    const res = await api('GET', q);
    clear(head, title(`a type the ${t.plugin} plugin declares`, plural(t.title).toUpperCase(), '', res.resources.length ? '×' + res.resources.length : ''));
    clear(rows, res.resources.length ? [rowsHead('name', 'state', 'what it is', 'where · when'), res.resources.map((r) => row(ctx, r, {owner: ctx.me.operator}))] :
      h('p', {class: 'muted empty'}, filter.state || filter.zone ? 'None like that.' : `No ${t.title.toLowerCase()} yet.`));
    return res.resources.some((r) => moving.includes(r.state) || r.pending);
  }

  const picks = states.map((s) => h('button', {type: 'button', class: 'btn small' + (s === '' ? ' chosen' : ''), onclick: (e) => {
    filter.state = s;
    for (const b of picks) b.classList.toggle('chosen', b === e.currentTarget);
    ctx.now(look);
  }}, s || 'all'));
  const zonePick = t.zones.length > 1 ? h('select', {class: 'input small', 'aria-label': 'zone', onchange: (e) => { filter.zone = e.target.value; ctx.now(look); }},
    h('option', {value: ''}, 'every zone'), t.zones.map((z) => h('option', {value: z}, z))) : null;

  const node = h('div', {class: 'page'},
    h('div', {class: 'page-head'}, h('div', {}, head, t.description ? h('p', {class: 'lede muted'}, t.description) : null),
      t.zones.length ? h('div', {class: 'asks'}, h('a', {class: 'btn primary', href: `#/t/${t.name}/new`}, '+ New ' + t.title.toLowerCase())) :
        h('div', {class: 'tiny muted'}, 'offered in no zone open to you')),
    h('div', {class: 'filters'}, h('span', {class: 'lbl'}, 'show'), picks, h('span', {class: 'grow'}), zonePick),
    rows);
  // looked at often while something is being made, seldom otherwise
  ctx.every(4000, look, 20000);
  return node;
}

function missing(words) {
  return h('div', {class: 'page'}, title('', 'NOTHING', 'HERE'), h('p', {class: 'muted'}, words), h('p', {}, h('a', {href: '#/'}, 'Home')));
}

// ---- Asking for one -------------------------------------------------------------

// refsFor lists what each reference of some fields may name in a zone: the
// person's own and what is shared with them, and — for what a schedule
// makes — « the newest » (@<schedule>, resolved by the brain).
async function refsFor(ctx, fields, zoneName) {
  const out = {};
  const wanted = new Map();
  for (const f of fields) if (f.ref && !wanted.has(f.ref)) wanted.set(f.ref, f);
  await Promise.all([...wanted.keys()].map(async (type) => {
    if (!ctx.type(type)) { out[type] = []; return; }
    const res = await api('GET', `/v1/resources?type=${encodeURIComponent(type)}&zone=${encodeURIComponent(zoneName)}&limit=500`);
    const opts = [];
    const schedules = new Set();
    for (const r of res.resources) {
      if (r.state !== 'ready') continue;
      const s = r.tags && r.tags['hangar:schedule'];
      if (s && !r.unusable) schedules.add(s);
      opts.push({value: r.id, own: r.owner === ctx.me.subject, disabled: !!r.unusable,
        label: nameOf(r) + (nameOf(r) === r.id ? '' : ' · ' + r.id) + (r.owner === ctx.me.subject ? '' : ' (shared by ' + ownerOf(r) + ')') + (r.unusable ? ' — ' + r.unusable : '')});
    }
    out[type] = [...[...schedules].sort().map((s) => ({value: '@' + s, own: false, label: `@${s} — the newest ${s}`})), ...opts];
  }));
  // what a thing is plugged into is one's own: nobody plugs into someone else's
  for (const f of fields) if (f.ref && f.attached) out[f.ref] = (out[f.ref] || []).filter((o) => o.own);
  return out;
}

export function create(ctx, typeName) {
  const t = ctx.type(typeName);
  if (!t) return missing(`No plugin here makes the type ${typeName}.`);
  if (!t.zones.length) return missing(`${t.title} is offered in no zone open to you.`);
  const holder = h('div', {class: 'panel'}, h('p', {class: 'muted'}, 'Loading…'));
  const aside = h('div', {class: 'aside'});
  let zoneName = t.zones[0];

  async function draw() {
    const refs = await refsFor(ctx, t.fields, zoneName);
    const zonePick = h('select', {id: 'zone', class: 'input', onchange: (e) => { zoneName = e.target.value; ctx.now(draw); }}, t.zones.map((z) => h('option', {value: z}, z)));
    zonePick.value = zoneName;
    const tags = h('textarea', {id: 'tags', class: 'input', rows: 2, spellcheck: 'false', placeholder: 'key=value, one per line'});
    // what it is called: the brain's own, on every type — kept while the zone is picked again
    const was = {name: (document.getElementById('called') || {}).value || '', description: (document.getElementById('described') || {}).value || ''};
    const called = h('input', {id: 'called', class: 'input', type: 'text', maxlength: 63, autocomplete: 'off', spellcheck: 'false', autocapitalize: 'none', placeholder: 'dev', value: was.name});
    const described = h('input', {id: 'described', class: 'input', type: 'text', maxlength: 256, autocomplete: 'off', value: was.description});
    // what the brain refuses of its name or its description is said beside it
    const errs = {name: h('div', {class: 'field-err', role: 'alert'}), description: h('div', {class: 'field-err', role: 'alert'})};
    const form = buildForm({
      fields: t.fields, refs, groups: ctx.me.groups, submit: 'ASK FOR IT',
      extra: [
        h('div', {class: 'field'}, h('label', {class: 'lbl', for: 'called'}, 'name'), called,
          h('div', {class: 'help'}, `What you call it: a-z, 0-9 and -. One thing among your ${plural(t.title).toLowerCase()}; it goes wherever its id goes. Unnamed, it is shown by its id.`), errs.name),
        h('div', {class: 'field'}, h('label', {class: 'lbl', for: 'zone'}, 'zone'), zonePick, h('div', {class: 'help'}, 'Where it is made.')),
        h('div', {class: 'field wide'}, h('label', {class: 'lbl', for: 'described'}, 'description'), described, h('div', {class: 'help'}, 'One line about it, for whoever reads the list.'), errs.description)],
      onCancel: () => ctx.go('#/t/' + t.name),
      onSubmit: async (spec) => {
        clear(aside);
        clear(errs.name);
        clear(errs.description);
        const body = {type: t.name, zone: zoneName, spec, client_token: clientToken()};
        if (called.value.trim()) body.name = called.value.trim();
        if (described.value.trim()) body.description = described.value.trim();
        const tagMap = readTags(tags.value);
        if (tagMap === null) { clear(aside, problemNode(new Error('a tag reads key=value, one per line'))); return; }
        if (Object.keys(tagMap).length) body.tags = tagMap;
        form.busy(true);
        try {
          const acc = await api('POST', '/v1/resources', body);
          ctx.go('#/r/' + acc.resource.id);
        } catch (p) {
          form.busy(false);
          if (!(p instanceof Problem)) throw p;
          // its own two words are no pointer into the spec: "name", "description"
          const own = (p.violations || []).filter((v) => errs[v.field]);
          for (const v of own) clear(errs[v.field], v.reason);
          clear(aside, refusal(p, form.setViolations((p.violations || []).filter((v) => !errs[v.field]))));
          aside.scrollIntoView({block: 'nearest'});
        }
      },
    });
    form.node.insertBefore(h('div', {class: 'field wide'}, h('label', {class: 'lbl', for: 'tags'}, 'tags'), tags, h('div', {class: 'help'}, 'Yours, to find it by.')), form.node.querySelector('.form-end'));
    clear(holder, form.node);
  }

  const node = h('div', {class: 'page'},
    h('div', {class: 'page-head'}, h('div', {}, title(`a type the ${t.plugin} plugin declares`, /^[aeiou]/i.test(t.title) ? 'ASK FOR AN' : 'ASK FOR A', t.title.toUpperCase()), t.description ? h('p', {class: 'lede muted'}, t.description) : null)),
    h('div', {class: 'two form-and-notice'}, holder, aside));
  ctx.now(draw);
  return node;
}

function readTags(text) {
  const out = {};
  for (const line of text.split('\n').map((s) => s.trim()).filter(Boolean)) {
    const i = line.indexOf('=');
    if (i < 1) return null;
    out[line.slice(0, i).trim()] = line.slice(i + 1).trim();
  }
  return out;
}

// ---- One resource ---------------------------------------------------------------

export function resource(ctx, id) {
  if (!idShape.test(id)) return missing('That is not an id.');
  const head = h('div', {});
  const actions = h('div', {class: 'actions'});
  const panel = h('div', {});
  // its terminal has a place of its own: an action asked meanwhile — a
  // reboot, watched from it — does not take it away
  const termBox = h('div', {class: 'term-panel'});
  const said = h('div', {class: 'said', 'aria-live': 'polite'});
  const specBox = h('div', {class: 'kv panel'});
  const seen = h('div', {class: 'kv screen'});
  const names = h('div', {class: 'rows'});
  const namedBy = h('div', {class: 'rows'});
  const history = h('div', {class: 'ops'});
  const danger = h('div', {});
  const node = h('div', {class: 'page'}, head, said, actions, panel, termBox,
    h('div', {class: 'two'}, h('div', {}, section('AS ASKED', '', specBox)), h('div', {}, section('AS SEEN', '', seen))),
    section('WHAT IT NAMES', '', names), section('WHAT NAMES IT', '', namedBy), section('ITS HISTORY', '', history), danger);
  let open = '';
  let gone = false;
  // the stream shown in the page — its terminal — while one is
  let shown = null;
  function hide() {
    if (!shown) return;
    shown.t.close();
    shown = null;
    clear(termBox);
    for (const b of actions.querySelectorAll('button[data-stream]')) { b.classList.remove('chosen'); b.setAttribute('aria-expanded', 'false'); }
  }
  // unfold opens it under the keys, with a switch to the whole window (its
  // own address: the same screen, alone) and a way to put it away
  function unfold(name) {
    if (shown && shown.name === name) { hide(); return; }
    hide();
    const t = terminal({id, stream: name, controls: [
      h('a', {class: 'btn small ghost', href: `#/r/${id}/${name}`, title: 'The whole window: the same screen, alone'}, 'Full screen'),
      h('button', {type: 'button', class: 'btn small ghost', onclick: hide}, 'Close')]});
    shown = {name, t};
    clear(termBox, t.node);
    for (const b of actions.querySelectorAll('button[data-stream]')) { const on = b.dataset.stream === name; b.classList.toggle('chosen', on); b.setAttribute('aria-expanded', String(on)); }
    t.open();
  }
  ctx.onLeave(hide);
  // come back from the whole window: it is shown again, where it was
  let asked = new URLSearchParams(location.hash.split('?')[1] || '').get('open') || '';
  // what a person unfolded stays unfolded while the page is looked at again
  const unfolded = new Set();

  async function look() {
    let r;
    try {
      r = await api('GET', '/v1/resources/' + id);
    } catch (p) {
      if (p instanceof Problem && p.status === 404) { clear(node, missing(`No resource ${id} — or not one you may see.`)); gone = true; return false; }
      throw p;
    }
    const t = ctx.type(r.type);
    const own = r.owner === ctx.me.subject || ctx.me.operator;
    // where you are, by what it is called
    const here = document.getElementById('here');
    if (here) here.textContent = nameOf(r);
    document.title = nameOf(r) + ' — Le Hangar';
    const set = (r.tags || {})['apply:set'];
    const place = r.observed && typeof r.observed.engine_ref === 'string' ? r.observed.engine_ref : '';
    clear(head, h('div', {class: 'page-head'}, h('div', {},
      title((t ? t.title : r.type) + ' · ' + r.id, nameOf(r).toUpperCase()),
      h('div', {class: 'under'}, stamp(r), h('span', {class: 'what-it-is'}, whatOf(ctx, r))),
      r.description ? h('p', {class: 'said-of'}, r.description) : null,
      h('div', {class: 'under'}, h('span', {class: 'muted'},
        `zone ${r.zone}` + (place ? ` · ${place}` : '') + ` · ${r.owner === ctx.me.subject ? 'yours' : 'owner ' + ownerOf(r)}` + (set ? ` · set ${set}` : '') + ` · made ${ago(r.created_at)}` +
        (r.hold ? ` · its room is held for ${r.hold}` : '') + (r.drift ? ` · ${r.drift}` : ''))))));

    // its actions: the ones its type offers in its zone
    const acts = t && own && r.state !== 'deleted' ? t.actions.filter((a) => a.zones.includes(r.zone)) : [];
    // its streams — its terminal: its owner's alone, an operator included
    const mine = r.owner === ctx.me.subject && r.state !== 'deleted';
    const streams = t && mine ? (t.streams || []).filter((s) => s.zones.includes(r.zone)) : [];
    clear(actions, acts.map((a) => h('button', {type: 'button', class: 'btn' + (open === a.name ? ' chosen' : ''), title: a.description || '', 'aria-expanded': a.fields.length ? String(open === a.name) : null,
      onclick: () => (a.fields.length ? toggle(a, r) : act(a, r, {}))}, label(a.name))),
      streams.map((s) => h('button', {type: 'button', class: 'btn' + (shown && shown.name === s.name ? ' chosen' : ''), 'data-stream': s.name, title: s.description || '',
        'aria-expanded': String(!!shown && shown.name === s.name), onclick: () => unfold(s.name)}, label(s.name))),
      // what it is called is the brain's own: every type takes a rename
      own && r.state !== 'deleted' ? h('button', {type: 'button', class: 'btn' + (open === renameWord ? ' chosen' : ''), title: 'What you call it, and your line about it. Its id stays.',
        'aria-expanded': String(open === renameWord), onclick: () => rename(r)}, renameWord) : null,
      !own && r.state !== 'deleted' ? h('span', {class: 'muted'}, `${ownerOf(r)}'s, shared with you to see and use — only its owner changes it.`) : null);

    // a stream that is no longer its owner's to open is put away
    if (shown && !streams.some((s) => s.name === shown.name)) hide();
    if (asked && !shown && streams.some((s) => s.name === asked)) unfold(asked);
    asked = '';

    drawPairs(specBox, t, r);
    drawSeen(seen, r);
    drawDanger(r, own);
    const [ops] = await Promise.all([api('GET', `/v1/operations?resource=${id}&limit=12`), drawNames(r, t)]);
    clear(history, ops.operations.length ? ops.operations.map((o) => opLine(o, ctx)) : h('p', {class: 'muted empty'}, 'Nothing yet.'));
    return moving.includes(r.state) || r.pending || ops.operations.some((o) => o.state === 'running');
  }

  function drawPairs(into, t, r) {
    const spec = r.spec || {};
    const keys = [...new Set([...(t ? t.fields.map((f) => f.name) : []), ...Object.keys(spec)])].filter((k) => spec[k] !== undefined);
    clear(into, keys.length ? keys.map((k) => pair(k, linked(spec[k], r.names || {}, 'asked.' + k))) : h('p', {class: 'muted'}, 'Everything at its default.'),
      Object.keys(r.tags || {}).length ? pair('tags', Object.entries(r.tags).map(([k, v]) => `${k}=${v}`).join(', ')) : null,
      r.shared_with && r.shared_with.length ? pair('shared with', r.shared_with.map((g) => (g === '*' ? 'everyone' : g)).join(', ')) : null);
  }

  function drawSeen(into, r) {
    const obs = r.observed || {};
    const held = Object.entries(r.usage || {}).filter(([, n]) => n).map(([k, n]) => `${n} ${k}`).join(' · ');
    const room = r.room && (r.room.guaranteed_mb || r.room.spot_mb) ?
      [r.room.guaranteed_mb ? `${r.room.guaranteed_mb} MB guaranteed` : '', r.room.spot_mb ? `${r.room.spot_mb} MB of the spot pool` : ''].filter(Boolean).join(' · ') : '';
    clear(into, Object.keys(obs).map((k) => pair(k, /_at$/.test(k) && typeof obs[k] === 'string' ? ago(obs[k]) : linked(obs[k], r.names || {}, 'seen.' + k))),
      held ? pair('holds', held) : null, room ? pair('room', room) : null, r.tier ? pair('counted under', 'tier ' + r.tier) : null,
      !Object.keys(obs).length && !held ? h('p', {class: 'muted'}, 'Nothing reported yet.') : null);
  }

  function pair(k, v) {
    return h('div', {class: 'kv-row'}, h('div', {class: 'muted'}, label(k)), h('div', {class: 'kv-val'}, v));
  }

  // linked: a value that is an id leads to that resource; one that does not
  // read on one line — an object, a text of several lines — is listed.
  function linked(v, names = {}, key = '') {
    const one = (x) => h('a', {href: '#/r/' + x, title: x}, names[x] || x);
    if (typeof v === 'string' && idShape.test(v)) return one(v);
    if (Array.isArray(v) && v.length && v.every((x) => typeof x === 'string' && idShape.test(x))) return v.map((x, i) => [i ? ', ' : '', one(x)]);
    if (nested(v)) return listing(v, {link: (x) => (idShape.test(x) ? one(x) : null), unfolded, key});
    return show(v);
  }

  // drawNames: what its references name, and what names it — read from the
  // schemas' x-hangar-ref, nothing known beforehand.
  async function drawNames(r, t) {
    const named = [];
    for (const f of t ? t.fields.filter((f) => f.ref) : []) {
      const v = (r.spec || {})[f.name];
      for (const x of Array.isArray(v) ? v : v ? [v] : []) if (typeof x === 'string' && idShape.test(x)) named.push(x);
    }
    const got = await Promise.all([...new Set(named)].map((x) => api('GET', '/v1/resources/' + x).catch(() => null)));
    clear(names, got.filter(Boolean).length ? got.filter(Boolean).map((x) => row(ctx, x)) : h('p', {class: 'muted empty'}, 'Nothing.'));

    const asks = [];
    for (const other of ctx.types) {
      const fields = other.fields.filter((f) => f.ref === r.type);
      if (fields.length) asks.push(api('GET', `/v1/resources?type=${encodeURIComponent(other.name)}&zone=${encodeURIComponent(r.zone)}&limit=500`)
        .then((res) => res.resources.filter((x) => fields.some((f) => { const v = (x.spec || {})[f.name]; return v === r.id || (Array.isArray(v) && v.includes(r.id)); }))));
    }
    const by = (await Promise.all(asks)).flat();
    clear(namedBy, by.length ? by.map((x) => row(ctx, x)) : h('p', {class: 'muted empty'}, 'Nothing.'));
  }

  // rename: what it is called and its owner's line about it — the brain's own
  // (PATCH), no operation: it is done when it answers.
  function rename(r) {
    open = open === renameWord ? '' : renameWord;
    for (const b of actions.querySelectorAll('button')) b.classList.toggle('chosen', b.textContent === label(open));
    if (!open) { clear(panel); return; }
    const form = buildForm({fields: calledFields, current: null, submit: 'RENAME',
      onCancel: () => { open = ''; clear(panel); ctx.now(look); },
      onSubmit: async () => {
        clear(said);
        const now = form.values();
        form.busy(true);
        try {
          await api('PATCH', '/v1/resources/' + r.id, {name: String(now.name || '').trim(), description: String(now.description || '').trim()});
          open = '';
          clear(panel);
          clear(said, h('p', {class: 'on'}, 'renamed'));
          await ctx.now(look);
        } catch (p) {
          form.busy(false);
          if (!(p instanceof Problem)) throw p;
          clear(said, refusal(p, form.setViolations(p.violations)));
        }
      }});
    clear(panel, h('div', {class: 'panel action-panel'}, h('p', {class: 'muted'}, h('b', {}, renameWord),
      ' — what you call it, and your line about it. Its id stays, and so does what it was born as on its engine.'), form.node));
    const [name, description] = panel.querySelectorAll('input');
    if (name) { name.value = r.name || ''; name.focus(); }
    if (description) description.value = r.description || '';
  }

  function toggle(a, r) {
    open = open === a.name ? '' : a.name;
    for (const b of actions.querySelectorAll('button')) b.classList.toggle('chosen', b.textContent === label(open));
    if (!open) { clear(panel); return; }
    ctx.now(async () => {
      const refs = await refsFor(ctx, a.fields, r.zone);
      const form = buildForm({fields: a.fields, refs, groups: ctx.me.groups, current: r.spec, submit: label(a.name).toUpperCase(),
        onCancel: () => { open = ''; clear(panel); ctx.now(look); },
        onSubmit: (params) => act(a, r, params, form)});
      clear(panel, h('div', {class: 'panel action-panel'}, h('p', {class: 'muted'}, h('b', {}, label(a.name)), a.description ? ' — ' + a.description : '',
        a.changes_usage ? ' It can change what you hold: it is checked against your limits.' : ''), form.node));
      const first = panel.querySelector('input, select, textarea');
      if (first) first.focus();
    });
  }

  async function act(a, r, params, form) {
    clear(said);
    const body = {client_token: clientToken()};
    if (Object.keys(params).length) body.params = params;
    if (form) form.busy(true);
    try {
      const acc = await api('POST', `/v1/resources/${r.id}/actions/${encodeURIComponent(a.name)}`, body);
      open = '';
      clear(panel);
      await follow(acc.operation, label(a.name));
    } catch (p) {
      if (form) form.busy(false);
      if (!(p instanceof Problem)) throw p;
      clear(said, refusal(p, form ? form.setViolations(p.violations) : p.violations));
    }
  }

  // follow waits for an operation and says how it ended.
  async function follow(op, what) {
    clear(said, h('p', {class: 'busy'}, what + ': running…'));
    ctx.now(look);
    const done = await settled(op.id);
    if (!ctx.alive()) return;
    clear(said, done.state === 'failed' ? h('div', {class: 'flyer', role: 'alert'}, h('div', {class: 'flyer-title'}, 'FAILED'), h('p', {class: 'flyer-detail'}, `${what}: ${done.error || 'it failed'}`))
      : h('p', {class: 'on'}, `${what}: done in ${took(done.created_at, done.finished_at || done.updated_at)}`));
    await ctx.now(look);
  }

  let confirming = false;
  function drawDanger(r, own) {
    if (!own || r.state === 'deleted' || r.state === 'deleting') { clear(danger); return; }
    if (confirming) return; // what a person is deciding is not redrawn under them
    clear(danger, h('section', {class: 'danger'}, h('div', {class: 'stripes', 'aria-hidden': 'true'}),
      h('div', {class: 'danger-body'}, h('div', {}, h('b', {}, `Delete this ${(ctx.type(r.type) || {title: r.type}).title.toLowerCase()}.`), h('span', {class: 'muted'}, ' It is refused while something is attached to it, or it to something: nothing is lost by surprise.')),
        h('button', {type: 'button', class: 'btn hazard', onclick: () => {
          confirming = true;
          clear(danger, h('div', {class: 'flyer', role: 'alertdialog', 'aria-label': 'Delete?'}, h('div', {class: 'flyer-title'}, 'DELETE?'),
            h('p', {class: 'flyer-detail'}, `${nameOf(r)} (${r.id}) will be gone, with what it holds. This cannot be undone.`),
            h('div', {class: 'flyer-end'}, h('button', {type: 'button', class: 'btn hazard solid', onclick: () => del(r)}, 'YES, DELETE IT'),
              h('button', {type: 'button', class: 'btn ghost dark', onclick: () => { confirming = false; drawDanger(r, own); }}, 'Keep it'))));
          danger.querySelector('button').focus();
        }}, 'DELETE…'))));
  }

  async function del(r) {
    confirming = false;
    clear(said);
    try {
      const acc = await api('DELETE', `/v1/resources/${r.id}?client_token=${clientToken()}`);
      clear(danger);
      await follow(acc.operation, 'delete');
    } catch (p) {
      if (!(p instanceof Problem)) throw p;
      clear(said, refusal(p));
      ctx.now(look);
    }
  }

  ctx.every(4000, async () => (gone ? false : look()), 15000);
  return node;
}

// ---- A stream: a resource's terminal, as a page of its own ------------------------

// stream is the page that is nothing but one of a resource's streams: the
// screen, and a bar that says whose it is and leads back.
export function stream(ctx, id, name) {
  const back = h('a', {class: 'term-back', href: '#/r/' + id, title: 'Its page'}, '‹ ' + id);
  const t = terminal({id, stream: name, controls: [
    h('a', {class: 'btn small ghost', href: `#/r/${id}?open=${name}`, title: 'Back in its page, the terminal still shown'}, 'Leave full screen')]});
  t.node.querySelector('.term-bar').prepend(back);
  ctx.onLeave(() => t.close());
  // what it is called, once read; a stream nobody may open says so itself
  ctx.now(async () => {
    const r = await api('GET', '/v1/resources/' + id);
    back.textContent = '‹ ' + nameOf(r);
    document.title = `${nameOf(r)} · ${name} — Le Hangar`;
  });
  t.open();
  return h('main', {class: 'term-page'}, t.node);
}

// ---- Operations -----------------------------------------------------------------

export function operations(ctx) {
  const ops = h('div', {class: 'ops'});
  const node = h('div', {class: 'page'},
    h('div', {class: 'page-head'}, title(ctx.me.operator ? 'everyone\'s — you are an operator' : 'what you asked for, newest first', 'OPERATIONS')), ops);
  ctx.every(4000, async () => {
    const res = await api('GET', '/v1/operations?limit=100');
    clear(ops, res.operations.length ? res.operations.map((o) => opLine(o, ctx)) : h('p', {class: 'muted empty'}, 'No operation yet.'));
    return res.operations.some((o) => o.state === 'running');
  }, 15000);
  return node;
}

// ---- API tokens -----------------------------------------------------------------

export function tokens(ctx) {
  const rows = h('div', {class: 'rows'});
  const made = h('div', {'aria-live': 'polite'});
  const name = h('input', {id: 'tok-name', class: 'input', type: 'text', maxlength: 64, autocomplete: 'off'});
  const life = h('select', {id: 'tok-life', class: 'input'}, [['1 day', 86400], ['7 days', 604800], ['30 days', 2592000]].map(([w, s]) => h('option', {value: s}, w)));
  const scope = h('select', {id: 'tok-scope', class: 'input'}, h('option', {value: 'read,write'}, 'read and write'), h('option', {value: 'read'}, 'read only'));

  async function look() {
    const res = await api('GET', '/v1/tokens');
    const now = Date.now();
    clear(rows, res.tokens.length ? res.tokens.map((t) => {
      const dead = t.revoked_at ? 'revoked' : Date.parse(t.expires_at) <= now ? 'expired' : '';
      return h('div', {class: 'row token' + (dead ? ' dead' : '')},
        h('div', {class: 'row-name'}, h('div', {class: 'name'}, t.name), h('div', {class: 'tiny muted'}, t.id)),
        h('div', {class: 'row-stamp'}, lamp(dead || 'live', dead ? 'off' : 'on')),
        h('div', {class: 'row-what'}, t.scopes.join(', '), h('div', {class: 'tiny muted'}, `expires ${day(t.expires_at)}` + (t.last_used ? ` · last used ${ago(t.last_used)}` : ' · never used'))),
        h('div', {class: 'row-when'}, dead ? null : h('button', {type: 'button', class: 'btn small hazard', onclick: async () => {
          try { await api('DELETE', '/v1/tokens/' + t.id); } catch (p) { clear(made, problemNode(p)); }
          ctx.now(look);
        }}, 'Revoke')));
    }) : h('p', {class: 'muted empty'}, 'No token yet.'));
  }

  const form = h('form', {class: 'form panel', onsubmit: async (e) => {
    e.preventDefault();
    clear(made);
    try {
      const out = await api('POST', '/v1/tokens', {name: name.value.trim(), expires_in: Number(life.value), scopes: scope.value.split(',')});
      name.value = '';
      // shown this once: the brain keeps only its hash
      clear(made, h('div', {class: 'flyer'}, h('div', {class: 'flyer-title'}, 'KEEP IT'), h('div', {class: 'flyer-sub'}, 'shown this once — the brain keeps only its hash'),
        h('p', {class: 'secret', tabindex: '0'}, out.secret), h('p', {class: 'flyer-detail'}, 'HANGAR_TOKEN for a script, or: hangar login URL --with-token')));
      ctx.now(look);
    } catch (p) {
      if (!(p instanceof Problem)) throw p;
      clear(made, refusal(p));
    }
  }},
    h('div', {class: 'field'}, h('label', {class: 'lbl', for: 'tok-name'}, 'name'), name, h('div', {class: 'help'}, 'What it is for.')),
    h('div', {class: 'field'}, h('label', {class: 'lbl', for: 'tok-life'}, 'lives'), life),
    h('div', {class: 'field'}, h('label', {class: 'lbl', for: 'tok-scope'}, 'may'), scope),
    h('div', {class: 'form-end wide'}, h('button', {type: 'submit', class: 'btn primary big'}, 'MAKE A TOKEN')));

  const node = h('div', {class: 'page'},
    h('div', {class: 'page-head'}, h('div', {}, title('for scripts and the command line', 'API', 'TOKENS'),
      h('p', {class: 'lede muted'}, 'A token acts as you, with your groups as they are now, until it expires or you revoke it. A token cannot make tokens.'))),
    h('div', {class: 'two form-and-notice'},
      session.via === 'provider' ? form : h('p', {class: 'panel muted'}, 'You signed in with a token: a token cannot make tokens. They are made on the brain\'s host (hangar token create), or by someone signed in at an identity provider.'),
      made),
    section('YOURS', '', rows));
  ctx.every(30000, look);
  return node;
}
