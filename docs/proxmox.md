# The Proxmox VE driver

The `proxmox` driver lets hangar make machines on a Proxmox VE cluster
through its HTTP API, with **one API token fenced to one pool**. It runs
nothing on the nodes and needs no root: what it can touch is what the token
may touch, and the plugin refuses a zone whose token reaches further (the
`fence.pool` capability, read from the token's own permissions).

Everything below was read on a live Proxmox VE 9.2 (the bench,
[`tools/bench`](../tools/bench/)), and each grant carries the reason it is
there.

## What the operator prepares, once

| thing | why |
|---|---|
| a **pool** for the machines (`hangar`) | the fence: the driver makes every guest in it, and sees nothing outside it |
| a **pool** for the images (`hangar-images`), holding VM templates | VMs are clones of a template found **by name** there (the image's `vm:` form) |
| container template archives on a storage (`vztmpl`) | containers are created from an archive (the image's `container:` form, a volume id) |
| a **storage of its own for seed discs**: a `dir` storage with content `iso` only (`hangar-seeds`) | a VM's first boot reads its user data from a small disc the driver uploads — see below |
| a bridge or an SDN vnet for the guests, and a VMID range nobody else uses | the zone's `bridge`, `vlan` and `vmids` options |
| a user and its **privilege-separated API token** | the plugin's one credential, `user@realm!name=secret` |
| for the volumes plugin: **its own** user and token, narrower (below), and the zone's `shelf_archive` — a container archive | its credential; a container's volume parked on no machine rests on a stopped container made from that archive — see [Volumes](#volumes) |
| for the images plugin: **its own** user and token (below), and the base disk images its recipes start from on an `import` storage (Debian's `genericcloud` qcow2, say) | its credential; a bake imports the base into its builder — see [Images](#images) |
| **the hook** (`hangar-hook`, a build of this repo) on a storage with content `snippets`, set as root on each VM template of the images pool and on each guest a zone keeps room for | the node's hand: it makes room when a priority guest starts, and admits every start of a machine — see [The hook](#the-hook) |

## The least the token needs

| path | privileges | why |
|---|---|---|
| `/pool/<machines pool>` | `VM.Allocate`, `VM.Audit`, `VM.Clone`, `VM.Config.CDROM`, `VM.Config.CPU`, `VM.Config.Cloudinit`, `VM.Config.Disk`, `VM.Config.HWType`, `VM.Config.Memory`, `VM.Config.Network`, `VM.Config.Options`, `VM.PowerMgmt`, `VM.GuestAgent.Audit`, `Datastore.AllocateSpace`, `Datastore.Audit`, **`Pool.Audit`** | making and running its guests. `Pool.Audit` looks optional and is not: without it `/cluster/resources` **leaves out every guest's pool** (`API2/Cluster.pm`), and the driver could not tell its guests from anyone's |
| `/pool/<images pool>` | `VM.Audit`, `VM.Clone`, `Pool.Audit` | reading and cloning the templates — never changing them |
| `/storage/<disks>` | `Datastore.AllocateSpace`, `Datastore.Audit` | the guests' disks |
| `/storage/<where the archives are>` | `Datastore.Audit` | `pct create` reads the archive (it asks for `Datastore.AllocateSpace` **or** `Datastore.Audit` there) |
| `/storage/<seed storage>` | `Datastore.Allocate`, `Datastore.AllocateTemplate`, `Datastore.Audit` | uploading a VM's seed disc takes `AllocateTemplate`; **deleting it takes `Datastore.Allocate`**, which on a shared storage would reach every ISO, template and backup there — hence a storage that holds seed discs and nothing else |
| `/sdn/zones/<zone>/<vnet>` (with SDN) | `SDN.Use` | attaching guests to the vnet |
| `/vms/<guest>` of each guest a zone's reservation waits on (`while_running`) | **`VM.Audit` and nothing else** | reading its power, to know when the room is in force. The fence accepts exactly this outside the pools — on the guests the zone names, and no other privilege there; anything more and the zone is not fenced. The token still cannot start, stop or change it (read live: `VM.PowerMgmt` refused) |

**The volumes plugin's token** — its own, narrower: disks, and the shelves
it makes, in the same pool; nothing of a guest's power, network or seed:

| path | privileges | why |
|---|---|---|
| `/pool/<machines pool>` | `VM.Allocate`, `VM.Audit`, `VM.Config.Disk`, `VM.Config.Options`, `Datastore.AllocateSpace`, `Datastore.Audit`, `Pool.Audit` | a volume's disk on a machine of the pool (`Config.Disk`, on both guests of a move — `target-vmid` asks it of each), the description line that says which disk is which volume (`Config.Options`), the shelves (`Allocate`) |
| `/storage/<disks>` | `Datastore.AllocateSpace`, `Datastore.Audit` | the volumes themselves |
| `/storage/<where the archives are>` | `Datastore.Audit` | a container shelf is made from the zone's `shelf_archive` |

**The images plugin's token** — its own: templates and the builders that
bake them in the images pool; in the machines' pool, what a save and a
refused delete read, nothing of their power or config:

| path | privileges | why |
|---|---|---|
| `/pool/<images pool>` | `VM.Allocate`, `VM.Audit`, `VM.Clone`, `VM.Config.CDROM`, `VM.Config.CPU`, `VM.Config.Disk`, `VM.Config.HWType`, `VM.Config.Memory`, `VM.Config.Network`, `VM.Config.Options`, `VM.PowerMgmt`, `VM.GuestAgent.Audit`, **`VM.GuestAgent.FileRead`**, `Datastore.AllocateSpace`, `Datastore.Audit`, `Pool.Audit` | a builder made (from a disk image, or a template copied whole), started, shut down, made a template (`VM.Allocate`: `POST …/template`); its host name read through the guest agent (`Audit`) — how its last step says done or failed — and one file, its first boot's log, when it failed (`FileRead`, a privilege of its own since Proxmox VE 9; nothing is ever written or run through the agent) |
| `/pool/<machines pool>` | `VM.Audit`, `VM.Clone`, `Pool.Audit` | a save: a stopped machine read (its volumes: none may go with it) and cloned whole into the images pool (`VM.Clone` on the source, `VM.Allocate` on the target pool); a refused delete names the machines whose disk is a linked clone of the image's |
| `/storage/<disks>` | `Datastore.AllocateSpace`, `Datastore.Audit` | the builders' and templates' disks |
| `/storage/<where the base disk images are>` | `Datastore.Audit` | `import-from` a disk image of content `import` asks `Datastore.AllocateSpace` **or** `Datastore.Audit` on its storage (`PVE::Storage::check_volume_access`) — read, not written |
| `/storage/<seed storage>` | `Datastore.Allocate`, `Datastore.AllocateTemplate`, `Datastore.Audit` | the builder's first boot arrives on a seed disc, as a VM's does |
| `/sdn/zones/<zone>/<vnet>` (with SDN) | `SDN.Use` | a builder's first boot fetches its packages |

A shelf container is made **without a host name**: pve-container counts
`hostname` as network (`VM.Config.Network`, read live and in
`check_ct_modify_config_perm`), which this token does not hold and a shelf
does not need.

Give the same ACLs to the user and to its token (a separated token has the
intersection of both). Nothing on `/`, `/vms` or `/nodes`: the driver reads
its guests' status through the pool, a node's state from the cluster's own
list, and whether its node is awake from `/nodes/<node>/version` — all open to
any token.

**What the token cannot do, by Proxmox's own rules** — and so the driver
does not try: attach a hookscript (`root@pam` only, whatever the privileges);
set a container's feature flags other than `nesting`; pass a device through.

## How it behaves

- **A guest carries the core's id** as the tag `hangar-id.<id>`, and from
  birth in its description (`made by hangar: <id>`). The description is
  there because **a pool-fenced token cannot tag a guest in the call that
  makes it**: Proxmox checks a tag change on `/vms/<vmid>`
  (`GuestHelpers.pm`, `assert_tag_permissions`), which is not in the pool
  until the guest exists — so the driver creates, then tags, and a create
  cut in between is found again by its description.
- **Other tags are `key.value`**, the contract with the node's hook: the
  machine's `class` (`spot`, `guaranteed`, `guaranteed+spot`), the memory
  `admitted` (MiB), its `floor` (MiB) and its cap `beside` (cores) where it
  has them, and `held.<key>` for each reservation holding its room back
  (`held.4100` while guest 4100 has it). A grown size is written before it
  takes effect and a shrunk one after, so the node never reads a size
  smaller than the machine's.
- **A hold's order**: the tag first — a node that reads it sees the hold
  before its effect — then a spot machine stopped, a floor shrunk (never
  below what the container holds plus 64 MB: it says what it could not give
  back), the CPU capped (`cpulimit`, live on both kinds); lifting it,
  regrown and uncapped first, the tag last, a spot machine started again
  unless it asked not to.
- **VMIDs** are the lowest free in the zone's range, the cluster asked about
  each candidate (`/cluster/nextid?vmid=`), since the fence hides guests
  that hold some. Two plugins on a zone pick at the same moment (a bake's
  builder beside a machine): the one refused with « already exists » takes
  the next.
- **A guest just made is addressed by the VMID it took**, never looked up:
  `/cluster/resources` may not list it for a pvestatd pass (read on the bench
  — a machine asked for a second after a bake began was not found by its own
  create, and left behind); **a delete that finds nothing looks once more**
  after that pass, before it calls the guest gone — the core clears a create
  that failed half-way at once.
- **Every long call waits for its task.** Proxmox answers a start with
  `200` and a task id at once; a pre-start hook that refuses fails the
  *task*. The driver reads the task's end and returns the refusal with the
  hook's own words.
- **A refusal's words, not its code**: the same refusal comes back as a
  `403` from one kind of guest and a `500` from the other.
- **Power is read live** (`status/current`), never from `/cluster/resources`,
  which trails it by a pvestatd pass.
- **Containers** are created from the archive with the owner's public keys
  (`ssh-public-keys`), unprivileged, `nesting=1`, swap 0. They take **no user
  data** (no cloud-init boots in them here). Running, they change cores and
  memory at once — memory down only **above what the container holds** plus
  64 MB: a limit written below it makes the kernel kill inside it, init
  included, and on ZFS the file cache is not in the container's group to be
  reclaimed.
- **VMs** are clones of the template (linked beside it, full elsewhere or
  with `full_clone`), with `numa` and memory hotplug on. Their **user data**
  arrives on a NoCloud seed disc the driver writes and uploads (an ISO
  labelled `cidata`: `meta-data` with the id, the host name and the public
  keys, `user-data` as given) — Proxmox's own cloud-init drive takes user
  data only from a snippets file, which its API cannot write, while it does
  take an uploaded ISO. Running, a VM's memory grows (a DIMM hot-plugged);
  its cores and a shrink wait for a stop. Addresses are read through the
  QEMU guest agent when the image runs one.
- **Delete** stops the guest, destroys it with `purge` and its unreferenced
  disks, then deletes its seed disc — **unless it holds a volume**: then it
  is refused, naming them (the core refuses it first; this is the engine's
  own guard).

## Idleness and hours

Both are read from the node itself, with the `VM.Audit` the token already
has on its pool — nothing is installed in a guest.

- **Since when a guest runs** is its `uptime` in `status/current`: the age of
  its own process (a VM's QEMU, a container's init), the same whichever API
  worker answers. A machine's hours are counted from it, once per core.
- **Whether it is idle** is read from the history the node keeps of every
  guest by itself: `GET /nodes/<node>/<kind>/<vmid>/rrddata`. On 9.2 it
  answers **one sample a minute** for the last hour (`timeframe=hour`, 60
  samples) and for the last day (`timeframe=day`, 1439); a week is
  half-hours — which is why an `idle_after` goes up to twelve hours. A sample
  is its minute's average, stamped with the minute's end: `cpu` as a share of
  the guest's cores (× `maxcpu` = cores' worth), `netout` in bytes a second.
  It is there within seconds of its minute's end. **A minute the guest did
  not run whole has no sample** — so a guest just started has no history, and
  is never idle before its first whole minutes.
- **The history is kept by VMID, and outlives the guest**
  (`/var/lib/rrdcached/db/pve-vm-9.0/<vmid>`, one file a number, never removed
  with the guest): a guest made on the number of one deleted a minute before
  finds that guest's samples under its own name. Read on the bench — a
  container 22 seconds old read quiet for five minutes, and was stopped for
  it. **Nothing older than the guest's own start is read** (its `uptime`
  says when that was), and the machines plugin never counts a machine quiet
  for longer than it has run.
- The driver walks the history back from the newest sample, for as long as
  each minute is under the thresholds, follows the next, and began after the
  guest started: a busy minute, a hole or the guest's start ends the walk. A newest sample older than five minutes is a history
  no longer written (the node's statistics daemon down): it says nothing, and
  nothing is stopped on it.
- **`status/current`'s own `cpu` is not used**: it is a rate since the same
  API worker last looked at that guest — three calls in a row answered `0`
  for a VM the node's statistics saw at 0.97.
- Read on a Debian 13 guest: an idle VM sits at **0.008 cores** and sends
  nothing, an idle container at 0 and 1 to 3 bytes a second (its DHCP and
  ARP); twenty seconds of one busy core read **0.33** in their minute; one
  ping a second, **50 to 100 bytes a second** sent. The defaults — 0.05
  cores' worth, 20 bytes a second sent — sit between; a guest that ships
  logs or metrics by itself sends more, and its operator raises `idle.sent_bps`
  (the machines plugin's settings) to what an idle one of theirs reads.

## Volumes

Proxmox VE keeps **no disk without a guest**: a disk is a line of a guest's
config, named after that guest (`vm-<vmid>-disk-N`, `subvol-<vmid>-disk-N`),
and **renamed when it moves** to another (`target-vmid`, qemu-server's
`move_disk` and pve-container's `move_volume`). So:

- **A volume is always a line of some guest's config** — a machine's, or a
  **shelf**'s: a guest of the pool the driver makes for one owner and one
  kind (named after a hash of the owner), never starts, and never tags
  `hangar-id` — no plugin and no hook takes it for a machine. A **VM shelf**
  has no disk of its own and takes block volumes; a **container shelf** is
  made from the zone's `shelf_archive` (1 GB, about 300 MB used for a Debian
  archive) and takes filesystem volumes. A stopped guest's disks and mount
  points go into vzdump's backups by their own `backup` flag, so **a parked
  volume keeps its backup**.
- **Which line is which volume** is written in that guest's description, one
  line each: `hangar volume <id> <key> <volid>` — the volid `-` while a move
  that will name it is on its way. A move writes the target's line first and
  takes the source's off after; a cut anywhere leaves the volume findable
  (by its volid wherever it went in its guest, else by a waiting line's key)
  and the next placement finishes it. (`qm config` and `pct config` print the
  description url-encoded; the API gives it as written.)
- **A block volume shows its guest the serial `vol0123…`** — the id without
  its dash, AWS's own form, all a drive's 20-byte serial holds:
  `/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_vol0123…`. Read in QEMU's
  monitor (`qom-get … serial`) on a hot-plugged disk.
- **A disk moves with its options only from a stopped guest.** Read on a
  throwaway, then in the source: a running VM lets go of a disk only by
  unplugging it (it becomes `unusedN`, its options dropped — the backup flag
  and the serial with them); a running container lets go of none (*cannot
  move in-use volume while the source CT is running*), and a container's
  unused volume reaches another only as unused. So **a volume leaving a
  running VM rests on its shelf**, its options written back there, and goes
  on from there — into a running VM hot-plugged with them; **one leaving a
  running container is refused**, in words. Into a running container it is
  hot-mounted (init untouched); a container's path travels on its line, so
  it is set where the volume is stopped, before the move.
- **Both guests of a move are on one node** (Proxmox's rule for
  `target-vmid`); a zone's guests all are.
- **Grow** is live on both kinds (`resize`); **shrink** never. **The backup
  flag** changes at once on a VM (a change Proxmox would leave pending is
  taken back and said), and only while stopped on a container (a mount
  point's options wait for its next stop — read on a throwaway).
- **Delete** is of a parked volume only: its line on the shelf deleted
  (it becomes `unusedN`), then the unused disk (destroyed).
- The two plugins pick guest ids from one range, each in its own process: a
  create that finds its id taken (*already exists*) takes the next.

## Images

An image is a **VM template in the images pool, named after its id** — a
machine born from it is a clone of it, linked beside it on the same storage
(or full, with `full_clone`), as it is of any template there. **It carries
no tag**: a clone copies its template's tags, and a machine born with an
image's `hangar-id` would not be found as itself (hit on the bench, the first
run). What it is lives in its description: its marker, and for a baked one
the bake's lines.

**A bake** is a builder, a VM made in the images pool from the recipe's base
— a disk image imported from an `import` storage (`import-from`), or a
template of the pool copied whole (an image of the product's included: a
recipe layers on another's result) — sized as the recipe says, tagged
`hangar-id` and `class.spot` while it works (a node making room for a
priority guest without the brain stops it, as a spot machine), its first
boot on a seed disc. Its user data is **a MIME multipart**: the recipe's
part untouched (a `#cloud-config` stays one, a script stays a script), then
the driver's own **last step**, a script queuing a unit ordered after
`cloud-final`. When cloud-init has finished, that unit reads its verdict
(`cloud-init status --wait`: 1 is an error, 2 only recoverable), keeps
`cloud-init status --long` and the end of `cloud-init-output.log` in
`/run/hangar-bake.log`, starts the guest agent — and:

- **clean**: clears what must differ between clones — `cloud-init clean
  --logs --seed --machine-id`, the ssh host keys, the random seed, the
  package cache, the journal — and sets the host name `hangar-bake-done`;
- **errors**: sets the host name `hangar-bake-failed`.

The driver reads the host name through the guest agent (`get-host-name`,
`VM.GuestAgent.Audit`) at each call: done, it writes `hangar bake <n> done`
in the description (a brain stopped between here and the end finds it done),
shuts the builder down, deletes its seed disc and its tags, and makes it a
template; failed, it reads the log (`file-read`, `VM.GuestAgent.FileRead`)
and destroys the builder. **So a recipe installs `qemu-guest-agent`** — which
is also how the machines born from it report their address; a builder the
agent never answers for fails at its `timeout`, saying so. A builder found
stopped before it finished is destroyed and made again (its first boot
cannot resume). Read on the bench: Debian 13's cloud image, the guest agent
installed, baked in 2 to 4.5 minutes; the machines born from it answer with
their address within a minute of their create on a quiet bench, each with
its own machine id and ssh host keys; a failing recipe comes back with
cloud-init's verdict and the end of its log.

**A new guest or template is read in its config**, never from
`/cluster/resources` alone, whose name (the placeholder `VM <vmid>`, not
the real one) and `template` flag trail a clone or a conversion by a
pvestatd pass (read on the bench, twice: a save found
nothing it had just cloned, and a machine asked for right after the save
found no template) — the lag already read for power. A save finishes the
clone by the VMID it took.

**A save** is a full clone of a stopped machine into the images pool (its
seed disc, unused disks and tags dropped), made a template: a machine holding
a volume is refused (an image is its system disk alone). **Nothing cleans
it**: a machine saved as it runs gives its clones its machine id and host
keys — clear them in it first (`cloud-init clean --machine-id`, then stop it)
where its clones must differ.

**A delete** of a template machines were born from as linked clones is
refused by Proxmox VE before anything is destroyed (`destroy_vm`: *base
volume … is still in use by linked cloned*); the driver names the machines
whose disk is `base-<vmid>-disk-…/…`.

**The hook on images.** Only `root@pam` attaches a hookscript, so a template
the product bakes or saves carries none — unless the machine it was saved
from had one — and its clones are admitted by the brain alone. Where the
node's own admission matters, set the hook on the images pool's templates as
root after each bake (a job of the node's).

**Baked again by a schedule** (ARCHITECTURE.md §3): the brain retires an
older image once newer ones are usable, and deletes it only once no machine
names it — the refusal above (a template with linked clones is not deleted)
stays the net under that rule, and a delete it refuses is asked again at the
schedule's next run.

## The hook

`hangar-hook` is a guest's hookscript: Proxmox VE runs it as root, with the
guest's id and phase, whoever starts the guest — the API, the console, `qm`,
a wake daemon. It needs no hypervisor credential (it is the node's own
root), only a token toward the brain of the scope `room`, which claims and
releases and does nothing else, not even read.

**On a product machine**, before it starts: the node's own admission. It
refuses a start above the memory the brain admitted (« m-… is set to 2048 MB,
and was admitted at 1024 MB: change its size through hangar »), a spot
machine's while a priority guest of the node has the room, a floor's above
its floor then. **Set it once, as root, on each VM template of the images
pool: a clone inherits its template's hookscript** (read live: the fenced
token's clone carries it, the hook runs at the token's start, and the token
cannot take it off — « only root can set 'hookscript' »). Containers are
made from archives, which is how they get their keys at birth, so they carry
no hook: the brain admits them, and the priority guest's hook makes room
around them.

**On a priority guest** (one a reservation waits on, outside the pools; set
it on that guest): before it starts, it phones the brain (`claim`) and waits
while the brain holds the zone; after it stops, `release`. When the brain
cannot be reached — or refuses the token, or does not answer within the
hook's `timeout` — the node does it alone, from the tags: every spot machine
of the node stopped, every floor shrunk, every cap applied, each tagged
first; and gives it back after the stop. It never refuses its guest's start.
A marker under `/run/hangar-hook` says a priority guest of the node is
starting or running, for the admission to read.

```sh
install -m 755 hangar-hook /var/lib/vz/snippets/hangar-hook   # a storage with content snippets
qm set 9000 --hookscript local:snippets/hangar-hook           # each VM template: every clone inherits it
qm set 4100 --hookscript local:snippets/hangar-hook           # each priority guest
hangar token create --subject hook-node-a --groups hooks --scopes room --name hook --ttl 8760h
# /etc/hangar/hook.json — and the token's secret in /etc/hangar/hook.token (0600)
#   {"brain": "https://hangar.example.com", "token_file": "/etc/hangar/hook.token", "zone": "lab",
#    "timeout": "90s", "shutdown_timeout": "30s", "grace": "10m"}
```

It logs to syslog as `hangar-hook` (and to the task's log).

**A node's state, read with care.** `/cluster/resources` says a node is
`online` only while pvestatd's stats are fresh, `offline` only when the
cluster's membership says so, and `unknown` otherwise (`API2Tools.pm`,
`extract_node_stats`) — a lone node, or one too busy to report, reads
`unknown` (read on the bench, a loaded nested node, the same afternoon it
read `online`). So a reservation `while_down` counts a node down only on
`offline`, and a zone is awake when its node answers `/nodes/<node>/version`
— never on its line in the list.

## Zone options

```yaml
zones:
  - name: lab
    driver: proxmox
    endpoint: https://pve.example.com:8006
    options:
      node: node-a                 # where guests are made
      pool: hangar                 # the fence
      images_pool: hangar-images   # where VM templates are found by name
      storage: local-zfs           # the guests' disks
      seed_storage: hangar-seeds   # VMs' seed discs, a storage of its own
      bridge: vnet1                # a bridge or an SDN vnet
      vlan: "30"                   # optional
      vmids: 11000-11099
      shutdown_timeout: "60"       # seconds a guest is asked before it is made to stop
      shelf_archive: local:vztmpl/debian-13-standard_13.6-1_amd64.tar.zst   # container shelves (volumes)
      ca_file: /etc/hangar/pve-root-ca.pem   # or fingerprint: <sha256 of the API's certificate>
    room:
      memory_gb: 62
      reservations:
        - {name: host, memory_gb: 15}
        - {name: priority, memory_gb: 32, while_running: "4100"}   # VM.Audit on /vms/4100
plugins:
  - name: machines
    builtin: machines
    zones: [lab]
    credentials:
      lab: {file: /run/secrets/pve-token}  # user@realm!name=secret
    settings:
      images:
        debian-13:
          vm: debian-13                                          # a template's name in images_pool
          container: local:vztmpl/debian-13-standard_13.6-1_amd64.tar.zst
  - name: volumes
    builtin: volumes
    zones: [lab]
    credentials:
      lab: {file: /run/secrets/pve-volumes-token}  # its own, narrower token
  - name: images
    builtin: images
    zones: [lab]
    credentials:
      lab: {file: /run/secrets/pve-images-token}   # its own: templates and builders, saves
    settings:
      recipes:
        debian-13:
          base: {vm: "local:import/debian-13-genericcloud-amd64.qcow2"}   # a disk image on an import storage
          disk_gb: 4
          memory_mb: 2048                         # the builder's: spot room borrowed while it bakes
          timeout: 30m
          user_data: |
            #cloud-config
            package_update: true
            package_upgrade: true
            packages: [qemu-guest-agent]          # the driver reads the builder — and its machines — through it
```

## The bench

`tools/bench/bench.sh` installs a throwaway Proxmox VE in a VM on any Linux
machine with podman and `/dev/kvm` (nested virtualisation on), and sets it up
as above (`tools/bench/setup.sh` is the operator's preparation, as a script).
The driver's tests and the binary's end-to-end test run against it:

```sh
sh tools/bench/bench.sh up
eval "$(sh tools/bench/bench.sh env)"
go test -p 1 ./driver/proxmox/ ./cmd/hangar/ -run Bench -v
```

The images' tests bake from the cloud image `setup.sh` already put on the
bench's `import` storage: nothing more to download but the packages a
recipe installs.

`-p 1`: one package at a time — both packages' tests drive the bench's one
priority guest (VM 100), and go test runs packages side by side unless told
not to (read: the watcher test started VM 100 while the room test was about
to). CI does not run them: they need a hypervisor.
