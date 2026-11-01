"""The bake — the image a run's machine is born from.

A machine from a base image (`@debian-13`: the newest the operator's
schedule baked), Docker by Docker's own packages (its key checked by its
digest), every image the runs will use pulled, sshd told to lend unix
sockets to anyone on the machine (`StreamLocalBindMask 0111`); then what
must differ between machines cleared — cloud-init's memory, the machine id,
the host keys, `authorized_keys` —, the machine stopped and saved as an
image of the brain's; the builder deleted; of the images of its family,
the newest `keep` kept, an older one deleted once no machine is born from
it (the brain refuses it until then — said, and left for the next bake).
"""

from __future__ import annotations

import re
import shlex
import sys
import time
import uuid
from collections.abc import Callable, Sequence
from dataclasses import dataclass

from dagster_hangar.brain import Brain, Problem, Unreachable
from dagster_hangar.settings import Settings, Size
from dagster_hangar.ssh import OpenSSH, Shell, SshError, Target

FAMILY_TAG = "dagster-hangar/family"
BAKE_TAG = "dagster-hangar/bake"  # one bake's builder, found again whatever its create answered
# the registry's login for the pulls lives in the tmpfs: the disk becomes the image
AUTH_DIR = "/run/dagster-hangar-auth"
LOST_WINDOW = 120.0
DOCKER_KEY_URL = "https://download.docker.com/linux/debian/gpg"
# Docker's apt key as Docker serves it, by its digest: a key that is not this
# one stops the bake
DOCKER_KEY_SHA256 = "1500c1f56fa9e26b9b8f42452a553675796ade0807cdce11975eb98170b3a570"

INSTALL = r"""set -eu
export DEBIAN_FRONTEND=noninteractive
apt-get -q update
apt-get -q install -y --no-install-recommends ca-certificates curl
install -d -m 0755 /etc/apt/keyrings
curl -fsSL "$1" -o /etc/apt/keyrings/docker.asc
echo "$2  /etc/apt/keyrings/docker.asc" | sha256sum -c --quiet - || {
	echo "Docker's key is not the one its digest names" >&2; rm -f /etc/apt/keyrings/docker.asc; exit 1; }
. /etc/os-release
echo "deb [signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian $VERSION_CODENAME stable" \
	>/etc/apt/sources.list.d/docker.list
apt-get -q update
apt-get -q install -y --no-install-recommends docker-ce docker-ce-cli containerd.io
systemctl enable --now docker
cat >/etc/ssh/sshd_config.d/dagster-hangar.conf <<'EOF'
# dagster-hangar: a run's sockets, carried here by the launcher's ssh -R,
# are reached by its worker's container whatever its user
AllowStreamLocalForwarding yes
StreamLocalBindMask 0111
StreamLocalBindUnlink yes
EOF
"""

CLEAN = r"""set -eu
rm -rf /run/dagster-hangar-auth
apt-get clean
cloud-init clean --logs --seed --machine-id >/dev/null 2>&1 || cloud-init clean --logs
rm -f /etc/ssh/ssh_host_*_key /etc/ssh/ssh_host_*_key.pub /var/lib/systemd/random-seed
for f in /root/.ssh/authorized_keys /home/*/.ssh/authorized_keys; do [ -e "$f" ] && : >"$f"; done
journalctl --rotate --vacuum-time=1s >/dev/null 2>&1 || true
sync
"""


class BakeError(Exception):
    pass


@dataclass
class Bake:
    name: str
    images: Sequence[str]
    family: str = ""
    keep: int = 2
    size: Size = Size(2, 4)
    from_image_id: str = "@debian-13"
    from_image: str = ""
    registry_auth: bytes | None = None
    docker_key_url: str = DOCKER_KEY_URL
    docker_key_sha256: str = DOCKER_KEY_SHA256

    def __post_init__(self) -> None:
        if not re.fullmatch(r"[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?", self.name):
            raise BakeError(f"{self.name!r}: an image's name is a-z, 0-9 and -, 1 to 63 characters")
        self.family = self.family or self.name.split("-", 1)[0]
        if self.keep < 1:
            raise BakeError("keep at least the image just baked: 1 or more")


def run(bake: Bake, s: Settings, brain: Brain, sh: Shell, say: Callable[[str], None] = print) -> str:
    """Bakes; returns the new image's id."""
    s.need("zone", "key_pair", "ssh_key_file")
    t0 = time.monotonic()

    def at() -> str:
        return f"{time.monotonic() - t0:.0f} s"

    private, public = sh.host_key()
    user_data = "#cloud-config\n" + _json({
        "ssh_deletekeys": True, "ssh_genkeytypes": [],
        "ssh_keys": {"ed25519_private": private, "ed25519_public": public},
    })
    spec = {
        "kind": "vm", "class": "spot", "resume": False, "cores": bake.size.cores, "memory_gb": bake.size.memory_gb,
        "disk_gb": s.disk_gb, "key_pairs": [s.key_pair], "user_data": user_data,
        "on_poweroff": "stop", "for": "2h", "at_end": "delete",
    }
    if bake.from_image:
        spec["image"] = bake.from_image
    else:
        spec["image_id"] = bake.from_image_id
    if s.cpu and s.cpu != "zone":
        spec["cpu"] = s.cpu
    if s.network:
        spec["network"] = s.network
    builder = ""
    target = None
    this = uuid.uuid4().hex[:16]
    try:
        res, op = brain.create("machine", s.zone, spec, name=_short(f"bake-{bake.name}"),
                               description=f"dagster-hangar: baking {bake.name}",
                               tags={FAMILY_TAG: bake.family, BAKE_TAG: this})
        builder = res["id"]
        op = brain.wait(op["id"], 600)
        if op.get("state") != "succeeded":
            raise BakeError(f"the builder {builder} was not born: {op.get('error') or op.get('state')}")
        say(f"born {builder} at {at()}")
        m = brain.get(builder) or {}
        obs = m.get("observed") or {}
        addr = obs.get("address") or next(iter(obs.get("addresses") or []), "")
        if not addr:
            raise BakeError(f"the builder {builder} has no address")
        target = Target(addr.split("/")[0], public, s.ssh_user)
        sh.wait_answering(target, s.ssh_patience)
        say(f"answering at {at()}")
        _sudo(sh, target, INSTALL, [bake.docker_key_url, bake.docker_key_sha256], "Docker", timeout=900)
        say(f"Docker at {at()}")
        if bake.registry_auth is not None:
            rc, _, err = sh.run(target, f"sudo install -D -m 0600 /dev/stdin {AUTH_DIR}/config.json",
                                stdin=bake.registry_auth, timeout=60)
            if rc != 0:
                raise BakeError(f"the registry's login: {err.strip() or rc}")
        for image in bake.images:
            _sudo(sh, target, f'set -eu; [ -d {AUTH_DIR} ] && export DOCKER_CONFIG={AUTH_DIR}; docker pull -q "$1" >/dev/null',
                  [image], f"the pull of {image}", timeout=1800)
        say(f"{len(bake.images)} images pulled at {at()}")
        _sudo(sh, target, CLEAN, [], "the clean", timeout=300)
        try:
            sh.close(target)
        except Exception:
            pass
        target = None
        op = brain.wait(brain.act(builder, "stop")["id"], 600)
        if op.get("state") != "succeeded":
            raise BakeError(f"the builder {builder} did not stop: {op.get('error') or op.get('state')}")
        res, op = brain.create("image", s.zone, {"machine": builder}, name=bake.name,
                               description=f"dagster-hangar: {len(bake.images)} images", tags={FAMILY_TAG: bake.family})
        image_id = res["id"]
        op = brain.wait(op["id"], 1800)
        if op.get("state") != "succeeded":
            raise BakeError(f"the image {bake.name} was not saved: {op.get('error') or op.get('state')}")
        say(f"saved {bake.name} ({image_id}) at {at()}")
    finally:
        if target is not None:
            try:
                sh.close(target)
            except Exception:
                pass
        # whatever happened, its builder goes: the one its create named, or
        # any carrying this bake's tag (a create applied whose answer was lost)
        try:
            for b in [builder] if builder else _search(brain, this, LOST_WINDOW):
                if not brain.end(b, 900):
                    say(f"the builder {b} is not deleted yet: it ends at its until")
        except Exception as e:
            say(f"the builder was not ended ({e}): it ends at its until")
    prune(brain, bake.family, bake.keep, say)
    return image_id


def _search(brain: Brain, this: str, window: float) -> list[str]:
    """A builder whose create's answer was lost: searched by this bake's tag
    for a while — a create still being planned commits after the client gave
    up on it."""
    deadline = time.monotonic() + window
    while True:
        try:
            found = [m["id"] for m in brain.list("machine", tags={BAKE_TAG: this}, states=["creating", "ready", "updating"])]
        except (Problem, Unreachable):
            found = []
        if found or time.monotonic() >= deadline:
            return found
        time.sleep(min(10.0, max(0.5, window / 12)))


def prune(brain: Brain, family: str, keep: int, say: Callable[[str], None] = print) -> None:
    """Of a family's images, the newest `keep` stay; an older one is deleted
    once no machine is born from it — refused until then, and said."""
    images = [i for i in brain.list("image", tags={FAMILY_TAG: family}, states=["ready", "creating"])]
    images.sort(key=lambda i: i.get("created_at", ""), reverse=True)
    for old in images[keep:]:
        name = old.get("name") or old["id"]
        try:
            op = brain.wait(brain.delete(old["id"])["id"], 300)
        except Problem as p:
            say(f"kept {name}: {p.detail}")
            continue
        if op.get("state") == "succeeded":
            say(f"deleted {name}, an older one")
        else:
            say(f"kept {name}: {op.get('error') or op.get('state')}")


def _sudo(sh: Shell, target: Target, script: str, args: list[str], what: str, timeout: float) -> None:
    rc, out, err = sh.run(target, "sudo bash -s -- " + shlex.join(args), stdin=script.encode(), timeout=timeout)
    if rc != 0:
        tail = "\n".join((err.strip() or out.strip()).splitlines()[-5:])
        raise BakeError(f"{what} failed (exit {rc}): {tail}")


def _short(name: str) -> str:
    name = re.sub(r"[^a-z0-9-]", "-", name.lower())[:63].strip("-")
    return name


def _json(v: object) -> str:
    import json

    return json.dumps(v, indent=1) + "\n"


def main(argv: Sequence[str], s: Settings) -> int:
    import argparse

    from dagster_hangar.settings import parse_size

    p = argparse.ArgumentParser(
        prog="python -m dagster_hangar bake",
        description="Bake the image a run's machine is born from: Docker and IMAGES on a machine "
                    "of Le Hangar, saved as NAME. Reads the launcher's HANGAR_* settings.",
    )
    p.add_argument("--name", required=True, help="the image's name, e.g. lake-0.9.0")
    p.add_argument("images", nargs="*", metavar="IMAGE", help="every image the runs will use")
    p.add_argument("--family", default="", help="the images it replaces (default: NAME up to its first -)")
    p.add_argument("--keep", type=int, default=2, help="how many of the family's newest stay (default 2)")
    p.add_argument("--size", default="2x4", help="the builder's <cores>x<G> (default 2x4)")
    p.add_argument("--from-image-id", default="@debian-13", help="the image it starts from (default @debian-13)")
    p.add_argument("--from-image", default="", help="or the operator's image name it starts from")
    p.add_argument("--registry-auth", default="", help="a Docker config.json for the pulls (removed before the save)")
    a = p.parse_args(argv)
    auth = None
    if a.registry_auth:
        with open(a.registry_auth, "rb") as f:
            auth = f.read()
    try:
        bake = Bake(a.name, a.images, a.family, a.keep, parse_size(a.size, "--size"), a.from_image_id, a.from_image, auth)
        s.need("url", "token_file")
        brain = Brain.from_token_file(s.url, s.token_file)
        sh = OpenSSH(s.ssh_key_file, s.ssh_user, s.jump, s.jump_host_key)
        image = run(bake, s, brain, sh, say=lambda m: print(f"bake: {m}", file=sys.stderr))
    except (BakeError, Problem, Unreachable, SshError, ValueError, OSError) as e:
        print(f"bake: {e}", file=sys.stderr)
        return 1
    print(image)
    return 0
