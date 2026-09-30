# Changelog

## Unreleased — les images

System disks machines are born from — proved on the repo's throwaway Proxmox
VE, through the binary's API, the machines and images plugins each on its
own token.

- **The images plugin** (`images`): `image` (`img-…`) — **baked** from a
  recipe of the operator's (a base on the engine per kind, a disk size, the
  builder's cores, memory and time, a `#cloud-config` or `#!` first boot), or
  **saved** from a stopped machine of the owner's (its system disk alone: a
  machine with a volume plugged in is refused). Actions: `share`, `retire`
  (no machine born from it any more; those born from it run on), `rebake` (a
  failed bake again, from the recipe as it is now). Limits: `images.count`,
  `images.size_gb`, and two choices — `images.source` (recipe, machine) and
  `images.visibility` (private, shared, public). A recipe is copied into each
  image it bakes (`spec.from`).
- **A bake is not one call**: the create answers `pending`, the brain's
  reconcile moves it forward — `available`, or `failed` with the words of the
  builder's first boot. The builder **borrows spot room** (it wakes a sleeping
  zone, is refused while the room is held) and **is let go at once** when the
  room is needed: its bake **starts over by itself** when the room is back,
  never counted as a failure. A failure of the recipe is never retried
  blindly; a builder that stops unasked three times fails its bake.
- **Machines born from an image** (`image_id`, beside the operator's names in
  `image`): one's own or one shared with one, available, not retired; the
  disk at least the image's.
- **Shares** in the core (`"x-hangar-share": true` on a spec's array of
  groups, `*` = everyone): a person in one of the groups sees the resource
  (get, and the listing now holds one's own and what is shared with one's
  groups) and names it in their requests (a reference, never an attachment);
  only its owner or an operator changes or deletes it (403 `shared`). A
  person shares only with groups they are in, or with everyone as their tier
  allows. Shares follow the spec, like relations (registry schema 3).
- **A plugin sees a resource's room** (`Resource.room` in the protocol), and a
  resource whose room changes on its own returns it from reconcile.
- **A reference to a type no enabled plugin declares** no longer stops the
  start: it is logged, and refused at the request.
- **The drivers' images facet** (`driver.Images`), on the fake engine and on
  Proxmox VE: VM templates in the images pool, named after their id and
  tagged with nothing (read on the bench: a clone copies its template's
  tags, and a machine born with an image's id was not found as itself). A
  bake's builder is imported from a disk image (`import-from`, which a fenced
  token may do with `Datastore.Audit` on its storage) or copied from a
  template, boots the recipe then the driver's last step (a unit after
  `cloud-final`: cloud-init's verdict, the disk made ready to be cloned —
  cloud-init's memory, the machine id, the host keys cleared — the verdict
  said by the host name), read through the guest agent; a failure's log read
  with `VM.GuestAgent.FileRead`. A template machines were born from as
  linked clones refuses its delete, naming them. **A new guest's name and a
  template's flag are read in its config** — `/cluster/resources` gives them
  a pvestatd pass late (read on the bench: a save lost its own clone, a
  machine asked for right after it found no template).
- **The bench** gains the images plugin's user, roles and token
  (`HANGAR_BENCH_IMAGES_TOKEN_FILE`).

## Unreleased — les volumes

Disks that belong to their owner rather than to a machine — proved on the
repo's throwaway Proxmox VE, through the binary's API, with both plugins
running, each on its own token.

- **The volumes plugin** (`volumes`): `volume` (`vol-…`) — a size, a content
  fixed at birth (`block`: a disk its VM formats itself; `filesystem`: a
  directory its container mounts at a path), a backup flag, and the machine
  it is plugged into, or none (parked). Actions: `attach`, `detach`, `move`
  (to another machine of the same owner), `resize` (it only grows),
  `set_backup`. Limits: `volumes.count`, `volumes.size_gb`, and
  **`volumes.backup_gb`** — backups are a size budget: a tier that names none
  backs up nothing.
- **Attachments** (`"x-hangar-attached": true` beside `x-hangar-ref`): the
  resource lives inside the one it names on the engine; **neither end is
  deleted while it is** (409 `attached`, naming what to detach). A lost
  resource binds nothing.
- **Relations follow the spec**: added from an action's planned spec at its
  admission, set anew from the spec as it ends (and when reconcile moves a
  spec). **A plan sees what the request names** (`refs` in `PlanRequest`),
  so a disk for a container is refused before anything is admitted.
- **The drivers' volumes facet** (`driver.Volumes`), on the fake engine and
  on Proxmox VE (`volume.move_between_guests`): a volume is always a line of
  some guest's config — a machine's, or a **shelf**'s (a stopped guest per
  owner and kind the driver makes and never starts; containers' from the
  zone's new `shelf_archive`), so a parked volume keeps its backup. Which
  disk is which volume is written in the guest's description, target first
  and source last, so a move cut anywhere is found and finished (a disk is
  renamed after each guest it moves to). A block volume shows its guest the
  serial `vol0123…` (AWS's form). Read on the bench: a disk leaves a running
  VM only unplugged, its options dropped — so it rests on its shelf, where
  they are written back, and reaches the next VM hot-plugged with them; a
  running container lets go of none (refused, in words); a shelf container
  is made without a host name (pve-container counts it as network).
- **A guest holding a volume refuses its delete** in both drivers, under the
  core's own refusal.
- Two processes pick guest ids from one range: a create whose id was just
  taken takes the next.
- **The bench** gains the volumes plugin's user, role and token
  (`HANGAR_BENCH_VOLUMES_TOKEN_FILE`), and its command runs one package at
  a time (`go test -p 1`): two packages' tests drive the bench's one
  priority guest.

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
