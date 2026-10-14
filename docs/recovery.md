# PwnMesh recovery

Recovery preserves execution identity and authority. Re-running a command is not
permission to repeat side effects or give an old lease access to current state.

| Operation | Meaning and retained state |
| --- | --- |
| Infrastructure interruption | `protocol.ErrInterrupted` permits bounded same-run recovery; retain identity, journal, original deadlines and consumed allowances |
| Ordinary cancellation | A permanent stop of that attempt; it does not imply same-run recovery |
| Project pause / Continue | Pause marks affected executions; Continue grants retry only to those eligible attempts, not unrelated failures |
| Explicit execution retry | A validated retry grant; generation, task eligibility and revoked authority are still checked |
| Result delivery retry | Deliver an already retained result; do not re-run the model or tools |
| Project restart | Archive the old round, increment generation, revoke old runs and rebuild current projections while preserving human inputs, counters and workspace files |
| Project termination | Stop an unfinished project without claiming success; retain evidence/history; a new execution round requires explicit restart |

Worker persists session identity and recovery counters before further requests.
The current same-run recovery limit is two resumptions after the initial attempt.
Recovery does not reset task
deadlines, repair/continuation budgets or already consumed allowances.

Role controllers own live graph observations separately from the saved session.
Each successful save captures their values and the supplied Agent checkpoint;
runtime-triggered saves detach the live checkpoint first. If a save fails, the
callback restores its own in-memory changes to their values at entry and reports
the failure. The session file is replaced atomically; a directory-sync error after
replacement leaves durability uncertain. The private `worker/internal/phase`
package decides deadlines, repair/continuation consumption and recovery actions
from values. Session persists each proposed transition before its effects and
restores both Session and Loop phase values if that save fails. The session JSON
layout remains unchanged; policy state is converted explicitly, never embedded as
a second live owner. Acceptance reads
only the identity-bound saved transcript or its verified committed journal prefix,
never an uncommitted crash tail.

The private journal owner verifies the saved byte offset and checksum before
recovery. Complete crash-tail records remain append-only; an incomplete final
record is archived and synced before truncation. Session saves obtain a synced
journal checkpoint before replacing the session file. Acceptance visits only the
verified committed prefix. Historical `replan_*` metrics are adapted while
replaying retained records; current event handling does not normalize them.

For Execute, an accepted receipt binds the run and input hash to retained session
and artifact hashes. Evidence is verified again on read. A changed repair target
cannot reuse the previous success. Deterministic acceptance failure is terminal;
an uncertain callback remains tied to its existing session recovery path.

For Decide and Curate, the server commit receipt is authoritative. Recovery checks
it before planning again. A private draft may be discarded after a conflict;
that does not erase a committed mutation or grant extra execution budget.
The historical planner-conflict adapter runs only after this receipt check. Old
Curate batches with an accepted receipt but pending settlement still use the
explicit apply path; newly committed batches settle within their Board transaction.

Automatic Decide and infrastructure-failed Curate retries grant a successor while
retaining the failed execution's Job and Result. The saved attempt history bounds
that allowance across dispatcher restarts. Defensive revocation, lease release and
the grant share a local savepoint; a failed grant rolls back those changes even
when the caller handles the error and commits unrelated work.

The `execute-v1` acceptance adapter has been removed. A `graph.json` without a
current `acceptance.json` is rejected before session callbacks, bookkeeping or
result output. Current receipts remain recoverable even if an old graph file is
left beside them. General Worker graphs require per-node definition digests and
never reconstruct missing bindings to resume or extend an old checkpoint.

## Operational checks

Before restoring, retain a consistent database backup and the matching workspace,
session journals, evidence and receipts. For a live SQLite database, use its backup
API/tooling; copying only the database file while WAL writes are active is not a
consistent backup. Do not rename runtime directories or edit saved JSON to force
a mismatch through validation.

The current binary opens schema `2` only and rejects schema `1` without changes.
To retain a schema `1` database, stop its Server and Dispatcher and use a previous
release that supports the namespace migration before starting this release.
Preserve installation identity, stored JSON and workspace paths. A binary that
queries the old names cannot share a schema `2` database; rollback requires a
compatible binary or the matching database backup.

After interruption, let Dispatcher reconcile generations and recover registered
executions before admitting new work. Investigate identity, evidence hash or
unsupported-version errors using the original artifacts. Removing checkpoints,
lease tombstones or acceptance receipts can invalidate recovery guarantees.

See [supported formats](formats.md) for supported representations and
[development](development.md) for focused regression commands.
