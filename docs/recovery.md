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

For Execute, an accepted receipt binds the run and input hash to retained session
and artifact hashes. Evidence is verified again on read. A changed repair target
cannot reuse the previous success. Deterministic acceptance failure is terminal;
an uncertain callback remains tied to its existing session recovery path.

For Decide and Curate, the server commit receipt is authoritative. Recovery checks
it before planning again. A private draft may be discarded after a conflict;
that does not erase a committed mutation or grant extra execution budget.

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

Stop the old Server and Dispatcher before opening a schema `1` database with the
current binary. The automatic upgrade to schema `2` renames SQL objects in one
transaction while preserving installation identity, stored JSON and workspace
paths. A previous binary that queries the old names cannot share that upgraded
database; rollback requires a compatible binary or the matching database backup.

After interruption, let Dispatcher reconcile generations and recover registered
executions before admitting new work. Investigate identity, evidence hash or
unsupported-version errors using the original artifacts. Removing checkpoints,
lease tombstones or acceptance receipts can invalidate recovery guarantees.

See [supported formats](formats.md) for supported representations and
[development](development.md) for focused regression commands.
