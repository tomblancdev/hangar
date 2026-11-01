"""The brain's REST API, by the standard library alone.

What the launcher and the bake ask of Le Hangar: a resource made, read,
deleted or acted on, an operation waited for, a list by tag. A refusal is a
Problem (the brain's own words: its status, its kind, its sentence); a brain
that does not answer is Unreachable — the two are never confused, because
the launcher waits out the second and acts on the first.
"""

from __future__ import annotations

import http.client
import json
import time
import urllib.error
import urllib.parse
import urllib.request
from collections.abc import Callable, Mapping
from typing import Any


class Problem(Exception):
    """The brain answered, and refused: status, kind ("room", "limit", "busy",
    "not-found" …) and its sentence."""

    def __init__(self, status: int, kind: str, detail: str, body: Mapping[str, Any] | None = None):
        super().__init__(detail or f"{status} {kind}")
        self.status = status
        self.kind = kind
        self.detail = detail
        self.body = dict(body or {})


class Unreachable(Exception):
    """The brain did not answer (no connection, a timeout, a 5xx with no
    problem of its own)."""


class Brain:
    """One brain, one token. The token is read from its file when the Brain
    is made — a token rotated on disk is the next Brain's."""

    def __init__(self, url: str, token: str, timeout: float = 30.0):
        self.url = url.rstrip("/")
        self._token = token
        self.timeout = timeout

    @classmethod
    def from_token_file(cls, url: str, token_file: str, timeout: float = 30.0) -> Brain:
        with open(token_file, encoding="utf-8") as f:
            token = f.read().strip()
        if not token:
            raise ValueError(f"the token file {token_file} is empty")
        return cls(url, token, timeout)

    # ---- the wire --------------------------------------------------------

    def call(
        self,
        method: str,
        path: str,
        body: Any = None,
        query: Mapping[str, Any] | None = None,
        timeout: float | None = None,
    ) -> tuple[int, dict[str, Any]]:
        url = self.url + path
        if query:
            url += "?" + urllib.parse.urlencode(query, doseq=True)
        data = None if body is None else json.dumps(body).encode()
        req = urllib.request.Request(url, data=data, method=method)
        req.add_header("Authorization", "Bearer " + self._token)
        req.add_header("Accept", "application/json")
        if data is not None:
            req.add_header("Content-Type", "application/json")
        try:
            with urllib.request.urlopen(req, timeout=timeout or self.timeout) as resp:
                return resp.status, _json(resp.read())
        except urllib.error.HTTPError as e:
            out = _json(e.read())
            if e.code >= 500 and "kind" not in out:
                raise Unreachable(f"{method} {path}: {e.code} {e.reason}") from e
            raise Problem(e.code, str(out.get("kind", "")), str(out.get("detail", "")), out) from e
        except (urllib.error.URLError, OSError, TimeoutError, http.client.HTTPException) as e:
            # an answer cut in the middle is no answer: a create it carried
            # may have been applied — its client token finds it again
            raise Unreachable(f"{method} {path}: {getattr(e, 'reason', e) or type(e).__name__}") from e

    # ---- resources -------------------------------------------------------

    def create(
        self,
        type: str,
        zone: str,
        spec: Mapping[str, Any],
        *,
        name: str = "",
        description: str = "",
        tags: Mapping[str, str] | None = None,
        client_token: str = "",
    ) -> tuple[dict[str, Any], dict[str, Any]]:
        """Asks for a resource; returns (resource, operation). The same client
        token for the same request is the same resource (200, replayed)."""
        body: dict[str, Any] = {"type": type, "zone": zone, "spec": dict(spec)}
        if name:
            body["name"] = name
        if description:
            body["description"] = description
        if tags:
            body["tags"] = dict(tags)
        if client_token:
            body["client_token"] = client_token
        _, out = self.call("POST", "/v1/resources", body)
        return out["resource"], out["operation"]

    def get(self, id: str) -> dict[str, Any] | None:
        """A resource as the brain holds it; None when it holds none of that
        id (a deleted one is still held, its state says so)."""
        try:
            return self.call("GET", "/v1/resources/" + urllib.parse.quote(id, safe=""))[1]
        except Problem as p:
            if p.status == 404:
                return None
            raise

    def delete(self, id: str, client_token: str = "") -> dict[str, Any]:
        """Asks for a resource's delete; returns its operation. A delete asked
        again under the same token replays the first, whatever became of it:
        end() asks with none."""
        query = {"client_token": client_token} if client_token else None
        _, out = self.call("DELETE", "/v1/resources/" + urllib.parse.quote(id, safe=""), query=query)
        return out["operation"]

    def act(self, id: str, action: str, params: Mapping[str, Any] | None = None) -> dict[str, Any]:
        """Asks for an action; returns its operation."""
        body = {"params": dict(params)} if params else None
        _, out = self.call("POST", f"/v1/resources/{urllib.parse.quote(id, safe='')}/actions/{action}", body)
        return out["operation"]

    def list(
        self,
        type: str = "",
        tags: Mapping[str, str] | None = None,
        states: list[str] | None = None,
        name: str = "",
    ) -> list[dict[str, Any]]:
        """Every resource of the caller's that matches, page after page."""
        query: dict[str, Any] = {"limit": 1000}
        if type:
            query["type"] = type
        if name:
            query["name"] = name
        if tags:
            query["tag"] = [f"{k}={v}" for k, v in tags.items()]
        if states:
            query["state"] = states
        out: list[dict[str, Any]] = []
        while True:
            _, page = self.call("GET", "/v1/resources", query=query)
            out.extend(page.get("resources") or [])
            if not page.get("next"):
                return out
            query["after"] = page["next"]

    # ---- operations ------------------------------------------------------

    def operation(self, id: str, wait: int = 0) -> dict[str, Any]:
        query = {"wait": wait} if wait else None
        return self.call("GET", "/v1/operations/" + urllib.parse.quote(id, safe=""), query=query, timeout=wait + self.timeout)[1]

    def wait(
        self,
        op_id: str,
        timeout: float,
        stop: Callable[[], bool] = lambda: False,
    ) -> dict[str, Any]:
        """Waits until an operation is no longer running, or timeout seconds;
        stop() is asked between looks (a run cancelled meanwhile). A brain
        that does not answer is asked again until then. Returns the operation
        as last read; Unreachable when none was read."""
        deadline = time.monotonic() + timeout
        op: dict[str, Any] | None = None
        while True:
            left = deadline - time.monotonic()
            try:
                op = self.operation(op_id, wait=max(0, min(20, int(left))))
            except Unreachable:
                if left <= 0:
                    if op is None:
                        raise
                    return op
                time.sleep(min(2.0, max(0.1, left)))
                continue
            if op.get("state") != "running" or left <= 0 or stop():
                return op

    def end(self, id: str, patience: float = 900.0, every: float = 3.0) -> bool:
        """Deletes a resource until it is gone: one still being made or
        changed (409 busy) is waited for, a brain that does not answer and a
        delete that failed are asked again. True once it is gone; False past
        patience (a machine then ends at its until)."""
        deadline = time.monotonic() + patience
        while True:
            try:
                r = self.get(id)
                if r is None or r.get("state") in ("deleted", "failed"):
                    return True
                if r.get("state") not in ("creating", "updating", "deleting"):
                    op = self.delete(id)
                    op = self.wait(op["id"], max(1.0, min(120.0, deadline - time.monotonic())))
                    if op.get("state") == "succeeded":
                        return True
            except Problem as p:
                if p.status == 404:
                    return True
            except Unreachable:
                pass
            if time.monotonic() >= deadline:
                return False
            time.sleep(every)


def _json(raw: bytes) -> dict[str, Any]:
    if not raw:
        return {}
    try:
        out = json.loads(raw)
    except ValueError:
        return {"detail": raw.decode(errors="replace")[:500]}
    return out if isinstance(out, dict) else {"value": out}
