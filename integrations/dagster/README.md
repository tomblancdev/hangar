# dagster-hangar — each Dagster run on a machine of Le Hangar

A Dagster run launcher. A run gets a machine of its size from Le Hangar —
a spot VM born from an image that holds Docker and the runs' images —, its
worker runs there, and the machine powers itself off when the worker ends;
the brain deletes it. Nothing beyond Dagster: the brain's REST API by the
standard library, ssh by the `openssh-client` a platform's image holds.
Proved on Dagster 1.12.3.

```yaml
# dagster.yaml — the platform's, the same file everywhere it runs
run_launcher:
  module: dagster_hangar
  class: HangarRunLauncher
  config:
    default_size: 2x4
```

## Its word

| | |
|---|---|
| `default_size: 2x4` | every run gets a machine of 2 cores and 4 G |
| a run tagged `hangar/size: 8x16` | a machine of that size — as ECS reads `ecs/cpu` |
| neither | the run runs in-process, by Dagster's own launcher held inside this one |
| **no `HANGAR_URL`** | **every run in-process**: a deployment without Le Hangar runs unchanged |
| a run tagged `hangar/sockets: workspace` | the code servers' unix sockets named `workspace` (the `sockets` setting) are carried to its machine by the launcher's own ssh (`ssh -R`), each checked to answer before the worker starts — a run that reads the other code locations runs on a machine too |

**A run's machine**: `kind: vm`, `class: spot`, `cpu: host`, the size's
cores and memory, 16 G of disk, born from `machine_image` on `network` with
`key_pair`; `resume: false` (a game's stop is a lost run, never a VM booted
again under a dead worker); `on_poweroff: delete`; `for` = the run's
`dagster/max_runtime` + 10 minutes (`max_runtime` when it names none),
`at_end: delete`; tags `dagster/run_id` and `dagster/job`; the run's id is
its client token. Its first boot is handed a host key the launcher made and
nothing else: the machine is reached through its network's gateway, both
host keys pinned, nothing trusted on first use.

**`launch_run` returns at once.** A thread of the launcher's asks for the
machine; **a `409 room`** (the zone's borrowed room held) **is waited out**
— the brain's sentence once on the run's page, asked again every
`room_every` up to `room_patience`, then the run fails with it; **a `403
limit` fails it at once**. A refused ask writes nothing in the brain: the
same client token asked again is a new ask, and an ask that reached the
brain twice is one machine. Then: the machine's birth, ssh answering, the
run's env and files into its memory (`/run/hangar`, a tmpfs), `docker run`
of the run's image with `dagster api execute_run …` — the env mounted
read-only and read inside the container, never handed to Docker, which
writes a container's env into its config on the disk; a refusal of
Docker's fails the run there —, and a wrapper that waits for the worker, says two
lines of its own to the syslog address (started; ended, with the exit and
the OOM flag), and powers the machine off whatever the end (`--init` gives
the worker a PID 1 that forwards `docker stop`'s SIGTERM and reaps its
steps). An engine event says « launched on m-… (2x4): born N s, answering
M s ». **A worker that ends before its run starts** (an image that does not
load its code, say) **fails the run then**, with its exit, instead of
leaving it STARTING until `start_timeout`: health is asked only of a started
run. The env reaches the worker's command, not an image's `ENTRYPOINT`. The run's status is
read again before each ask and before the worker starts: a run cancelled
meanwhile gets no worker. **A machine the launcher ends is deleted until it
is gone**, in a thread of its own: one still being born is waited for, a
delete that failed is asked again, and a run whose create was applied but
answered by silence has its machine found by its id (past fifteen minutes,
or in a process that ends, the brain ends it at its `until`). What the
launcher decided for a run is written on it (`hangar/launched`): health and
terminate read it there, never in settings that changed since.

**Health** (`run_monitoring`): the brain's record of the run's machine —
gone (its `on_poweroff`, its `until`, a delete) → `FAILED`; **held** for a
game → `FAILED`, and **the launcher deletes it** (a held machine never
powers itself off, and monitoring fails a run without ending it); stopped
with no hold — the worker died without its run's end (an OOM, a crash) →
`FAILED`, deleted. `RUNNING` while it runs, for a run in-process (its code
server watches it), and when the brain does not answer (said, transient):
monitoring fails a run on `NOT_FOUND` or `UNKNOWN`. A run's sockets cut are
opened again at each check.

**Terminate**: a run whose worker runs gets its SIGTERM by ssh (`docker
stop`: Dagster's own cancel — the worker writes `RUN_CANCELED`, the machine
powers off), whether a person cancelled it or monitoring did (past its
`dagster/max_runtime`); a run with no worker yet, or a process with no way
in, gets its machine ended by the brain and is reported canceled by the
launcher, once. **The webserver a
run is cancelled from runs `terminate` in its own process**: given the key
and the gateway's host key it stops the worker; given only the token it
deletes the machine. Either ends the run.

## Its settings

The instance file's `config:` first, else the process's environment — read
when a run is launched, checked or ended, never when the launcher is built:
a run's worker rebuilds the instance, and must need none of them.

| key | variable | |
|---|---|---|
| `url` | `HANGAR_URL` | the brain; none = every run in-process |
| `token_file` | `HANGAR_TOKEN_FILE` | a file holding the token (never the token in a variable: the worker would resolve it again) |
| `zone` | `HANGAR_ZONE` | |
| `default_size` | `HANGAR_DEFAULT_SIZE` | `<cores>x<G>` |
| `machine_image` | `HANGAR_MACHINE_IMAGE` | the image a run's machine is born from — the bake's, by name or id |
| `network` | `HANGAR_NETWORK` | its network (by name or id) |
| `key_pair` | `HANGAR_KEY_PAIR` | the key pair it is born with |
| `ssh_key_file` | `HANGAR_SSH_KEY_FILE` | that key pair's private key |
| `ssh_user` | `HANGAR_SSH_USER` | `debian` |
| `jump`, `jump_host_key` | `HANGAR_JUMP`, `HANGAR_JUMP_HOST_KEY` | the network's gateway (`jump@203.0.113.201`) and its host key |
| `disk_gb` | `HANGAR_DISK_GB` | 16 |
| `cpu` | `HANGAR_CPU` | `host`; `zone` = the zone's own model |
| `worker_image` | `HANGAR_WORKER_IMAGE` | the worker's image when its code location sets none (`DAGSTER_CURRENT_IMAGE` does) |
| `env_dir` | `HANGAR_ENV_DIR` | a directory of `<location>.env` files (Docker's env-file form: `KEY=value` as written, `#` a comment): the run's is relayed to its machine's memory and read inside its container — a location with none, or a line that is not `KEY=value`, fails its runs at their launch (the line said by its number) |
| `syslog` | `HANGAR_SYSLOG` | `udp://host:514`: the worker's lines by Docker's syslog driver, in RFC 5424, and the wrapper's two |
| `hosts` | `HANGAR_HOSTS` | `{"name": "address"}` (JSON in the variable): pinned by `--add-host` in the worker |
| `sockets` | `HANGAR_SOCKETS` | `{"workspace": ["/opt/dagster/grpc/a.sock", …]}`: the sockets a run's tag may name |
| `max_runtime` | `HANGAR_MAX_RUNTIME` | seconds, 7200 |
| `room_patience`, `room_every` | `HANGAR_ROOM_PATIENCE`, `HANGAR_ROOM_EVERY` | seconds, 1800 and 60 |
| `ssh_patience` | `HANGAR_SSH_PATIENCE` | seconds, 300 |

**The instance file's own part**: `run_monitoring: {enabled: true,
start_timeout_seconds: <above room_patience and a birth>,
max_resume_run_attempts: 0}` (with it off, `dagster/max_runtime` is enforced
by nothing, and a lost machine is never seen); `run_retries` with
`retry_on_asset_or_op_failure: false` runs a run lost with its machine
again — a retry inherits its parent's tags, and the launcher overwrites its
`hangar/machine`.

## The bake

```sh
python -m dagster_hangar bake --name lake-1-2-3 IMAGE…
```

A machine from `@debian-13` (the newest the operator's schedule baked),
Docker by Docker's own packages (its key checked by its digest), every
`IMAGE` pulled (`--registry-auth`: a Docker config.json, kept in the
machine's memory, never on the disk that becomes the image), sshd told to lend unix sockets to anyone on the machine
(`StreamLocalBindMask 0111`); cloud-init's memory, the machine id, the host
keys and `authorized_keys` cleared; stopped, saved as `NAME`; the builder
deleted. Of its family (`NAME` up to its first `-`), the newest
`--keep` (2) stay; an older one is deleted once no machine is born from it.
It reads the same settings; `--help` says the rest.

## Its tests

```sh
go build -o /tmp/hangar ./cmd/hangar          # from the repository's root
cd integrations/dagster
pip install -e '.[test]' dagster==1.12.3
HANGAR_BIN=/tmp/hangar python -m pytest
```

Against **this repository's brain on the fake engine** (served by the
tests, its engine's file edited to power a guest off or start the game a
reservation waits on) and Dagster's own instance; the ssh half is faked
there. **The ssh half itself** runs on a bench — two sshd's (a gateway and a
run's machine) in a throwaway container, the launcher's own client through
them: `tests/test_ssh_bench.py` says how.
