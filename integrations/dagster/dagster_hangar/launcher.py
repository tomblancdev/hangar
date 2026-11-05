"""HangarRunLauncher — each Dagster run on a machine of Le Hangar.

Its word:

- **`default_size`** in its config (`2x4`) gives every run a machine; a run
  tagged **`hangar/size: <cores>x<G>`** names a bigger one (as ECS reads
  `ecs/cpu`). With neither, a run runs in-process, by Dagster's own
  launcher held inside this one.
- **No brain named (no `HANGAR_URL`): every run in-process** — the instance
  file names this class everywhere, and a deployment without Le Hangar runs
  unchanged.
- **`hangar/sockets: <name>`** lends the run the code servers' unix sockets
  of that name (its config's `sockets`), carried to its machine by the
  launcher's own ssh (`ssh -R`): each checked to answer before the worker
  starts, a cut one opened again at each health check.

A run's machine: a spot VM of its size, `cpu: host`, born from the lake's
image on its network, `resume: false` (a game's stop is a lost run, never a
VM booted again under a dead worker), `on_poweroff: delete`, `for` its
`dagster/max_runtime` and ten minutes, `at_end: delete`, the run's id as its
client token. Its worker is `dagster api execute_run` in the run's image;
when it ends the machine powers itself off and the brain deletes it.

`launch_run` returns at once: a thread of the launcher's asks for the
machine, waits out a `409 room` (the brain's sentence on the run's page,
asked again every minute up to its patience), reaches it, starts the worker.
"""

from __future__ import annotations

import json
import math
import os
import threading
import time
from collections.abc import Mapping
from typing import Any

from dagster import Field, Permissive
from dagster._core.launcher import CheckRunHealthResult, LaunchRunContext, RunLauncher, WorkerStatus
from dagster._core.launcher.default_run_launcher import DefaultRunLauncher
from dagster._core.storage.dagster_run import DagsterRun, DagsterRunStatus
from dagster._core.storage.tags import MAX_RUNTIME_SECONDS_TAG
from dagster._grpc.types import ExecuteRunArgs
from dagster._serdes import ConfigurableClass, ConfigurableClassData

from dagster_hangar import worker
from dagster_hangar.brain import Brain, Problem, Unreachable
from dagster_hangar.settings import Settings, SettingsError, Size, parse_size, read
from dagster_hangar.ssh import OpenSSH, Shell, SshError, Target, Tunnel, host_key_line

SIZE_TAG = "hangar/size"
SOCKETS_TAG = "hangar/sockets"
MACHINE_TAG = "hangar/machine"
# what the launcher decided for a run, written at its launch: health and
# terminate read it there, never in settings that may have changed since
LAUNCHED_TAG = "hangar/launched"
RUN_ID_TAG = "dagster/run_id"  # on the machine, in the brain
JOB_TAG = "dagster/job"

# a machine past its run's longest life is deleted by the brain: the run's
# max_runtime and this, or the launcher's default and this
END_MARGIN = 600
# how long a machine's birth is waited for, once the brain took the ask
BIRTH_PATIENCE = 600.0
# how long a machine the launcher ends is asked to go: its birth, then its delete
REAP_PATIENCE = BIRTH_PATIENCE + 300.0
# how long the brain is searched for a machine a create made whose answer was
# lost: a create still being planned commits after the client gave up on it
LOST_WINDOW = 120.0
LIVE = ["creating", "ready", "updating"]
GONE = {"deleting", "deleted", "failed", "lost"}


class LaunchFailed(Exception):
    """What the launcher could not do for a run, in a sentence for its page."""


class HangarRunLauncher(RunLauncher, ConfigurableClass):
    def __init__(self, inst_data: ConfigurableClassData | None = None, **config: Any):
        self._inst_data = inst_data
        self._config = {k: v for k, v in config.items() if v is not None}
        self._inner = DefaultRunLauncher()
        self._lock = threading.Lock()
        self._threads: dict[str, threading.Thread] = {}
        self._wake: dict[str, threading.Event] = {}
        self._tunnels: dict[str, Tunnel] = {}
        self._shells: dict[tuple, Shell] = {}
        self._reaping: set[str] = set()
        # tests hand these in: a shell that boots nothing, an environment
        self.shell: Shell | None = None
        self.environ: Mapping[str, str] | None = None
        # how long the sockets lent to a run are waited for on its machine
        self.socket_patience = 20.0
        # how long a worker is watched after its start, until its run starts
        self.start_watch = 90.0
        self.lost_window = LOST_WINDOW
        super().__init__()

    # ---- Dagster's contract ---------------------------------------------

    @property
    def inst_data(self) -> ConfigurableClassData | None:
        return self._inst_data

    @classmethod
    def config_type(cls) -> dict[str, Field]:
        # plain values only: an {env: …} here would be resolved again by every
        # run's worker, which holds none of it — the deployment's values come
        # from HANGAR_* variables, read when a run is launched (settings.py)
        text = ["url", "token_file", "zone", "default_size", "machine_image", "network", "key_pair",
                "ssh_key_file", "ssh_user", "jump", "jump_host_key", "cpu", "worker_image", "env_dir", "syslog"]
        out = {k: Field(str, is_required=False) for k in text}
        out.update({k: Field(int, is_required=False) for k in ("disk_gb", "max_runtime")})
        out.update({k: Field(float, is_required=False) for k in ("room_patience", "room_every", "ssh_patience")})
        out.update({k: Field(Permissive(), is_required=False) for k in ("hosts", "sockets")})
        return out

    @classmethod
    def from_config_value(cls, inst_data: ConfigurableClassData, config_value: Mapping[str, Any]) -> HangarRunLauncher:
        return cls(inst_data=inst_data, **config_value)

    def register_instance(self, instance) -> None:
        super().register_instance(instance)
        # the launcher held inside has no instance of its own otherwise
        self._inner.register_instance(instance)

    @property
    def supports_check_run_worker_health(self) -> bool:
        return True

    def dispose(self) -> None:
        with self._lock:
            tunnels, self._tunnels = list(self._tunnels.values()), {}
        for t in tunnels:
            t.close()
        self._inner.dispose()

    def join(self, timeout: int = 30) -> None:
        self._inner.join(timeout)

    # ---- the word --------------------------------------------------------

    def settings(self) -> Settings:
        return read(self._config, self.environ)

    def machine_size(self, run: DagsterRun, s: Settings | None = None) -> Size | None:
        """The size of the run's machine; None: it runs in-process."""
        s = s or self.settings()
        if not s.on:
            return None
        tag = run.tags.get(SIZE_TAG)
        if tag:
            return parse_size(tag, f"the run's tag {SIZE_TAG}")
        return s.default_size

    def _sockets(self, run: DagsterRun, s: Settings) -> tuple[str, ...]:
        name = run.tags.get(SOCKETS_TAG, "").strip()
        if not name:
            return ()
        if name not in s.sockets:
            known = ", ".join(sorted(s.sockets)) or "none"
            raise LaunchFailed(f"the run's tag {SOCKETS_TAG} names {name!r}: the launcher lends {known}")
        return s.sockets[name]

    def _shell(self, s: Settings) -> Shell:
        if self.shell is not None:
            return self.shell
        key = (s.ssh_key_file, s.ssh_user, s.jump, s.jump_host_key)
        with self._lock:
            sh = self._shells.get(key)
            if sh is None:
                sh = self._shells[key] = OpenSSH(s.ssh_key_file, s.ssh_user, s.jump, s.jump_host_key)
            return sh

    def _brain(self, s: Settings) -> Brain:
        s.need("token_file")
        return Brain.from_token_file(s.url, s.token_file)

    # ---- launch ----------------------------------------------------------

    def launch_run(self, context: LaunchRunContext) -> None:
        run = context.dagster_run
        s = self.settings()
        size = self.machine_size(run, s)
        if size is None:
            self._instance.add_run_tags(run.run_id, {LAUNCHED_TAG: "in-process"})
            self._inner.launch_run(context)
            return
        # refused here, the run fails at once with the sentence (Dagster's own
        # handling of a launch that raises)
        s.need("token_file", "zone", "machine_image", "key_pair", "ssh_key_file")
        if s.jump and not s.jump_host_key:
            raise SettingsError("jump without jump_host_key (HANGAR_JUMP_HOST_KEY): nothing is trusted on first use")
        sockets = self._sockets(run, s)
        if run.job_code_origin is None:
            raise LaunchFailed("the run carries no code origin: a machine has nothing to run")
        image = self._image(run, s)
        env = self._env(run, s)
        # a retry inherits every tag of its parent: its machine is not this one's
        self._instance.add_run_tags(run.run_id, {LAUNCHED_TAG: "machine", MACHINE_TAG: ""})
        wake = threading.Event()
        t = threading.Thread(
            target=self._launch, args=(run.run_id, size, sockets, image, env, wake),
            name=f"hangar-launch-{run.run_id[:8]}", daemon=True,
        )
        with self._lock:
            self._threads[run.run_id] = t
            self._wake[run.run_id] = wake
        t.start()

    def _image(self, run: DagsterRun, s: Settings) -> str:
        origin = run.job_code_origin
        image = origin.repository_origin.container_image if origin else None
        image = image or s.worker_image
        if not image:
            raise LaunchFailed(
                "no image for its worker: its code location sets none (DAGSTER_CURRENT_IMAGE) "
                "and the launcher names none (worker_image, HANGAR_WORKER_IMAGE)"
            )
        return image

    @staticmethod
    def _location(run: DagsterRun) -> str:
        origin = run.remote_job_origin
        return origin.repository_origin.code_location_origin.location_name if origin else ""

    def _env(self, run: DagsterRun, s: Settings) -> bytes | None:
        """The run's location's env file, relayed to its machine's memory."""
        if not s.env_dir:
            return None
        loc = self._location(run)
        if not loc or "/" in loc or loc.startswith("."):
            raise LaunchFailed(f"no code location to read an env file for (location {loc!r})")
        path = os.path.join(s.env_dir, loc + ".env")
        try:
            with open(path, "rb") as f:
                data = f.read()
        except OSError as e:
            raise LaunchFailed(f"the env file of location {loc} cannot be read ({e.strerror}): {path}") from None
        # read here as its worker will read it: a line it cannot take fails the
        # run now, said by its number — never by what it holds
        for n, line in enumerate(data.decode("utf-8", "surrogateescape").splitlines(), 1):
            if not line.strip() or line.lstrip().startswith("#"):
                continue
            key = line.split("=", 1)[0].lstrip()
            if "=" not in line or not worker.ENV_KEY.fullmatch(key):
                raise LaunchFailed(f"the env file of location {loc}, line {n}: not KEY=value ({path})")
        return data

    def _event(self, run_id: str, message: str) -> None:
        run = self._instance.get_run_by_id(run_id)
        if run:
            self._instance.report_engine_event(message, dagster_run=run, cls=self.__class__)

    def _gone(self, run_id: str) -> bool:
        """The run no longer wants a machine (cancelled, failed, ended). A run
        cancelled before its worker started is reported canceled here: no
        worker will write it."""
        run = self._instance.get_run_by_id(run_id)
        if run is None:
            return True
        if run.status == DagsterRunStatus.CANCELING:
            self._canceled(run_id, "canceled before its worker started: no machine runs it")
            return True
        return run.status not in (DagsterRunStatus.STARTING, DagsterRunStatus.NOT_STARTED)

    @staticmethod
    def _machine_run(run: DagsterRun) -> bool:
        """The launcher gave it a machine: its launch said so — or, launched
        before the launcher wrote that, it names one."""
        launched = run.tags.get(LAUNCHED_TAG)
        return launched == "machine" or (launched is None and bool(run.tags.get(MACHINE_TAG)))

    def _canceled(self, run_id: str, message: str) -> None:
        """Reports a run canceled once: the thread and terminate may both get
        there, and a run-status sensor counts every RUN_CANCELED."""
        with self._lock:
            run = self._instance.get_run_by_id(run_id)
            if run is not None and not run.is_finished:
                self._instance.report_run_canceled(run, message)

    def _wake_thread(self, run_id: str) -> None:
        with self._lock:
            wake = self._wake.get(run_id)
        if wake is not None:
            wake.set()

    def _reap_run(self, brain: Brain, run_id: str, machine: str = "") -> threading.Thread:
        """Ends what a run was born on, in a thread of its own: its machine,
        or — none known — any the brain holds with its id (a create applied
        whose answer was lost). Each is deleted until it is gone (Brain.end):
        a machine still being born waited for, a failed delete asked again.
        Past REAP_PATIENCE, or in a process that ends, the brain ends it at
        its until."""

        def end(mid: str) -> None:
            with self._lock:
                if mid in self._reaping:
                    return
                self._reaping.add(mid)
            try:
                brain.end(mid, REAP_PATIENCE)
            finally:
                with self._lock:
                    self._reaping.discard(mid)

        def search() -> None:
            seen: set[str] = set()
            deadline = time.monotonic() + self.lost_window
            while True:
                try:
                    found = [m["id"] for m in brain.list("machine", tags={RUN_ID_TAG: run_id}, states=LIVE)]
                except (Unreachable, Problem):
                    found = []
                for mid in found:
                    if mid not in seen:
                        seen.add(mid)
                        threading.Thread(target=end, args=(mid,), name=f"hangar-end-{mid[:12]}", daemon=True).start()
                if time.monotonic() >= deadline:
                    return
                time.sleep(min(10.0, max(0.5, self.lost_window / 12)))

        if machine:
            t = threading.Thread(target=end, args=(machine,), name=f"hangar-end-{machine[:12]}", daemon=True)
        else:
            t = threading.Thread(target=search, name=f"hangar-search-{run_id[:8]}", daemon=True)
        t.start()
        return t

    def _launch(self, run_id: str, size: Size, sockets: tuple[str, ...], image: str, env: bytes | None,
                wake: threading.Event) -> None:
        machine = ""
        brain = None
        target = None
        sh = None
        started = False
        asked = False
        try:
            s = self.settings()
            run = self._instance.get_run_by_id(run_id)
            if run is None or self._gone(run_id):
                return
            brain = self._brain(s)
            sh = self._shell(s)
            private, public = sh.host_key()
            spec = self._spec(run, s, size, private, public)
            first = time.monotonic()
            asked = True
            got = self._create(brain, run, s, size, spec, wake)
            if got is None:
                return
            # its birth is counted from the ask that got it: a room's wait is
            # said apart, never as a slow birth
            res, op, t0, waited = got
            machine = res["id"]
            self._instance.add_run_tags(run_id, {MACHINE_TAG: machine})
            if self._gone(run_id):
                return
            op = brain.wait(op["id"], BIRTH_PATIENCE, stop=lambda: self._gone(run_id))
            if self._gone(run_id):
                return
            if op.get("state") != "succeeded":
                why = op.get("error") or op.get("detail") or op.get("state")
                raise LaunchFailed(f"its machine {machine} was not born: {why}")
            born = time.monotonic() - t0
            target = Target(self._address(brain, machine, s), public, s.ssh_user)
            sh.wait_answering(target, s.ssh_patience)
            answering = time.monotonic() - t0
            if self._gone(run_id):
                return
            if sockets:
                self._lend(run_id, sh, target, s, sockets)
            run = self._instance.get_run_by_id(run_id)
            self._start(run, sh, target, s, image, env, sockets)
            started = True
            lent = f", {len(sockets)} sockets lent" if sockets else ""
            wait = f"waited {t0 - first:.0f} s for {waited}, " if waited else ""
            self._event(run_id, f"launched on {machine} ({size}): {wait}born {born:.0f} s, answering {answering:.0f} s{lent}")
            self._watch_start(run_id, sh, target, brain, machine)
        except Exception as e:  # anything it could not do fails the run, its machine with it
            if started:
                return
            msg = str(e) if isinstance(e, (LaunchFailed, SettingsError, Problem, SshError)) else f"{type(e).__name__}: {e}"
            run = self._instance.get_run_by_id(run_id)
            if run is not None and not run.is_finished:
                where = f" ({machine})" if machine else ""
                self._instance.report_run_failed(run, f"Le Hangar's launcher{where}: {msg}")
        finally:
            if not started:
                self._close_tunnel(run_id)
                if asked and brain is not None:
                    self._reap_run(brain, run_id, machine)
            if target is not None and sh is not None:
                try:
                    sh.close(target)
                except Exception:
                    pass
            with self._lock:
                self._threads.pop(run_id, None)
                self._wake.pop(run_id, None)

    def _watch_start(self, run_id: str, sh: Shell, target: Target, brain: Brain, machine: str) -> None:
        """Until its run starts, a worker that ended is a run that will never
        start: failed now, with its exit — not after start_timeout. (Its
        machine powers itself off; health is asked only of a started run.)"""
        deadline = time.monotonic() + self.start_watch
        while time.monotonic() < deadline:
            run = self._instance.get_run_by_id(run_id)
            if run is None or run.status != DagsterRunStatus.STARTING:
                return
            ended = ""
            try:
                rc, out, _ = sh.run(target, worker.inspect_command(), timeout=30)
                state = out.split()
                if rc == 0 and state and state[0] == "exited":
                    ended = f"exit {state[1] if len(state) > 1 else 'unknown'}"
                elif rc != 0:
                    m = brain.get(machine)
                    if judge(machine, m):
                        ended = "its machine is gone"
            except Exception:
                pass
            if ended:
                run = self._instance.get_run_by_id(run_id)
                if run is not None and run.status == DagsterRunStatus.STARTING:
                    self._instance.report_run_failed(
                        run, f"Le Hangar's launcher ({machine}): its worker ended before its run started ({ended}) — its lines are in the syslog")
                    self._reap_run(brain, run_id, machine)
                return
            time.sleep(3)

    def _spec(self, run: DagsterRun, s: Settings, size: Size, private: str, public: str) -> dict[str, Any]:
        seconds = float(s.max_runtime)
        raw = run.tags.get(MAX_RUNTIME_SECONDS_TAG) or run.tags.get("dagster/max_runtime_seconds")
        if raw:
            try:
                if float(raw) > 0:
                    seconds = float(raw)
            except ValueError:
                pass
        minutes = min(math.ceil((seconds + END_MARGIN) / 60), 365 * 24 * 60)
        # its first boot is handed a host key the launcher made, and nothing
        # else: the key it is reached by is known before it is born
        user_data = "#cloud-config\n" + json.dumps({
            "ssh_deletekeys": True,
            "ssh_genkeytypes": [],
            "ssh_keys": {"ed25519_private": private, "ed25519_public": public},
        }, indent=1) + "\n"
        spec: dict[str, Any] = {
            "kind": "vm", "class": "spot", "resume": False,
            "cores": size.cores, "memory_gb": size.memory_gb, "disk_gb": s.disk_gb,
            "image_id": s.machine_image, "key_pairs": [s.key_pair], "user_data": user_data,
            "on_poweroff": "delete", "for": f"{minutes}m", "at_end": "delete",
        }
        if s.cpu and s.cpu != "zone":
            spec["cpu"] = s.cpu
        if s.network:
            spec["network"] = s.network
        return spec

    def _create(self, brain: Brain, run: DagsterRun, s: Settings, size: Size, spec: dict[str, Any],
                wake: threading.Event) -> tuple[dict[str, Any], dict[str, Any], float, str] | None:
        """Asks for the machine, waiting out a room held or a brain that does
        not answer up to room_patience; None: the run went meanwhile. The run's
        id is its client token — a refusal keeps nothing of it, so asked again
        it is a new ask, and an ask that reached the brain twice is one
        machine. Returned with the moment the ask that got it left, and what
        it waited for before ("": nothing)."""
        deadline = time.monotonic() + s.room_patience
        said = ""
        waited: list[str] = []
        job = run.job_name or ""
        while True:
            if self._gone(run.run_id):
                return None
            sent = time.monotonic()
            try:
                res, op = brain.create(
                    "machine", s.zone, spec,
                    name=f"run-{run.run_id[:8]}",
                    description=f"Dagster run {run.run_id} · {job}"[:256],
                    tags={RUN_ID_TAG: run.run_id, JOB_TAG: job[:256]},
                    client_token=run.run_id,
                )
                return res, op, sent, " and ".join(waited)
            except Problem as p:
                # a room held, a reference that moved while it asked, a brain
                # that is not itself (a plugin starting again): asked again
                if p.kind not in ("room", "busy") and p.status < 500:
                    raise LaunchFailed(f"Le Hangar refused its machine ({size}): {p.detail}") from None
                kind, why = p.kind or str(p.status), p.detail
            except Unreachable as e:
                kind, why = "unreachable", f"Le Hangar does not answer: {e}"
            what = {"room": "room", "busy": "a reference that moved"}.get(kind, "the brain")
            if what not in waited:
                waited.append(what)
            if time.monotonic() >= deadline:
                raise LaunchFailed(f"no machine ({size}) after {s.room_patience:.0f} s: {why}")
            if said != kind:
                said = kind
                self._event(run.run_id, f"waiting for its machine ({size}): {why} — asked again every "
                            f"{s.room_every:.0f} s, for up to {s.room_patience:.0f} s")
            wake.wait(min(s.room_every, max(0.0, deadline - time.monotonic())))

    def _address(self, brain: Brain, machine: str, s: Settings) -> str:
        """The address its zone gave it at its birth, or the one it took."""
        deadline = time.monotonic() + s.ssh_patience
        while True:
            try:
                m = brain.get(machine) or {}
            except Unreachable:
                if time.monotonic() > deadline:
                    raise
                time.sleep(2)
                continue
            obs = m.get("observed") or {}
            addr = obs.get("address") or next(iter(obs.get("addresses") or []), "")
            if addr:
                return addr.split("/")[0]
            if m.get("state") in GONE or time.monotonic() > deadline:
                raise LaunchFailed(f"its machine {machine} has no address (state {m.get('state', 'unknown')})")
            time.sleep(2)

    def _start(self, run: DagsterRun, sh: Shell, target: Target, s: Settings, image: str,
               env: bytes | None, sockets: tuple[str, ...]) -> None:
        args = ExecuteRunArgs(
            job_origin=run.job_code_origin,  # type: ignore[arg-type]
            run_id=run.run_id,
            instance_ref=self._instance.get_ref(),
            set_exit_code_on_failure=True,
        ).get_command_args()
        python = run.job_code_origin.executable_path if run.job_code_origin else ""
        argv = worker.docker_argv(
            run.run_id, image, args, env_file=env is not None, syslog=s.syslog, hosts=s.hosts,
            socket_dirs=worker.socket_dirs(sockets) if sockets else (), python=python or "python",
        )
        rc, _, err = sh.run(target, worker.start_command(run.run_id, s.syslog), stdin=worker.bundle(argv, env), timeout=120)
        if rc != 0:
            raise LaunchFailed(f"its worker did not start: {_last(err) or f'exit {rc}'}")

    # ---- the sockets lent ------------------------------------------------

    def _lend(self, run_id: str, sh: Shell, target: Target, s: Settings, sockets: tuple[str, ...]) -> None:
        rc, _, err = sh.run(target, worker.prepare_dirs_command(sockets, target.user))
        if rc != 0:
            raise LaunchFailed(f"the sockets' directories on its machine: {_last(err) or f'exit {rc}'}")
        self._open_tunnel(run_id, sh, target, sockets)
        bad = self._probe(sh, target, sockets, patience=self.socket_patience)
        if bad:
            raise LaunchFailed("the sockets it is lent do not answer on its machine: " + " ".join(bad))

    def _open_tunnel(self, run_id: str, sh: Shell, target: Target, sockets: tuple[str, ...]) -> None:
        self._close_tunnel(run_id)
        tunnel = sh.forward(target, [(p, p) for p in sockets])
        with self._lock:
            self._tunnels[run_id] = tunnel

    def _close_tunnel(self, run_id: str) -> None:
        with self._lock:
            t = self._tunnels.pop(run_id, None)
        if t is not None:
            t.close()

    def _sweep(self) -> None:
        """Lets go of the tunnels whose ssh ended (their machine gone)."""
        with self._lock:
            dead = [k for k, t in self._tunnels.items() if not t.alive()]
        for k in dead:
            self._close_tunnel(k)

    @staticmethod
    def _probe(sh: Shell, target: Target, sockets: tuple[str, ...], patience: float) -> list[str]:
        """The sockets that do not answer as a gRPC server does, on the machine."""
        deadline = time.monotonic() + patience
        while True:
            rc, out, err = sh.run(target, worker.probe_command(sockets), stdin=worker.PROBE.encode(), timeout=60)
            if rc == 0:
                return []
            bad = out.split() or [f"(the probe: {_last(err) or f'exit {rc}'})"]
            if time.monotonic() > deadline:
                return bad
            time.sleep(2)

    def _mend(self, run: DagsterRun, s: Settings, m: Mapping[str, Any], sockets: tuple[str, ...]) -> str:
        """Opens the run's tunnel again when a socket stopped answering on its
        machine; says what it did, "" when nothing was cut."""
        target = _target(m, s)
        if target is None:
            return "its machine's host key is not in its spec: its sockets were not checked"
        sh = self._shell(s)
        try:
            if not self._probe(sh, target, sockets, patience=0):
                return ""
            self._open_tunnel(run.run_id, sh, target, sockets)
            bad = self._probe(sh, target, sockets, patience=self.socket_patience / 2)
            if bad:
                return "its sockets were cut, opened again, still silent: " + " ".join(bad)
            self._event(run.run_id, "its sockets were cut: the tunnel is open again")
            return ""
        except Exception as e:
            return f"its sockets could not be checked: {e}"
        finally:
            try:
                sh.close(target)
            except Exception:
                pass

    # ---- health ----------------------------------------------------------

    def check_run_worker_health(self, run: DagsterRun) -> CheckRunHealthResult:
        """The brain's record of the run's machine. RUNNING for a run
        in-process (its code server watches it) and when the brain does not
        answer — run monitoring fails a run on NOT_FOUND or UNKNOWN."""
        self._sweep()
        if not self._machine_run(run):
            return CheckRunHealthResult(WorkerStatus.RUNNING, "in-process: its code server watches it")
        try:
            s = self.settings()
        except SettingsError as e:
            return CheckRunHealthResult(WorkerStatus.RUNNING, f"the launcher's settings: {e}", transient=True)
        try:
            brain = self._brain(s)
            machine = self._machine_of(run, brain)
            m = brain.get(machine) if machine else None
        except (Unreachable, Problem, OSError, SettingsError) as e:
            return CheckRunHealthResult(WorkerStatus.RUNNING, f"Le Hangar did not answer: {e}", transient=True)
        verdict = judge(machine, m)
        if verdict:
            if machine and m is not None and m.get("state") not in ("deleted", "failed"):
                self._reap_run(brain, run.run_id, machine)
            self._close_tunnel(run.run_id)
            return CheckRunHealthResult(WorkerStatus.FAILED, verdict, run_worker_id=machine or None)
        msg = None
        try:
            sockets = self._sockets(run, s)
        except LaunchFailed as e:
            sockets, msg = (), str(e)
        if sockets and s.ssh_ready:
            msg = self._mend(run, s, m or {}, sockets) or None
        return CheckRunHealthResult(WorkerStatus.RUNNING, msg, run_worker_id=machine)

    def get_run_worker_debug_info(self, run: DagsterRun, include_container_logs: bool | None = True) -> str | None:
        if not self._machine_run(run):
            return None
        try:
            s = self.settings()
            brain = self._brain(s)
            machine = self._machine_of(run, brain)
            if not machine:
                return "Le Hangar: no machine was born for it"
            m = brain.get(machine) or {}
        except Exception as e:
            return f"Le Hangar: {e}"
        obs = m.get("observed") or {}
        return (f"Le Hangar: its machine {machine}, {m.get('state', 'unknown')}, "
                f"{'running' if obs.get('running') else 'not running'}"
                + (f", held for {m['hold']}" if m.get("hold") else ""))

    def _machine_of(self, run: DagsterRun, brain: Brain) -> str:
        """Its machine's id: the run's tag, else the brain's machine that
        carries its id (a launch that ended between the birth and the tag)."""
        tag = run.tags.get(MACHINE_TAG, "")
        if tag:
            return tag
        found = brain.list("machine", tags={RUN_ID_TAG: run.run_id}, states=LIVE)
        return found[0]["id"] if found else ""

    # ---- terminate -------------------------------------------------------

    def terminate(self, run_id: str) -> bool:
        """A run whose worker runs: its SIGTERM, by ssh (Dagster's own cancel —
        the worker writes RUN_CANCELED, the machine powers off), whether the
        cancel is a person's or monitoring's (past max_runtime, the run is
        CANCELING before this is called). No worker yet, or no way in (a
        process that holds no key): its machine ended by the brain, the run
        reported canceled by the launcher. A run cancelled before its machine
        was born: reported canceled — the launch's thread, if it is asking
        now, ends whatever it gets."""
        if not self.has_instance:
            return False
        run = self._instance.get_run_by_id(run_id)
        if not run or run.is_finished:
            return False
        if not self._machine_run(run):
            return self._inner.terminate(run_id)
        self._instance.report_run_canceling(run)
        self._close_tunnel(run_id)
        try:
            s = self.settings()
            brain = self._brain(s)
            machine = self._machine_of(run, brain)
            m = brain.get(machine) if machine else None
        except (Unreachable, Problem, OSError, SettingsError) as e:
            self._canceled(run_id, f"canceled; Le Hangar could not be asked ({e}): a machine born for it ends at its until")
            self._wake_thread(run_id)
            return True
        if not machine:
            self._canceled(run_id, "canceled before its machine was born")
            self._wake_thread(run_id)
            return True
        why = ""
        if m is not None and m.get("state") == "ready" and self._has_worker(run):
            target = _target(m, s)
            if not s.ssh_ready:
                why = " (by the brain alone: this process holds no way into its machine)"
            elif target is not None:
                sh = self._shell(s)
                try:
                    rc, _, err = sh.run(target, worker.stop_command(), timeout=90)
                    if rc == 0:
                        return True
                    why = f" (its worker's stop: {_last(err) or f'exit {rc}'})"
                except Exception as e:
                    why = f" (its worker's stop: {e})"
                finally:
                    try:
                        sh.close(target)
                    except Exception:
                        pass
        self._reap_run(brain, run_id, machine)
        self._canceled(run_id, f"canceled: its machine {machine} is deleted{why}")
        self._wake_thread(run_id)
        return True

    def _has_worker(self, run: DagsterRun) -> bool:
        """Its worker began: the run started (RUN_START is the worker's)."""
        if run.status == DagsterRunStatus.STARTED:
            return True
        try:
            return self._instance.get_run_stats(run.run_id).start_time is not None
        except Exception:
            return False


def judge(machine: str, m: Mapping[str, Any] | None) -> str:
    """What the brain's record says of a run's machine; "" while it runs."""
    if not machine:
        return "it has no machine: none was born for it, or none is left"
    if m is None:
        return f"its machine {machine} is gone: Le Hangar holds no such machine"
    spec = m.get("spec") or {}
    obs = m.get("observed") or {}
    state = m.get("state", "")
    if spec.get("ending"):
        return f"its machine {machine} is gone: {spec['ending']}"
    if state in GONE:
        by = {"lost": "its engine no longer has it", "failed": "it was never born"}.get(state, f"it is {state}")
        return f"its machine {machine} is gone: {by}"
    if m.get("hold") or obs.get("held"):
        return f"a game took the room: its machine {machine} was stopped for {m.get('hold') or ', '.join(obs['held'])}"
    if obs.get("stopped_unasked") or (spec.get("running") is False and obs.get("running") is False):
        return (f"its worker ended without its run's end: its machine {machine} stopped "
                "(an OOM, a crash — its wrapper powered it off)")
    return ""


def _target(m: Mapping[str, Any], s: Settings) -> Target | None:
    """A machine's address and the host key its spec's first boot carried."""
    spec = m.get("spec") or {}
    obs = m.get("observed") or {}
    addr = obs.get("address") or next(iter(obs.get("addresses") or []), "")
    ud = spec.get("user_data") or ""
    try:
        keys = json.loads(ud.split("\n", 1)[1]).get("ssh_keys") or {}
        public = host_key_line(keys["ed25519_public"])
    except (IndexError, ValueError, KeyError, AttributeError, TypeError):
        return None
    if not addr:
        return None
    return Target(addr.split("/")[0], public, s.ssh_user)


def _last(text: str) -> str:
    lines = [ln for ln in text.strip().splitlines() if ln.strip()]
    return lines[-1] if lines else ""
