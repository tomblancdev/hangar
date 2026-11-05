"""The launcher against a real brain on the fake engine, in Dagster's own
instance. Each test reads what the brain holds and what Dagster wrote."""

from __future__ import annotations

import io
import os
import re
import tarfile
import textwrap
import time
from datetime import datetime, timedelta, timezone

import pytest
from dagster._core.code_pointer import FileCodePointer
from dagster._core.events import DagsterEvent, DagsterEventType
from dagster._core.launcher import WorkerStatus
from dagster._core.origin import JobPythonOrigin, RepositoryPythonOrigin
from dagster._core.remote_origin import GrpcServerCodeLocationOrigin, RemoteJobOrigin, RemoteRepositoryOrigin
from dagster._core.storage.dagster_run import DagsterRunStatus
from dagster._core.test_utils import create_run_for_test

from dagster_hangar import worker
from dagster_hangar.brain import Brain, Problem, Unreachable
from dagster_hangar.launcher import LAUNCHED_TAG, MACHINE_TAG, LaunchFailed
from tests.conftest import PUBLIC_KEY, Hangar, dagster_instance, wait_for

IMAGE = "ghcr.io/example/flows:1"


def config(hangar: Hangar, lake: dict[str, str], key_file: str, subject: str = "lake", **more) -> dict:
    c = {
        "url": hangar.url, "token_file": hangar.tokens[subject], "zone": "tower",
        "machine_image": lake["image"], "network": lake["network"], "key_pair": lake["key_pair"],
        "ssh_key_file": key_file, "default_size": "2x4",
        "syslog": "udp://192.0.2.20:514", "hosts": {"s3.example.org": "192.0.2.10"},
        "room_every": 0.5, "room_patience": 60.0, "ssh_patience": 10.0,
    }
    c.update(more)
    return {k: v for k, v in c.items() if v is not None}


def make_run(instance, tags=None, location="flows", image=IMAGE):
    repo = RepositoryPythonOrigin(
        executable_path="/usr/local/bin/python",
        code_pointer=FileCodePointer("/opt/flows/definitions.py", "defs", working_directory="/opt/flows"),
        container_image=image, entry_point=["dagster"],
    )
    remote = RemoteJobOrigin(
        RemoteRepositoryOrigin(
            GrpcServerCodeLocationOrigin(host="localhost", socket=f"/opt/dagster/grpc/{location}.sock",
                                         location_name=location),
            "__repository__",
        ),
        "tiny",
    )
    return create_run_for_test(instance, job_name="tiny", tags=tags or {}, remote_job_origin=remote,
                               job_code_origin=JobPythonOrigin("tiny", repo))


def launch(instance, run):
    instance.launch_run(run.run_id, None)
    launcher = instance.run_launcher
    wait_for("the launch's thread ended", lambda: run.run_id not in launcher._threads, timeout=60)
    return instance.get_run_by_id(run.run_id)


def messages(instance, run_id, kind=None):
    """What the run's page says, the launcher's engine events without the
    prefix Dagster gives them."""
    out = []
    for e in instance.all_logs(run_id):
        if kind is None or (e.dagster_event and e.dagster_event.event_type == kind):
            m = e.message or (e.dagster_event.message if e.dagster_event else "") or ""
            out.append(m.removeprefix("[HangarRunLauncher] "))
    return out


def started(instance, run):
    """The worker begins: Dagster's RUN_START, as a real one writes it."""
    instance.report_dagster_event(DagsterEvent(DagsterEventType.RUN_START.value, job_name=run.job_name), run.run_id)
    run = instance.get_run_by_id(run.run_id)
    assert run.status == DagsterRunStatus.STARTED
    return run


def machines_of(hangar: Hangar, run_id: str, states=None):
    return hangar.brain().list("machine", tags={"dagster/run_id": run_id}, states=states)


def bundle_files(stdin: bytes) -> dict[str, bytes]:
    with tarfile.open(fileobj=io.BytesIO(stdin)) as tar:
        return {m.name: tar.extractfile(m).read() for m in tar.getmembers()}


# ---- its word ---------------------------------------------------------------


def test_every_run_gets_a_machine_of_the_default_size(hangar, lake, shell, key_file):
    with dagster_instance(config(hangar, lake, key_file), shell) as instance:
        run = launch(instance, make_run(instance, tags={"dagster/max_runtime": "1800"}))
        assert run.status == DagsterRunStatus.STARTING, messages(instance, run.run_id)
        machine = run.tags[MACHINE_TAG]
        m = hangar.brain().get(machine)
        spec = m["spec"]
        assert (spec["kind"], spec["class"], spec["cpu"], spec["resume"]) == ("vm", "spot", "host", False)
        assert (spec["cores"], spec["memory_gb"], spec["disk_gb"]) == (2, 4, 16)
        assert (spec["on_poweroff"], spec["at_end"]) == ("delete", "delete")
        assert spec["network"] == lake["network"] and spec["key_pairs"] == [lake["key_pair"]]
        assert spec["image_id"]
        until = datetime.fromisoformat(spec["until"].replace("Z", "+00:00"))
        left = until - datetime.now(timezone.utc)
        assert timedelta(minutes=38) < left <= timedelta(minutes=40), left  # max_runtime + 10 min
        assert m["tags"]["dagster/run_id"] == run.run_id and m["tags"]["dagster/job"] == "tiny"
        assert m["name"] == "run-" + run.run_id[:8]
        ops = hangar.brain().call("GET", "/v1/operations", query={"resource": machine})[1]["operations"]
        assert [o["client_token"] for o in ops if o["kind"] == "create"] == [run.run_id]
        assert "ed25519_public" in spec["user_data"]

        command, stdin = shell.started()
        assert "systemd-run --unit hangar-run" in command
        files = bundle_files(stdin)
        argv = files["argv"].split(b"\0")[:-1]
        argv = [a.decode() for a in argv]
        assert argv[:5] == ["run", "-d", "--init", "--name", "hangar-run"]
        assert argv[argv.index("--add-host") + 1] == "s3.example.org:192.0.2.10"
        assert "syslog-format=rfc5424" in argv and "syslog-address=udp://192.0.2.20:514" in argv
        i = argv.index(IMAGE)
        assert argv[i + 1:i + 4] == ["dagster", "api", "execute_run"]
        assert run.run_id in argv[i + 4]
        assert "run.env" not in files
        launched = [m for m in messages(instance, run.run_id) if m.startswith("launched on")]
        assert launched and f"launched on {machine} (2x4): born " in launched[0], launched  # no wait: none said


def test_a_tag_names_a_bigger_machine(hangar, lake, shell, key_file):
    with dagster_instance(config(hangar, lake, key_file), shell) as instance:
        run = launch(instance, make_run(instance, tags={"hangar/size": "4x8"}))
        spec = hangar.brain().get(run.tags[MACHINE_TAG])["spec"]
        assert (spec["cores"], spec["memory_gb"]) == (4, 8)
        # no max_runtime of its own: the launcher's default and ten minutes
        until = datetime.fromisoformat(spec["until"].replace("Z", "+00:00"))
        assert timedelta(minutes=128) < until - datetime.now(timezone.utc) <= timedelta(minutes=130)


def test_a_size_that_is_none_fails_at_once(hangar, lake, shell, key_file):
    with dagster_instance(config(hangar, lake, key_file), shell) as instance:
        run = make_run(instance, tags={"hangar/size": "big"})
        with pytest.raises(Exception, match="is no size"):
            instance.launch_run(run.run_id, None)
        assert instance.get_run_by_id(run.run_id).status == DagsterRunStatus.FAILURE
        assert not machines_of(hangar, run.run_id)


DEFINITIONS = """
from dagster import Definitions, job, op

@op
def hello():
    return 1

@job
def tiny():
    hello()

defs = Definitions(jobs=[tiny])
"""


@pytest.mark.parametrize("why", ["no brain named", "no size"])
def test_in_process_when_no_machine_is_asked(hangar, lake, shell, key_file, tmp_path, why):
    from dagster._core.workspace.context import WorkspaceProcessContext
    from dagster._core.workspace.load_target import PythonFileTarget

    path = tmp_path / "definitions.py"
    path.write_text(textwrap.dedent(DEFINITIONS))
    cfg = config(hangar, lake, key_file, default_size=None) if why == "no size" else {"default_size": "2x4"}
    with dagster_instance(cfg, shell, environ={}) as instance:
        target = PythonFileTarget(python_file=str(path), attribute=None, working_directory=str(tmp_path),
                                  location_name="flows")
        with WorkspaceProcessContext(instance, target) as ctx:
            request = ctx.create_request_context()
            remote = request.get_code_location("flows").get_repository("__repository__").get_full_job("tiny")
            run = create_run_for_test(instance, job_name="tiny", remote_job_origin=remote.get_remote_origin(),
                                      job_code_origin=remote.get_python_origin(), job_snapshot=remote.job_snapshot)
            instance.launch_run(run.run_id, request)
            wait_for("the run in-process ended",
                     lambda: instance.get_run_by_id(run.run_id).status in (DagsterRunStatus.SUCCESS, DagsterRunStatus.FAILURE),
                     timeout=90)
            run = instance.get_run_by_id(run.run_id)
            assert run.status == DagsterRunStatus.SUCCESS, messages(instance, run.run_id)
            health = instance.run_launcher.check_run_worker_health(run)
            assert health.status == WorkerStatus.RUNNING and "in-process" in health.msg
    assert not shell.calls
    assert not machines_of(hangar, run.run_id, states=["creating", "ready", "deleted"])


# ---- the room, the limit ----------------------------------------------------


def test_a_room_refusal_keeps_no_client_token(hangar, lake):
    """The build's first read: a 409 room writes nothing of the ask, so the
    same client token asked again is a new ask — not a replay of the
    refusal — and an ask that reached the brain twice is one machine."""
    b = hangar.brain()
    spec = {"image_id": lake["image"], "cores": 1, "memory_gb": 1, "network": lake["network"], "class": "spot"}
    hangar.held(True)
    with pytest.raises(Exception) as refused:
        b.create("machine", "tower", spec, client_token="run-1")
    assert refused.value.status == 409 and refused.value.kind == "room"
    hangar.held(False)
    status, out = b.call("POST", "/v1/resources", {"type": "machine", "zone": "tower", "spec": spec, "client_token": "run-1"})
    assert status == 202
    again, twice = b.call("POST", "/v1/resources", {"type": "machine", "zone": "tower", "spec": spec, "client_token": "run-1"})
    assert again == 200 and twice["resource"]["id"] == out["resource"]["id"]


def test_a_room_held_is_waited_out(hangar, lake, shell, key_file):
    hangar.held(True)
    with dagster_instance(config(hangar, lake, key_file), shell) as instance:
        run = make_run(instance)
        instance.launch_run(run.run_id, None)
        wait_for("the wait said", lambda: [m for m in messages(instance, run.run_id) if m.startswith("waiting for its machine")])
        assert not machines_of(hangar, run.run_id)
        time.sleep(1.5)  # asked again meanwhile: said once
        hangar.held(False)
        wait_for("the thread ended", lambda: run.run_id not in instance.run_launcher._threads, timeout=30)
        run = instance.get_run_by_id(run.run_id)
        waits = [m for m in messages(instance, run.run_id) if m.startswith("waiting for its machine")]
        assert len(waits) == 1 and "held for arcade" in waits[0], waits
        assert run.tags[MACHINE_TAG] and shell.started()


def test_the_rooms_wait_is_said_apart_from_the_birth(hangar, lake, shell, key_file):
    """A refused ask, then a granted one: the launch says the wait, and its
    birth counted from the ask that got the room — never the wait as a slow
    birth."""
    hangar.held(True)
    with dagster_instance(config(hangar, lake, key_file), shell) as instance:
        run = make_run(instance)
        instance.launch_run(run.run_id, None)
        wait_for("the wait said", lambda: [m for m in messages(instance, run.run_id) if m.startswith("waiting for its machine")])
        time.sleep(4)
        hangar.held(False)
        wait_for("the thread ended", lambda: run.run_id not in instance.run_launcher._threads, timeout=30)
        launched = [m for m in messages(instance, run.run_id) if m.startswith("launched on")]
        said = re.search(r"\(2x4\): waited (\d+) s for room, born (\d+) s, answering (\d+) s$", launched[0] if launched else "")
        assert said, messages(instance, run.run_id)
        waited, born, answering = map(int, said.groups())
        assert waited >= 4 and born <= answering < waited, launched


def test_a_brain_answers_5xx_is_asked_again(hangar, lake, shell, key_file, monkeypatch):
    real, calls = Brain.create, []

    def flaky(self, *a, **k):
        calls.append(1)
        if len(calls) == 1:
            raise Problem(503, "plugin-down", "plugin machines is down: it is started again")
        return real(self, *a, **k)

    monkeypatch.setattr(Brain, "create", flaky)
    with dagster_instance(config(hangar, lake, key_file), shell) as instance:
        run = launch(instance, make_run(instance))
        assert run.tags[MACHINE_TAG] and shell.started(), messages(instance, run.run_id)
        waits = [m for m in messages(instance, run.run_id) if m.startswith("waiting for its machine")]
        assert len(waits) == 1 and "plugin machines is down" in waits[0], waits
        launched = [m for m in messages(instance, run.run_id) if m.startswith("launched on")]
        assert launched and re.search(r": waited \d+ s for the brain, born ", launched[0]), launched


def test_a_room_held_past_its_patience_fails_the_run(hangar, lake, shell, key_file):
    hangar.held(True)
    with dagster_instance(config(hangar, lake, key_file, room_patience=1.5), shell) as instance:
        run = launch(instance, make_run(instance))
        assert run.status == DagsterRunStatus.FAILURE
        failure = messages(instance, run.run_id, DagsterEventType.RUN_FAILURE)
        assert any("no machine (2x4) after 2 s" in m and "held for arcade" in m for m in failure), failure
        assert not machines_of(hangar, run.run_id)


def test_a_limit_fails_the_run_at_once(hangar, lake, shell, key_file):
    tb = hangar.brain("tiny")
    kp, op = tb.create("keypair", "tower", {"public_key": PUBLIC_KEY}, name="tiny")
    assert tb.wait(op["id"], 20)["state"] == "succeeded"
    net, op = tb.create("network", "tower", {"key_pairs": [kp["id"]]}, name="tiny")
    assert tb.wait(op["id"], 20)["state"] == "succeeded"
    cfg = config(hangar, lake, key_file, subject="tiny", network=net["id"], key_pair=kp["id"],
                 machine_image=lake["image_id"])
    with dagster_instance(cfg, shell) as instance:
        run = launch(instance, make_run(instance))
        assert run.status == DagsterRunStatus.FAILURE
        failure = " ".join(messages(instance, run.run_id, DagsterEventType.RUN_FAILURE))
        assert "Le Hangar refused its machine (2x4)" in failure and "machines.count" in failure, failure
        assert not [m for m in messages(instance, run.run_id) if m.startswith("waiting")]


# ---- its end, its health ----------------------------------------------------


def power_off(hangar: Hangar, machine: str) -> None:
    # the fake keeps its guests by the brain's id
    hangar.tamper(lambda st: st["guests"][machine].__setitem__("running", False))
    b = hangar.brain()
    wait_for("its stop seen", lambda: (b.get(machine)["observed"] or {}).get("stopped_unasked"), timeout=30)


def test_its_own_power_off_deletes_it_and_fails_an_unfinished_run(hangar, lake, shell, key_file):
    with dagster_instance(config(hangar, lake, key_file), shell) as instance:
        run = started(instance, launch(instance, make_run(instance)))
        machine = run.tags[MACHINE_TAG]
        power_off(hangar, machine)
        # the second look, thirty seconds later by the engine's clock
        later = (datetime.now(timezone.utc) + timedelta(seconds=40)).isoformat().replace("+00:00", "Z")
        hangar.tamper(lambda st: st.__setitem__("now", later))
        b = hangar.brain()
        wait_for("deleted by the brain", lambda: b.get(machine)["state"] == "deleted", timeout=30)
        health = instance.run_launcher.check_run_worker_health(run)
        assert health.status == WorkerStatus.FAILED
        assert f"its machine {machine} is gone" in health.msg and "on_poweroff says delete" in health.msg


def test_a_machine_stopped_without_its_run_end_fails_it_and_is_deleted(hangar, lake, shell, key_file):
    with dagster_instance(config(hangar, lake, key_file), shell) as instance:
        run = started(instance, launch(instance, make_run(instance)))
        machine = run.tags[MACHINE_TAG]
        power_off(hangar, machine)
        health = instance.run_launcher.check_run_worker_health(run)
        assert health.status == WorkerStatus.FAILED and "its worker ended without its run's end" in health.msg
        b = hangar.brain()
        wait_for("deleted by the launcher", lambda: b.get(machine)["state"] == "deleted", timeout=30)


def test_a_held_machine_fails_its_run_and_is_deleted(hangar, lake, shell, key_file):
    with dagster_instance(config(hangar, lake, key_file), shell) as instance:
        run = started(instance, launch(instance, make_run(instance)))
        machine = run.tags[MACHINE_TAG]
        b = hangar.brain()
        hangar.held(True)
        wait_for("its room taken", lambda: b.get(machine).get("hold"), timeout=30)
        health = instance.run_launcher.check_run_worker_health(run)
        assert health.status == WorkerStatus.FAILED and "a game took the room" in health.msg, health
        wait_for("deleted by the launcher", lambda: b.get(machine)["state"] == "deleted", timeout=30)


def test_health_is_running_while_it_runs_and_when_the_brain_is_silent(hangar, lake, shell, key_file):
    with dagster_instance(config(hangar, lake, key_file), shell) as instance:
        run = started(instance, launch(instance, make_run(instance)))
        health = instance.run_launcher.check_run_worker_health(run)
        assert health.status == WorkerStatus.RUNNING and health.run_worker_id == run.tags[MACHINE_TAG]
        hangar.proc.terminate()
        hangar.proc.wait(10)
        health = instance.run_launcher.check_run_worker_health(run)
        assert health.status == WorkerStatus.RUNNING and "did not answer" in health.msg and health.transient


# ---- terminate --------------------------------------------------------------


def test_terminate_while_it_waits_makes_no_machine(hangar, lake, shell, key_file):
    hangar.held(True)
    with dagster_instance(config(hangar, lake, key_file), shell) as instance:
        run = make_run(instance)
        instance.launch_run(run.run_id, None)
        wait_for("the wait said", lambda: [m for m in messages(instance, run.run_id) if m.startswith("waiting")])
        assert instance.run_launcher.terminate(run.run_id)
        assert instance.get_run_by_id(run.run_id).status == DagsterRunStatus.CANCELED
        wait_for("the thread ended", lambda: run.run_id not in instance.run_launcher._threads, timeout=10)
        hangar.held(False)
        time.sleep(1)
        assert not machines_of(hangar, run.run_id, states=["creating", "ready", "deleting", "deleted"])
        assert not shell.started()
        assert len(messages(instance, run.run_id, DagsterEventType.RUN_CANCELED)) == 1


def test_a_cancel_is_reported_once(hangar, lake, shell, key_file):
    """The thread and terminate may both find a run CANCELING (another
    process, a wait that ended at that moment): one RUN_CANCELED."""
    with dagster_instance(config(hangar, lake, key_file), shell) as instance:
        run = make_run(instance)
        instance.report_run_canceling(run)
        instance.run_launcher._canceled(run.run_id, "by the thread")
        instance.run_launcher._canceled(run.run_id, "by terminate")
        assert messages(instance, run.run_id, DagsterEventType.RUN_CANCELED) == ["by the thread"]


def test_terminate_while_its_create_is_in_flight_ends_what_it_gets(hangar, lake, shell, key_file, monkeypatch):
    real = Brain.create
    with dagster_instance(config(hangar, lake, key_file), shell) as instance:
        run = make_run(instance)

        def late(self, *a, **k):
            # the cancel lands while the ask is on its way: none is known yet
            instance.run_launcher.terminate(run.run_id)
            return real(self, *a, **k)

        monkeypatch.setattr(Brain, "create", late)
        run = launch(instance, run)
        assert run.status == DagsterRunStatus.CANCELED and not shell.started()
        assert len(messages(instance, run.run_id, DagsterEventType.RUN_CANCELED)) == 1
        made = machines_of(hangar, run.run_id, states=["creating", "ready", "deleting", "deleted"])
        assert len(made) == 1
        b = hangar.brain()
        wait_for("ended by the launcher", lambda: b.get(made[0]["id"])["state"] == "deleted", timeout=30)


def test_a_create_applied_whose_answer_was_lost_is_found_and_ended(hangar, lake, shell, key_file, monkeypatch):
    real, asks = Brain.create, []

    def lost(self, *a, **k):
        asks.append(1)
        if len(asks) == 1:
            real(self, *a, **k)  # applied by the brain…
        raise Unreachable("POST /v1/resources: timed out")  # …its answer never came, nor any after

    monkeypatch.setattr(Brain, "create", lost)
    # the first search finds nothing yet: a create the brain was still planning
    real_list, lists = Brain.list, []

    def late_list(self, *a, **k):
        lists.append(1)
        return [] if len(lists) == 1 else real_list(self, *a, **k)

    monkeypatch.setattr(Brain, "list", late_list)
    with dagster_instance(config(hangar, lake, key_file, room_patience=1.5), shell) as instance:
        run = launch(instance, make_run(instance))
        assert run.status == DagsterRunStatus.FAILURE and not run.tags.get(MACHINE_TAG)
        made = machines_of(hangar, run.run_id, states=["creating", "ready", "deleting", "deleted"])
        assert len(made) == 1
        b = hangar.brain()
        wait_for("found by its run's id and ended", lambda: b.get(made[0]["id"])["state"] == "deleted", timeout=30)


def test_terminate_once_it_runs_stops_its_worker(hangar, lake, shell, key_file):
    with dagster_instance(config(hangar, lake, key_file), shell) as instance:
        run = started(instance, launch(instance, make_run(instance)))
        # the worker's SIGTERM is Dagster's own cancel: the worker writes it
        shell.on_stop = lambda: instance.report_run_canceled(instance.get_run_by_id(run.run_id), "the worker, stopped")
        assert instance.run_launcher.terminate(run.run_id)
        assert "sudo docker stop -t 30 hangar-run" in shell.commands()
        assert instance.get_run_by_id(run.run_id).status == DagsterRunStatus.CANCELED
        # its end is its power-off's: the launcher deletes nothing
        assert hangar.brain().get(run.tags[MACHINE_TAG])["state"] == "ready"


def test_monitorings_cancel_stops_the_worker_too(hangar, lake, shell, key_file):
    """Past its max_runtime, monitoring reports the run CANCELING, then asks
    terminate: its worker is stopped as a person's cancel stops it."""
    with dagster_instance(config(hangar, lake, key_file), shell) as instance:
        run = started(instance, launch(instance, make_run(instance)))
        instance.report_run_canceling(run, message="Canceling due to exceeding maximum runtime of 60 seconds.")
        assert instance.run_launcher.terminate(run.run_id)
        assert "sudo docker stop -t 30 hangar-run" in shell.commands()
        assert hangar.brain().get(run.tags[MACHINE_TAG])["state"] == "ready"


def test_what_was_launched_in_process_stays_so(hangar, lake, shell, key_file):
    """The launch's decision is read on the run, never in settings that
    changed since: a run launched in-process before Le Hangar was turned on
    is not failed for having no machine."""
    with dagster_instance(config(hangar, lake, key_file), shell) as instance:
        run = started(instance, make_run(instance, tags={LAUNCHED_TAG: "in-process"}))
        health = instance.run_launcher.check_run_worker_health(run)
        assert health.status == WorkerStatus.RUNNING and "in-process" in health.msg
        assert not shell.calls


def test_the_zones_processor_names_none(hangar, lake, shell, key_file):
    with dagster_instance(config(hangar, lake, key_file, cpu="zone"), shell) as instance:
        run = launch(instance, make_run(instance))
        assert "cpu" not in hangar.brain().get(run.tags[MACHINE_TAG])["spec"]


def test_terminate_while_it_is_born_starts_no_worker(hangar, lake, shell, key_file):
    with dagster_instance(config(hangar, lake, key_file), shell) as instance:
        run = make_run(instance)
        shell.on_answering = lambda: instance.run_launcher.terminate(run.run_id)
        run = launch(instance, run)
        assert run.status == DagsterRunStatus.CANCELED and not shell.started()
        b = hangar.brain()
        wait_for("deleted", lambda: b.get(run.tags[MACHINE_TAG])["state"] == "deleted", timeout=30)


def test_terminate_without_a_way_in_deletes_by_the_brain(hangar, lake, shell, key_file):
    with dagster_instance(config(hangar, lake, key_file), shell) as instance:
        run = started(instance, launch(instance, make_run(instance)))
        machine = run.tags[MACHINE_TAG]
        # a process holding no key (the webserver a run is cancelled from)
        instance.run_launcher._config["ssh_key_file"] = os.path.join(os.path.dirname(key_file), "none")
        assert instance.run_launcher.terminate(run.run_id)
        assert not [c for c in shell.commands() if "docker stop" in c]
        run = instance.get_run_by_id(run.run_id)
        assert run.status == DagsterRunStatus.CANCELED
        canceled = " ".join(messages(instance, run.run_id, DagsterEventType.RUN_CANCELED))
        assert f"its machine {machine} is deleted" in canceled and "by the brain alone" in canceled
        b = hangar.brain()
        wait_for("deleted", lambda: b.get(machine)["state"] == "deleted", timeout=30)


def test_a_retry_does_not_keep_its_parents_machine(hangar, lake, shell, key_file):
    with dagster_instance(config(hangar, lake, key_file), shell) as instance:
        parent = "m-0123456789abcdef0"
        run = make_run(instance, tags={MACHINE_TAG: parent, "dagster/parent_run_id": "x"})
        instance.launch_run(run.run_id, None)
        assert instance.get_run_by_id(run.run_id).tags.get(MACHINE_TAG) != parent
        wait_for("the thread ended", lambda: run.run_id not in instance.run_launcher._threads, timeout=30)
        mine = instance.get_run_by_id(run.run_id).tags[MACHINE_TAG]
        assert mine and mine != parent and hangar.brain().get(mine)["tags"]["dagster/run_id"] == run.run_id


# ---- what it is handed -------------------------------------------------------


def test_its_location_env_is_relayed(hangar, lake, shell, key_file, tmp_path):
    (tmp_path / "flows.env").write_text("PIPELINE_KEY=not-a-secret\n")
    with dagster_instance(config(hangar, lake, key_file, env_dir=str(tmp_path)), shell) as instance:
        run = launch(instance, make_run(instance))
        files = bundle_files(shell.started()[1])
        assert files["run.env"] == b"PIPELINE_KEY=not-a-secret\n"
        argv = files["argv"].decode().split("\0")
        # mounted from the machine's memory and read inside: never given to
        # Docker, which writes a container's env into its config on the disk
        assert "--env-file" not in argv and "-e" not in argv
        assert argv[argv.index("-v") + 1] == "/run/hangar/run.env:/run/hangar/run.env:ro"
        i = argv.index(IMAGE)
        assert argv[i + 1:i + 4] == ["/usr/local/bin/python", "-c", worker.READ_ENV]
        assert argv[i + 4:i + 7] == ["dagster", "api", "execute_run"]
        other = make_run(instance, location="elsewhere")
        with pytest.raises(LaunchFailed, match="the env file of location elsewhere"):
            instance.launch_run(other.run_id, None)
        assert instance.get_run_by_id(other.run_id).status == DagsterRunStatus.FAILURE
        # a line its worker could not take fails the run at its launch, said
        # by its number and never by what it holds
        (tmp_path / "broken.env").write_text("# fine\nGOOD=1\n=secret-value\n")
        broken = make_run(instance, location="broken")
        with pytest.raises(LaunchFailed, match=r"location broken, line 3: not KEY=value") as refused:
            instance.launch_run(broken.run_id, None)
        assert "secret-value" not in str(refused.value)
        assert not machines_of(hangar, broken.run_id)


def test_a_worker_that_ends_before_its_run_starts_fails_it(hangar, lake, shell, key_file):
    shell.answers["docker inspect"] = (0, "exited 1\n", "")
    with dagster_instance(config(hangar, lake, key_file), shell) as instance:
        instance.run_launcher.start_watch = 10
        run = launch(instance, make_run(instance))
        assert run.status == DagsterRunStatus.FAILURE
        failure = " ".join(messages(instance, run.run_id, DagsterEventType.RUN_FAILURE))
        assert "its worker ended before its run started (exit 1)" in failure, failure
        b = hangar.brain()
        wait_for("deleted", lambda: b.get(run.tags[MACHINE_TAG])["state"] == "deleted", timeout=30)


def test_a_run_named_by_its_machine_alone_is_the_launchers(hangar, lake, shell, key_file):
    with dagster_instance(config(hangar, lake, key_file), shell) as instance:
        run = started(instance, make_run(instance, tags={MACHINE_TAG: "m-0123456789abcdef0"}))
        health = instance.run_launcher.check_run_worker_health(run)
        assert health.status == WorkerStatus.FAILED and "holds no such machine" in health.msg


def test_a_worker_docker_refuses_fails_the_run_and_its_machine(hangar, lake, shell, key_file):
    shell.answers["systemd-run"] = (125, "", "docker: Error response from daemon: pull access denied\n")
    with dagster_instance(config(hangar, lake, key_file), shell) as instance:
        run = launch(instance, make_run(instance))
        assert run.status == DagsterRunStatus.FAILURE
        failure = " ".join(messages(instance, run.run_id, DagsterEventType.RUN_FAILURE))
        assert "its worker did not start: docker: Error response from daemon: pull access denied" in failure
        b = hangar.brain()
        wait_for("deleted", lambda: b.get(run.tags[MACHINE_TAG])["state"] == "deleted", timeout=30)


SOCKETS = {"workspace": ["/opt/dagster/grpc/a.sock", "/opt/dagster/grpc/b.sock"]}


def test_the_sockets_are_lent_and_mended(hangar, lake, shell, key_file):
    with dagster_instance(config(hangar, lake, key_file, sockets=SOCKETS), shell) as instance:
        run = launch(instance, make_run(instance, tags={"hangar/sockets": "workspace"}))
        assert run.status == DagsterRunStatus.STARTING, messages(instance, run.run_id)
        forwards = [c[1][1] for c in shell.calls if c[0] == "forward"]
        assert forwards == [[(p, p) for p in SOCKETS["workspace"]]]
        assert any("install -d -m 0755 -o debian /opt/dagster/grpc" in c for c in shell.commands())
        argv = bundle_files(shell.started()[1])["argv"].decode().split("\0")
        assert argv[argv.index("-v") + 1] == "/opt/dagster/grpc:/opt/dagster/grpc"
        run = started(instance, run)
        # cut: the probe on its machine finds a socket silent
        shell.probe_bad, shell.heal_on_forward = ["/opt/dagster/grpc/b.sock"], True
        health = instance.run_launcher.check_run_worker_health(run)
        assert health.status == WorkerStatus.RUNNING and not health.msg, health
        assert len(shell.tunnels) == 2 and not shell.tunnels[0].open and shell.tunnels[1].open
        assert "its sockets were cut: the tunnel is open again" in messages(instance, run.run_id)


def test_sockets_silent_at_launch_fail_the_run(hangar, lake, shell, key_file):
    shell.probe_bad = ["/opt/dagster/grpc/a.sock"]
    with dagster_instance(config(hangar, lake, key_file, sockets=SOCKETS), shell) as instance:
        instance.run_launcher.socket_patience = 0.5
        run = launch(instance, make_run(instance, tags={"hangar/sockets": "workspace"}))
        assert run.status == DagsterRunStatus.FAILURE
        failure = " ".join(messages(instance, run.run_id, DagsterEventType.RUN_FAILURE))
        assert "do not answer on its machine: /opt/dagster/grpc/a.sock" in failure, failure
        assert not shell.started() and not shell.tunnels[0].open


def test_sockets_of_no_name_it_lends_fail_at_once(hangar, lake, shell, key_file):
    with dagster_instance(config(hangar, lake, key_file, sockets=SOCKETS), shell) as instance:
        run = make_run(instance, tags={"hangar/sockets": "nope"})
        with pytest.raises(LaunchFailed, match="names 'nope': the launcher lends workspace"):
            instance.launch_run(run.run_id, None)
        assert not machines_of(hangar, run.run_id)
