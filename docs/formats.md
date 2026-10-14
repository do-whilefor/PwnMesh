# Supported PwnMesh formats

PwnMesh supports the current database and execution formats. Retired formats are
not upgraded or replayed automatically. This is a support cutoff, not evidence
that an operator's database, environment or workspace has already been migrated.

## Accepted inputs

| Boundary | Requirement |
| --- | --- |
| Database | Empty database for initialization, or a valid current migration ledger and execution metadata |
| Configuration | `PWNMESH_*` settings; no `XLOOM_*` alias lookup or normalization |
| Server project | Orchestration version `1`; older project endpoints are rejected |
| Execution | Current registered Job: live graph bridge, result contract `2`, Decide context `2` |
| Worker session | Bound session schema `1` with matching run, input, workspace and budget identity |
| Acceptance | Receipt schema `1` in `acceptance.json`; an old `graph.json` alone is rejected before callbacks or writes |
| Worker graph | Schema `1` with matching graph and per-node definition digests; missing node bindings cannot be reconstructed |
| Evidence | Validated current `.pwnmesh/runs/.../evidence/` references; no `.xloom` fallback |

Both inline state and immutable input snapshots are current Job forms. Small
inputs still use inline state. Optional timing fields may be absent; they do not
authorize reconstruction of missing execution identity or evidence.

Existing current acceptance receipts take precedence over leftover old graph
files. They still pass identity and evidence checks. Unsupported files remain
untouched for inspection; absence of a supported receipt must never turn an old
run into a new model/tool execution.

## Database initialization and upgrades

`internal/board/migrations.go` validates the schema ledger and initializes only
empty databases. Nonempty unversioned databases and unsupported execution metadata
are rejected, without schema adoption or raw Job/Result reconstruction. Retain a
previous release separately if an old installation still needs a one-time upgrade;
this runtime does not provide that compatibility path.

The supported ledger records version `1` as `unversioned-baseline`. That is the
already-applied migration identity from the preceding release, not permission to
open an unversioned database. Its name remains stable for databases already
migrated by that release. Future schema changes append migrations and tests;
editing fresh-database DDL alone cannot upgrade existing data.

`xloom_*` is still the active physical SQL namespace, including newly initialized
databases. It is not an alias or an unused compatibility layer. Renaming it needs
a separate schema migration. Current default rows, installation ownership, UI
projections, counters and stale read-view maintenance remain part of database
integrity; old execution metadata is no longer synthesized from stored Jobs.

Current Job and State JSON bytes bind live session and input hashes. Keep their
field order, tags and omission rules stable. Board/Worker type aliases share
current model/protocol definitions rather than implementing a historical format.
Likewise, `FactRecord.Legacy` is used for current human input without execution
evidence; removing it would change current planning and evidence policy.

## Upgrade operations

Keep a consistent SQLite backup and matching workspace, journals, evidence and
receipts before upgrading. Preserve the installation identity row to retain
container ownership. Use SQLite backup tooling rather than copying only an active
WAL database file. No automatic down-migration or conversion of user data is
performed by this cleanup.

Regression coverage checks empty initialization, current reopen, unsupported
input nonmutation, atomic failure, canonical JSON hashes, current recovery and
rejection before side effects. Obsolete recovery implementations and their
success fixtures have been removed; rejection and current-format tests replace
them.
