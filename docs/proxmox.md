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

## The least the token needs

| path | privileges | why |
|---|---|---|
| `/pool/<machines pool>` | `VM.Allocate`, `VM.Audit`, `VM.Clone`, `VM.Config.CDROM`, `VM.Config.CPU`, `VM.Config.Cloudinit`, `VM.Config.Disk`, `VM.Config.HWType`, `VM.Config.Memory`, `VM.Config.Network`, `VM.Config.Options`, `VM.PowerMgmt`, `VM.GuestAgent.Audit`, `Datastore.AllocateSpace`, `Datastore.Audit`, **`Pool.Audit`** | making and running its guests. `Pool.Audit` looks optional and is not: without it `/cluster/resources` **leaves out every guest's pool** (`API2/Cluster.pm`), and the driver could not tell its guests from anyone's |
| `/pool/<images pool>` | `VM.Audit`, `VM.Clone`, `Pool.Audit` | reading and cloning the templates — never changing them |
| `/storage/<disks>` | `Datastore.AllocateSpace`, `Datastore.Audit` | the guests' disks |
| `/storage/<where the archives are>` | `Datastore.Audit` | `pct create` reads the archive (it asks for `Datastore.AllocateSpace` **or** `Datastore.Audit` there) |
| `/storage/<seed storage>` | `Datastore.Allocate`, `Datastore.AllocateTemplate`, `Datastore.Audit` | uploading a VM's seed disc takes `AllocateTemplate`; **deleting it takes `Datastore.Allocate`**, which on a shared storage would reach every ISO, template and backup there — hence a storage that holds seed discs and nothing else |
| `/sdn/zones/<zone>/<vnet>` (with SDN) | `SDN.Use` | attaching guests to the vnet |

Give the same ACLs to the user and to its token (a separated token has the
intersection of both). Nothing on `/`, `/vms` or `/nodes`: the driver reads
its guests' status through the pool.

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
- **Other tags are `key.value`**: the machine's class is `class.spot` or
  `class.guaranteed`, for a node-side hook to read.
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
      ca_file: /etc/hangar/pve-root-ca.pem   # or fingerprint: <sha256 of the API's certificate>
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
