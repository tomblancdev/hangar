# Le Hangar — architecture

*A small, self-hosted cloud control plane — a brain with an API — that turns
requests (« a machine of this size, this disk, this image ») into resources on
the hypervisors you already own, within limits set per group, through plugins
that each hold their own key. It knows which capacity is guaranteed and which
is borrowed, gives borrowed room back the moment a priority guest needs it,
and puts idle machines to sleep.*

This document is the design. Each section says whether it is **built** (in
this repository, tested) or **designed** (agreed, not yet written). The
[OpenAPI document](api/openapi.yaml) and the
[plugin protocol](proto/hangar/plugin/v1/plugin.proto) are the contracts;
where this page and they disagree, they win.

| part | status |
|---|---|
| The core: identity (OIDC + API tokens), tiers and limits, the registry, operations, reconcile, audit, the plugin host | **built** |
| The plugin protocol and its SDK; the fake driver; the toy plugin | **built** |
| The API (`/v1`), `/healthz`, `/metrics`, `/openapi.json` | **built** |
| Zones' capacity: pools, classes, reservations, holds, the claim, waking; the Proxmox VE hook (`hangar-hook`) | **built** (§6) — proved on a throwaway Proxmox VE |
| Idle machines put to sleep, awake hours counted against a tier, keep awake | designed (§6, power) |
| The machines plugin (machines, key pairs); the Proxmox VE driver; references between resources | **built** (§7, §5, §4) — proved on a throwaway Proxmox VE ([docs/proxmox.md](docs/proxmox.md)) |
| The volumes plugin (volumes, parked on a shelf where the engine keeps no disk without a guest); attachments between resources | **built** (§7, §5, §4) — proved on a throwaway Proxmox VE |
| The images plugin | designed (§7) |
| The command line and the console generated from the schemas | designed (§2) |
| Names, ports, snapshots, object storage, databases; the Incus and AWS drivers | designed (§7) |

---

## 1. What it is — for anyone

**A cloud provider's control plane for a handful of machines**: a home lab,
a club, a small office. People sign in with the identity provider they
already run, ask for machines, volumes and images (and, through plugins,
anything else), and get them within their group's limits — on the engines
they already own, through drivers. **The AWS mindset, not the AWS wire**:
self-service, an API first, resources with IDs, types, images, user data,
tags, on-demand and spot — no EC2 API clone.

**Not in scope, said plainly:** not a hypervisor (it drives yours); not an
AWS API clone; no money billing (the unit of cost is *awake hours*); no
highly-available brain (one brain, its database backed up).

**Why Go:** the engines' client libraries are Go (Incus's own client, the
AWS SDK, Proxmox API clients), and a single static binary runs in a
`scratch` image.

## 2. Five layers

```mermaid
flowchart TB
  subgraph WHO["Who"]
    admin["the operator"]
    users["users, in groups with limits"]
  end
  subgraph DOORS["Front doors — all clients of the one API"]
    cli["command line (generated from the plugins' schemas)"]
    console["web console (generated from the plugins' schemas)"]
    api["the API · a Terraform provider later"]
  end
  subgraph CORE["The core"]
    id["identity: OIDC + API tokens"]
    lim["tiers and limits"]
    reg[("registry: every resource, its ID, owner, tags, relations")]
    zones["zones: pools, classes, reservations, preemption"]
    ops["operations + reconcile loop"]
    audit["audit: every call an event"]
  end
  subgraph PLUG["Plugins — own process, own key"]
    m["machines"]
    v["volumes"]
    i["images"]
    more["names · ports · snapshots · object storage · …"]
  end
  subgraph DRV["Drivers — one per engine"]
    pve["Proxmox"]
    inc["Incus"]
    aws["AWS"]
    fake["fake (tests)"]
  end
  WHO --> DOORS --> CORE --> PLUG --> DRV
```

**One binary.** `hangar serve` is the brain; `hangar check` starts every
plugin, holds the config against what they declare and stops (the service is
its own checker); `hangar token create` makes the first API token on the
brain's host, before any identity provider is wired; `hangar plugin <name>`
is a built-in plugin's process — the core starts it itself, as a separate
process with its own credential, exactly as it starts a third party's
program. Being compiled into the same file changes nothing about the walls
between them.

**The doors are generated.** Every type a plugin declares comes with the JSON
Schema of its spec and of each action's params; `GET /v1/types` serves them,
and the command line's flags and the console's forms are drawn from them — a
new plugin appears in both without a line of their code changing. *(Designed:
neither door exists yet; the API they will read does.)*

## 3. The core — what never changes when a service is added **(built)**

| piece | what it does | built with |
|---|---|---|
| **Identity** | a bearer token signed by the operator's OIDC provider for the configured client id (its `groups` claim maps a person to a **tier**), or an API token `hgr_…` for automation — only its hash kept, expiring, read-only or read-write, carrying its owner's groups as they were when it was made; **a token cannot make tokens**. The provider is reached on the first token, never at start-up | `coreos/go-oidc` |
| **Tiers and limits** | a tier = a line of limits per plugin dimension (§7); the **first** tier in the file whose groups a person is in is theirs; a dimension a tier does not name is allowed **nothing**; the core counts usage and refuses what exceeds it, **with the numbers** | data (YAML) |
| **Registry** | every resource: an ID in AWS's style (`box-0123456789abcdef0`: a prefix, seventeen hex digits), type, owner, zone, state, desired spec, observed state, tags, what it holds per dimension, relations; a deleted resource keeps its row | SQLite (WAL, pure Go) |
| **The API** | REST + JSON, OpenAPI 3.1, spec first; **client tokens** on every create, delete and action (AWS's `ClientToken`: a retry returns the first operation, a reused token for another request is refused); long actions return an **operation** to poll or wait on; every refusal an RFC 9457 problem | `net/http` |
| **Operations + reconcile** | an operation is written before the plugin is called and ended after; one the brain died during is **run again at the next start** (plugins are idempotent on the resource id); a loop compares every settled resource with its engine: in sync, **repaired**, **drifted** (reported) or **lost** (and found again) | the core |
| **Zones** | a zone = an engine connection (driver, endpoint, options); a plugin is enabled per zone and reports what its driver can do there (§5) | data |
| **Audit** | one structured line per API call — who, through which credential, which resource, the result, why refused — plus one per operation's end, per plugin event, per reconcile finding; JSON on stdout, `"kind":"audit"` | `log/slog` |
| **Plugin host** | starts each plugin as its own process with an **empty environment**, a socket directory of its own and mutual TLS; refuses a declaration that collides with another plugin's or names a capability no driver documents; a program at a path can be pinned by its SHA-256; a plugin whose process dies is started again, and must describe itself exactly as before | HashiCorp `go-plugin` (gRPC — the model of Terraform's providers) |

### The ask — one flow for every request

```mermaid
flowchart LR
  ask["someone asks<br/>(command line, console, API)"] --> who{"signed in?<br/>which tier?"}
  who -- no --> r1["401 sign in · 403 no tier"]
  who -- yes --> zone{"zone open to the tier,<br/>able to host the type?"}
  zone -- no --> r0["403 zone · 422 not offered there"]
  zone -- yes --> valid{"valid against the schema?<br/>accepted by the plugin's plan?"}
  valid -- no --> r4["422, field by field"]
  valid -- yes --> lim{"within the tier's limits?"}
  lim -- no --> r2["403, with the numbers:<br/>« 2 of 2 toy.boxes used; this asks for 1 more »"]
  lim -- yes --> room{"room in the zone's pool?<br/>(designed, §6)"}
  room -- no --> r3["refused, with the arithmetic:<br/>« ask for spot, or less »"]
  room -- yes --> make["202: an operation;<br/>the plugin makes it (its own key, its driver)"]
  make --> ready["ready: an ID, audit lines,<br/>the observed state"]
```

**Admission is one transaction.** The plugin's plan says what the request
would hold per dimension; the core counts the owner's usage and writes the
resource in the same write transaction, one admission at a time — two
requests can never both fit in the last free unit. Only **growth** is
admitted (shrinking always fits, even over a limit lowered since), and only a
**changed** choice is checked (a start is never refused because the tier's
list moved). A resource holds what it holds while it is `creating`, `ready`,
`updating`, `deleting` or `lost`; `deleted` and `failed` hold nothing.

## 4. The plugin contract **(built)**

**Everything a person can ask for is a plugin, the first ones included.** The
protocol is [`plugin.proto`](proto/hangar/plugin/v1/plugin.proto); a plugin is
a Go program calling `sdk.Serve` (see [`plugins/toy`](plugins/toy/toy.go),
the one to copy). A plugin declares, and only declares:

| a plugin declares (`Describe`) | what the core does with it |
|---|---|
| **its name**; its dimensions are spelled `<name>.<something>` | refuses a plugin that calls itself by another name than the config enables, or a dimension outside its name |
| **resource types**, each with an ID prefix and a JSON Schema (2020-12) | validates every request against the schema before the plugin sees it (offline — a `$ref` elsewhere is refused, never fetched); serves it to the doors |
| **limit dimensions**: *quantities* (summed: `machines.memory_gb`) and *choices* (a value from a set: `machines.kind`) | counts and checks them per owner and tier; a plan naming an undeclared one is refused |
| **actions** per type, each with a params schema, whether it **changes usage** (planned and admitted), and the **capabilities** it needs | exposes them in the API; offers each only in the zones whose driver has what it needs |
| **what it requires of a driver** (capability flags, §5) | refuses to use it on a zone whose driver lacks them, and says so |
| **its credential** — a description of the one secret it needs per zone | hands it that secret alone, per zone, in `Configure`; no plugin can read another's |
| **events** | writes them to the audit |

and **acts** when asked: `Configure` (open a driver per zone, report its
capabilities) · `Plan` (what a create or an action would hold — no side
effects) · `Create`, `Delete`, `Act` · `Reconcile` (desired against actual).

**Four rules a plugin keeps:** (1) `Create`, `Delete` and `Act` are
**idempotent on the resource id** — the id is minted by the core before the
call, written on the engine object, and a retry finds it again; a delete of
what is gone succeeds; (2) `Plan` has no side effects; (3) an action's params
are absolute (« memory 8 », never « +2 »); (4) refusals are gRPC statuses —
`INVALID_ARGUMENT` (never as written), `FAILED_PRECONDITION` (not in the
engine's present state), `UNAVAILABLE` (the core retries).

**References.** A spec's (or an action's params') top-level property that
names other resources carries `"x-hangar-ref": "<type>"` in its schema — a
string, or an array whose items are marked (the machines plugin's
`key_pairs`). The core checks every id before the plugin is asked: a
resource of that type, **the owner's own** (someone else's reads exactly
like one that does not exist), in the same zone, ready. It records the link
(`relations`: the field's name as its kind) and hands the plugin the
resources themselves in `refs` with the plan, the create or the action — a
plugin never reads the registry, and its plan can refuse what does not fit
what is named (a disk for a container) before anything is admitted. A config
enabling a type that names a type no plugin declares is refused at start.
**Relations follow the spec**: written at the create's admission, added
from an action's planned spec at its admission, and set anew from the spec
as it ends — what the resource names now, nothing it named before.
**(built)**

**Attachments.** A reference whose schema also carries `"x-hangar-attached":
true` is an attachment: on the engine, the resource lives inside the one it
names (a volume plugged into a machine). **Neither end is deleted while it
is** — 409 `attached`, « m-… has vol-… attached: detach it first — it keeps
its data » — because the engine would take the attached one's data along. A
lost resource binds nothing (else a machine the engine lost could never be
let go of). The drivers keep the same rule underneath: a guest holding a
volume refuses its delete. **(built)**

**Adding a plugin = three things:** the plugin (built in, or a program of its
own at a path, pinned by its SHA-256), one config block enabling it on zones,
its credential per zone (from a file or an environment variable of the
core's — never written in the config).

## 5. Drivers — the engines and what they can do

A plugin talks to an engine through a driver (`driver` package: a registry by
name, like `database/sql`'s). Each driver advertises **capability flags**; the
core and the plugins use the flags, never the engine's name. A driver may only
advertise the documented flags.

| capability flag | Proxmox | Incus | AWS | fake |
|---|---|---|---|---|
| `kind.container` | yes (LXC) | yes | no | yes |
| `kind.vm` | yes (QEMU) | yes | yes (EC2) | yes |
| `resize.live.memory_down` | **containers only** | containers | no | yes |
| `resize.live.cpu_cap` | yes (VM `cpulimit`, CT `cores`/`cpulimit`) | yes | no | yes |
| `volume.move_between_guests` | yes (`target-vmid`, both guests on one node) | yes | yes (EBS) | yes |
| `guest.suspend_to_disk` | VMs without a passed-through device | yes | hibernate | yes |
| `guest.tags` | yes | yes (config keys) | yes | yes |
| `hook.pre_start` | yes (hookscript; **a failing one aborts the start**) | no (the core admits instead) | no | yes |
| `gpu.shared` / `gpu.passthrough` | device nodes / PCI mapping | yes / yes | instance types | — |
| `fence.pool` (a credential limited to the product's guests) | yes (a pool + a role) | yes (a project) | yes (IAM + tags) | yes |

**The fake driver is built** and ships first: it lets the whole core and
every plugin be proved with no hypervisor. Its zone's endpoint is empty (in
memory) or a JSON file — and then **the file is the engine**: every call reads
it first, so editing it by hand is changing the engine behind the brain's
back, which is what reconcile exists to catch. Its options narrow its
capabilities (to prove what a zone without one refuses) and can demand a
credential (to prove the plugin received its own).

**The guests facet** (`driver.Guests`: create, find, list, delete, power,
reboot, resize — create idempotent on the core's id) carries a guest's name,
image (the engine's own name for it, per kind), root disk, public keys, user
data, tags, holds and a CPU cap; it reports its node, addresses and what a
running guest holds. **`Traits(kind)`** says what a
guest of one kind takes (user data) and changes while it runs (cores, memory
up, memory down) — finer than a flag, which speaks for the whole engine: on
Proxmox a container changes everything live and a VM only grows its memory.
**The watcher facet** (`driver.Watcher`) reads what a zone's reservations
wait on: a watched guest's power, a node's state, whether the zone is awake.
**The volumes facet** (`driver.Volumes`: create, find, place, resize, set
backup, delete — idempotent on the core's id) makes a volume on a guest or
parked, and places it on another guest or back parked, keeping its data,
size and backup flag; a block volume shows its guest the serial
`vol0123…` (the id without its dash, AWS's form). Where the engine keeps no
disk without a guest the driver parks one on a stopped **shelf** guest of
its owner — the plugin never sees a shelf; `CanPark` says where it cannot.

**The Proxmox VE driver is built** ([docs/proxmox.md](docs/proxmox.md)): one
API token fenced to one pool, `fence.pool` advertised only when the token's
own permissions reach nothing else; containers from a template archive, VMs
cloned from a template found by name and fed their user data on a NoCloud
seed disc the driver writes and uploads; every long call waits for its task
(a refused start is a `200` and a failed task). It advertises `kind.*`,
`guest.tags`, `resize.live.memory_down` (containers, above what they hold),
`resize.live.cpu_cap` (`cpulimit`, live on both kinds), `hook.pre_start` (its
hook, `hangar-hook`, §6), `volume.move_between_guests` (the volumes facet: a
guest's description says which disk is which volume, since a disk is renamed
after each guest it moves to; a volume leaving a running VM rests on its
shelf, where its options are written back; one leaving a running container
is refused — [docs/proxmox.md](docs/proxmox.md#volumes)) and `fence.pool` —
the one thing its token may do outside its pools is read the power of the
guests the zone watches.
*(The Incus and AWS drivers are designed.)*

## 6. Zones, pools, classes, reservations, preemption **(built)**

**A zone** is where machines run: one engine connection, its nodes, one
network (a bridge or vnet, an address range, a gateway, resolvers — the
product's IPAM hands out addresses, *designed*), and its capacity rules, in
the zone's `room`:

```yaml
zones:
  - name: lab
    room:
      memory_gb: 62          # what the product's resources may count on here
      grace: 10m             # see "the claim"
      reservations:
        - {name: host,     memory_gb: 15}                          # always
        - {name: priority, memory_gb: 32, while_running: "4100"}  # a guest of the operator's
        - {name: failover, memory_gb: 10, while_down: node-b}      # a node's guests land here
    wake:                    # a zone that sleeps
      url: https://power.example.com/api/targets/node-c/wake
      body: '{"wait": true}'
      headers: {Authorization: {file: /run/secrets/wake}}
```

- **Reservations** — room kept for someone else, **with a condition**:
  always (none written), `while_running: <guest>`, `while_down: <node>`.
  They are how a zone shares hardware with things more important than it.
- **Two pools follow**: **guaranteed** = the memory less every reservation,
  as if all were in force — what can be *promised*; **spot** = the room the
  conditional reservations keep while none of them is — what can be
  *lent*. Unused guaranteed room is never lent: a promise never waits for
  a borrower to leave. **When one conditional reservation is in force it
  takes the whole spot pool back** — exact for a zone with one, erring on
  the side of the one that needs the room with several (a finer split is a
  flip trigger, not built).
- **What a resource takes** is its plugin's to say, per plan: MiB booked in
  the guaranteed pool while it lives, MiB borrowed from the spot pool while
  it runs, whether it will run. **Admission** is in the same transaction as
  the tier's limits, growth only: a promise beyond the guaranteed pool, or a
  borrower beyond the spot pool, is refused (409 `room`) with the
  arithmetic — « zone lab's spot pool holds 32 GB; 30 GB in use; this asks
  for 4 GB more — ask for less, or when one stops ». **While the spot pool
  is held, a resource runs on what it booked only**: a floor starts on its
  floor, one with nothing booked does not start (« held for priority (while
  guest 4100 runs): ask again when it ends, or for guaranteed room »).
- **Classes** (the machines plugin's): *guaranteed* (all booked, never
  stopped) · *spot* (all borrowed: stopped when the room is needed, started
  again when it returns — unless `resume: false`) · *guaranteed + spot*
  (only where a running guest of its kind gives memory back,
  `resize.live.memory_down`: a floor booked, a top-up that goes — it shrinks
  instead of stopping, never below what it holds, and says what it could
  not give back). `cores_beside` caps the CPU of the ones that keep running.
  **The class, floor, cap and admitted size are written on the guest as
  tags**, so the engine's node can act without the brain.
- **Holds.** While a conditional reservation is in force, the core writes
  its key on every room-taking resource of the zone (`hold` in the protocol)
  and the plugin brings each to what a hold means for it — in one order:
  the tag first (a node reading it sees the hold before its effect), spot
  stopped, floors shrunk, CPU capped; lifted in the reverse order. **A
  reservation's owner is never delayed by a refusal of ours.**
- **What a reservation waits on** is read by surveys (a plugin's `Survey`:
  its driver reads the guest's power — the one thing its credential may
  read outside its fence, and nothing more — or the node's state), at every
  reconcile pass and at start.
- **The claim — the guest's hook phones the brain first.** Before a
  reservation's guest starts, its hook calls `POST /v1/zones/{zone}/claim`
  (a token of the scope `room`, for a tier with `room: true`: it claims and
  releases, and nothing else, not even read); the brain holds the zone
  before it answers, and finishes even if the hook stopped waiting. After
  the guest stops, `release` lifts the holds and starts the spot machines
  meant to run. A claim stands for the zone's `grace` before its guest reads
  running (the start's own time), past it only while it does — **a release
  that never came** (the node lost its power) **ends there** — on a guest
  read not running; one that cannot be read keeps it (a hiccup of the
  engine's API is not a guest gone). **One room decision at a time per
  zone**: a claim or a release, and a pass's survey and holds, never overlap
  — a survey that once came mid-release took the release's own tags, not yet
  taken off, for a node acting alone.
- **The node alone.** When the brain cannot be reached (or refuses the
  hook's token), the hook does on its node what the brain would, from the
  tags alone, and never refuses its guest's start; after the stop it gives
  back what it took. The brain, back, reads the guest's power: running, it
  keeps the holds; the node's hold on a guest not yet running is **adopted**
  as a claim for the grace.
- **Admission on the node, whoever starts.** On an engine with
  `hook.pre_start` the same hook, set on the images' templates and inherited
  by every clone, refuses a start above the size admitted, a spot machine's
  while a priority guest of the node has the room, a floor's above its
  floor then — from the brain, the engine's console, or its command line.
- **Waking.** A zone that sleeps declares a `wake` webhook; before anything
  starts there (a create, a start), the core asks whether it is awake, calls
  the webhook when it is not, and waits for it to answer (`timeout`). A
  reconcile never wakes a zone: while it sleeps its resources are not
  judged.
- **Power — designed:** the core stops each machine idle for its
  `idle_after` (read from the engine's own counters: CPU and network), and
  counts **awake hours** against the owner's tier (a limit per month); a
  machine can be kept awake, at that cost.

## 7. Every plugin — capabilities and limits (the first two **built**)

### The first three

**The machines plugin is built**, as the row below says but for four
things. The key pair type is **`keypair`** (a type's name has the shape of an
id prefix); a key pair is **imported, never generated** (the brain would hold
a private key). **Console** comes with the terminal in the console (it needs
a stream through the core the protocol does not carry yet). **The classes,
`floor_gb`, `cores_beside` and `resume`** are built with §6's room; **awake
hours, keep awake and `idle_after`** come with §6's power; **GPU** and
**`peers`** later. A machine holds its size against its tier while it
exists, running or not; against its zone, as its class says (§6).

**The volumes plugin is built**, as its row says. A volume's **content** is
fixed at birth: `block` (a disk its VM formats itself) or `filesystem` (a
directory its container mounts at `mount`) — filesystem when a mount is
given. It names its machine (`machine`, an attachment, §4); a volume on none
is **parked**, and a filesystem volume keeps its last path for its next
attach. **Backups are a size budget**: `volumes.backup_gb` counts the sizes
of the volumes the engine's backups take — a tier that names no budget backs
up nothing, and no one fills the backup store. A running container lets go
of no volume (stop it first); a running VM does, live. Every action is
planned (the refusals come back before anything is admitted, with the
numbers), and its spec written at its admission.

| plugin | resources | actions | limit dimensions (per tier) | driver needs |
|---|---|---|---|---|
| **machines** | **`m-…`**: name, zone, **kind** (container / VM), **type** (AWS names — `t3.medium` = 2 vCPU / 4 G — or the operator's aliases, or free cores + memory), **image**, **class**, `cores_beside`, `floor` (guaranteed + spot), **user data** (cloud-init), **key pairs** (public keys; `kp-…`), **tags**, `idle_after`, GPU (none / shared / whole), `peers` group | create · start · stop · reboot · resize · console (serial / terminal) · delete · keep awake (costs awake hours) | count · vCPU · memory GB · awake hours a month · kinds allowed · classes allowed · zones allowed · GPU allowed | `kind.*`, `guest.tags`, `resize.live.*` for resize, `hook.pre_start` or core admission |
| **volumes** | **`vol-…`**: size, content (block / filesystem), backup yes/no, the machine it is attached to and its path there, tags | create · attach · detach · **move** (to another machine of the same owner) · resize (grow) · set_backup · delete | count · total GB · **backed-up GB** | `volume.move_between_guests`, `fence.pool`; where the engine keeps no disk without a guest, an unattached volume parks on a stopped **« shelf » guest** of its owner |
| **images** | **`img-…`**: name, family, version, visibility (operator / private / shared with a group), the recipe it came from | list · **bake** (operator: from a recipe) · **save** (a user: from their own stopped machine) · share · retire | own images count · own images GB | a template/clone path per driver (Proxmox: `qm template` + linked or full clones; container templates) |

**Recipes** (for `bake`): a base cloud image + cloud-init or a provisioning
script the operator provides — the product ships generic ones; an operator
adds their own (their agent, their monitoring). Rebuilt on a schedule the
operator sets.

### The next ones (each its own design, then its own release)

| plugin | resources | actions | limits | how it is driven |
|---|---|---|---|---|
| **names** | `name-…`: a host name under a domain the operator allows, pointing at a machine (A/AAAA or a reverse-proxy route) | request · point · release | names per tier · domains allowed | a DNS driver (an API-backed zone) + optionally a reverse-proxy driver for HTTPS |
| **ports** | `port-…`: a protocol + port on the operator's public address, forwarded to a machine, **for one source address at a time**, for a duration | open · extend · close | open ports per tier · hours per lease · protocols allowed | a firewall/NAT driver that keeps leases (e.g. [Le Videur](https://github.com/tomblancdev/videur)) |
| **snapshots** | `snap-…` of a machine or a volume | take · restore · delete | count · GB | engine snapshots |
| **object storage** | `bucket-…`, S3-compatible keys | create · keys · delete | buckets · GB | an S3-compatible server driver |
| **databases** | `db-…`: engine + version, a machine underneath | create · credentials · backup · delete | count · size | built on the machines plugin |
| **machines on AWS** | the machines plugin with the **AWS driver** | as machines | **money a month** per tier (the one money limit) | the AWS driver; never spends unasked |

## 8. The security model — five walls

| wall | what holds it | status |
|---|---|---|
| **1. The door** | OIDC sign-in (the operator's provider and its MFA); API tokens scoped (read / write) and expiring, never able to make tokens; the brain behind the operator's gateway (it speaks plain HTTP; TLS is the gateway's) | **built** |
| **2. The core and its plugins** | limits per tier; every call audited; **each plugin its own process, started with an empty environment, with its own credential and nothing else**; mutual TLS on its socket; a program pinned by its SHA-256; the API never returns an engine credential | **built** — the empty environment and the one-credential rule are proved by tests that run a probe plugin and read what it received |
| **3. The engine fence** | each driver's credential fenced to the product's own guests (`fence.pool`): on Proxmox a pool and a role — it cannot touch any other guest, and sees only the power of those a zone's reservations name | **built** for Proxmox: the driver reads the token's own permissions and advertises `fence.pool` only when nothing outside its pools is reachable but `VM.Audit` on a watched guest (root's token, the control, is refused with 416 reasons); the machines plugin requires it, so an unfenced zone is not one it acts on. The hook on the node needs no hypervisor credential at all: it is the node's own root, and holds only a `room` token toward the brain |
| **4. The network** | a zone's network is the operator's: the product assumes a lane where machines reach only what the operator allows, and each machine is alone on it unless two share a `peers` group | designed |
| **5. The machine** | untrusted users get **VMs** (their own kernel); containers are for trusted operators | designed (a tier's `kind` choice limit already enforces it) |

**What it cannot promise:** a hypervisor flaw lets a VM reach its host — so
**untrusted users belong on hosts that run nothing else of value**; the
brain holds the plugins' credentials — so it must never face the internet
directly, and each credential must stay fenced.

## 9. Decisions, and what was set aside

| decision | why |
|---|---|
| A control-plane product with its own database; what people create is its data, never a line in somebody's git | users must never write the operator's infrastructure repository |
| Drivers with capability flags | the engine is a choice; a plugin is written once |
| Plugins in their own processes, each with its own credential; the doors generated from their schemas | a new service costs no core change; a flawed one harms no other |
| `POST /v1/resources` with the type in the body (not `/v1/{type}`) | one collection, listed and filtered the same way it is created in; the type is data the catalogue serves |
| One binary that runs its built-in plugins as separate processes | one image, one build — and the walls between plugins unchanged |
| API tokens made on the brain's host by `hangar token create` | a first operator can start without an identity provider, and a lost provider is not a lost brain |
| A resource attached to another stops the delete of either (409 `attached`), rather than going with it or being parked by the core | nothing is lost by surprise: on the engine the volume is inside the machine, and a delete would take its data |
| Backups counted as a size per tier (`volumes.backup_gb`), not a yes or no | the backup store is finite: one person allowed backups could fill it |

**Set aside:** an EC2 API clone (nothing maintained speaks it for the engines
this targets — OpenStack's EC2 layer, CloudStack's `ec2stack`, Eucalyptus and
LocalStack are archived or retired); making Incus the only engine (it would
replace a host's hypervisor rather than drive it — it is a driver instead);
the heavy cloud stacks (OpenStack, OpenNebula, CloudStack) that own their
hosts.
