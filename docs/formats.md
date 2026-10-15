# Supported PwnMesh formats

PwnMesh supports current execution formats and database schema `4`, with an
atomic upgrade from schema `2` or `3`. Retired formats are rejected without replay or
workspace migration.

## Accepted inputs

| Boundary | Requirement |
| --- | --- |
| Database | Empty database for initialization, or a complete schema `2`, `3` or `4` database with a valid migration ledger and execution metadata `1` |
| Configuration | `PWNMESH_*` settings; no `XLOOM_*` alias lookup or normalization; retired `runtime.execution` and `runtime.prompt_group` selectors are rejected |
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

Registered planners publish only through version `2` decision batches. Retained
version `1` decision jobs remain stored, but cannot use live decision reads or
direct writes. HTTP timeout settings require JSON integers of at least `5`;
numeric strings, decimal and exponent notation are no longer coerced.

Existing current acceptance receipts take precedence over leftover old graph
files. They still pass identity and evidence checks. Unsupported files remain
untouched for inspection; absence of a supported receipt must never turn an old
run into a new model/tool execution.

## Database initialization and validation

`internal/board/internal/sqlstore/migrations.go` validates the schema ledger and execution metadata
before committing changes. Empty databases create schema `4` directly with
`pwnmesh_*` names for branded SQL objects. Runtime queries use those names only;
there are no views or aliases for the old namespace.

The ledger retains version `1` as `unversioned-baseline`, including its original
timestamp, and records version `2` as `pwnmesh-namespace`. Fresh databases record
all four versions without creating old SQL objects. Version `3` is
`incremental-state-storage`: it separates ordered Fact metadata and adds a
disposable scheduling projection. Version `4`, `normalized-state-records`, moves
goals, Step metadata, findings, relations, candidates and disputes into ordered
records and indexes Step identities for scheduling admission. The historical version `1` name
does not permit opening a schema `1` or unversioned database. Schema `1` databases,
nonempty unversioned databases, mixed namespaces, inconsistent or future ledgers
and unsupported execution metadata are rejected without adoption or raw Job/Result
reconstruction. A schema `2` upgrade validates the old namespace first, then
creates the new tables, moves Fact metadata and appends the ledger entry in one
transaction. Fact order, duplicate IDs and orphan records survive; public State
encoding, frozen input hashes, unrelated record bytes and installation identity
remain unchanged. Metadata JSON physically loses its extracted Facts array.
Current-schema reopen does not repeat this backfill. Future schema
changes append migrations and tests; editing fresh-database DDL alone cannot
upgrade existing data.

The version `3` to `4` migration leaves Fact records and immutable payloads
untouched. Ordered positions preserve duplicate IDs and orphan records. Empty
array markers distinguish normalized collections from absent or null values;
curation progress and unknown fields remain in the compact state header.
Historical headers with noncanonical or repeated known keys stay inline so Go's
field matching and merge order remain unchanged. Ordinary application writes use
canonical keys and normalize their collections.
The entire version `2` to `4` sequence rolls back if any intermediate migration
fails. Reopening schema `4` does not reapply either backfill.

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
the namespace migration; this release does not perform that upgrade. A schema
`2` or `3` database upgrades on first open. Older binaries cannot open schema `4`.
There is no automatic down-migration; restore the matching backup to return to
an older binary.

Regression coverage checks direct canonical initialization, the schema `2` and `3`
upgrades, current reopen, schema `1` and other unsupported-input nonmutation, atomic failure,
canonical JSON hashes, current recovery and rejection before side effects.
Obsolete execution recovery implementations and their success fixtures have been
removed; rejection and current-format tests replace them.
