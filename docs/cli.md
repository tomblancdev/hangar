# The command line

`hangar` is also the command line people ask a brain with — a door on the API,
like the console, **drawn from what the brain serves**: every type the enabled
plugins declare is a command, its flags drawn from the type's JSON Schema, its
actions verbs. A new plugin appears here without a line of the command line
changing. The brain checks everything; the command line turns words into JSON
and prints the brain's refusals as they come — field by field, with the
numbers.

```console
$ hangar login https://hangar.example.org
To sign in to https://hangar.example.org, open

    https://id.example.org/device?code=WDJB-MJHT

and check the code there reads  WDJB-MJHT

Waiting…
Signed in to https://hangar.example.org as alice — tier users.

$ hangar machine create --kind container --image debian-13 --type t3.medium
created m-0123456789abcdef0 (ready, 14s)
$ hangar machine resize m-0123456789abcdef0 --memory-gb 8 --cores 2
$ hangar volume create --size-gb 16 --mount /home --machine m-0123456789abcdef0
$ hangar apply dev.yaml
```

## Signing in

**`hangar login URL`** asks the brain where people sign in (`GET /v1/signin`:
the identity provider, the client id, the scopes), then runs the **device
flow** (RFC 8628) at the provider: a code to approve on the provider's own
page, in any browser, on any device — the password, the second factor and
the passkey never pass through the command line. It keeps the refresh token
and sends the ID token (the brain reads tokens issued to its client id),
refreshed when it ends; **`hangar logout`** revokes the refresh token at the
provider (RFC 7009) and forgets it.

The sign-ins are kept in `$HANGAR_HOME/credentials.json` (default
`~/.config/hangar`), the file `0600` — a refresh token is a credential, kept
as the AWS CLI's SSO cache and kubelogin keep theirs. A directory the command
line makes is `0700`; one that exists is left as it is.

**Without a provider**, or for a script: an API token (`hgr_…`) —
`HANGAR_TOKEN=hgr_… hangar …`, or `hangar login URL --with-token < file`. A
brain that has no provider says so at `login`.

`HANGAR_URL` names the brain; without it, the brain signed in to last.

### What the operator sets up at the provider

The brain's `identity.oidc.audience` is the client id — **a public client**
(the brain holds no client secret: it only reads tokens) **with the device
flow on**. Its tokens must carry the groups claim (`groups_claim`), and the
scopes the command line asks for (`identity.oidc.scopes`, default `openid
profile offline_access`) must bring it and a refresh token.

- **authentik** (proved on 2026.8.3, a throwaway driven through its own
  pages): an OAuth2 provider, client type *public*, **its `grant_types`
  listing the device code (`urn:ietf:params:oauth:grant-type:device_code`)
  and `refresh_token`** — a provider answers only the grants it lists
  (`invalid_client` otherwise) —, `redirect_uris` given even empty (the
  field is required), the scope mappings `openid`, `profile` (it carries
  `groups`) and `offline_access`; an application for it; and **the brand's
  *device code flow*** set (any flow — even an empty one, *stage
  configuration* — whose authentication requires a signed-in user): without
  it `/device` answers 404. A device code lives the provider's
  `access_code_validity` (**one minute by default** — give people ten); the
  device endpoint is throttled (20 an hour). The provider's authorization
  flow runs once the code is entered (an implicit-consent flow signs the
  device in at once); refresh tokens are rotated at each refresh, and
  revoked by `logout`. Its `sub` is a hashed id unless `sub_mode` says
  otherwise — a resource's owner is that `sub`.
- Keycloak: the client's *OAuth 2.0 Device Authorization Grant* on, a groups
  mapper on its dedicated scope. Dex: `deviceFlow`, a public static client.

## A type's commands

```text
hangar <type> create [--zone Z] [FIELDS] [-f spec.yaml] [--tag k=v] [--no-wait] [-o yaml|json|id]
hangar <type> list [--zone Z] [--tag k=v] [--state S] [--owner SUBJECT] [-o …]
hangar <type> get ID | delete ID
hangar <type> <action> ID [PARAMS]           e.g. hangar volume set-backup vol-… --backup
hangar <type> --help                         its fields and actions, from its schema
```

- **Flags from fields.** A top-level property `memory_gb` is `--memory-gb`
  (or `--memory_gb`); an integer, a boolean (`--resume=false`), a string, an
  array (repeated, or comma-separated), anything else as YAML. A reference
  (`x-hangar-ref`) takes an id, or `@<schedule>` for the newest it made.
  `--set key=value` reaches any field (its value YAML), and `-f` reads a
  whole spec (flags are written over it).
- **The zone**: `--zone`, else `$HANGAR_ZONE`, else the only zone the type is
  offered in.
- **Waiting**: a create, delete or action waits for its operation and says
  how it ended; `--no-wait` answers at once (`hangar wait OP` later).
- **The command line's own words win**: `login`, `logout`, `whoami`, `types`,
  `zones`, `limits`, `operations`, `wait`, `apply`, `type`; a type of one of
  those names is `hangar type <name> …`. An action named `create`, `list`,
  `get` or `delete` is `hangar <type> act ID <action>`.

## `hangar apply`: a spec file made true

```yaml
set: dev-box                 # what apply keeps these resources under
zone: lab                    # every resource's, unless one says otherwise
resources:
  home:                      # any order: apply sorts them by what they name
    type: volume
    spec: {size_gb: 64, mount: /home, backup: true, machine: box}
  box:
    type: machine
    spec: {kind: container, class: guaranteed+spot, cores: 12, memory_gb: 40, floor_gb: 12,
           image: debian-13, key_pairs: [me]}
  me:
    type: keypair
    spec: {public_key: "ssh-ed25519 AAAA… alice@laptop"}
```

Each entry is **one resource, in its type's own words** — the schema the API
already checks, no second format. A reference names **an id, `@<schedule>`,
or another entry** (`machine: box`): apply makes them in that order, waiting
for each to be ready and usable. `zone:` and `tags:` may be given per entry.

**The brain is the state** — no state file: each resource carries the tags
`apply:set=<set>` and `apply:name=<entry>`. **Only the caller's own count**: a
resource someone else shared with them, or (for an operator, whose listing is
everyone's) someone else's under the same set, is never theirs to change.

`hangar apply FILE` prints the plan, then does it:

| mark | what | how |
|---|---|---|
| `+` | missing | created, after what it names |
| `~` | differs | brought to the file **by the steps its plugin names** (`POST /v1/resources/{id}/plan`: « resize {memory_gb: 48} », « detach, attach {machine, mount} ») — each asked as any action is: planned, admitted, audited |
| `=` | in sync | nothing |
| `-` | left the file | **deleted — after asking** (`--yes` for a script; with no one at stdin, it refuses without it); unplugged first from what it is attached to, by its plugin's own steps |
| `!` | a field set at its birth differs | **nothing is changed at all**: put the file back, or delete the resource and apply again |

`--plan` prints it and changes nothing. A step the engine refuses stops the
run with the engine's words; what came before it is done, and the next apply
starts from there (on Proxmox VE a running container lets go of no volume,
and takes a volume's backup flag only while stopped: stop it, apply again).

A spec is **whole**: a field it leaves out is wanted at its default (a
machine's `disk_gb` excepted, whose default follows its image). A reference
written **`@<schedule>` never counts as a change**: a machine born from last
week's image is where `@debian-13` wants it — it is never moved to the newest
(what `@` names is what a *new* machine is born from).

*Designed, not built: **a rebuild** — a field set at birth changed, the
resource made again (delete, then create, its attached volumes carried across:
the machine stopped, each detached, re-attached to the new one), shown in the
plan and asked, as Terraform's `-/+`.*
