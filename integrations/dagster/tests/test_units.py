"""The parts that need no brain: the settings, the verdicts, what a machine
is handed, the ssh config."""

from __future__ import annotations

import io
import os
import shlex
import socket
import subprocess
import sys
import tarfile
import tempfile
import threading

import pytest

from dagster_hangar import worker
from dagster_hangar.launcher import judge
from dagster_hangar.settings import Settings, SettingsError, Size, parse_size, read
from dagster_hangar.ssh import OpenSSH, Target, host_key_line, split_jump


def test_sizes():
    assert parse_size("2x4", "t") == Size(2, 4)
    assert str(parse_size("16x64", "t")) == "16x64"
    for bad in ("2", "x4", "0x4", "2x0", "2X4", "2x4G", "129x4", "2x1025", "-1x2"):
        with pytest.raises(SettingsError):
            parse_size(bad, "t")


def test_settings_come_from_the_file_then_the_environment():
    env = {
        "HANGAR_URL": "http://192.0.2.1:8080", "HANGAR_ZONE": "tower", "HANGAR_DEFAULT_SIZE": "2x4",
        "HANGAR_HOSTS": '{"s3.example.org": "192.0.2.10"}',
        "HANGAR_SOCKETS": '{"workspace": ["/opt/dagster/grpc/a.sock"]}', "HANGAR_ROOM_EVERY": "30",
    }
    s = read({"zone": "other", "disk_gb": 32}, env)
    assert s.on and s.url == "http://192.0.2.1:8080" and s.zone == "other" and s.disk_gb == 32
    assert s.default_size == Size(2, 4) and s.hosts == {"s3.example.org": "192.0.2.10"}
    assert s.sockets == {"workspace": ("/opt/dagster/grpc/a.sock",)} and s.room_every == 30.0
    assert not read({}, {}).on, "no HANGAR_URL: every run in-process"
    with pytest.raises(SettingsError, match="HANGAR_HOSTS"):
        read({}, {"HANGAR_HOSTS": "s3=192.0.2.10"})
    with pytest.raises(SettingsError, match="absolute"):
        read({"sockets": {"w": ["relative.sock"]}}, {})
    with pytest.raises(SettingsError, match="without /v1"):
        read({"url": "http://192.0.2.1/v1"}, {})
    with pytest.raises(SettingsError, match="above zero"):
        read({"room_patience": 0}, {})
    with pytest.raises(SettingsError, match=r"zone \(HANGAR_ZONE\)"):
        Settings(url="http://192.0.2.1").need("zone")


def test_settings_are_never_read_when_the_launcher_is_built(monkeypatch):
    """A run's worker rebuilds the launcher: nothing of its own may be needed
    there — a broken setting is said when a run is launched, not before."""
    pytest.importorskip("dagster")
    from dagster_hangar.launcher import HangarRunLauncher

    monkeypatch.setenv("HANGAR_HOSTS", "not json")
    launcher = HangarRunLauncher(default_size="2x4")
    with pytest.raises(SettingsError):
        launcher.settings()


def test_the_verdicts():
    alive = {"state": "ready", "spec": {"running": True}, "observed": {"running": True}}
    assert judge("m-1", alive) == ""
    assert "none was born" in judge("", None)
    assert "holds no such machine" in judge("m-1", None)
    assert "on_poweroff says delete" in judge("m-1", {"state": "ready", "spec": {"ending": "it stopped … on_poweroff says delete"}})
    assert "it is deleted" in judge("m-1", {"state": "deleted", "spec": {}})
    assert "no longer has it" in judge("m-1", {"state": "lost", "spec": {}})
    assert "a game took the room" in judge("m-1", {"state": "ready", "hold": "5003", "spec": {"running": False}})
    assert "a game took the room" in judge("m-1", {"state": "ready", "spec": {}, "observed": {"held": ["arcade"]}})
    stopped = {"state": "ready", "spec": {"running": True}, "observed": {"running": False, "stopped_unasked": "2026-01-01T00:00:00Z"}}
    assert "its worker ended without its run's end" in judge("m-1", stopped)
    # a reading not yet refreshed is no verdict: running in its spec, nothing seen stopped
    assert judge("m-1", {"state": "ready", "spec": {"running": True}, "observed": {"running": False}}) == ""


def test_the_worker_argv_and_its_bundle():
    argv = worker.docker_argv("r1", "img:1", ["dagster", "api", "execute_run", "{}"], env_file=True,
                              syslog="udp://192.0.2.20:514", hosts={"b.example.org": "192.0.2.2", "a.example.org": "192.0.2.1"},
                              socket_dirs=["/opt/dagster/grpc"])
    assert argv[:7] == ["run", "-d", "--init", "--name", "hangar-run", "--label", "dagster/run_id=r1"]
    assert argv[argv.index("-v") + 1] == "/run/hangar/run.env:/run/hangar/run.env:ro"
    hosts = [argv[i + 1] for i, a in enumerate(argv) if a == "--add-host"]
    assert hosts == ["a.example.org:192.0.2.1", "b.example.org:192.0.2.2"]
    assert argv[-8:] == ["img:1", "python", "-c", worker.READ_ENV, "dagster", "api", "execute_run", "{}"]
    data = worker.bundle(argv, b"K=V\n")
    with tarfile.open(fileobj=io.BytesIO(data)) as tar:
        members = {m.name: m for m in tar.getmembers()}
        assert set(members) == {"argv", "run.sh", "run.env"}
        assert all(m.uid == 0 for m in members.values())
        assert (members["argv"].mode, members["run.sh"].mode, members["run.env"].mode) == (0o600, 0o600, 0o644)
        assert tar.extractfile("argv").read().split(b"\0")[:-1] == [a.encode() for a in argv]


def test_the_scripts_a_machine_runs_are_bash():
    with tempfile.NamedTemporaryFile("w", suffix=".sh") as f:
        f.write(worker.WRAPPER)
        f.flush()
        subprocess.run(["bash", "-n", f.name], check=True)
    words = shlex.split(worker.start_command("r1", "udp://192.0.2.20:514"))
    assert words[:3] == ["sudo", "bash", "-c"]
    subprocess.run(["bash", "-n", "-c", words[3]], check=True)
    assert "bash /run/hangar/run.sh r1 udp 192.0.2.20 514" in words[3]
    assert worker.syslog_parts("tcp://192.0.2.20") == ("tcp", "192.0.2.20", "514")
    with pytest.raises(ValueError):
        worker.syslog_parts("192.0.2.20:514")


@pytest.mark.parametrize("docker_wait", ["0", "fails"])
def test_the_wrapper_powers_off_whatever_the_end(tmp_path, docker_wait):
    bin_ = tmp_path / "bin"
    bin_.mkdir()
    log = tmp_path / "log"
    wait = 'echo 137' if docker_wait == "0" else 'exit 1'
    stand_ins = {
        "docker": f'case "$1" in wait) {wait};; inspect) echo true;; esac',
        "logger": f'echo "logger: $*" >> {log}',
        "systemctl": f'echo "systemctl: $*" >> {log}',
        "sync": "true", "hostname": "echo run-1",
    }
    for name, body in stand_ins.items():
        (bin_ / name).write_text(f"#!/bin/sh\n{body}\n")
        (bin_ / name).chmod(0o755)
    script = tmp_path / "run.sh"
    script.write_text(worker.WRAPPER)
    subprocess.run(["bash", str(script), "r1", "udp", "192.0.2.20", "514"],
                   env=dict(os.environ, PATH=f"{bin_}:/usr/bin:/bin"), check=True)
    lines = log.read_text().splitlines()
    assert lines[-1] == "systemctl: poweroff"
    said = [ln for ln in lines if ln.startswith("logger:")]
    assert len(said) == 2 and "--server 192.0.2.20 --port 514 --udp --rfc5424" in said[0]
    assert "its worker started on run-1" in said[0]
    end = "exit 137, oom true" if docker_wait == "0" else "exit unknown, oom true"
    assert end in said[1], said


def test_the_start_unpacks_and_starts(tmp_path):
    """The start command's own script, run with docker and systemd-run as
    stand-ins: the files land, the argv reaches docker word for word."""
    bin_ = tmp_path / "bin"
    bin_.mkdir()
    log = tmp_path / "log"
    for name in ("docker", "systemd-run"):
        (bin_ / name).write_text(f'#!/bin/sh\nfor a in "$@"; do printf "{name}:%s\\n" "$a"; done >> {log}\n')
        (bin_ / name).chmod(0o755)
    run_dir = tmp_path / "run"
    inner = shlex.split(worker.start_command("r1"))[3].replace(worker.RUN_DIR, str(run_dir))
    argv = ["run", "-d", "--name", "hangar-run", "img:1", "dagster", "api", "execute_run", '{"a": "b c"}']
    subprocess.run(["bash", "-c", inner], input=worker.bundle(argv, None),
                   env=dict(os.environ, PATH=f"{bin_}:{os.environ['PATH']}"), check=True)
    lines = log.read_text().splitlines()
    assert [ln[len("docker:"):] for ln in lines if ln.startswith("docker:")] == argv
    assert f"systemd-run:{run_dir}/run.sh" in lines and "systemd-run:r1" in lines
    assert (run_dir / "run.sh").exists()


def test_the_env_is_read_as_docker_reads_an_env_file(tmp_path):
    env = tmp_path / "run.env"
    env.write_text("# a comment\n\nA=b=c d\n  SPACED=1\nNO_EQUALS\n=no-key\nbad key=1\nQUOTED=\"kept\"\r\n")
    reader = worker.READ_ENV.replace("/run/hangar/run.env", str(env))
    out = subprocess.run([sys.executable, "-c", reader, "env"], capture_output=True, text=True,
                         env={"PATH": os.environ["PATH"]}, check=True).stdout.splitlines()
    assert "A=b=c d" in out and "SPACED=1" in out and 'QUOTED="kept"' in out
    assert not [ln for ln in out if ln.startswith(("NO_EQUALS", "#"))]


class _Brain:
    """Brain.end against what the brain answers, call by call."""

    def __init__(self, states, deletes=(), waits=()):
        self.states, self.deletes, self.waits, self.asked = list(states), list(deletes), list(waits), []

    def get(self, id):
        return {"state": self.states.pop(0) if len(self.states) > 1 else self.states[0]}

    def delete(self, id, client_token=""):
        self.asked.append(client_token)
        what = self.deletes.pop(0)
        if isinstance(what, Exception):
            raise what
        return {"id": "op"}

    def wait(self, op, timeout, stop=None):
        return {"state": self.waits.pop(0)}


def test_end_waits_a_birth_and_asks_a_failed_delete_again():
    from dagster_hangar.brain import Brain, Problem, Unreachable

    b = _Brain(["creating", "creating", "ready"],
               [Problem(409, "busy", "m is creating"), Unreachable("cut"), "op", "op"], ["failed", "succeeded"])
    b.end = Brain.end.__get__(b)
    assert b.end("m-1", patience=5, every=0.01)
    assert b.asked == ["", "", "", ""], "a delete asked anew never replays a failed one"
    assert not b.deletes and not b.waits
    born = _Brain(["creating"])
    born.end = Brain.end.__get__(born)
    assert not born.end("m-1", patience=0.1, every=0.01) and born.asked == []
    gone = _Brain(["deleted"])
    gone.end = Brain.end.__get__(gone)
    assert gone.end("m-1", patience=0.1) and gone.asked == []


def _server(path: str, answer: bytes | None) -> threading.Thread:
    srv = socket.socket(socket.AF_UNIX)
    srv.bind(path)
    srv.listen(4)

    def serve():
        while True:
            try:
                c, _ = srv.accept()
            except OSError:
                return
            if answer is not None:
                c.sendall(answer)
                c.recv(64)
            c.close()

    t = threading.Thread(target=serve, daemon=True)
    t.start()
    return t


def test_the_probe_tells_a_grpc_server_from_a_cut_tunnel():
    d = tempfile.mkdtemp(prefix="p", dir="/tmp")
    good, cut, gone = f"{d}/good.sock", f"{d}/cut.sock", f"{d}/gone.sock"
    _server(good, b"\x00\x00\x06\x04\x00\x00\x00\x00\x00" + b"\x00\x03\x00\x00\x00\x64")  # SETTINGS
    _server(cut, None)  # accepts, closes: what an ssh -R with no far end does
    p = subprocess.run([sys.executable, "-", good, cut, gone], input=worker.PROBE.encode(), capture_output=True)
    assert p.returncode == 1 and p.stdout.decode().split() == [cut, gone]
    p = subprocess.run([sys.executable, "-", good], input=worker.PROBE.encode(), capture_output=True)
    assert p.returncode == 0


def test_jumps_and_host_keys():
    assert split_jump("jump@203.0.113.201") == ("jump", "203.0.113.201", 22)
    assert split_jump("jump@203.0.113.201:2222") == ("jump", "203.0.113.201", 2222)
    assert split_jump("jump@[2001:db8::1]:22") == ("jump", "2001:db8::1", 22)
    assert host_key_line("gw.example.org ssh-ed25519 AAAA comment") == "ssh-ed25519 AAAA"
    with pytest.raises(ValueError):
        host_key_line("AAAA")


def test_the_ssh_config_pins_both_keys(tmp_path):
    with pytest.raises(ValueError, match="trusted on first use"):
        OpenSSH(str(tmp_path / "key"), jump="jump@203.0.113.201")
    sh = OpenSSH(str(tmp_path / "key"), jump="jump@203.0.113.201", jump_host_key="203.0.113.201 ssh-ed25519 JUMPKEY")
    cfg = sh._config(Target("192.0.2.5", "ssh-ed25519 RUNKEY"))
    text = open(cfg).read()
    known = open(os.path.join(os.path.dirname(cfg), "known_hosts")).read().split("\n")
    assert "hangar-run ssh-ed25519 RUNKEY" in known and "hangar-jump ssh-ed25519 JUMPKEY" in known
    assert "ProxyJump hangar-jump" in text and "StrictHostKeyChecking yes" in text and "BatchMode yes" in text
    assert "HostName 192.0.2.5" in text and "HostName 203.0.113.201" in text and "User jump" in text
    assert "StrictHostKeyChecking no" not in text and "accept-new" not in text
