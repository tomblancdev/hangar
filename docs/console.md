# The console

The web console is a door on the brain's API, like the command line — and,
like it, **drawn from what the brain serves**: every type the enabled plugins
declare gets a list, a form to ask for one, a page of its own and a button
per action, all read from `GET /v1/types`. A new plugin appears in it without
a line of the console changing. The brain checks everything; the console
turns a form into JSON and shows the brain's refusals as they come — beside
the field they name, with the numbers.

It is at **`/console/`** on the brain (the brain's own address leads there).
What it shows:

- **Home** — your tier's limits as gauges (a meter's month and the day it is
  back), what you may ask for, the zones and their room (guaranteed, spot,
  what is kept for whom), what you hold, your last operations.
- **A type's list** — its resources, each stamped with its state; an
  operator's is everyone's, with whose.
- **Asking for one** — a form: every top-level property of the type's schema
  a field, in the schema's order, the schema's own description under it.
- **A resource** — as asked (its spec), as seen (what its plugin reports),
  what it names and what names it (read from the schemas' references), its
  history, **its actions as buttons** — the ones its type offers in its zone
  — each with a form of its params where it has any, and its delete, asked
  twice.
- **Operations**, and **API tokens**: made, shown once, revoked.

The terminal into a machine is not built (it needs a stream the plugin
protocol does not carry yet), and `apply` stays the command line's.

## Signing in

**With an identity provider** — the authorization code flow with PKCE (RFC
7636), with **the brain's own client id**: the same public client the command
line signs in with by the device flow. The brain still holds no client
secret. The browser is sent to the provider's own page — the password, the
second factor and the passkey never pass through the console — and comes
back with a code the console trades for tokens, server to server. The
console then asks the brain who that is (`GET /v1/whoami`): a sign-in the
brain refuses — groups that reach no tier — is said at once, in the brain's
words.

**The console's server keeps the sign-in; the browser holds a cookie.** The
ID token and the refresh token never reach the page: every call the page
makes goes to `/console/api/v1/…`, and the console passes it on to the API
with the person's own token — renewed when it ends (one renewal at a time: a
refresh token is good once). So the brain's audit names the person, signed in
at the provider, for everything the console asks; the console adds nothing
and decides nothing. What the browser holds:

| | |
|---|---|
| the cookie | a random value, `HttpOnly` (no script reads it), `SameSite=Strict` (sent with no request from another site), `Secure` and named `__Host-…` behind TLS; the console keeps only its hash |
| what changes something | carries a token the page reads from `/console/session` in a header (`X-Hangar-Csrf`) — and is refused when its `Sec-Fetch-Site` or `Origin` says another site's page sent it |
| the page | a policy that loads nothing from elsewhere and runs no inline code (`default-src 'none'`, `script-src 'self'`…), sits in no frame; built from nodes, never from markup — what the brain says is always text |

**Nothing of a sign-in is written to disk.** A restart of the console signs
everyone out, and the provider signs them back in (a redirect, silent while
its own session lives). A sign-in left unused for `console.idle` (12h) ends (a page left open and
in view keeps asking, and so keeps it; a tab out of view asks nothing);
one the provider no longer renews (an account disabled, a session revoked)
ends at its next call; **Sign out** forgets it and revokes its refresh token
at the provider (RFC 7009) — the provider's own session is ended on the
provider's page.

A sign-in finishes only in the browser that began it (a cookie set when it
leaves for the provider), once (its `state`), for the request that asked
(its `nonce`), and comes back to a place inside the console and nowhere
else.

**Without a provider** the sign-in page takes an **API token** (`hgr_…`, made
on the brain's host by `hangar token create`) — the same session, holding
that token. Where a provider is configured, a pasted token is not a way in.

### What the operator sets up at the provider

The client the command line already uses, plus **one redirect address**:
`<the console's address>/console/callback`, exactly. Its tokens carry the
groups claim, and `identity.oidc.scopes` (default `openid profile
offline_access`) bring a refresh token — without one, a sign-in lasts as long
as one ID token.

- **authentik** (proved on 2026.8.3, a throwaway signed in to through its
  own pages — `tools/provider/authentik.sh`, `TestProviderTheConsole`): the
  OAuth2 provider of [docs/cli.md](cli.md) — client type *public* — with
  **`authorization_code` among its `grant_types`** and **the redirect address
  added, matching mode *strict***: one it does not know — or the right one
  with anything added — is answered on authentik's own page (400), never
  sent back. It takes a public client's request with or without PKCE (the
  console always sends `S256`). The ID token lives the provider's
  `access_token_validity` (five minutes unless set); the console renews it
  half a minute before it ends, each refresh token good once, and the
  sign-out's revocation is answered 200. A person whose groups reach no tier
  signs in at authentik and is told so by the console, in the brain's words.

## Configuration

```yaml
console:
  enabled: true                          # default; false: the brain serves none
  url: https://hangar.example.org        # the address people open it at: the provider sends
                                         #   them back to <url>/console/callback, and an https
                                         #   one makes the cookie Secure. Empty: read from each
                                         #   request (its Host, the gateway's X-Forwarded-Proto)
  idle: 12h                              # a sign-in unused this long ends
```

## On its own, in front of a brain

```sh
hangar console --brain http://brain:8080 --listen :8081 --url https://hangar.example.org
```

The same console as **a process of its own**: it reads no config file, opens
no registry, starts no plugin and holds no engine's key. It knows the brain's
address, and reaches it for the API alone — with the token of whoever is
signed in. It keeps their sign-ins, in memory, and nothing else.

This is the process to put on a public door: the brain holds the plugins'
credentials and must never face the internet (ARCHITECTURE.md §8); the
console holds none. The brain behind it turns its own off (`console:
{enabled: false}`); a gateway routes `/console/` to this one. Its flags are
also `$HANGAR_CONSOLE_BRAIN`, `_LISTEN`, `_URL`, `_HOUSE`, `_IDLE`; its
`/healthz` says whether its brain answers.

Both ways are one code path — inside `hangar serve` the console calls the
brain's own handler as a request would — and the tests run each on both.

## How a form is drawn

The console reads a schema as the command line does: each **top-level
property** is a field. Its control comes from the schema alone:

| the property | its control |
|---|---|
| `x-hangar-ref: <type>` (a string) | a list of what may be named: yours and what is shared with you in the zone, what is not usable shown and locked — and **`@<schedule>` — the newest**, for what a schedule makes; with `x-hangar-attached`, yours only |
| an array whose items carry `x-hangar-ref` | a box per resource |
| `x-hangar-share: true` | a box per group you are in, and *everyone* — on a type's field, and on an action's param that sets it (the images plugin's `share`) |
| `enum` | chips when it has a default and few values, a list otherwise |
| `boolean` | a box when it must be said or has a default; a third answer (—) when it may be left unsaid |
| `integer`, `number` | a number, its `minimum` and `maximum` |
| `string` | a line; a text area past `maxLength` 256 (a first-boot script) |
| an array of strings | one per line |
| anything else (an object, a list of them) | **JSON, in an editor** (CodeMirror 6): numbered lines, colours, a bracket or a quote bringing its pair, indentation kept, folds, a search (Ctrl-F), several cursors, its own undo — and under it the console's verdict: it reads, or the line and column it breaks at and what was expected there, that line lit |

A field left empty is left out: the brain's default applies. An action's
form shows what each field is **now**. A refusal lands beside the field its
violation points at — and, inside what was typed as JSON (`/labels/stage`),
with the place in words and its line lit —; a limit's or a room's, on a
notice, with the numbers.

**How a value is shown.** A plain value reads as it is: `yes`, `a, b`, an id
as a link by its name. A value that does not read on one line — an object, a
list that holds one, a text of several lines — is **listed as JSON a person
reads**: indented, one thing a line, what is short kept on one, a text of
several lines as its own lines (a first-boot script, never one string of
`\n`), an id inside still a link. Past twelve lines the rest waits behind
*show all* — what was unfolded stays so while the page looks again —, and
*copy* puts the whole of it, as JSON, on the clipboard. One listing serves a
resource's page (as asked, as seen), « Now: » in an action's form and an
operation's line — where an action's params that are plain read as the
command line writes them: `cores=4 · memory_gb=8`.

**What a resource reads as is the brain's**, not the page's: its `name` (its
id when unnamed), the word it wears and its colour (`status`, `light` —
running or stopped, attached or parked, pending, failed…), its type's own
sentence (`summary`), its owner by the name they sign in under, and the
names of what it names — so a list here and `hangar <type> list` say the same
words, and a plugin added later reads well in both. Every type's form begins
with **name** and **description** (the core's own, beside the zone), every
resource's page has a **rename**, and a name already held is refused beside
the field, with the id that holds it. **Home** draws everything you hold as
`hangar list` does: each machine, and under it what hangs on it.

## Its look

`ui/console/` holds the app as it is served: hand-written ES modules, no
build step — what is in the directory is what runs — and **one file the app
did not write**, below.
`theme.css` holds the tokens (colours, faces, corners, how much a notice
tilts, a screen's lines); `app.css` builds on them. Three things, in this
order:

- **A console as consoles are laid out**: a side that lists what may be
  asked for (the catalogue's types), a bar that says where you are, lists as
  tables.
- **An old one's screen**: what the machine reports sits behind glass — lit
  numbers, faint lines, gauges of lit segments —, a state in a list is a
  lamp and its word, where you are is written as a prompt, and the last line
  of the screen is a status line.
- **The street, on top**: a stencil names things; a paper notice taped over
  a panel is something said to you — a refusal, a question, a secret shown
  once —; a resource's own state is stamped; one word a page is sprayed by
  hand; a stripe marks the edge; the button that asks is a sticker.

The faces are Big Shoulders Stencil, IBM Plex Mono and Sedgwick Ave Display,
embedded with their licences (SIL OFL).

## The one file the app did not write

The box JSON is typed in is **CodeMirror 6**, vendored:
`ui/console/vendor/codemirror.js`, one file of 364 KB (118 KB compressed),
with the licences of what is in it beside it (MIT, each as its authors wrote
it). Everything else about that box is the console's own, in `code.js`: its
look (the console's tokens), its verdict (`json.js` reads what is typed, in
words — the browser's own reader says whether, seldom where, and never the
same from one browser to the next), the line a refusal lights.

- **How it is made, and held.** `tools/editor/` pins every package
  (`package-lock.json`); `sh tools/editor/build.sh` builds the file in a
  container (no package's install script runs, nothing of Node on the
  machine); `--check` builds it again aside and compares, byte for byte —
  CI runs that, so what is vendored is what the lock builds. A version
  moves in `package.json`, then `--lock`, then a build: the diff is read.
- **Fetched by a form that has such a field, never before.** A page with no
  JSON to type pays nothing for it; a plain box holds what is typed until
  the editor is there, and stays if it never comes — the verdict under it is
  the same.
- **The page's policy is as it was** (`style-src 'self'`, no inline style):
  an editor writes its styles by script, which on a page means a style tag
  the policy refuses. It sits in **a root of its own** (a shadow root),
  where its styles are sheets the root adopts; the console's tokens reach
  it through that root, so it wears the same colours and faces.
- **The app's own guard reads it like the rest** (`TestTheAppHoldsNoWayIn`:
  no markup sink, no storage, nothing loaded from elsewhere), but for one
  name: the one every SVG is written under, in an image it draws from its
  own bytes.

## Proving it

`internal/console` holds the server's tests (each run on the console inside
the brain, then on its own) and the app's, **in a real browser**: Chromium
driven over the DevTools protocol on a pipe (`internal/browsertest`, the
standard library alone). `HANGAR_BROWSER=/path/to/chromium go test
./internal/console/` — without it the browser tests skip; `HANGAR_SHOTS=dir`
keeps pictures. `TestBenchTheConsole` (`cmd/hangar`) is the same on the
repo's throwaway Proxmox VE. JSON has two of its own: `TestJSONTypedAndListed`
holds the reader to the browser's own verdict on what is JSON and to its
words on where it breaks, the listing to its lines, and the box to its
verdict, its look and its styles (none of them a style tag); and
`TestAnObjectTypedAndRead` types an object into the toy plugin's `labels` as
a keyboard does — broken, refused by the brain inside it, then made, changed
by an action and read back — on a page that fetched no editor before it
needed one.
