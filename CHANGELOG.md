# Changelog

## Unreleased — la naissance

The product is born: the core's skeleton, proved end to end on a fake engine.
No release is cut; the first tag comes with the first plugin that makes a real
machine.

- **The core:** identity (OIDC bearer tokens; API tokens `hgr_…`, hashed,
  expiring, read-only or read-write, unable to make tokens), tiers and limits
  (quantities and choices, the first matching tier wins, an unnamed dimension
  allows nothing, every refusal with its numbers), the registry (SQLite,
  AWS-style ids, tags, usage, relations), operations (client tokens, waiting,
  resumed after a crash), reconcile (in sync, repaired, drifted, lost and
  found), the audit (one line per call), the plugin host.
- **The contracts:** the API as OpenAPI 3.1 (`api/openapi.yaml`, served at
  `/openapi.json`, held to the routes by a test); the plugin protocol
  (`proto/`, gRPC over HashiCorp's go-plugin) and its SDK.
- **The walls, proved:** a plugin starts with an empty environment and
  receives only its own credential, per zone — tests run a probe plugin and
  read what it was given (and fail when the wall is removed).
- **The fake driver** (in memory, or a JSON file that *is* the engine) and
  **the toy plugin** (`box`: create, delete, start, stop, resize, suspend,
  reconcile) — the example to copy.
- One static binary: `hangar serve | check | token | plugin | version`.
