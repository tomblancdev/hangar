"""What a run's machine is handed: its worker's command, its wrapper, the
check that the sockets it is lent answer.

The wrapper travels with every run (never baked): what the launcher asks and
what the machine does can never be of two versions. It runs the worker in
Docker, says two lines of its own to the lake's syslog address — a machine
of a minute carries no shipper —, and powers the machine off whatever the
worker's end: the power-off is the machine's give-back, the brain deletes it
(`on_poweroff: delete`).
"""

from __future__ import annotations

import io
import re
import shlex
import tarfile
import urllib.parse
from collections.abc import Mapping, Sequence

RUN_DIR = "/run/hangar"  # a tmpfs: nothing of the run's is written to the disk
WORKER = "hangar-run"  # the worker's container, and the wrapper's unit

WRAPPER = r"""#!/bin/bash
# dagster-hangar: a Dagster run's worker, started on this machine of its own
# — waited for, then the machine's power-off, whatever the worker's end.
# Handed over with the run.
#   run.sh <run id> [<syslog proto> <syslog host> <syslog port>]
set -u
run_id=$1 proto=${2:-} host=${3:-} port=${4:-}
say() {
	[ -n "$host" ] || return 0
	logger --server "$host" --port "$port" "--$proto" --rfc5424 --tag dagster-hangar -- "$*" 2>/dev/null || true
}
say "run $run_id: its worker started on $(hostname)"
code=$(docker wait hangar-run 2>/dev/null) || code=unknown
oom=$(docker inspect -f '{{.State.OOMKilled}}' hangar-run 2>/dev/null) || oom=unknown
say "run $run_id: its worker ended, exit $code, oom $oom"
sync
systemctl poweroff
"""

# A gRPC server is an HTTP/2 one: it answers a connection's preface with its
# SETTINGS frame. A socket carried by ssh whose far end is gone accepts, then
# closes — connecting alone would say it answers.
PROBE = r"""import socket, sys
hello = b"PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n" + b"\x00\x00\x00\x04\x00\x00\x00\x00\x00"
bad = []
for path in sys.argv[1:]:
    head = b""
    try:
        s = socket.socket(socket.AF_UNIX)
        s.settimeout(5)
        s.connect(path)
        s.sendall(hello)
        while len(head) < 9:
            chunk = s.recv(9 - len(head))
            if not chunk:
                break
            head += chunk
        s.close()
    except OSError:
        pass
    if len(head) < 9 or head[3] != 4:
        bad.append(path)
print(" ".join(bad))
sys.exit(1 if bad else 0)
"""


# The run's env is read inside its container, from the file mounted out of
# the machine's tmpfs: given to Docker (--env-file, -e), it would be written
# into the container's config on the disk. Docker's env-file form: KEY=VALUE
# as written, # a comment, a line without = skipped.
READ_ENV = r"""import os, re, sys
for line in open("/run/hangar/run.env", encoding="utf-8", errors="surrogateescape"):
    line = line.rstrip("\r\n")
    if line.strip() and not line.lstrip().startswith("#") and "=" in line:
        key, value = line.split("=", 1)
        if re.fullmatch(r"[A-Za-z_][A-Za-z0-9_.-]*", key.lstrip()):
            os.environ[key.lstrip()] = value
os.execvp(sys.argv[1], sys.argv[1:])
"""
# a key READ_ENV takes; the launcher refuses a file with any other at launch
ENV_KEY = re.compile(r"[A-Za-z_][A-Za-z0-9_.-]*")


def syslog_parts(address: str) -> tuple[str, str, str]:
    """udp://host:port (Docker's syslog-address form) → (proto, host, port)."""
    u = urllib.parse.urlsplit(address)
    if u.scheme not in ("udp", "tcp") or not u.hostname:
        raise ValueError(f"syslog {address!r}: udp://host:port or tcp://host:port")
    return u.scheme, u.hostname, str(u.port or 514)


def docker_argv(
    run_id: str,
    image: str,
    command: Sequence[str],
    *,
    env_file: bool,
    syslog: str = "",
    hosts: Mapping[str, str] | None = None,
    socket_dirs: Sequence[str] = (),
    python: str = "python",
) -> list[str]:
    """`docker run -d …` for the worker: its env from the machine's memory
    (read inside the container), its lines to syslog in RFC 5424, the lab's
    names pinned (the network's resolver knows none of them), the sockets it
    is lent mounted."""
    # --init: a small init as its PID 1 forwards docker stop's SIGTERM to the
    # worker and reaps the steps' processes
    argv = ["run", "-d", "--init", "--name", WORKER, "--label", f"dagster/run_id={run_id}"]
    if env_file:
        argv += ["-v", f"{RUN_DIR}/run.env:{RUN_DIR}/run.env:ro"]
    if syslog:
        argv += ["--log-driver", "syslog", "--log-opt", f"syslog-address={syslog}",
                 "--log-opt", "syslog-format=rfc5424", "--log-opt", "tag=dagster-run"]
    for name, address in sorted((hosts or {}).items()):
        argv += ["--add-host", f"{name}:{address}"]
    for d in sorted(set(socket_dirs)):
        argv += ["-v", f"{d}:{d}"]
    argv.append(image)
    if env_file:
        argv += [python, "-c", READ_ENV]
    argv += list(command)
    return argv


def bundle(argv: Sequence[str], env: bytes | None) -> bytes:
    """The run's files as one archive, for one ssh call: the docker argv
    (NUL-separated, read by bash's mapfile), the env, the wrapper."""
    files = {"argv": b"\0".join(a.encode() for a in argv) + b"\0", "run.sh": WRAPPER.encode()}
    if env is not None:
        files["run.env"] = env
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w") as tar:
        for name, data in files.items():
            info = tarfile.TarInfo(name)
            info.size, info.uid, info.gid = len(data), 0, 0
            # the env is read by the worker's own user, whoever its image
            # names; the machine holds no other person (its one user is a sudoer)
            info.mode = 0o644 if name == "run.env" else 0o600
            tar.addfile(info, io.BytesIO(data))
    return buf.getvalue()


def start_command(run_id: str, syslog: str = "") -> str:
    """Unpacks the run's files into its tmpfs, starts the worker — a refusal
    of Docker's comes back on this very call — and the wrapper as a unit of
    its own: the ssh call returns at once, the worker runs on."""
    wrapper = ["bash", f"{RUN_DIR}/run.sh", run_id]
    if syslog:
        wrapper += list(syslog_parts(syslog))
    inner = (
        f"install -d -m 0700 {RUN_DIR} && tar -x -o -C {RUN_DIR} && "
        f"mapfile -d '' -t argv < {RUN_DIR}/argv && docker \"${{argv[@]}}\" >/dev/null && "
        f"systemd-run --unit {WORKER} --collect --quiet " + shlex.join(wrapper)
    )
    return "sudo bash -c " + shlex.quote(inner)


def inspect_command() -> str:
    """The worker's state and exit, as Docker says them."""
    return "sudo docker inspect -f '{{.State.Status}} {{.State.ExitCode}}' " + WORKER


def stop_command(grace: int = 30) -> str:
    """The worker's SIGTERM — Dagster's own cancel: the worker writes its
    run's RUN_CANCELED; the wrapper then powers the machine off."""
    return f"sudo docker stop -t {int(grace)} {WORKER}"


def probe_command(paths: Sequence[str]) -> str:
    return "python3 - " + shlex.join(paths)


def socket_dirs(paths: Sequence[str]) -> list[str]:
    return sorted({p.rsplit("/", 1)[0] or "/" for p in paths})


def prepare_dirs_command(paths: Sequence[str], user: str) -> str:
    """The sockets' directories on the machine, the ssh user's: sshd binds a
    forwarded socket as that user."""
    return "sudo install -d -m 0755 -o " + shlex.quote(user) + " " + shlex.join(socket_dirs(paths))
