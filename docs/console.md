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
  twice. And, on your own, **its streams as keys**: a machine's terminal
  (below).
- **Operations**, and **API tokens**: made, shown once, revoked.

`apply` stays the command line's.

## A machine's terminal

A machine's own screen and keyboard, in the page: no key, no client, nothing
installed in the machine or on your computer. It is a **stream** its type
declares (ARCHITECTURE.md §4), and the console draws any type's stream the
same way — it does not know what a machine is.

- **Where.** The `terminal` key sits among the machine's keys **on its
  owner's page, and on nobody else's** — an operator's page of that machine
  has none, and the brain refuses the address. It unfolds a screen under the
  keys; an action asked meanwhile does not take it away — **a reboot is
  watched from it**: the machine goes down and comes back on the same
  screen. **Full screen** leads to the same screen alone, at an address of
  its own (`#/r/<id>/terminal`); **Leave full screen** comes back with it
  still shown — and so does any other way back to the machine's page: the
  name in the terminal's bar, the browser's own. Both ways it is **the same
  terminal, carried**: its screen, the line being typed, its socket —
  nothing is opened again, and the audit holds one opening. It is handed to
  the page the same navigation shows, and to no other: `Close`, or leaving
  for anywhere else, lets go of it at once.
- **Signed in, or asked.** A VM born with no key pair lands you in a shell
  as its user, nothing asked: the gateway and its second factor already said
  who you are. One born with a key asks a login unless its form says
  `terminal: open`. It is set at the machine's birth (the machines plugin's
  `terminal`).
- **Its size.** A machine's serial port carries no window size: the machine
  asks the terminal when it signs you in, and takes what it answers. A
  window that changes afterwards redraws here at once, and the bar says so —
  `exit` signs you in again at the new size, and the bar stops saying it
  when the machine asks again. (A terminal opened again shows a shell at the
  size of the window it was signed in from.) A program that draws the whole
  screen — an editor, a pager — draws at the size it was started at: on a
  smaller screen it is cut. No shell on a serial port is
  told its window changed, so none draws its line again: the line you are on
  is wrapped and unwrapped with the others as the window narrows and widens,
  never cut at the narrower edge. A window with no room for a terminal —
  under 16 columns or 4 rows, a moment on its way to another size — is not
  fitted to.
- **One place at a time.** Opened again — another tab, your phone — it moves
  there, and the first is told (« TAKEN »), with a key to take it back. It
  is one screen: what was running on it is still there.
- **Opened again, it is where it was left — and says nothing by itself.** A
  machine keeps no picture of its screen, and nothing on the way to you
  does: a terminal opened again (the page loaded anew, your phone, `Close`
  then `terminal`) starts empty, the shell that was left there waiting for a
  key. After four seconds without a word from the machine the page
  says so on a slip over the screen, with a **Redraw** key: it types Ctrl-L,
  which a shell, an editor or a pager answers by drawing its screen again —
  the line you had begun on it too. **The page types nothing by itself**: a
  key pressed for you would land in whatever reads the keyboard then — a
  password being asked, a file being written. At a login, Enter asks again.
  The slip leaves at the machine's first word, or at your first key. A first
  opening never shows it: a machine about to sign you in asks the terminal
  its size every two seconds, and the four are counted from the moment the
  brain's own stream is open, which the console says in the socket. A screen
  this page kept across an ending (« OPEN IT HERE ») is told to forget one
  thing before its machine is opened again: a program's asking to be told of
  the window's focus — that program may be gone, and the page taking the
  keyboard would type a report into whatever reads it now.
- **Why it ended** is said on a notice over the screen, in the brain's own
  words: the machine was stopped (for a game on the zone's room, by its idle
  rule, by you), it was opened elsewhere, your sign-in ended, the brain
  refused it (« m-… is stopped: start it, then open its terminal »).
- **On a phone**, a row of the keys its keyboard has none of: Esc, Tab, Ctrl
  (pressed, then a letter), the four arrows.

**Underneath.** The page opens a WebSocket on the console
(`api/v1/resources/<id>/streams/<stream>`), on its cookie; the console opens
the brain's with the person's own token and carries the messages between the
two as they come. Two things are the console's own, because a browser
reaches it on a cookie: the socket is taken **only from the console's own
pages** (its `Origin` — a cookie rides a WebSocket's opening as it rides any
request), and a refusal is **said inside the socket** before it closes (a
page cannot read why an opening failed) — as is, for the same reason, the
moment the brain's own stream is open (`{"opened":true}`): the page says
« open » then, not when its socket was taken. What is open on a sign-in ends with
it — and while a terminal is open the sign-in under it is **looked at**,
every twenty seconds: its token renewed at the provider before it ends, the
new one handed to the brain (which asks again, every half minute, what the
opening asked); a sign-in the provider no longer renews — an account
disabled, a session ended there — closes, and its terminal with it. Typing
is using a sign-in; it is not a way to keep one the provider ended. The
console keeps nothing of what passes, and the brain's audit holds the
opening and the closing — how long, how many bytes — never a word.

**Named, not hidden:** an open terminal is not what a machine's `idle_after`
counts (its CPU and what it sends are) — `keep awake` holds a quiet machine;
a shell left on the screen stays there for whoever opens the terminal next,
which is only ever its owner; the screen is the machine's console too — a
line of its init, or of its shutdown, can land among what you type (Redraw
is one key on a phone; Ctrl-L anywhere); a terminal has no say in a screen
reader yet.

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
build step — what is in the directory is what runs — and **two files the app
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

## The two files the app did not write

**The terminal's screen is xterm.js**, vendored:
`ui/console/vendor/xterm.js` — the library, its fit to a box and its canvas
renderer, one file of 471 KB (123 KB compressed) —, its stylesheet
(`xterm.css`) and the licences of what is in them (MIT). Everything else
about a terminal is the console's own, in `terminal.js`: the socket, the
colours (the theme's tokens), the keys a phone lacks, the notices.

- **Made and held as the editor is** (below): `tools/terminal/` pins every
  package; `sh tools/terminal/build.sh` builds in a container, `--check`
  compares byte for byte, and CI runs it.
- **Fetched when a terminal is opened, never before.**
- **The page's policy is as it was** — and names one thing more, the
  console's own socket (`connect-src 'self' wss://<its address>`: not every
  browser reads `'self'` as covering a WebSocket). xterm.js writes three
  `<style>` by script — its colours, its cells' size, its scrollbar's —,
  which the policy refuses; each becomes **a sheet the document adopts**
  instead (`terminal.js`: for `<style>` alone, for the life of the page),
  and where the browser can, the screen is drawn on a canvas. What is left
  to refuse without the canvas is one thing: a 24-bit colour's own style,
  and that text then takes the default colour.
- **It asks the terminal one thing the library answers only when told to**:
  its size, in characters (`windowOptions.getWinSizeChars`) — how a machine
  learns the window it is opened in.

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
- **The app's own guard reads both like the rest**
  (`TestTheAppHoldsNoWayIn`: no markup sink, no storage, nothing loaded from
  elsewhere), but for two names: the ones every SVG and every HTML element
  are made under — names of what they draw from their own bytes.

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

A terminal has three. `TestAStreamThroughTheConsole` and its two neighbours
hold the door: what passes, passes as it came, inside the brain and on its
own; a refusal is read in the socket; another site's page with the person's
cookie is refused before anything opens; a sign-out closes what was open,
and so does a provider that stops renewing the sign-in under it; a stream
the brain opened says so first, in the console's own word.
`TestATerminalInThePage` is the page in a real browser, under its own
policy, with the browser's complaints read (none): a machine with no key
made and entered, the window's size told, the whole window and back as one
terminal (a line begun on one page ended on the other, one opening in the
audit; back by its key, by the browser's way, by the name in its bar), the
shell's size kept until `exit`, a window narrowed and widened under a long
line and shrunk to one pixel, the phone taking it — an empty screen, the
slip, Redraw drawing a half-typed line without entering it — and the desk
taking it back, its slip gone at a key the machine answers nothing to, no
report typed when it takes the keyboard again, an operator offered none and
refused by its address, the machine stopped under it. The engine it runs on
is the fake one, **whose port has learnt the real one's form**: its other
end outlives a console, a second opening hears nothing, a shell keeps the
size of its sign-in (`driver/fake/console.go`) — it used to greet at every
opening and take every window's size, and hid all of the above. And `TestBenchATerminalInThePage` (`cmd/hangar`) is
the same on a real Proxmox VE — a real machine's first boot, its user, its
colours, its own shell drawn again at Redraw — with the audit's lines read
and nothing typed found in the brain's log.
