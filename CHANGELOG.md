# Changelog

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
