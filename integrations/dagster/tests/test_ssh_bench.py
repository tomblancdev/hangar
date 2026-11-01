"""The ssh half on a bench: two sshd's in this very container — a gateway
(the jump) and a run's machine — reached by the launcher's own OpenSSH, every
host key pinned, the sockets carried by ssh -R, the start and the stop of a
worker with Docker and systemd-run as stand-ins.

It needs root and openssh-server where it runs (a throwaway container), and
says so by DAGSTER_HANGAR_SSH_BENCH=1; anywhere else it skips:

    apt-get install -y openssh-server openssh-client sudo
    DAGSTER_HANGAR_SSH_BENCH=1 python -m pytest tests/test_ssh_bench.py
"""

from __future__ import annotations

import os
import pwd
import re
import shutil
import socket
import subprocess
import tempfile
import threading
import time

import pytest

from dagster_hangar import bake, worker
from dagster_hangar.ssh import OpenSSH, SshError, Target

pytestmark = pytest.mark.skipif(
    os.environ.get("DAGSTER_HANGAR_SSH_BENCH") != "1" or os.geteuid() != 0 or not shutil.which("sshd"),
    reason="the ssh bench runs as root in a throwaway container: DAGSTER_HANGAR_SSH_BENCH=1",
)

JUMP_PORT = 2201
SETTINGS = b"\x00\x00\x06\x04\x00\x00\x00\x00\x00" + b"\x00\x03\x00\x00\x00\x64"


def _user(name: str, shell: str) -> str:
    try:
        return pwd.getpwnam(name).pw_dir
    except KeyError:
        subprocess.run(["useradd", "-m", "-s", shell, name], check=True)
        # no password, and not locked: sshd without PAM refuses a locked account
        subprocess.run(["usermod", "-p", "*", name], check=True)
        return pwd.getpwnam(name).pw_dir


def _keygen(path: str) -> str:
    subprocess.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", path], check=True)
    with open(path + ".pub") as f:
        return f.read().strip()


def _sshd_lines() -> str:
    """The bake's own sshd settings, as it writes them on the image."""
    m = re.search(r"dagster-hangar\.conf <<'EOF'\n(.*?)\nEOF", bake.INSTALL, re.S)
    assert m, "the bake writes no sshd settings"
    return m.group(1)


class Bench:
    def __init__(self) -> None:
        self.dir = tempfile.mkdtemp(prefix="b", dir="/tmp")
        os.chmod(self.dir, 0o755)
        self.client_key = os.path.join(self.dir, "client")
        client_pub = _keygen(self.client_key)
        for name, shell in (("jump", "/usr/sbin/nologin"), ("debian", "/bin/bash")):
            home = _user(name, shell)
            os.makedirs(f"{home}/.ssh", exist_ok=True)
            with open(f"{home}/.ssh/authorized_keys", "w") as f:
                f.write(client_pub + "\n")
            shutil.chown(f"{home}/.ssh", name, name)
            shutil.chown(f"{home}/.ssh/authorized_keys", name, name)
            os.chmod(f"{home}/.ssh/authorized_keys", 0o600)
        with open("/etc/sudoers.d/debian", "w") as f:
            f.write("debian ALL=(ALL) NOPASSWD:ALL\n")
        os.makedirs("/run/sshd", exist_ok=True)
        self.jump_key = _keygen(os.path.join(self.dir, "jump_host"))
        self.procs: list[subprocess.Popen] = []
        self.sh = OpenSSH(self.client_key, user="debian", jump=f"jump@127.0.0.1:{JUMP_PORT}",
                          jump_host_key=self.jump_key)
        # the run's machine: born with the host key the launcher made for it
        private, self.host_key = self.sh.host_key()
        path = os.path.join(self.dir, "run_host")
        with open(path, "w") as f:
            f.write(private)
        os.chmod(path, 0o600)
        self._sshd("jump", JUMP_PORT, os.path.join(self.dir, "jump_host"), "AllowUsers jump\nPermitTTY no\n")
        self._sshd("run", 22, path, "AllowUsers debian\n" + _sshd_lines() + "\n")
        self.target = Target("127.0.0.1", self.host_key, "debian")

    def _sshd(self, name: str, port: int, host_key: str, more: str) -> None:
        cfg = os.path.join(self.dir, f"{name}.conf")
        with open(cfg, "w") as f:
            f.write(f"Port {port}\nListenAddress 127.0.0.1\nHostKey {host_key}\nPasswordAuthentication no\n"
                    f"KbdInteractiveAuthentication no\nPubkeyAuthentication yes\nUsePAM no\n"
                    f"PidFile {self.dir}/{name}.pid\n{more}")
        self.procs.append(subprocess.Popen(["/usr/sbin/sshd", "-D", "-e", "-f", cfg],
                                           stderr=open(os.path.join(self.dir, f"{name}.log"), "wb")))
        deadline = time.monotonic() + 10
        while True:
            with socket.socket() as s:
                if s.connect_ex(("127.0.0.1", port)) == 0:
                    return
            if time.monotonic() > deadline:
                raise AssertionError(f"sshd {name} never listened")
            time.sleep(0.1)

    def stop(self) -> None:
        for p in self.procs:
            p.terminate()
            p.wait(5)
        shutil.rmtree(self.dir, ignore_errors=True)


@pytest.fixture(scope="module")
def bench():
    b = Bench()
    try:
        yield b
    finally:
        b.stop()


def test_it_answers_through_the_jump_with_both_keys_pinned(bench):
    bench.sh.wait_answering(bench.target, 20)
    assert bench.sh.run(bench.target, "id -un")[:2] == (0, "debian\n")
    assert bench.sh.run(bench.target, "cat", stdin=b"carried\n")[1] == "carried\n"
    bench.sh.close(bench.target)


def test_a_machine_with_another_host_key_is_not_answered(bench):
    other = Target("127.0.0.1", bench.jump_key.rsplit(" ", 1)[0], "debian")  # a key, not its own
    with pytest.raises(SshError, match="does not answer ssh"):
        bench.sh.wait_answering(other, 3)
    bench.sh.close(other)


def test_a_jump_with_another_host_key_is_not_crossed(bench):
    liar = OpenSSH(bench.client_key, user="debian", jump=f"jump@127.0.0.1:{JUMP_PORT}",
                   jump_host_key=bench.host_key)
    with pytest.raises(SshError):
        liar.wait_answering(bench.target, 3)
    liar.close(bench.target)


def _grpc_like(path: str) -> socket.socket:
    srv = socket.socket(socket.AF_UNIX)
    srv.bind(path)
    os.chmod(path, 0o666)
    srv.listen(8)

    def serve():
        while True:
            try:
                c, _ = srv.accept()
            except OSError:
                return
            c.sendall(SETTINGS)
            try:
                c.recv(64)
            except OSError:
                pass
            c.close()

    threading.Thread(target=serve, daemon=True).start()
    return srv


def test_the_sockets_ride_its_ssh(bench):
    local_dir = tempfile.mkdtemp(prefix="l", dir="/tmp")
    local = os.path.join(local_dir, "a.sock")
    remote = ["/opt/dagster/grpc/a.sock"]
    srv = _grpc_like(local)
    sh, target = bench.sh, bench.target
    assert sh.run(target, worker.prepare_dirs_command(remote, "debian"))[0] == 0
    tunnel = sh.forward(target, [(remote[0], local)])
    try:
        time.sleep(1)
        rc, out, err = sh.run(target, worker.probe_command(remote), stdin=worker.PROBE.encode())
        assert rc == 0, (out, err)
        # anyone on the machine reaches it: a worker's container, whatever its user
        assert sh.run(target, "stat -c %a " + remote[0])[1].strip() == "666"
        # its far end gone: the socket still accepts, and the probe says so
        srv.close()
        os.unlink(local)
        rc, out, _ = sh.run(target, worker.probe_command(remote), stdin=worker.PROBE.encode())
        assert rc == 1 and out.split() == remote
        # opened again, over the old socket (StreamLocalBindUnlink)
        srv = _grpc_like(local)
        tunnel.close()
        tunnel = sh.forward(target, [(remote[0], local)])
        time.sleep(1)
        assert sh.run(target, worker.probe_command(remote), stdin=worker.PROBE.encode())[0] == 0
    finally:
        tunnel.close()
        srv.close()
        sh.close(target)


def test_a_worker_is_started_and_stopped(bench):
    # stand-ins: each writes its arguments NUL-separated (one of them is a
    # program of several lines)
    logs = {name: f"/tmp/{name}.args" for name in ("docker", "systemd-run")}
    for name, log in logs.items():
        if os.path.exists(log):
            os.unlink(log)
        with open(f"/usr/local/bin/{name}", "w") as f:
            f.write(f'#!/bin/sh\nfor a in "$@"; do printf "%s\\0" "$a"; done >> {log}\n')
        os.chmod(f"/usr/local/bin/{name}", 0o755)

    def args(name: str) -> list[str]:
        with open(logs[name]) as f:
            return f.read().split("\0")[:-1]

    argv = worker.docker_argv("r1", "img:1", ["dagster", "api", "execute_run", '{"a": "b c"}'], env_file=True)
    sh, target = bench.sh, bench.target
    rc, _, err = sh.run(target, worker.start_command("r1", "udp://192.0.2.20:514"), stdin=worker.bundle(argv, b"K=V\n"))
    assert rc == 0, err
    assert args("docker") == argv
    assert args("systemd-run")[-6:] == ["bash", "/run/hangar/run.sh", "r1", "udp", "192.0.2.20", "514"]
    st = os.stat("/run/hangar/run.env")
    assert (st.st_uid, st.st_mode & 0o777) == (0, 0o644) and open("/run/hangar/run.env").read() == "K=V\n"
    assert (os.stat("/run/hangar").st_mode & 0o777, os.stat("/run/hangar/argv").st_mode & 0o777) == (0o700, 0o600)
    os.unlink(logs["docker"])
    assert sh.run(target, worker.stop_command())[0] == 0
    assert args("docker") == ["stop", "-t", "30", "hangar-run"]
    sh.close(target)
