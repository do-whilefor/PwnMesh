# PwnMesh architecture

PwnMesh is a Linux application with three commands in `cmd/pwnmesh`: `serve`,
`dispatch`, and `worker`. The browser talks to Server; Dispatcher prepares and
schedules work through Server; Docker runs Workers and bridges their graph and
model requests. Server owns the authoritative SQLite state. Workers retain local
session journals, tool artifacts and acceptance receipts in their workspace.

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

The model bridge keeps upstream model credentials in Dispatcher rather than
giving tools the provider credentials. Job inputs are immutable snapshots or
bound state views. A model response alone is not a successful board mutation:
Execute evidence must pass acceptance, and Decide/Curate rely on server receipts.

## Ownership

| Location | Responsibility |
| --- | --- |
| `cmd/` | CLI configuration and process wiring |
| `internal/server/` | HTTP decoding, admission, responses and read/write transaction choice |
| `internal/board/` | Transactional business operations, admission, mutations and consistent state assembly |
| `internal/board/model/` | Storage-free canonical state, shared execution values, evidence rules and bounded reading views |
| `internal/board/internal/sqlstore/` | SQLite connections, transactions, targeted graph writes, execution/lease/input rows, bounded UI reads, lifecycle and archive storage, metadata, events, receipts, snapshots, schema and migrations |
| `internal/board/internal/projection/` | Metadata decoding and pure state, dependency support and asset derivation |
| `internal/protocol/` | Shared jobs, results, RPC validation, paging and trace binding |
| `internal/dispatcher/` | Ownership, reconciliation, scheduling, leases, delivery |
| `internal/docker/` | Container ownership, transport, graph/model bridges |
| `internal/worker/` | Session lifecycle, recovery, runtime tools, evidence acceptance |
| `internal/worker/internal/phase/` | Pure deadline, allowance, recovery and phase transition policy |
| `internal/worker/internal/journal/` | Append-only event bytes, committed-prefix verification, crash-tail archival and journal checkpoints |
| `internal/worker/dag/` | Mixed-node execution, artifact inputs, snapshots and verified reuse |
| `internal/agent/` | Provider-independent model/tool loop and context checkpoints |
| `internal/provider/`, `internal/modelrpc/` | Anthropic-compatible model client and bridge wire contract |
| `internal/tools/`, `internal/process/` | Workspace tools and Linux process lifecycle |
| `internal/config/`, `internal/modelconfig/` | Dispatcher configuration, model settings and environment resolution |
| `internal/contract/` | Result/evidence contracts and policy checks |
| `internal/artifactcheck/`, `internal/cvss/` | Deterministic artifact validation and vulnerability scoring |
| `internal/workergraph/` | Local dependency graphs within a Worker run |
| `web/` | Embedded browser UI and browser/Node regressions |
| `tests/` | Cross-component scenarios and opt-in acceptance |
| `container/` | Worker image, tool environment and HTTP replay helper |
| `scripts/` | Linux build, checks and explicit acceptance commands |
| `docs/` | Maintained architecture, protocol, recovery, format and development guides |

`board/model` owns the canonical State, its JSON encoding and decision hash,
shared execution/scheduling/input values, evidence rules, bounded context and
completion views, history views and task budgets. Board and Config retain aliases
for their existing callers. Protocol, Worker, Dispatcher and Docker use the
storage-free definitions directly; their production imports do not depend on
aggregate Board or SQLite.
Board retains business validation and transactional operation composition.
Its private `sqlstore` package maps canonical graph values to rows using the
caller's existing transaction. It also owns immutable snapshot storage, chunk
verification and execution row persistence. Board constructs snapshot identities
and decodes canonical State; storage retains the exact bytes and keeps chunks
and manifests together with a savepoint. Execution admission queries return
observed pending work, retry grants and prior attempts. Board chooses their
scope and decides blocking, write conflicts and retry eligibility. Summary reads
keep archived Job/Result bodies out of scheduling and state projection. Execution
registration, result updates, revocation and resume counters use that same caller
transaction; Board still validates transitions and combines them with review
state, revisions and failure events. Historical execution archival streams rows
in registration order. Retry/dispute queries, dispatcher ownership and the
read-lease clock comparison still contain SQL in Board. There is one canonical
model, not a duplicate transport model.

Targeted graph storage owns immutable Fact insertion, scoped ID sequences,
Intent conclusion/abandonment/deletion and asset anchors. Board normalizes assets,
chooses authorization and diagnoses allocation conflicts. Storage reports only
primary-key collisions as an existing Fact; other constraint failures propagate.
Writes preserve unrelated graph columns. Write reservations combine current
Intent leases and pending executions; Board applies Step path conflict policy.
Evidence conclusion uses a local savepoint for its optional final Fact, receipt,
Intent update, revision and completion event, including a nested Fact action.

Input storage separates metadata from binary file contents and preserves batch
receipt text. Board owns filenames, content-derived identities, quotas, workspace
paths and replay validation. An upload's file, hint, revision and event share a
local savepoint, so a caller can handle failure without keeping a partial upload.

UI storage reads bounded public result fields, streams execution changes and
aggregates project summaries without loading historical Jobs. Board owns Unicode
truncation, encoded page budgets, continuation cursors and generation fences.
Visitors stop as soon as the page is full and retain the caller's read snapshot.
Trace storage streams registered Job bytes for Board to validate before exposing
file access. Execution identity queries omit Job/Result; decision input queries
omit Result. Decision read views preserve raw bytes, while Board validates their
version and project binding. Observation writes update Result alone, retaining
execution identity, status and read-view cleanup semantics.

Sqlstore owns state metadata, event and action receipt rows. Board supplies the
encoded metadata, initial state, revision changes and canonical receipt bytes,
then combines them with graph mutations and execution settlement. Event-only
increments retain existing metadata bytes. Event reads stream raw bytes for Board
to decode, or for round archival to retain unchanged; discovery remains bounded
to 1000 entries while archival has no page limit. Change indexes use the SQL
revision and project only the requested JSON fields. Curate folds streamed event
markers into its own review and correction policy. Receipt storage preserves
request/response text and insertion order; Board alone decides replay eligibility.
Board serializes each round's State and input manifest and streams its events,
receipts and executions to archive storage. Sqlstore hashes and retains those
exact bytes, orders archive metadata and reads BLOB byte ranges. Board owns
public cursor/limit validation, continuation fields and missing-record errors.
Restart deletes the current state records only after archival and revocation,
within the same caller transaction. Storage resets current round rows while
retaining inputs, hints, assets, snapshots, counters, lease tombstones and history.
Execution deletion triggers run before their UI change markers are cleared;
the UI revision sequence survives and generation rejects old cursors.

Pause markers and termination/round records belong to sqlstore. Board decides
which paused executions may retry and cancels pending work through its normal
execution transition, preserving immutable pending results and failure events.
Termination also cancels outstanding retry grants without replacing their
stored result or emitting a second failure event.

The private `projection` package consumes canonical graph/runtime values and
compact metadata without querying storage or changing its input. Metadata decoding
keeps historical status adaptation at the read boundary. Board loads metadata,
latest attempts, failure diagnostics and assets within one transaction snapshot.
Pure projection computes effective evidence, Step dependencies and finding asset
associations. Derived Finding support uses a separate slice: Curate explicitly
retains the recalculated support in its pending metadata after each correction,
without changing the input used to measure progress through remaining groups.

The embedded frontend keeps project form rules in `project-model.js`, provider
settings in `model-settings.js`, and activity rendering in `activity-view.js`.
`app.js` owns UI state and lifecycle; `service.js` owns fenced refreshes and caches.

## Lifecycle and boundaries

Dispatcher's `scheduler.go` gives the Run/Step ordering. `runtime.go` owns startup,
wakeups and shutdown; `reconciliation.go` handles project generations and cleanup;
`scheduling.go` handles admission; `lease.go` owns heartbeats; `executions.go`
recovers registered runs and delivers saved results. Recovery precedes fresh
admission, and workers join before dispatcher ownership is released.

Worker's `worker.go` coordinates its session. The `session_*_linux.go` files own
locking and cancellation, persisted state, recovery, Agent setup and phase
effects. The private `phase` package receives values and decides transitions,
without access to the session, Agent, prompts, filesystem or provider.
The private `journal` package owns its file handle, digest, append position and
crash-tail archive. It returns a synced checkpoint and recovery values; Session
owns the identity, transcript and atomic session replacement. Decision metrics
belong to the per-run runtime, with historical shadow-event adaptation applied
only during replay. Neither Session nor nested Agents access the journal handle.
`graph_run_linux.go` and `acceptance_linux.go` enforce the outer
evidence receipt boundary. Journals/checkpoints precede side effects; durable
receipts are checked before issuing more model or tool calls.

`worker/dag` adapts command and Agent nodes to the existing `workergraph` scheduler.
Worker supplies the canonical execution identity and an Agent callback; Agent
sessions, scenario policy and publication stay in Worker. DAG owns dependency
files and artifact receipts, including verification when Worker selects evidence.
The child package never imports Worker or a model provider.

Worker Options contains caller configuration. A per-invocation role runtime owns
tools, private drafts, commit receipts and graph version/conflict values; it never
holds pointers into the durable session. Session publishes proposed phase states
through its persistence boundary, then performs prompt/evidence effects. Failed
transition saves restore the prior Session and Loop phase values. Explicit state
conversion preserves the existing session JSON fields and order. Session also
captures graph values and the explicit Agent checkpoint when saving. Graph update
reading returns a proposed cursor and notice; Session publishes them with the
transcript and restores the prior cursor on a rejected save. Acceptance reads an
identity-bound committed transcript rather than interpreting session storage
fields itself. The retained session JSON and journal commit boundaries are unchanged.

Board's `Store.Do` keeps lease/generation validation, idempotency lookup, mutation,
revisions and receipts in one `BEGIN IMMEDIATE` transaction. `Store.View` uses
query-only WAL readers for consistent snapshots without reserving the writer.
Keep these operations together even when their source files are split. Health
and settings GET requests use observational reads and do not expire leases;
scheduling and settings mutations retain their write transaction maintenance.
Server calls semantic Board operations for project management and all three
role leases; it does not execute SQL. Board callbacks expose query operations,
but no underlying `sql.Tx` field or transaction lifecycle methods. A private query
wrapper also prevents recovering the raw transaction through a type assertion.
The private store owns normal commit and rollback; query methods still permit raw
SQL inside Board. It does not interpret or restrict SQL statement text.
Multiwrite management operations, including restart and termination, use a local
savepoint, so callers may handle a rejection and continue the outer transaction
without retaining partial archive rows, revocations, events or projection resets.
Decision batches pass their validated scope only to private action and completion
calls. The transaction carries no mutable batch authorization. Public state actions
and direct completion always enforce the registered planner's protocol boundary;
each batched action still checks its lease, role, idempotency and capacity. The
batch owns its local snapshot and savepoint through preview, rollback or commit.
Board chooses lease authority, eligibility, expiry scope, supported orchestration
version and timeouts. Sqlstore applies targeted lease writes, conditional curator
release, project revocation and expiry in the caller's transaction. Intent release
and expiry retain heartbeat metadata; completed intents keep their owners. Planner
cleanup after batch commit or retry uses the same storage operation as lease release.
Source-boundary tests prevent SQL from returning to HTTP
handlers or storage dependencies from returning to execution clients.

The schema and migration sequence live in `sqlstore/schema.go` and
`sqlstore/migrations.go`; Board does not own connection pools or migration state.
Schema version `2` uses `pwnmesh_*` names for branded SQL objects. A supported
version `1` database receives a transactional namespace migration on open; new
databases create the current names directly. Runtime queries use only the current
names. This migration preserves stored records and execution identities.
Current-format validation stays beside storage and execution boundaries.
Unsupported inputs fail before they can trigger fresh work or implicit upgrades.

See [protocol](protocol.md) for wire identities, [recovery](recovery.md) for
operation semantics, [supported formats](formats.md) for supported formats, and
[development](development.md) for executable checks.
