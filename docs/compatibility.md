# PwnMesh compatibility

PwnMesh is the current product name. Cairn, X-Loom and `xloom` appear only where
they identify historical formats or compatibility behavior. They are not a
request to rewrite persisted data. These legacy formats do not carry a release
version that identifies their full compatibility scope; support below is
identified by format and tested behavior.

| Boundary | Supported behavior and owner |
| --- | --- |
| Environment settings | `internal/config/env.go` prefers `PWNMESH_*`; matching `XLOOM_*` is a fallback only when the canonical key is absent. An explicit empty canonical value wins. Worker lookups use their resolved map, not ambient host credentials |
| SQLite names | Existing `xloom_*` tables, indexes and triggers remain the on-disk namespace for both existing and new databases |
| Unversioned databases | The baseline migration upgrades known historical layouts atomically and records the new migration sequence; no historical release version is inferred |
| Legacy graph/state JSON | Board retains historical field ordering and omission semantics used by snapshot/decision hashes; historical fallback reads do not rewrite raw jobs/results |
| Historical execution results | Missing failure-event diagnostics may be read from retained result JSON through the bounded legacy reader |
| Historical server execution | Read-only when orchestration/result/decision versions do not satisfy current registration; start a current project to continue |
| Worker session | Only bound session schema `1` can resume. Unversioned/unbound sessions are rejected and retained, not silently upgraded |
| `execute-v1` graph checkpoint | Existing checkpoints use `internal/worker/compatibility_execute_v1_linux.go`; preserve original DAG definition/hash and no-replay semantics, then seal accepted recovery with receipt schema `1` |
| Evidence paths | Worker retains a validated `.xloom/runs/.../evidence/` read fallback alongside current `.pwnmesh` evidence; this is not automatic directory relocation |
| Go package moves | Existing Worker/Board aliases preserve source compatibility while shared types have one owner; JSON bytes remain unchanged |

## Database migration policy

`internal/board/migrations.go` owns an ordered append-only migration sequence and
the `pwnmesh_schema_migrations` ledger. Version `1` is the baseline introduced by
this refactor, not the original historical database version. A database without
a ledger is unversioned. Fresh and known historical databases enter through the
same baseline, including bootstrap metadata and execution input metadata upgrades.

Ledger validation, schema/data upgrades and version recording occur in one
transaction. A failure rolls back the upgrade and ledger writes together.
Unsupported future versions, gaps and unexpected migration names fail instead
of attempting a downgrade. Repeat open skips completed schema migrations; required
counter repair, missing default/installation/UI projection rows, execution
metadata backfill and stale read-view cleanup remain explicit open-time
maintenance. Already-versioned execution rows are not decoded or rewritten by
that backfill. Missing tables in a versioned database are errors, not a reason to
replay the baseline. Restore the installation identity with the database when
retaining container ownership: the historical fallback generates a new identity
if its row is absent, and cannot adopt the old installation's containers.

Append a new migration for the next schema change. Keep applied migration names
and version numbers stable, preserve the baseline for unversioned inputs, and add
fresh/upgrade/reopen/rollback tests. Updating a `CREATE TABLE IF NOT EXISTS`
definition alone does not upgrade a database whose ledger already records it.
Per-execution `metadata_version` remains a separate row-level format marker.

Back up SQLite and matching runtime artifacts before upgrading. There is no
automatic down-migration. Code rollback is safe only when the previous binary
supports the resulting schema; otherwise restore the matching backup.

## Retention and removal

There is no established end-of-support date for these legacy readers. Keep the
adapters until the support scope is explicitly changed, affected stored formats
have a tested migration/export path, and recovery/hash fixtures demonstrate that
remaining supported data still works. Renaming a variable or updating UI text
does not satisfy those conditions.

The regression anchors are Board migration/State fixtures, protocol wire/hash
fixtures, Worker session and execute-v1 acceptance tests, environment precedence
tests, and Server historical-read-only tests. Compatibility belongs beside its
own boundary; this document is the common index, not a new runtime framework.
