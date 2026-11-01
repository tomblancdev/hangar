"""A real brain on the fake engine, and a Dagster instance with the launcher.

The brain is the product's own binary (HANGAR_BIN, or built from this tree
with go), served on a fake zone whose file is the engine: a test edits the
file to power a guest off, or to start the guest a reservation waits on.
The ssh half is a FakeShell: the fake engine boots nothing to ssh into.
"""

from __future__ import annotations

import base64
import contextlib
import json
import os
import shutil
import socket
import struct
import subprocess
import tempfile
import time
from collections.abc import Callable, Iterator
from typing import Any

import pytest

from dagster_hangar.brain import Brain
from dagster_hangar.ssh import Target

REPO = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))


def public_key(comment: str = "test") -> str:
    """An ed25519 public key in OpenSSH's form, of no machine and no person."""
    blob = struct.pack(">I", 11) + b"ssh-ed25519" + struct.pack(">I", 32) + os.urandom(32)
    return f"ssh-ed25519 {base64.b64encode(blob).decode()} {comment}"


PUBLIC_KEY = public_key()
HOST_KEY = " ".join(public_key().split()[:2])


def wait_for(what: str, fn: Callable[[], Any], timeout: float = 30, every: float = 0.2) -> Any:
    deadline = time.monotonic() + timeout
    while True:
        got = fn()
        if got:
            return got
        if time.monotonic() > deadline:
            raise AssertionError(f"never: {what}")
        time.sleep(every)


@pytest.fixture(scope="session")
def hangar_bin(tmp_path_factory) -> str:
    given = os.environ.get("HANGAR_BIN")
    if given:
        return given
    if not shutil.which("go"):
        pytest.fail("the tests need the brain: HANGAR_BIN=<a hangar binary>, or go to build it from this tree")
    out = str(tmp_path_factory.mktemp("bin") / "hangar")
    subprocess.run(["go", "build", "-o", out, "./cmd/hangar"], cwd=REPO, check=True)
    return out


class Hangar:
    """One brain: its config, its tokens, its engine's file, its API."""

    CONFIG = """
data_dir: {dir}/data
tiers:
  - name: lake
    groups: [cloud-lake]
    zones: [tower]
    limits: {{"*": unlimited}}
  - name: tiny
    groups: [cloud-tiny]
    zones: [tower]
    limits: {{"*": unlimited, machines.count: 0}}
zones:
  - name: tower
    driver: fake
    endpoint: {dir}/zone.json
    room:
      memory_gb: 64
      reservations:
        - {{name: arcade, memory_gb: 32, while_running: "5003"}}
plugins:
  - name: machines
    builtin: machines
    zones: [tower]
    settings:
      images: {{debian-13: {{vm: debian-13}}}}
  - {{name: networks, builtin: networks, zones: [tower]}}
  - {{name: images, builtin: images, zones: [tower]}}
reconcile: {{every: 5s}}
"""

    def __init__(self, binary: str):
        self.bin = binary
        # short: the plugins' unix sockets live under it (108 bytes at most)
        self.dir = tempfile.mkdtemp(prefix="h", dir="/tmp")
        self.cfg = os.path.join(self.dir, "hangar.yaml")
        self.engine = os.path.join(self.dir, "zone.json")
        with open(self.cfg, "w") as f:
            f.write(self.CONFIG.format(dir=self.dir))
        self.tokens: dict[str, str] = {}
        self.proc: subprocess.Popen | None = None
        self.url = ""

    def token(self, subject: str, group: str) -> str:
        out = subprocess.run(
            [self.bin, "token", "create", "--config", self.cfg, "--subject", subject, "--groups", group,
             "--name", subject, "--ttl", "1h"],
            check=True, capture_output=True, text=True,
        ).stdout.strip()
        assert out.startswith("hgr_"), out
        path = os.path.join(self.dir, f"{subject}.token")
        with open(path, "w") as f:
            f.write(out + "\n")
        self.tokens[subject] = path
        return path

    def serve(self) -> None:
        with socket.socket() as s:
            s.bind(("127.0.0.1", 0))
            addr = "127.0.0.1:%d" % s.getsockname()[1]
        self.log = open(os.path.join(self.dir, "brain.log"), "wb")
        env = dict(os.environ, HANGAR_LISTEN=addr)
        env.pop("HANGAR_DATA_DIR", None)
        self.proc = subprocess.Popen([self.bin, "serve", "--config", self.cfg], env=env,
                                     stdout=self.log, stderr=subprocess.STDOUT)
        self.url = "http://" + addr
        import urllib.request

        def healthy() -> bool:
            try:
                return urllib.request.urlopen(self.url + "/healthz", timeout=2).status == 200
            except OSError:
                return False

        wait_for("the brain healthy", healthy, timeout=20)

    def brain(self, subject: str = "lake") -> Brain:
        return Brain.from_token_file(self.url, self.tokens[subject])

    def read_engine(self) -> dict[str, Any]:
        with open(self.engine) as f:
            return json.load(f)

    def tamper(self, fn: Callable[[dict[str, Any]], None]) -> None:
        """Changes the engine behind the brain's back: the file is the engine."""
        state = self.read_engine()
        fn(state)
        tmp = self.engine + ".t"
        with open(tmp, "w") as f:
            json.dump(state, f)
        os.replace(tmp, self.engine)

    def held(self, on: bool) -> None:
        """The guest the reservation waits on starts (a game), or stops; the
        brain's survey sees it."""
        self.tamper(lambda st: st.setdefault("watched", {}).__setitem__("5003", on))
        b = self.brain()

        def seen() -> bool:
            zone = b.call("GET", "/v1/zones")[1]["zones"][0]
            return bool((zone.get("room") or {}).get("held_by")) == on

        wait_for(f"the room {'held' if on else 'free'}", seen, timeout=30)

    def stop(self) -> None:
        if self.proc and self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(10)
            except subprocess.TimeoutExpired:
                self.proc.kill()
        if getattr(self, "log", None):
            self.log.close()
        shutil.rmtree(self.dir, ignore_errors=True)

    def logs(self) -> str:
        with open(os.path.join(self.dir, "brain.log"), "rb") as f:
            return f.read().decode(errors="replace")[-4000:]


@pytest.fixture
def hangar(hangar_bin) -> Iterator[Hangar]:
    h = Hangar(hangar_bin)
    h.token("lake", "cloud-lake")
    h.token("tiny", "cloud-tiny")
    h.serve()
    try:
        yield h
    except BaseException:
        print(h.logs())
        raise
    finally:
        h.stop()


@pytest.fixture
def lake(hangar: Hangar) -> dict[str, str]:
    """The lake's own: a key pair, a network that lets it jump, an image a
    run's machine is born from (saved from a stopped machine, as the bake
    does)."""
    b = hangar.brain()
    kp, op = b.create("keypair", "tower", {"public_key": PUBLIC_KEY}, name="lakehouse")
    assert b.wait(op["id"], 20)["state"] == "succeeded"
    net, op = b.create("network", "tower", {"key_pairs": [kp["id"]]}, name="lake")
    assert b.wait(op["id"], 20)["state"] == "succeeded"
    seed, op = b.create("machine", "tower", {"image": "debian-13", "cores": 1, "memory_gb": 1, "network": "lake"},
                        name="seed")
    assert b.wait(op["id"], 20)["state"] == "succeeded"
    assert b.wait(b.act(seed["id"], "stop")["id"], 20)["state"] == "succeeded"
    img, op = b.create("image", "tower", {"machine": seed["id"], "shared_with": ["*"]}, name="lake-test")
    assert b.wait(op["id"], 30)["state"] == "succeeded"
    assert b.wait(b.delete(seed["id"])["id"], 20)["state"] == "succeeded"
    return {"key_pair": kp["id"], "network": net["id"], "image": "lake-test", "image_id": img["id"]}


class FakeTunnel:
    def __init__(self, pairs: list[tuple[str, str]]):
        self.pairs = pairs
        self.open = True

    def alive(self) -> bool:
        return self.open

    def close(self) -> None:
        self.open = False


class FakeShell:
    """What the launcher asks of ssh, written down; what it answers, set by
    the test (an exit, its words) per command's first word."""

    def __init__(self) -> None:
        self.calls: list[tuple[str, Any]] = []
        self.tunnels: list[FakeTunnel] = []
        self.answers: dict[str, tuple[int, str, str]] = {}
        self.probe_bad: list[str] = []
        # a tunnel opened again carries what the cut one lost
        self.heal_on_forward = False
        self.on_stop: Callable[[], None] | None = None
        self.on_answering: Callable[[], None] | None = None

    def host_key(self) -> tuple[str, str]:
        return ("-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----\n",
                HOST_KEY)

    def wait_answering(self, target: Target, patience: float) -> None:
        self.calls.append(("answering", target))
        if self.on_answering:
            self.on_answering()

    def run(self, target: Target, command: str, stdin: bytes | None = None, timeout: float = 60) -> tuple[int, str, str]:
        self.calls.append(("run", (target, command, stdin)))
        if command.startswith("python3 - "):
            bad = [p for p in self.probe_bad if p in command]
            return (1, " ".join(bad), "") if bad else (0, "", "")
        if "docker stop" in command and self.on_stop:
            self.on_stop()
        for word, answer in self.answers.items():
            if word in command:
                return answer
        return 0, "", ""

    def forward(self, target: Target, pairs: list[tuple[str, str]]) -> FakeTunnel:
        t = FakeTunnel(pairs)
        if self.heal_on_forward:
            self.probe_bad = []
        self.tunnels.append(t)
        self.calls.append(("forward", (target, pairs)))
        return t

    def close(self, target: Target) -> None:
        self.calls.append(("close", target))

    def commands(self) -> list[str]:
        return [c[1][1] for c in self.calls if c[0] == "run"]

    def started(self) -> tuple[str, bytes] | None:
        for kind, call in self.calls:
            if kind == "run" and "systemd-run" in call[1]:
                return call[1], call[2]
        return None


@pytest.fixture
def shell() -> FakeShell:
    return FakeShell()


@pytest.fixture
def key_file(tmp_path) -> str:
    p = tmp_path / "key"
    p.write_text("not a key: the fake shell reads none\n")
    return str(p)


@contextlib.contextmanager
def dagster_instance(config: dict[str, Any], shell: FakeShell | None, environ: dict[str, str] | None = None):
    from dagster._core.test_utils import instance_for_test

    overrides = {"run_launcher": {"module": "dagster_hangar", "class": "HangarRunLauncher", "config": config}}
    with instance_for_test(overrides=overrides) as instance:
        launcher = instance.run_launcher
        launcher.shell = shell
        launcher.environ = environ if environ is not None else {}
        # a test's worker never starts its run: nothing to watch; and a lost
        # create is searched for a few seconds, not two minutes
        launcher.start_watch = 0
        launcher.lost_window = 3.0
        yield instance
