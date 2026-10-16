# PwnMesh recovery

Recovery preserves execution identity and authority. Retrying does not authorize
duplicate side effects or restore a revoked lease.

## Recovery operations

| Operation | Behavior |
| --- | --- |
| Infrastructure interruption | `protocol.ErrInterrupted` permits bounded recovery of the same run |
| Ordinary cancellation | Stops the attempt; does not grant same-run recovery |
| Pause / Continue | Grants retry only to eligible attempts affected by the pause |
| Explicit execution retry | Validates the grant, generation, eligibility and authority |
| Result delivery retry | Sends the retained result without rerunning models or tools |
| Project restart | Archives the round, increments generation and revokes old runs; preserves human inputs, counters and workspace files |
| Project termination | Stops unfinished work and retains history; another round requires explicit restart |

Same-run recovery allows two resumptions after the initial attempt. It preserves
original deadlines and consumed repair, continuation and recovery allowances.

## Durable evidence and authority

- Persist session identity and proposed transitions before their effects. Failed
  saves restore the caller's in-memory changes. Session replacement is atomic,
  but a directory-sync failure after replacement leaves durability uncertain.
- Verify the journal's saved offset and checksum before recovery. Complete crash
  records remain append-only; archive and sync an incomplete tail before truncation.
  Acceptance reads only the identity-bound transcript or verified committed prefix.
  Unknown historical events remain intact without advancing current replay state.
- Execute receipts bind the run and input to session/artifact hashes, which are
  verified again on read. A changed repair target cannot reuse prior success.
  Deterministic acceptance failure is terminal; an uncertain callback retains its
  existing session recovery path.
- Decide and Curate check the authoritative server commit receipt before planning
  again. Discarding a conflicting draft neither erases a commit nor grants budget.
  Automatic retries retain the failed Job/Result and count saved attempt history;
  revocation, lease release and the retry grant roll back together on failure.
- Require current acceptance receipts and graph-node definition digests. An old
  `graph.json` alone cannot authorize recovery; do not reconstruct missing identity
  bindings. See [supported formats](formats.md).
- A child Agent's retained terminal receipt can reconcile a running graph node
  after verifying its run/node/input identity, committed final journal message,
  output and artifact hashes. This performs no model or tool calls. Commands,
  unfinished Agents and sessions without a terminal receipt remain fail-closed.
- An interrupted receipt import re-verifies its source without replaying it.
  Graph resume and inspection share a regular-file reader that rejects symlinks,
  special files, trailing JSON and detected changes during reading. Retained error
  strings have no size cap, so checkpoint loading has no fixed total byte limit.

## Restore and cleanup

1. Retain a consistent SQLite backup and matching workspace files, journals,
   evidence and receipts. Use SQLite backup tooling for a live database; copying
   only its main file during WAL writes is unsafe.
2. Check the database version before starting. This binary only initializes or
   opens schema `4` and rejects older schemas without mutation. Use a compatible
   release or migration tooling for older data; rollback needs a compatible binary
   or matching backup.
3. Preserve installation identity, stored JSON and workspace paths. Do not rename
   runtime directories or edit checkpoints to bypass validation.
4. Let Dispatcher reconcile generations and registered executions before new work.
   Investigate identity, hash and version errors using the original artifacts;
   deleting receipts or lease tombstones can invalidate recovery.

| Data | Retention |
| --- | --- |
| Uploaded inputs, snapshots, archived rounds and execution inputs | Stored in SQLite; survive restart and cascade on project deletion |
| Scheduling projection | Rebuilt as needed; carries no independent authority |
| Worker workspaces, journals and receipts | Survive restart; back up and manage separately from SQLite |
| Upload/download staging | Request-scoped; unlinked immediately on Linux, with descriptors closed on completion or process exit |

Do not prune snapshots merely because an execution ended: archived Jobs may still
reference them. Row deletion makes SQLite pages reusable without necessarily
shrinking its file. Workspace removal follows the ownership rules in
[container operations](../container/README.md), not database row deletion alone.
