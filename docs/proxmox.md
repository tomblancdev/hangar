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
  that hold some.
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
  disks, then deletes its seed disc.

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
```

## The bench

`tools/bench/bench.sh` installs a throwaway Proxmox VE in a VM on any Linux
machine with podman and `/dev/kvm` (nested virtualisation on), and sets it up
as above (`tools/bench/setup.sh` is the operator's preparation, as a script).
The driver's tests and the binary's end-to-end test run against it:

```sh
sh tools/bench/bench.sh up
eval "$(sh tools/bench/bench.sh env)"
go test ./driver/proxmox/ ./cmd/hangar/ -run Bench -v
```

CI does not run them: they need a hypervisor.
