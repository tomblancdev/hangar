// api.js — the app's calls. The console's server holds the sign-in; the
// browser holds a cookie no script reads. Every call to the brain's API goes
// to api/v1/… on the console, which passes it on with the person's own
// token; what changes something carries the session's token in a header.

// Problem is a refusal as the API returns it (RFC 9457): which question
// failed, and why — field by field, with the numbers.
export class Problem extends Error {
  constructor(status, body) {
    super((body && body.detail) || `the brain answered ${status}`);
    this.status = status;
    this.kind = (body && body.kind) || '';
    this.title = (body && body.title) || '';
    this.violations = (body && body.violations) || [];
    this.refusals = (body && body.refusals) || [];
    this.room = (body && body.room) || null;
    this.operation = (body && body.operation) || '';
  }
}

export const session = {signedIn: false, csrf: '', via: '', how: '', provider: '', version: ''};

let onSignedOut = () => {};
export function whenSignedOut(f) { onSignedOut = f; }

async function read(resp) {
  const text = await resp.text();
  let body = null;
  if (text) {
    try { body = JSON.parse(text); } catch { body = {detail: text.slice(0, 300)}; }
  }
  if (!resp.ok) throw new Problem(resp.status, body);
  return body;
}

export async function loadSession() {
  const s = await read(await fetch('session', {headers: {Accept: 'application/json'}}));
  session.signedIn = s.signed_in === true;
  session.csrf = s.csrf || '';
  session.via = s.via || '';
  session.how = s.how || '';
  session.provider = s.provider || '';
  session.version = s.version || '';
  return session;
}

// api asks the brain: api('GET', '/v1/types'), api('POST', '/v1/resources', {…}).
export async function api(method, path, body) {
  const init = {method, headers: {Accept: 'application/json'}};
  if (method !== 'GET') init.headers['X-Hangar-Csrf'] = session.csrf;
  if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  let resp;
  try {
    resp = await fetch('api' + path, init);
  } catch {
    throw new Problem(0, {kind: 'unreachable', detail: 'the console cannot be reached: check your connection'});
  }
  try {
    return await read(resp);
  } catch (p) {
    if (p instanceof Problem && p.status === 401) {
      session.signedIn = false;
      onSignedOut(p);
    }
    throw p;
  }
}

export async function signInWithToken(token) {
  const s = await read(await fetch('signin/token', {
    method: 'POST', headers: {'Content-Type': 'application/json', Accept: 'application/json'},
    body: JSON.stringify({token}),
  }));
  session.signedIn = true;
  session.csrf = s.csrf;
  session.via = s.via;
}

export async function signOut() {
  await fetch('signout', {method: 'POST', headers: {'X-Hangar-Csrf': session.csrf}});
  session.signedIn = false;
  session.csrf = '';
}

// clientToken makes a retry of a request return its first operation instead
// of starting another.
export function clientToken() {
  const b = new Uint8Array(16);
  crypto.getRandomValues(b);
  return 'web-' + Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');
}

// settled waits for an operation to end, holding the wait open at the brain
// (so it answers the moment it ends), and tells of each look.
export async function settled(id, tick) {
  for (;;) {
    const op = await api('GET', `/v1/operations/${encodeURIComponent(id)}?wait=25`);
    if (op.state !== 'running') return op;
    if (tick) tick(op);
  }
}
