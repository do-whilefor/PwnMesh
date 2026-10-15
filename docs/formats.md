# Supported PwnMesh formats

PwnMesh supports current execution formats and database schema `4` only.
Retired formats are rejected without replay or migration.

## Accepted inputs

| Boundary | Requirement |
| --- | --- |
| Database | Empty database for initialization, or a complete schema `4` database with a valid migration ledger and execution metadata `1` |
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

The current ledger contains four historical entries: `unversioned-baseline`,
`pwnmesh-namespace`, `incremental-state-storage` and `normalized-state-records`.
Fresh databases record all four without creating old SQL objects. Reopening a
current database preserves its ledger and timestamps; those entries do not enable
old-version compatibility. Schema `1`, `2` and `3` databases, nonempty unversioned
databases, mixed namespaces, incomplete or future ledgers and unsupported execution
metadata are rejected without modification. There is no automatic upgrade path.

Schema `4` stores ordered Fact metadata separately and uses ordered records for
goals, Step metadata, findings, relations, candidates and disputes. Step identities
are indexed for scheduling admission. Ordered positions preserve duplicate IDs
and orphan records. Empty array markers distinguish normalized collections from
absent or null values; curation progress and unknown fields remain in the compact
state header. Known header keys must be unique and use their exact lowercase
names. Reads reject old inline collection payloads, aliases and duplicate known
keys. Application writes normalize canonical arrays into records atomically.
Public State encoding and frozen input hashes retain their current format.

Current default rows, installation ownership, UI projections, counters and stale
read-view maintenance remain part of database integrity; old execution metadata
is no longer synthesized from stored Jobs.

Current Job and State JSON bytes bind live session and input hashes. Keep their
field order, tags and omission rules stable. Board/Worker type aliases share
current model/protocol definitions rather than implementing a historical format.
Likewise, `FactRecord.Legacy` is used for current human input without execution
evidence; removing it would change current planning and evidence policy.

## Database operations

Keep a consistent SQLite backup and matching workspace, journals, evidence and
receipts before changing binaries. Preserve the installation identity row to retain
container ownership. Use SQLite backup tooling rather than copying only an active
WAL database file. Stop the old Server and Dispatcher before changing binaries.
Old databases require a previous release or migration tooling that supports their
format before this binary can open them. This binary only initializes or opens
schema `4`. Older binaries cannot open schema `4`.
There is no automatic down-migration; restore the matching backup to return to
an older binary.

Regression coverage checks direct canonical initialization, current reopen,
old-schema and other unsupported-input nonmutation, atomic metadata writes,
canonical JSON hashes, current recovery and rejection before side effects.
Obsolete execution recovery implementations and their success fixtures have been
removed; rejection and current-format tests replace them.
