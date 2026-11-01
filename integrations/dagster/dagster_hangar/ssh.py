"""How the launcher reaches a run's machine: ssh, through its network's
gateway, every host key pinned.

The launcher speaks to a `Shell`; `OpenSSH` is the one that runs `ssh`
(the `openssh-client` a platform image already holds). A test hands the
launcher a fake one — the fake engine boots nothing to ssh into.

Nothing is trusted on first use: the gateway's host key is the deployment's
(`jump_host_key`), the machine's is the one the launcher made for it and
handed to its first boot. A machine whose key is not that one is not
answered.
"""

from __future__ import annotations

import os
import shlex
import shutil
import subprocess
import tempfile
import threading
import time
from dataclasses import dataclass
from typing import Protocol


class SshError(Exception):
    """ssh did not do what was asked: its last words."""


@dataclass(frozen=True)
class Target:
    """A run's machine: its address and the host key it was born with."""

    address: str
    host_key: str  # "ssh-ed25519 AAAA…"
    user: str = "debian"


class Tunnel(Protocol):
    def alive(self) -> bool: ...
    def close(self) -> None: ...


class Shell(Protocol):
    def host_key(self) -> tuple[str, str]:
        """A new host key pair for a machine: (private key in OpenSSH's form,
        public key line)."""
        ...

    def wait_answering(self, target: Target, patience: float) -> None:
        """Returns once the machine answers ssh; SshError after patience."""
        ...

    def run(self, target: Target, command: str, stdin: bytes | None = None, timeout: float = 60) -> tuple[int, str, str]:
        """Runs one shell command on the machine: (exit, stdout, stderr)."""
        ...

    def forward(self, target: Target, pairs: list[tuple[str, str]]) -> Tunnel:
        """Carries each local unix socket to the machine at its remote path
        (ssh -R remote:local), for as long as the tunnel lives."""
        ...

    def close(self, target: Target) -> None:
        """Lets go of what was kept open for that machine."""
        ...


def host_key_line(value: str) -> str:
    """The `type key` of a host key given as a public key line or as a
    known_hosts line (its host names, its comment dropped)."""
    words = value.split()
    for i, w in enumerate(words):
        if (w.startswith("ssh-") or w.startswith("ecdsa-") or w.startswith("sk-")) and i + 1 < len(words):
            return f"{w} {words[i + 1]}"
    raise ValueError(f"no host key in {value[:40]!r}…: ssh-ed25519 AAAA…")


def split_jump(jump: str) -> tuple[str, str, int]:
    """user@host[:port] — the host may be [an IPv6 address]."""
    user, sep, rest = jump.rpartition("@")
    if not sep:
        user, rest = "", jump
    port = 22
    if rest.startswith("["):
        host, _, tail = rest[1:].partition("]")
        if tail.startswith(":"):
            port = int(tail[1:])
    elif rest.count(":") == 1:
        host, p = rest.split(":")
        port = int(p)
    else:
        host = rest
    if not host:
        raise ValueError(f"jump {jump!r}: user@host[:port]")
    return user, host, port


class _Process:
    """A tunnel: an `ssh -N -R …` the launcher keeps."""

    def __init__(self, proc: subprocess.Popen):
        self.proc = proc

    def alive(self) -> bool:
        return self.proc.poll() is None

    def close(self) -> None:
        if self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(5)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait()


class OpenSSH:
    """ssh as OpenSSH's client runs it: one config file per machine (the
    gateway as its ProxyJump, both host keys pinned under fixed aliases),
    one connection shared by every call to it (ControlMaster)."""

    def __init__(
        self,
        key_file: str,
        user: str = "debian",
        jump: str = "",
        jump_host_key: str = "",
        connect_timeout: int = 10,
        ssh: str = "ssh",
    ):
        if jump and not jump_host_key:
            raise ValueError("a jump without its host key: nothing is trusted on first use")
        self.key_file = key_file
        self.user = user
        self.jump = split_jump(jump) if jump else None
        self.jump_host_key = host_key_line(jump_host_key) if jump_host_key else ""
        self.connect_timeout = connect_timeout
        self.ssh = ssh
        self._dirs: dict[Target, str] = {}
        self._lock = threading.Lock()

    def host_key(self) -> tuple[str, str]:
        d = tempfile.mkdtemp(prefix="hangar-key-")
        try:
            path = os.path.join(d, "key")
            subprocess.run(
                ["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "hangar-run", "-f", path],
                check=True, capture_output=True, timeout=30,
            )
            with open(path, encoding="ascii") as f:
                private = f.read()
            with open(path + ".pub", encoding="ascii") as f:
                public = host_key_line(f.read())
            return private, public
        finally:
            shutil.rmtree(d, ignore_errors=True)

    def _config(self, target: Target) -> str:
        with self._lock:
            d = self._dirs.get(target)
            if d:
                return os.path.join(d, "config")
            d = tempfile.mkdtemp(prefix="hgr-")
            self._dirs[target] = d
        known = os.path.join(d, "known_hosts")
        lines = [f"hangar-run {host_key_line(target.host_key)}"]
        common = [
            f"  IdentityFile {self.key_file}",
            "  IdentitiesOnly yes",
            f"  UserKnownHostsFile {known}",
            "  GlobalKnownHostsFile /dev/null",
            "  StrictHostKeyChecking yes",
            "  UpdateHostKeys no",
            "  BatchMode yes",
            f"  ConnectTimeout {self.connect_timeout}",
            "  ServerAliveInterval 15",
            "  ServerAliveCountMax 3",
            "  LogLevel ERROR",
        ]
        cfg = ["Host hangar-run", f"  HostName {target.address}", f"  User {target.user or self.user}",
               "  HostKeyAlias hangar-run", f"  ControlPath {d}/cm", "  ControlMaster auto",
               "  ControlPersist 120", *common]
        if self.jump:
            juser, jhost, jport = self.jump
            lines.append(f"hangar-jump {self.jump_host_key}")
            cfg.append("  ProxyJump hangar-jump")
            cfg += ["Host hangar-jump", f"  HostName {jhost}", f"  Port {jport}", "  HostKeyAlias hangar-jump", *common]
            if juser:
                cfg.append(f"  User {juser}")
        with open(known, "w", encoding="ascii") as f:
            f.write("\n".join(lines) + "\n")
        path = os.path.join(d, "config")
        with open(path, "w", encoding="ascii") as f:
            f.write("\n".join(cfg) + "\n")
        return path

    def run(self, target: Target, command: str, stdin: bytes | None = None, timeout: float = 60) -> tuple[int, str, str]:
        cfg = self._config(target)
        try:
            p = subprocess.run(
                [self.ssh, "-F", cfg, "hangar-run", command],
                input=stdin if stdin is not None else b"", capture_output=True, timeout=timeout,
            )
        except subprocess.TimeoutExpired as e:
            raise SshError(f"no answer in {timeout:.0f} s: {shlex.split(command)[0] if command else ''}") from e
        return p.returncode, p.stdout.decode(errors="replace"), p.stderr.decode(errors="replace")

    def wait_answering(self, target: Target, patience: float) -> None:
        deadline = time.monotonic() + patience
        last = ""
        while True:
            try:
                rc, _, err = self.run(target, "true", timeout=self.connect_timeout + 10)
                if rc == 0:
                    return
                last = err.strip().splitlines()[-1] if err.strip() else f"exit {rc}"
            except SshError as e:
                last = str(e)
            if time.monotonic() > deadline:
                raise SshError(f"{target.address} does not answer ssh after {patience:.0f} s: {last}")
            time.sleep(3)

    def forward(self, target: Target, pairs: list[tuple[str, str]]) -> Tunnel:
        cfg = self._config(target)
        argv = [self.ssh, "-F", cfg, "-N", "-o", "ExitOnForwardFailure=yes", "-o", "ControlMaster=no",
                "-o", "ControlPath=none"]
        for remote, local in pairs:
            argv += ["-R", f"{remote}:{local}"]
        argv.append("hangar-run")
        # its words go to a file, never a pipe nobody reads while it lives
        log = tempfile.TemporaryFile()
        proc = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=log)
        # a bind refused ends it at once (ExitOnForwardFailure): give it the
        # time a connection through the jump takes to say so
        try:
            proc.wait(3)
        except subprocess.TimeoutExpired:
            log.close()
            return _Process(proc)
        log.seek(0)
        err = log.read().decode(errors="replace").strip()
        log.close()
        raise SshError(f"the sockets' tunnel ended at once: {err or f'exit {proc.returncode}'}")

    def close(self, target: Target) -> None:
        with self._lock:
            d = self._dirs.pop(target, None)
        if not d:
            return
        subprocess.run([self.ssh, "-F", os.path.join(d, "config"), "-O", "exit", "hangar-run"],
                       capture_output=True, timeout=10, check=False)
        shutil.rmtree(d, ignore_errors=True)
