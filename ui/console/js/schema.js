// schema.js — a type's JSON Schema, read as the fields of a form. Pure: no
// page here, so it is tested on its own. It is the console's reading of what
// the command line reads the same way (internal/cli/catalogue.go): every
// top-level property a field, in the order the schema writes them.
//
// The brain checks everything. This only decides which control a field gets
// and turns what was typed into JSON.

import {check} from './json.js';

function kindOf(t) {
  if (typeof t === 'string') return t;
  if (Array.isArray(t)) return t.find((x) => typeof x === 'string' && x !== 'null') || '';
  return '';
}

// fieldsOf reads a schema's top-level properties in the order it writes them.
export function fieldsOf(schema) {
  const props = (schema && schema.properties) || {};
  const required = (schema && schema.required) || [];
  return Object.entries(props).map(([name, p]) => {
    p = p || {};
    const items = p.items || {};
    return {
      name,
      kind: kindOf(p.type),
      items: kindOf(items.type),
      enum: Array.isArray(p.enum) ? p.enum.map(String) : [],
      itemsEnum: Array.isArray(items.enum) ? items.enum.map(String) : [],
      def: p.default,
      desc: p.description || '',
      ref: p['x-hangar-ref'] || items['x-hangar-ref'] || '',
      attached: p['x-hangar-attached'] === true,
      share: p['x-hangar-share'] === true,
      required: required.includes(name),
      min: p.minimum,
      max: p.maximum,
      maxLength: p.maxLength,
    };
  });
}

// label is a field's name as a person reads it: memory_gb → memory gb.
export function label(name) {
  return name.replace(/[_-]+/g, ' ');
}

// widget says which control a field gets.
export function widget(f) {
  if (f.ref) return f.kind === 'array' ? 'refs' : 'ref';
  if (f.kind === 'array' && f.share) return 'share';
  if (f.kind === 'array' && f.itemsEnum.length) return 'checks';
  if (f.kind === 'array' && (f.items === 'string' || f.items === '')) return 'lines';
  if (f.enum.length) return f.def !== undefined && f.enum.length <= 4 ? 'chips' : 'select';
  // a yes/no that must be said, or has a default: a box. One that may be
  // left unsaid keeps a third answer.
  if (f.kind === 'boolean') return f.required || f.def !== undefined ? 'check' : 'tristate';
  if (f.kind === 'integer' || f.kind === 'number') return 'number';
  if (f.kind === 'string') return (f.maxLength || 0) > 256 ? 'textarea' : 'text';
  return 'json';
}

// read turns what a control holds into the field's JSON value. It returns
// undefined for a field left empty (the brain's default applies), and throws
// an Error in the person's words for what cannot be read at all.
export function read(f, raw) {
  switch (widget(f)) {
    case 'check':
      return raw === true;
    case 'tristate':
      return raw === 'yes' ? true : raw === 'no' ? false : undefined;
    case 'refs':
    case 'share':
    case 'checks':
      // an empty list is said only where the field must be (« shared with no one »)
      return raw.length || f.required ? raw.slice() : undefined;
    case 'lines': {
      const lines = String(raw).split('\n').map((s) => s.trim()).filter(Boolean);
      return lines.length || f.required ? lines : undefined;
    }
    case 'number': {
      const s = String(raw).trim();
      if (s === '') return undefined;
      const n = Number(s);
      if (!Number.isFinite(n)) throw new Error('a number');
      if (f.kind === 'integer' && !Number.isInteger(n)) throw new Error('a whole number');
      return n;
    }
    case 'json': {
      if (String(raw).trim() === '') return undefined;
      // where it breaks, and what was expected there: as its box says it
      const got = check(String(raw));
      if (got.message) throw new Error(`line ${got.line}, column ${got.column}: ${got.message}`);
      return got.value;
    }
    case 'textarea': {
      // kept as typed: a first-boot script's blank lines and indentation are its own
      return String(raw).trim() === '' ? undefined : String(raw);
    }
    default: {
      const s = String(raw).trim();
      return s === '' ? undefined : s;
    }
  }
}

// collect reads a whole form: the document to send, and what could not be
// read, by field.
export function collect(fields, raws) {
  const doc = {};
  const errors = {};
  for (const f of fields) {
    if (!(f.name in raws)) continue;
    try {
      const v = read(f, raws[f.name]);
      if (v !== undefined) doc[f.name] = v;
    } catch (e) {
      errors[f.name] = e.message;
    }
  }
  return {doc, errors};
}

// summary is a spec's scalar fields, in its schema's order, short — the
// command line's own line for a resource.
export function summary(fields, spec, skip = []) {
  const parts = [];
  for (const f of fields) {
    if (skip.includes(f.name)) continue;
    const v = spec ? spec[f.name] : undefined;
    if (typeof v === 'string') {
      if (v !== '') parts.push(f.name + '=' + (v.length > 32 ? v.slice(0, 29) + '…' : v));
    } else if (typeof v === 'number' || typeof v === 'boolean') {
      parts.push(f.name + '=' + v);
    } else if (Array.isArray(v) && v.length) {
      parts.push(f.name + '=' + v.length);
    }
  }
  return parts.join(' · ');
}

// fieldOf names the field a violation points at: a JSON pointer's first
// step (/key_pairs/0 → key_pairs).
export function fieldOf(pointer) {
  const m = /^\/?([^/]+)/.exec(pointer || '');
  return m ? m[1].replace(/~1/g, '/').replace(/~0/g, '~') : '';
}

// insideOf is the rest of that pointer: where, inside the field, a
// violation points (/rules/1/port → /1/port); '' when at the field itself.
export function insideOf(pointer) {
  const m = /^\/?[^/]+(\/.*)$/.exec(pointer || '');
  return m ? m[1] : '';
}

// plural: a type's title, for a list of them.
export function plural(word) {
  if (/(s|x|z|ch|sh)$/i.test(word)) return word + 'es';
  if (/[^aeiou]y$/i.test(word)) return word.slice(0, -1) + 'ies';
  return word + 's';
}

// show is a value as a person reads it.
export function show(v) {
  if (v === null || v === undefined) return '';
  if (typeof v === 'boolean') return v ? 'yes' : 'no';
  if (Array.isArray(v)) return v.every((x) => typeof x !== 'object') ? v.join(', ') : JSON.stringify(v);
  if (typeof v === 'object') return JSON.stringify(v);
  return String(v);
}
