# Supported PwnMesh formats

PwnMesh supports current execution formats and database schema `2`. Retired
formats are rejected without conversion, replay or workspace migration.

## Accepted inputs

| Boundary | Requirement |
| --- | --- |
| Database | Empty database for initialization, or a complete schema `2` database with a valid migration ledger and execution metadata `1` |
| Configuration | `PWNMESH_*` settings; no `XLOOM_*` alias lookup or normalization |
| Server project | Orchestration version `1`; older project endpoints are rejected |
| Execution | Current registered Job: live graph bridge, result contract `2`, Decide context `2`; lease roles `reason`, `explore`, `curate` only |
| Worker session | Bound session schema `1` with matching run, input, workspace and budget identity |
| Acceptance | Receipt schema `1` in `acceptance.json`; an old `graph.json` alone is rejected before callbacks or writes |
| Worker graph | Schema `1` with matching graph and per-node definition digests; missing node bindings cannot be reconstructed |
| Evidence | Validated current `.pwnmesh/runs/.../evidence/` references; no `.xloom` fallback |

Both inline state and immutable input snapshots are supported current Job forms.
Server preparation creates immutable snapshots for new runs; retained current
inline jobs remain readable and recoverable. Optional timing fields may be absent;
they do not authorize reconstruction of missing execution identity or evidence.

Existing current acceptance receipts take precedence over leftover old graph
files. They still pass identity and evidence checks. Unsupported files remain
untouched for inspection; absence of a supported receipt must never turn an old
run into a new model/tool execution.

## Database initialization and validation

`internal/board/internal/sqlstore/migrations.go` validates the schema ledger and execution metadata
before committing changes. Empty databases create schema `2` directly with
`pwnmesh_*` names for branded SQL objects. Runtime queries use those names only;
there are no views or aliases for the old namespace.

The ledger retains version `1` as `unversioned-baseline`, including its original
timestamp, and records version `2` as `pwnmesh-namespace`. Fresh databases record
both versions without creating old SQL objects. The historical version `1` name
does not permit opening a schema `1` or unversioned database. Schema `1` databases,
nonempty unversioned databases, mixed namespaces, inconsistent or future ledgers
and unsupported execution metadata are rejected without adoption or raw Job/Result
reconstruction. Current row IDs, record bytes, hashes and installation identity
remain unchanged. Future schema
changes append migrations and tests; editing fresh-database DDL alone cannot
upgrade existing data.

Current default rows, installation ownership, UI projections, counters and stale
read-view maintenance remain part of database integrity; old execution metadata
is no longer synthesized from stored Jobs.

Current Job and State JSON bytes bind live session and input hashes. Keep their
field order, tags and omission rules stable. Board/Worker type aliases share
current model/protocol definitions rather than implementing a historical format.
Likewise, `FactRecord.Legacy` is used for current human input without execution
evidence; removing it would change current planning and evidence policy.

## Upgrade operations

Keep a consistent SQLite backup and matching workspace, journals, evidence and
receipts before upgrading. Preserve the installation identity row to retain
container ownership. Use SQLite backup tooling rather than copying only an active
WAL database file. Stop the old Server and Dispatcher before changing binaries.
Schema `1` databases must first be upgraded with a previous release that supports
the namespace migration; this release does not perform that upgrade. Older
binaries that query the former namespace cannot share a schema `2` database.
There is no automatic down-migration; restore the matching backup to return to
an older binary.

Regression coverage checks direct canonical initialization, current reopen,
schema `1` and other unsupported-input nonmutation, atomic failure,
canonical JSON hashes, current recovery and rejection before side effects.
Obsolete execution recovery implementations and their success fixtures have been
removed; rejection and current-format tests replace them.
