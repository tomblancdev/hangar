"""What the launcher is told: its instance file's config, else the process's
environment — read when it is used, never when the launcher is built.

Why the environment: a Dagster instance file is the platform's, one file for
every place it runs, and it must not name a network (a brain's address, a
gateway, a key's path are the deployment's). So the file names the class
and the platform's own word — a `default_size` — and the deployment gives the
rest as `HANGAR_*` variables to the processes that launch and end runs (the
daemon, the webserver that cancels). **No `HANGAR_URL` anywhere: every run
in-process**, as if the launcher were Dagster's own.

Why at use: a run's worker rebuilds the instance from its reference, the
launcher with it — what the launcher reads of the environment must never be
asked there (a `{env: …}` in the file would be resolved again on the run's
machine, which holds none of it). Nothing here is read until a run is
launched, checked or ended.
"""

from __future__ import annotations

import json
import os
import re
from collections.abc import Mapping
from dataclasses import dataclass, field
from typing import Any

# The keys, each with its variable. A key given in the instance file wins.
ENV = {
    "url": "HANGAR_URL",
    "token_file": "HANGAR_TOKEN_FILE",
    "zone": "HANGAR_ZONE",
    "default_size": "HANGAR_DEFAULT_SIZE",
    "machine_image": "HANGAR_MACHINE_IMAGE",
    "network": "HANGAR_NETWORK",
    "key_pair": "HANGAR_KEY_PAIR",
    "ssh_key_file": "HANGAR_SSH_KEY_FILE",
    "ssh_user": "HANGAR_SSH_USER",
    "jump": "HANGAR_JUMP",
    "jump_host_key": "HANGAR_JUMP_HOST_KEY",
    "disk_gb": "HANGAR_DISK_GB",
    "cpu": "HANGAR_CPU",
    "worker_image": "HANGAR_WORKER_IMAGE",
    "env_dir": "HANGAR_ENV_DIR",
    "syslog": "HANGAR_SYSLOG",
    "hosts": "HANGAR_HOSTS",
    "sockets": "HANGAR_SOCKETS",
    "max_runtime": "HANGAR_MAX_RUNTIME",
    "room_patience": "HANGAR_ROOM_PATIENCE",
    "room_every": "HANGAR_ROOM_EVERY",
    "ssh_patience": "HANGAR_SSH_PATIENCE",
}

SIZE = re.compile(r"^([1-9][0-9]{0,2})x([1-9][0-9]{0,3})$")


class SettingsError(ValueError):
    """A setting that cannot be used: said with its key and its variable."""


@dataclass(frozen=True)
class Size:
    cores: int
    memory_gb: int

    def __str__(self) -> str:
        return f"{self.cores}x{self.memory_gb}"


def parse_size(value: str, where: str) -> Size:
    """`<cores>x<G>`: 2x4, 8x16 — as ECS reads its cpu and memory tags."""
    m = SIZE.match(value.strip())
    if not m:
        raise SettingsError(f"{where}: {value!r} is no size — <cores>x<G>, as 2x4 or 8x16")
    cores, gb = int(m.group(1)), int(m.group(2))
    if cores > 128 or gb > 1024:
        raise SettingsError(f"{where}: {value!r} is past a machine's 128 cores or 1024 G")
    return Size(cores, gb)


@dataclass(frozen=True)
class Settings:
    url: str = ""
    token_file: str = ""
    zone: str = ""
    default_size: Size | None = None
    machine_image: str = ""
    network: str = ""
    key_pair: str = ""
    ssh_key_file: str = ""
    ssh_user: str = "debian"
    jump: str = ""
    jump_host_key: str = ""
    disk_gb: int = 16
    cpu: str = "host"
    worker_image: str = ""
    env_dir: str = ""
    syslog: str = ""
    hosts: Mapping[str, str] = field(default_factory=dict)
    sockets: Mapping[str, tuple[str, ...]] = field(default_factory=dict)
    # seconds: a run's longest life when it names no dagster/max_runtime
    max_runtime: int = 7200
    room_patience: float = 1800.0
    room_every: float = 60.0
    ssh_patience: float = 300.0

    @property
    def on(self) -> bool:
        """A brain is named: runs may get machines."""
        return bool(self.url)

    def need(self, *keys: str) -> None:
        """Refuses, naming each key and its variable, when one is missing."""
        missing = [k for k in keys if not getattr(self, k)]
        if missing:
            raise SettingsError(
                "the launcher needs " + ", ".join(f"{k} ({ENV[k]})" for k in missing)
            )

    @property
    def ssh_ready(self) -> bool:
        """Whatever reaches a run's machine is there: a key, and a jump with
        its host key pinned — or no jump at all."""
        if not self.ssh_key_file or not os.access(self.ssh_key_file, os.R_OK):
            return False
        return not self.jump or bool(self.jump_host_key)


def read(config: Mapping[str, Any], environ: Mapping[str, str] | None = None) -> Settings:
    """The settings, from the instance file's config first, then the
    environment's HANGAR_* variables."""
    env = os.environ if environ is None else environ
    raw: dict[str, Any] = {}
    for key, var in ENV.items():
        v = config.get(key)
        if v is None or v == "":
            v = env.get(var, "")
            if v == "":
                continue
            if key in ("hosts", "sockets"):
                try:
                    v = json.loads(v)
                except ValueError as e:
                    raise SettingsError(f"{var}: not JSON ({e})") from None
        raw[key] = v

    out: dict[str, Any] = {}
    for key in ("url", "token_file", "zone", "machine_image", "network", "key_pair", "ssh_key_file",
                "ssh_user", "jump", "jump_host_key", "cpu", "worker_image", "env_dir", "syslog"):
        if key in raw:
            out[key] = str(raw[key]).strip()
    if "default_size" in raw:
        out["default_size"] = parse_size(str(raw["default_size"]), f"default_size ({ENV['default_size']})")
    for key in ("disk_gb", "max_runtime"):
        if key in raw:
            out[key] = _number(raw[key], key, int)
    for key in ("room_patience", "room_every", "ssh_patience"):
        if key in raw:
            out[key] = _number(raw[key], key, float)
    if "hosts" in raw:
        hosts = raw["hosts"]
        if not isinstance(hosts, Mapping) or not all(isinstance(v, str) for v in hosts.values()):
            raise SettingsError(f"hosts ({ENV['hosts']}): a map of a name to its address")
        out["hosts"] = {str(k): v for k, v in hosts.items()}
    if "sockets" in raw:
        sockets = raw["sockets"]
        if not isinstance(sockets, Mapping) or not all(
            isinstance(v, list) and v and all(isinstance(p, str) and p.startswith("/") for p in v)
            for v in sockets.values()
        ):
            raise SettingsError(f"sockets ({ENV['sockets']}): a map of a name to a list of absolute socket paths")
        out["sockets"] = {str(k): tuple(v) for k, v in sockets.items()}
    if out.get("url", "").rstrip("/").endswith("/v1"):
        raise SettingsError("url: the brain's own address, without /v1")
    return Settings(**out)


def _number(v: Any, key: str, kind: type) -> Any:
    try:
        n = kind(v)
    except (TypeError, ValueError):
        raise SettingsError(f"{key} ({ENV[key]}): {v!r} is no number") from None
    if n <= 0:
        raise SettingsError(f"{key} ({ENV[key]}): {v!r} must be above zero")
    return n
