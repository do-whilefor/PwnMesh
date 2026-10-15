# PwnMesh architecture

PwnMesh is a Linux application with three commands: `serve`, `dispatch`, and
`worker`. Server owns the authoritative SQLite state. Dispatcher schedules work
through Server and launches Docker Workers. Workers retain session journals,
artifacts and acceptance receipts in their workspace.

```text
Browser -> Server -> Board transactions -> private sqlstore -> SQLite
              ^
Dispatcher ---+  prepare, register, heartbeat, settle
    |
    +-> Docker -> Worker -> Agent loop -> tools / workspace evidence
           |         |
           +---------+-> graph bridge -> Server
           +-----------> model bridge -> configured provider
```

The model bridge keeps upstream credentials in Dispatcher. Worker requests
cannot choose the provider endpoint or credentials. Jobs use immutable snapshots
or bound state views. A model response alone cannot establish success: Execute
evidence must pass acceptance; Decide and Curate require server receipts.

## Components

| Location | Responsibility |
| --- | --- |
| `cmd/`, `internal/config/`, `internal/modelconfig/` | Process wiring and configuration |
| `internal/server/` | HTTP handling and transaction selection |
| `internal/board/` | Business validation and transactional operations |
| `internal/board/model/` | Storage-free canonical state and shared execution values |
| `internal/board/internal/sqlstore/` | SQLite connections, schema, queries and persistence |
| `internal/board/internal/projection/` | Pure dependency and evidence projections |
| `internal/protocol/` | Job/result contracts and identity binding |
| `internal/dispatcher/`, `internal/docker/` | Scheduling, recovery, containers and bridges |
| `internal/worker/` | Durable sessions, tools and evidence acceptance |
| `internal/worker/dag/`, `internal/workergraph/` | Local dependency execution and verified artifact reuse |
| `internal/agent/`, `internal/provider/`, `internal/modelrpc/` | Model/tool loop, provider client and bridge contract |

Execution clients depend on `board/model`, not aggregate Board or SQLite. Board
composes policy and mutations; its private store persists values in the caller's
transaction. Pure projections neither query storage nor mutate their inputs.
Runtime status comes from authoritative execution records.

## State and transactions

`Store.Do` keeps lease/generation checks, idempotency, mutations, revisions and
receipts in one immediate write transaction. Multiwrite operations use local
savepoints so handled failures cannot retain partial changes. Server calls Board
operations and never runs SQL. Store owns commit and rollback.

`Store.View` uses query-only WAL readers for consistent snapshots. Observational
reads account for lease expiry without clearing authority or reserving the
writer. Planner reads that pin a view and decision previews still need a writer.

Board owns authorization, retry eligibility, conflicts, quotas and public cursor
validation. Storage retains snapshots, executions, receipts, events and archives.
Scheduling uses a disposable paged projection, invalidated transactionally when
source state changes; cached results never replace admission checks.

Uploads spool network bytes before opening a database transaction. Downloads
verify staged contents under one snapshot and release the database before
serving clients. Restart archives the round and revokes work atomically before
resetting current state. Inputs, assets, snapshots and history survive; generation
fences reject stale cursors. Decision batches own the completion path, with
individual authority and idempotency checks for every action.

Empty databases initialize the current schema. Existing databases must have the
complete schema version `4` and ledger; older or nonempty unversioned databases
are rejected without an implicit upgrade. See [supported formats](formats.md).

## Execution and recovery

Dispatcher recovers registered executions before admitting fresh work. It
reconciles project generations, maintains leases and delivers saved results.
Workers stop before dispatcher ownership is released.

Worker Session owns identity, transcript, checkpoints and atomic session
replacement. Its private `phase` package decides transitions without filesystem
or provider access. Its private `journal` package owns append-only bytes,
committed-prefix verification and crash-tail archival. Failed saves restore the
prior phase and cursor values.

Journals and checkpoints precede side effects. Recovery checks durable receipts
before additional model or tool calls. Acceptance reads an identity-bound
committed transcript. Per-run tools and drafts stay separate from durable state.

The local DAG executes command and Agent nodes through `workergraph`, retaining
dependency files and verified artifact receipts. Worker supplies execution
identity and the Agent callback; DAG does not import Worker or a provider.
See [recovery](recovery.md) for interruption and retry semantics.

## Browser

Vue 3, TypeScript and Vite produce `web/dist`, which Go embeds. Node is only a
build dependency. Hash routing separates navigation from root-level API routes.

The API layer owns transport and wire types; pure workspace models derive display
values. `project-resource.ts` owns accepted reads and caches, with one
`useWorkspaceSync.ts` polling lifecycle. Failed or superseded reads cannot
overwrite accepted state.

Project commands own writes and upload continuation. Client round checks do not
replace server generation fences. Results, details and exports share evidence
projections. The graph feature owns its DOM and scopes view state to project and
generation. Model settings have an independent lifecycle; secret drafts clear
when their page is disposed.

Files and colocated tests follow feature ownership. Components use local state
and props/events; server state has one resource owner. See [protocol](protocol.md)
for wire identities and [development](development.md) for focused checks.
