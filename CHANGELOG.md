# Changelog

## Unreleased — la place

A zone's room: what can be promised, what is only lent, and the guests that
matter more than yours — proved on the repo's throwaway Proxmox VE, with the
hook running on its node.

- **Zones count room** (`room` in a zone): its memory, and reservations kept
  for someone else — always, `while_running` a guest of the operator's, or
  `while_down` a node. Two pools follow: **guaranteed** (the memory less
  every reservation — booked while a resource lives) and **spot** (the room
  the conditional reservations keep while none is in force — lent while it
  runs). Admission is in the limits' transaction: a request beyond a pool is
  refused (409 `room`) with the arithmetic; while the spot pool is held a
  resource runs on what it booked only.
- **Holds**: while a conditional reservation is in force, the core holds
  every room-taking resource of the zone (`hold` in the protocol) and the
  plugin brings each to what that means for it; lifted, it brings them back.
- **The claim**: `POST /v1/zones/{zone}/claim` and `/release` — a guest's
  hook, before it starts and after it stops, with a token of the new scope
  `room` (claims and releases, nothing else) for a tier with `room: true`.
  The brain holds before it answers. A claim stands for the zone's `grace`,
  then only while its guest reads running — or cannot be read: a hiccup is
  not a guest gone; a release that never came ends there. A hold the node
  placed alone is adopted. One room decision at a time per zone: a pass's
  survey never reads a release's own tags mid-way (read on the bench).
- **Surveys** (the protocol's `Survey`, the drivers' `Watcher` facet): what
  a reservation waits on, which resources carry a hold, whether the zone is
  awake — at start and at every reconcile pass.
- **Waking**: a zone's `wake` webhook, called before anything starts in it
  when it reads asleep, then waited for; a reconcile never wakes a zone, and
  does not judge its resources while it sleeps.
- **The machines plugin**: classes `guaranteed`, `spot`, `guaranteed+spot`
  (`floor_gb`: it shrinks to its floor instead of stopping, never below what
  it holds, and says what it could not give back — only where a running
  guest of its kind gives memory back); `cores_beside` (the CPU cap while
  held); `resume` (a spot machine starts again when the room returns, or
  stays stopped); start and stop are planned (they change the room). The
  class, admitted size, floor and cap are written on the guest as tags.
- **The Proxmox VE driver**: `resize.live.cpu_cap` (`cpulimit`) and
  `hook.pre_start`; the fence accepts `VM.Audit` — alone — on the guests a
  zone watches; a node reads down only when the cluster's membership says
  `offline` (`unknown` cannot tell), and a zone awake when its node answers.
- **`hangar-hook`**, a second binary (standard library only): a guest's
  hookscript on the node. On a product machine, the node's own admission
  (set on the VM templates, inherited by every clone); on a priority guest,
  the claim — the brain first, the node alone from the tags when it cannot be
  reached — never refusing that guest's start.
- **The bench** gains a priority guest (VM 100) and the watch grant on it.

## Unreleased — les machines

The first plugin that makes real machines, and the first real engine:
proved on a throwaway Proxmox VE installed by the repo's own bench.

- **The machines plugin** (`machines`): `machine` (`m-…`) — a container or a
  VM, sized by AWS's type names (`t3.micro` … `r5.2xlarge`), the operator's
  aliases, or cores and memory; started from an image the operator names per
  kind; a class (`guaranteed`, `spot`) written on the guest as a tag;
  cloud-init user data where the kind boots it; start, stop, reboot, resize
  (a running machine changes only what its kind can change live), delete,
  reconcile. `keypair` (`kp-…`) — a public key, imported, never generated
  (the brain holds no private key). Dimensions: count, vCPU, memory, disk,
  key pairs; kinds and classes as choices. It requires `fence.pool`.
- **The Proxmox VE driver** (`proxmox`), its own small client on the
  standard library: one API token fenced to one pool (the fence read from the
  token's own permissions — a token that reaches further opens, and is not
  called fenced); containers from a template archive, VMs cloned from a
  template and fed their first boot by a NoCloud seed disc the driver writes
  (a small ISO 9660 + Joliet writer) and uploads; every long call waits for
  its task, so a start a hook refused returns the hook's own words. The least
  the token needs, and why each grant: [docs/proxmox.md](docs/proxmox.md).
- **References between resources:** a schema property marked
  `"x-hangar-ref": "<type>"` names other resources by id; the core checks each
  is the owner's own, in the same zone and ready (someone else's reads as one
  that does not exist), records the relation, and hands the plugin the
  resources with the create or the action (`refs` in the protocol).
- **The driver contract grows:** a guest's name, image, root disk, public
  keys, user data, node and addresses; `Reboot`; `Traits` (what a guest of a
  kind can take, and change while it runs — finer than a capability flag).
- **The bench** (`tools/bench/`): `bench.sh up` installs Proxmox VE unattended
  in a VM (podman + `/dev/kvm`, nothing else on the host) and prepares it as
  an operator would; the driver's tests and the binary's end-to-end test run
  against it when its variables are set, and skip otherwise.

## la naissance

The product is born: the core's skeleton, proved end to end on a fake engine.
No release is cut; the first tag comes with the first plugin that makes a real
machine.

- **The core:** identity (OIDC bearer tokens; API tokens `hgr_…`, hashed,
  expiring, read-only or read-write, unable to make tokens), tiers and limits
  (quantities and choices, the first matching tier wins, an unnamed dimension
  allows nothing, every refusal with its numbers), the registry (SQLite,
  AWS-style ids, tags, usage, relations), operations (client tokens, waiting,
  resumed after a crash), reconcile (in sync, repaired, drifted, lost and
  found), the audit (one line per call), the plugin host.
- **The contracts:** the API as OpenAPI 3.1 (`api/openapi.yaml`, served at
  `/openapi.json`, held to the routes by a test); the plugin protocol
  (`proto/`, gRPC over HashiCorp's go-plugin) and its SDK.
- **The walls, proved:** a plugin starts with an empty environment and
  receives only its own credential, per zone — tests run a probe plugin and
  read what it was given (and fail when the wall is removed).
- **The fake driver** (in memory, or a JSON file that *is* the engine) and
  **the toy plugin** (`box`: create, delete, start, stop, resize, suspend,
  reconcile) — the example to copy.
- One static binary: `hangar serve | check | token | plugin | version`.
