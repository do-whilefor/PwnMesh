# Supported PwnMesh formats

Current execution formats and schema `4` are supported. Retired
formats are rejected without migration or replay; see [protocol](protocol.md).

## Accepted inputs

| Boundary | Requirement |
| --- | --- |
| Database | Empty for initialization, or complete schema `4` with a valid ledger and execution metadata `1` |
| Configuration | `PWNMESH_*` only; no `XLOOM_*` aliases; retired `runtime.execution` and `runtime.prompt_group` selectors are rejected |
| Project | Orchestration version `1` |
| Execution | Registered Job, live graph bridge, result contract `2`, Decide context `2`; roles `reason`, `explore`, `curate` |
| Recovery environment | Version `2` digest covers provider settings, container settings and configured tool environment; provider tokens are excluded |
| Worker session | Schema `1`, matching run, input, workspace and budget identity |
| Acceptance | Schema `1` in `acceptance.json`; old `graph.json` alone is rejected before callbacks or writes |
| Worker graph | Schema `1`, matching graph and per-node definition digests; missing bindings cannot be reconstructed |
| Evidence | Validated `.pwnmesh/runs/.../evidence/` references; no `.xloom` fallback |

New runs use immutable snapshots; current inline Jobs remain recoverable when
their environment identity matches. Earlier environment digests cannot authorize
unfinished runs because they omitted tool settings. Saved results and committed
control receipts still settle without reexecution.
Optional timing fields may be absent, but missing execution identity or evidence
cannot be reconstructed. Planners publish through version `2` decision batches;
retained version `1` decision jobs cannot use live reads or direct writes.
HTTP timeouts require JSON integers of at least `5`, without string, decimal or
exponent coercion.

Current receipts take precedence over old graph files but still require identity
and evidence checks. Unsupported files remain untouched. Missing current receipts
must not cause old runs to execute model or tools again.

## Database compatibility

Empty databases initialize schema `4` and its four-entry ledger with
`pwnmesh_*` SQL names. Current reopen preserves ledger timestamps. Old schemas,
nonempty unversioned databases, mixed namespaces, incomplete/future ledgers and
unsupported execution metadata are rejected unchanged.

Schema `4` normalizes ordered state collections atomically while preserving
duplicate IDs, orphan records and absent/null/empty distinctions. Known header keys
must be unique and exactly lowercase; old inline collections and aliases are
rejected. Public State encoding and frozen input hashes remain stable.

Preserve Job/State JSON field order, tags and omission rules: they bind session and
input hashes. `FactRecord.Legacy` remains valid for current human input without
execution evidence.

## Changing binaries

Stop Server and Dispatcher. Back up SQLite consistently with its matching
workspace, journals, evidence and receipts; copying only an active WAL database
file is insufficient. Preserve the installation identity for container ownership.
Old databases need a compatible previous release or migration tool. Binaries
without schema `4` support require a compatible database backup for rollback.
