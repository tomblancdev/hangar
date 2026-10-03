// form.js — a form drawn from fields (schema.js), for a type's spec or an
// action's params. Each field: its label, the control its kind gets, the
// schema's own description under it, and a place for what the brain refuses
// about it.

import {h, clear} from './dom.js';
import {widget, label, collect, fieldOf, insideOf, show} from './schema.js';
import {nested} from './json.js';
import {listing, editor} from './code.js';

let seq = 0;

// buildForm returns {node, values(), setProblem(p), busy(on)}.
//
//   fields   from fieldsOf(schema)
//   refs     {<type>: [{value, label, disabled}]} — what a reference may name
//   groups   the person's groups (a share's choices)
//   current  a resource's spec: an action's form shows what each field is now
//   submit   the button's words; danger: a red one
//   extra    nodes before the fields (a zone picker, tags)
export function buildForm({fields, refs = {}, groups = [], current = null, submit, danger = false, extra = [], onSubmit, onCancel}) {
  const controls = {};
  const slots = {};
  const editors = {};
  const rows = fields.map((f) => {
    const id = 'f' + (++seq);
    const w = widget(f);
    const err = h('div', {class: 'field-err', role: 'alert'});
    slots[f.name] = err;
    const was = current ? current[f.name] : undefined;
    const now = was !== undefined ? show(was) : '';
    // what does not read on one line is listed under the description
    const help = h('div', {class: 'help'}, f.desc, nested(was) ? h('div', {class: 'now'}, 'Now:', listing(was))
      : now ? h('span', {class: 'now'}, (f.desc ? ' ' : '') + 'Now: ' + now + '.') : null);
    const name = [label(f.name), f.ref ? h('span', {class: 'points'}, ' → ' + label(f.ref)) : null, f.required ? h('span', {class: 'req', title: 'required'}, ' *') : null];
    const [control, getter, wide, grouped, ed] = make(f, w, id, {refs, groups});
    controls[f.name] = getter;
    if (ed) editors[f.name] = ed;
    const body = grouped
      ? h('fieldset', {class: 'field' + (wide ? ' wide' : '')}, h('legend', {class: 'lbl'}, name), control, help, err)
      // a label leads to its field: an editor is not one a browser knows to lead to
      : h('div', {class: 'field' + (wide ? ' wide' : '')}, h('label', {class: 'lbl', for: id, onclick: ed ? () => ed.focus() : null}, name), control, help, err);
    body.dataset.field = f.name;
    return body;
  });

  const notice = h('div', {class: 'form-notice'});
  const go = h('button', {type: 'submit', class: danger ? 'btn hazard big' : 'btn primary big'}, submit);
  const node = h('form', {class: 'form', novalidate: true, onsubmit: (e) => {
    e.preventDefault();
    for (const s of Object.values(slots)) clear(s);
    const {doc, errors} = collect(fields, values());
    if (Object.keys(errors).length) {
      for (const [k, msg] of Object.entries(errors)) clear(slots[k], msg);
      return;
    }
    onSubmit(doc);
  }}, extra, rows, h('div', {class: 'form-end wide'}, go, onCancel ? h('button', {type: 'button', class: 'btn ghost', onclick: onCancel}, 'Cancel') : null), notice);

  function values() {
    const out = {};
    for (const [k, get] of Object.entries(controls)) out[k] = get();
    return out;
  }
  return {
    node,
    values,
    notice,
    busy(on) { go.disabled = on; go.classList.toggle('working', on); },
    // what the brain refused, field by field: beside the field it names —
    // and, inside what was typed as JSON, where: in words, and its line lit
    setViolations(violations) {
      const rest = [];
      for (const v of violations || []) {
        const name = fieldOf(v.field);
        const slot = slots[name];
        if (!slot) { rest.push(v); continue; }
        const inside = insideOf(v.field);
        slot.append(h('div', {}, inside ? h('b', {}, inside.slice(1).split('/').map((s) => s.replace(/~1/g, '/').replace(/~0/g, '~')).join(' › ') + ': ') : null, v.reason));
        if (inside && editors[name]) editors[name].mark(inside);
      }
      return rest;
    },
  };
}

// make builds one control: [node, getter, wide, grouped, editor].
function make(f, w, id, {refs, groups}) {
  switch (w) {
    case 'ref': {
      const sel = h('select', {id, class: 'input'}, h('option', {value: ''}, f.required ? 'choose…' : '— none —'),
        (refs[f.ref] || []).map((o) => h('option', {value: o.value, disabled: o.disabled}, o.label)));
      return [sel, () => sel.value, false, false];
    }
    case 'refs':
    case 'share':
    case 'checks': {
      const options = w === 'refs' ? (refs[f.ref] || []).filter((o) => !o.value.startsWith('@'))
        : w === 'share' ? [...groups.map((g) => ({value: g, label: g})), {value: '*', label: 'everyone'}]
        : f.itemsEnum.map((v) => ({value: v, label: v}));
      const boxes = options.map((o) => {
        const box = h('input', {type: 'checkbox', value: o.value, disabled: o.disabled});
        return [box, h('label', {class: 'pick'}, box, h('span', {}, o.label))];
      });
      const node = h('div', {class: 'picks'}, boxes.length ? boxes.map((b) => b[1]) : h('div', {class: 'muted'}, w === 'refs' ? `you have no ${label(f.ref)} yet` : 'nothing to choose from'));
      return [node, () => boxes.filter((b) => b[0].checked).map((b) => b[0].value), false, true];
    }
    case 'chips': {
      const radios = f.enum.map((v) => {
        const r = h('input', {type: 'radio', name: id, value: v});
        r.checked = String(f.def) === v;
        return [r, h('label', {class: 'pick'}, r, h('span', {}, v))];
      });
      return [h('div', {class: 'picks inline'}, radios.map((r) => r[1])), () => (radios.find((r) => r[0].checked) || [{value: ''}])[0].value, false, true];
    }
    case 'select': {
      const sel = h('select', {id, class: 'input'}, h('option', {value: ''}, f.def !== undefined ? `default (${f.def})` : '—'), f.enum.map((v) => h('option', {value: v}, v)));
      return [sel, () => sel.value, false, false];
    }
    case 'check': {
      const box = h('input', {type: 'checkbox', id});
      box.checked = f.def === true;
      return [h('label', {class: 'pick'}, box, h('span', {}, 'yes')), () => box.checked, false, true];
    }
    case 'tristate': {
      const sel = h('select', {id, class: 'input'}, h('option', {value: ''}, '—'), h('option', {value: 'yes'}, 'yes'), h('option', {value: 'no'}, 'no'));
      return [sel, () => sel.value, false, false];
    }
    case 'number': {
      const range = f.min !== undefined && f.max !== undefined ? `${f.min} – ${f.max}` : '';
      const inp = h('input', {id, class: 'input', type: 'number', inputmode: 'numeric', min: f.min, max: f.max, step: f.kind === 'integer' ? 1 : 'any',
        placeholder: f.def !== undefined ? `default ${f.def}` : range});
      return [inp, () => inp.value, false, false];
    }
    case 'json': {
      const ed = editor({id, label: label(f.name)});
      return [ed.node, () => ed.value, true, false, ed];
    }
    case 'textarea':
    case 'lines': {
      const ta = h('textarea', {id, class: 'input', rows: w === 'textarea' ? 6 : 3, spellcheck: 'false',
        placeholder: w === 'lines' ? 'one per line' : ''});
      return [ta, () => ta.value, true, false];
    }
    default: {
      const inp = h('input', {id, class: 'input', type: 'text', autocomplete: 'off', spellcheck: 'false', maxlength: f.maxLength,
        placeholder: f.def !== undefined ? `default ${f.def}` : ''});
      return [inp, () => inp.value, false, false];
    }
  }
}

// refusal draws what the brain refused as a notice: the question that
// failed, why, and — over a limit, or out of room — the numbers.
export function refusal(p, unplaced = p.violations) {
  const lines = [];
  for (const v of unplaced || []) lines.push(h('li', {}, v.field ? h('b', {}, fieldOf(v.field) + ': ') : null, v.reason));
  for (const r of p.refusals || []) if (r.message) lines.push(h('li', {}, r.message));
  if (p.room && p.room.message) lines.push(h('li', {}, p.room.message));
  // the first line the brain says is often the first of its list: said once
  const said = (p.refusals || []).some((r) => r.message === p.message) || (p.room && p.room.message === p.message);
  return h('div', {class: 'flyer', role: 'alert'},
    h('div', {class: 'flyer-title'}, 'REFUSED'),
    h('div', {class: 'flyer-sub'}, (p.title || p.kind || 'refused') + ' · nothing was changed'),
    said ? null : h('p', {class: 'flyer-detail'}, p.message),
    lines.length ? h('ul', {}, lines) : null,
    p.operation ? h('p', {}, h('a', {href: '#/operations'}, 'the operation holding it: ' + p.operation)) : null);
}
