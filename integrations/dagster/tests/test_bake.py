"""The bake against the real brain on the fake engine: its builder, what it
runs there, the image saved, the family pruned."""

from __future__ import annotations

from dagster_hangar import bake
from dagster_hangar.settings import read
from tests.conftest import Hangar, wait_for


def settings(hangar: Hangar, lake: dict[str, str], key_file: str):
    return read({"url": hangar.url, "token_file": hangar.tokens["lake"], "zone": "tower", "network": lake["network"],
                 "key_pair": lake["key_pair"], "ssh_key_file": key_file, "ssh_patience": 10.0}, {})


def test_a_bake_saves_an_image_and_keeps_the_newest(hangar, lake, shell, key_file):
    s = settings(hangar, lake, key_file)
    b = hangar.brain()
    said: list[str] = []
    images = ["ghcr.io/example/flows:1", "ghcr.io/example/platform:1"]
    first = bake.run(bake.Bake("lake-1", images, from_image="debian-13"), s, b, shell, said.append)
    img = b.get(first)
    assert img["name"] == "lake-1" and img["tags"][bake.FAMILY_TAG] == "lake" and img["state"] == "ready"
    # what it ran on its builder, in order: Docker by its key's digest, the
    # pulls, the clean
    runs = [(c[1][1], c[1][2]) for c in shell.calls if c[0] == "run"]
    install = next(i for i, (cmd, stdin) in enumerate(runs) if stdin and b"docker-ce" in stdin)
    assert bake.DOCKER_KEY_SHA256 in runs[install][0] and bake.DOCKER_KEY_URL in runs[install][0]
    assert b"StreamLocalBindMask 0111" in runs[install][1]
    pulls = [cmd for cmd, _ in runs if "ghcr.io/example/" in cmd]
    assert [p.rsplit(" ", 1)[1] for p in pulls] == images
    clean = next(i for i, (cmd, stdin) in enumerate(runs) if stdin and b"cloud-init clean" in stdin)
    assert install < clean and b"authorized_keys" in runs[clean][1] and b"ssh_host_" in runs[clean][1]
    # its builder: stopped by the brain before the save, then deleted
    builders = b.list("machine", tags={bake.FAMILY_TAG: "lake"}, states=["creating", "ready", "deleting", "deleted"])
    assert len(builders) == 1
    wait_for("the builder deleted", lambda: b.get(builders[0]["id"])["state"] == "deleted", timeout=30)
    assert builders[0]["spec"]["on_poweroff"] == "stop" and builders[0]["spec"]["at_end"] == "delete"

    bake.run(bake.Bake("lake-2", images, from_image="debian-13"), s, b, shell, said.append)
    third = bake.run(bake.Bake("lake-3", images, from_image="debian-13"), s, b, shell, said.append)
    names = sorted(i["name"] for i in b.list("image", tags={bake.FAMILY_TAG: "lake"}, states=["ready"]))
    assert names == ["lake-2", "lake-3"], said
    assert "deleted lake-1, an older one" in said

    # an older one a machine was born from stays, said, until it is free
    m, op = b.create("machine", "tower", {"image_id": "lake-2", "cores": 1, "memory_gb": 1, "network": lake["network"]})
    assert b.wait(op["id"], 20)["state"] == "succeeded"
    bake.run(bake.Bake("lake-4", images, from_image="debian-13"), s, b, shell, said.append)
    names = sorted(i["name"] for i in b.list("image", tags={bake.FAMILY_TAG: "lake"}, states=["ready"]))
    assert names == ["lake-2", "lake-3", "lake-4"], said
    assert any(line.startswith("kept lake-2:") for line in said), said
    assert third != first


def test_the_registrys_login_never_reaches_the_disk(hangar, lake, shell, key_file):
    s = settings(hangar, lake, key_file)
    bake.run(bake.Bake("lake-1", ["ghcr.io/example/flows:1"], from_image="debian-13", registry_auth=b'{"auths": {}}'),
             s, hangar.brain(), shell, lambda m: None)
    runs = [(c[1][1], c[1][2] or b"") for c in shell.calls if c[0] == "run"]
    login = [cmd for cmd, stdin in runs if stdin == b'{"auths": {}}']
    assert login == [f"sudo install -D -m 0600 /dev/stdin {bake.AUTH_DIR}/config.json"] and bake.AUTH_DIR.startswith("/run/")
    pull = next(stdin for cmd, stdin in runs if "ghcr.io/example/flows:1" in cmd)
    assert f"export DOCKER_CONFIG={bake.AUTH_DIR}".encode() in pull
    assert f"rm -rf {bake.AUTH_DIR}".encode() in bake.CLEAN.encode()


def test_the_zones_processor_names_none(hangar, lake, shell, key_file):
    s = read({"url": hangar.url, "token_file": hangar.tokens["lake"], "zone": "tower", "network": lake["network"],
              "key_pair": lake["key_pair"], "ssh_key_file": key_file, "cpu": "zone"}, {})
    b = hangar.brain()
    bake.run(bake.Bake("lake-5", [], from_image="debian-13"), s, b, shell, lambda m: None)
    builder = b.list("machine", tags={bake.FAMILY_TAG: "lake"}, states=["deleting", "deleted"])[0]
    assert "cpu" not in builder["spec"]


def test_a_bake_that_raises_leaves_no_builder(hangar, lake, shell, key_file, monkeypatch):
    s = settings(hangar, lake, key_file)
    b = hangar.brain()
    real = shell.run

    def broken(target, command, stdin=None, timeout=60):
        if stdin and b"docker-ce" in stdin:
            raise RuntimeError("the ssh connection broke")
        return real(target, command, stdin, timeout)

    monkeypatch.setattr(shell, "run", broken)
    try:
        bake.run(bake.Bake("lake-8", ["ghcr.io/example/flows:1"], from_image="debian-13"), s, b, shell, lambda m: None)
    except RuntimeError:
        pass
    else:
        raise AssertionError("a broken bake baked")
    builders = b.list("machine", tags={bake.FAMILY_TAG: "lake"}, states=["creating", "ready", "deleting", "deleted"])
    assert builders and all(b.get(m["id"])["state"] == "deleted" for m in builders)


def test_a_bake_that_fails_leaves_no_builder(hangar, lake, shell, key_file):
    s = settings(hangar, lake, key_file)
    b = hangar.brain()
    shell.answers["ghcr.io/example/none:1"] = (1, "", "Error response from daemon: manifest unknown\n")
    try:
        bake.run(bake.Bake("lake-9", ["ghcr.io/example/none:1"], from_image="debian-13"), s, b, shell, lambda m: None)
    except bake.BakeError as e:
        assert "the pull of ghcr.io/example/none:1 failed" in str(e) and "manifest unknown" in str(e)
    else:
        raise AssertionError("a pull that failed baked")
    builders = b.list("machine", tags={bake.FAMILY_TAG: "lake"}, states=["creating", "ready", "deleting", "deleted"])
    wait_for("the builder deleted", lambda: all(b.get(m["id"])["state"] == "deleted" for m in builders), timeout=30)
    assert not b.list("image", tags={bake.FAMILY_TAG: "lake"}, states=["ready", "creating"])
