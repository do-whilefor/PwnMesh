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

Fact and Candidate writes reuse the graph and metadata already updated within
the operation, refreshing execution and asset data before projection. Other
mutations retain their normal state readback. Short mutation benchmarks show
lower allocation costs; they do not establish a latency or throughput guarantee.

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

## Blackboard and discovery

The shared Board records authorized Steps, immutable observations and explicit
support. A Worker's local DAG coordinates execution and retained files; finishing
a DAG does not publish a shared Fact or complete a Step. Explicit `derived_from`
edges connect computed observations to their source Facts. Corrections invalidate
their derived descendants and supported Step results, without invalidating
independent raw observations merely because they share a producer or asset.

Decide may give coordinated Steps an immutable `assessment` with a canonical
claim and scope. Their Candidates use that proposition and express agreement or
disagreement through status. This supports deterministic reconciliation without
fuzzy claim merging. Free-form Facts remain available.

An explicit curation request requires a committed review of every requested
source. Curate can resolve it, justify no change, or retain it as `needs_review`.
A resolved/no-change request clears only after all pending groups are covered;
partial commits retain its request and scan boundary. `needs_review` still blocks
completion but permits evidence-gathering Steps and unrelated curation without
discarding its review. Decide can explicitly request another review while keeping
all original source identities, including ones later invalidated, and optionally
adding effective observations. An unrelated request cannot erase the unresolved
question.

Assets are discovery identities, not evidence, dependencies, permissions or
coverage. A completed Step's untagged result can be found through its declared
assets using projected anchors marked with `discovery_step_id`. Explicit Fact
assets take precedence; this fallback is not stored as an observed association
or propagated into a Finding's support. Exact asset lookup remains the default.
Optional neighborhood reads include existing syntax-defined ancestors and
descendants, without DNS resolution, identity merging or sibling expansion.
These projections and their provenance are bound to the read snapshot and round.

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
Ordinary outputs stay in run/node directories. Declared path resources are
compared after lexical normalization, including parent/child overlaps; conflicting
nodes require dependency ordering. Shared workspace declarations must fit the
Step's registered `write_paths`. These declarations coordinate cooperating work;
they do not constrain arbitrary shell access or resolve filesystem aliases.

Reuse checks retained bytes and original execution identity. An interrupted graph
can supply a verified successful subgraph only when it is independent of every
unresolved node through dependencies and declared resources. Uncertain effects
are never replayed. Evidence reports verification time separately from the
original execution window; reuse does not observe a mutable target again.
See [recovery](recovery.md) for interruption and retry semantics.

Control writes retain the full state-version comparison and completion checks.
Concurrent observations can still make a control attempt stale. Structured
`control task finished` logs report outcome, stale classification, input revision,
generation, resume count and local attempt wall time without input/error bodies.
Join existing decision observations by run ID for model metrics. Attempt wall
time includes startup and cleanup; it is not model time or a billing estimate.

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
