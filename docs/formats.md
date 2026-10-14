# Supported PwnMesh formats

PwnMesh supports the current execution formats and a one-time database namespace
upgrade from schema `1` to `2`. That SQL upgrade does not convert retired execution
formats, replay old work or migrate an operator's environment and workspace.

## Accepted inputs

| Boundary | Requirement |
| --- | --- |
| Database | Empty database for initialization, or a complete schema `1` or `2` database with a valid migration ledger and execution metadata `1` |
| Configuration | `PWNMESH_*` settings; no `XLOOM_*` alias lookup or normalization |
| Server project | Orchestration version `1`; older project endpoints are rejected |
| Execution | Current registered Job: live graph bridge, result contract `2`, Decide context `2` |
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

## Database initialization and upgrades

`internal/board/internal/sqlstore/migrations.go` validates the schema ledger and execution metadata
before committing changes. Empty databases create schema `2` directly with
`pwnmesh_*` names for branded SQL objects. Runtime queries use those names only;
there are no views or aliases for the old namespace.

A complete, versioned schema `1` database uses the historical `xloom_*` SQL
namespace. On open, one transaction renames its tables and indexes, rebuilds
triggers from current definitions, updates foreign-key references, checks
foreign-key integrity and appends version `2`. Stored row IDs, record bytes,
hashes and installation identity remain
unchanged. The obsolete metadata-backfill index is removed in the same transaction.
Any migration failure rolls back object names, data and the new ledger entry.

The ledger retains version `1` as `unversioned-baseline`, including its original
timestamp, and records version `2` as `pwnmesh-namespace`. Fresh databases record
both versions without creating old SQL objects. The historical version `1` name
does not permit opening an unversioned database. Nonempty unversioned databases,
mixed namespaces, inconsistent or future ledgers and unsupported execution metadata
are rejected without adoption or raw Job/Result reconstruction. Future schema
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
WAL database file. Stop the old Server and Dispatcher before the first upgraded
startup. Older binaries that query the former namespace cannot share the upgraded
database. There is no automatic down-migration; restore the matching backup to
return to an older binary.

Regression coverage checks direct canonical initialization, a populated version
`1` upgrade, current reopen, unsupported-input nonmutation, atomic failure,
canonical JSON hashes, current recovery and rejection before side effects.
Obsolete execution recovery implementations and their success fixtures have been
removed; rejection and current-format tests replace them.
