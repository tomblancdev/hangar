<p align="center"><img src="ui/static/logo-animated.svg" alt="le hangar — ask for a machine" width="640"></p>

# Le Hangar

**A small cloud's control plane, for the machines you already own.** People
sign in with the identity provider you already run, ask for machines, volumes
and images — and, through plugins, anything else — and get them within their
group's limits, on your hypervisors. Every no says why, with the numbers:

```json
{ "type": "urn:hangar:problem:limit", "status": 403,
  "detail": "2 of 2 toy.boxes used; this asks for 1 more" }
```

It keeps **the AWS mindset, not the AWS wire**: an API first, resources with
ids (`m-0123456789abcdef0`), types, images, user data, tags, client tokens,
on-demand and spot — for a home lab, a club, a small office. It is not a
hypervisor (it drives yours) and not an EC2 clone.

> **Status: the command line.** The core is built and proved end to end —
> identity, tiers and limits, the registry, operations, reconcile, the audit,
> the plugin host, the API. **The machines plugin and the Proxmox VE driver
> are built**, proved on a throwaway Proxmox VE the repo installs itself
> ([`tools/bench`](tools/bench/), [docs/proxmox.md](docs/proxmox.md)):
> containers and VMs made, resized live, stopped and deleted through the API.
> **So is a zone's capacity**: guaranteed and borrowed room, reservations for
> the guests that matter more than yours, and the hook that makes room for
> one when it starts — spot machines stopped, floors shrunk, whoever starts
> it, the brain reachable or not. **And volumes**: disks of your own, plugged
> into a machine, moved to another, parked on none, grown — their data kept
> throughout, and neither end deleted while one is in the other. **And
> images**: baked from a recipe, or saved from your stopped machine, shared
> with your group or everyone — a machine born from one usable at once —
> **and baked again by themselves**: a recipe on the brain's own clock, a
> machine asking for `@debian-13` born from the newest, the older ones
> retired and deleted once nothing is born from them. **And the command
> line**, drawn from what the brain serves: signed in by the device flow at
> your identity provider, every type a command, and `hangar apply` making a
> spec file true — created, changed by the steps each plugin names, deleted
> after asking ([docs/cli.md](docs/cli.md)). The console comes next. No
> release is cut yet. [ARCHITECTURE.md](ARCHITECTURE.md) says what is built
> and what is designed, section by section.

## How it fits together

- **The core** knows ids, owners, zones, states and what each resource holds
  per dimension. It never knows what a resource *is*.
- **Everything a person can ask for is a plugin** — its own process, started
  with an empty environment, holding only its own credential. A plugin
  declares its types (each with a JSON Schema), its limit dimensions, its
  actions and what it needs of an engine; the command line and the console
  are drawn from those declarations. The contract:
  [`plugin.proto`](proto/hangar/plugin/v1/plugin.proto); the example to copy:
  [`plugins/toy`](plugins/toy/toy.go).
- **Drivers** reach the engines and advertise **capability flags**
  (`kind.vm`, `resize.live.memory_down`, …); nothing decides by an engine's
  name. The fake driver ships first, so everything can be tried with nothing
  to install.
- **The API** is the only door: [`api/openapi.yaml`](api/openapi.yaml), served
  at `/openapi.json`.

## Try it — no hypervisor needed

```sh
go build -o hangar ./cmd/hangar
./hangar check --config example/hangar.yaml
TOKEN=$(./hangar token create --config example/hangar.yaml \
          --subject alice --groups example-users --name try)
./hangar serve --config example/hangar.yaml &

curl -s -H "Authorization: Bearer $TOKEN" localhost:8080/v1/types
curl -s -H "Authorization: Bearer $TOKEN" -X POST localhost:8080/v1/resources \
     -d '{"type":"box","zone":"playground","spec":{"cores":2}}'
curl -s -H "Authorization: Bearer $TOKEN" localhost:8080/v1/limits
```

The same binary is the command line — every type the brain offers a
command, its flags drawn from the type's schema ([docs/cli.md](docs/cli.md)):

```sh
export HANGAR_URL=http://localhost:8080 HANGAR_TOKEN=$TOKEN
./hangar types
./hangar box list
cat > boxes.yaml <<'YAML'
set: try
zone: playground
resources:
  one: {type: box, spec: {cores: 1}}
YAML
./hangar apply boxes.yaml       # again: nothing to do; cores: 2, then again: a resize
```

With an identity provider, `hangar login https://…` signs you in by the
device flow instead of a token.

The example's zone runs on the fake engine, whose state is
`data/playground.json`: edit it by hand — change a box's cores, or delete one
— and watch the next reconcile put it back, or mark it `lost`.

Or with the image: `ghcr.io/tomblancdev/hangar` (a `scratch` image, uid
65532, read-only root; the config at `/etc/hangar/hangar.yaml`, the registry
in the `/data` volume).

## Configuration

One file — [`example/hangar.yaml`](example/hangar.yaml) is the whole of it:
the identity provider (`issuer`, the client id tokens are issued to, the
claim holding groups), **tiers** (groups → limits per dimension; the first
tier a person's groups reach is theirs; a dimension a tier does not name is
allowed nothing), **zones** (a driver and how to reach it), **plugins** (built
in, or a program at a path pinned by its SHA-256; enabled per zone; one
credential per zone read from a file or an environment variable, never
written in the file), **schedules** (a create on a cron line, in the name of
a subject and groups, keeping the newest few). `hangar check` starts every
plugin, refuses a limit that names no declared dimension and a schedule its
plugins cannot answer, and prints each schedule's next run — before anything
serves.

The brain speaks plain HTTP: put it behind your gateway, and never on the
internet directly — it holds the plugins' credentials.

## The house contract

`/healthz` (503 with the reasons when the registry or a plugin is down) ·
`/metrics` (Prometheus: plugins up, resources by type and state, operations,
refusals by reason, calls by route) · `/openapi.json` · the audit: one JSON
line per call on stdout, `"kind":"audit"`, with who, which credential, which
resource, the result and, for a refusal, why.

## Development

```sh
go test ./...              # the whole suite, the binary's end-to-end run included
sh tools/bench/bench.sh up # a throwaway Proxmox VE in a VM (podman + /dev/kvm), then:
eval "$(sh tools/bench/bench.sh env)" && go test -p 1 ./driver/proxmox/ ./cmd/hangar/ -run Bench -v
sh tools/no-environment.sh # this repo describes nowhere: RFC documentation reserves only
sh tools/protogen.sh       # regenerate the plugin protocol's Go code (tools pinned in tools/go.mod)
```

[La Loge](https://github.com/tomblancdev/la-loge) keeps the door,
[Le Videur](https://github.com/tomblancdev/videur) decides who passes,
[Le Veilleur](https://github.com/tomblancdev/veilleur) keeps watch while the
machines sleep — and Le Hangar hands them out.

MIT licensed.
