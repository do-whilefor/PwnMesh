# PwnMesh architecture

PwnMesh is a Linux application with three commands: `serve`, `dispatch`, and
`worker`. Server owns the authoritative SQLite state. Dispatcher schedules work
through Server and launches Docker Workers. Workers retain session journals,
artifacts and acceptance receipts in their workspace.

```text
Browser -> Server -> execution use cases / Board -> private sqlstore -> SQLite
              ^
Dispatcher ---+  prepare, register, heartbeat, settle
    |
    +-> Docker -> Worker -> Agent loop -> tools / workspace evidence
           |         |
           +---------+-> graph bridge -> Server
           +-----------> model bridge -> configured provider
```

The model bridge keeps upstream credentials in Dispatcher. Worker requests
cannot choose the provider endpoint or credentials. Worker sizing and upstream
serialization share a credential-free request format, including reasoning mode.
One Docker client shares a 16-request model limit across launches; waiting is
cancellable and consumes the request timeout. Model capacity is released before
response delivery. Jobs use immutable snapshots or bound state views. A model
response alone cannot establish
success: Execute evidence must pass acceptance; Decide and Curate require server receipts.

Graph RPC separates its 20-second operation deadline from a 5-second response
delivery budget. Both remain bound to the launch, so operation failure can be
reported while launch cancellation still stops delivery.

## Components

| Location | Responsibility |
| --- | --- |
| `cmd/`, `internal/config/`, `internal/modelconfig/` | Process wiring and configuration |
| `internal/server/` | HTTP handling and transaction selection |
| `internal/execution/` | Preparation, registration, settlement and recovery use cases |
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
Configuration owns the tool environment shared by Docker delivery and recovery
identity. Explicit empty values override image defaults; provider token rotation
does not change identity. See [formats](formats.md) for the recovery fence.
Configuration, execution admission and Worker preflight reject timeout seconds
that overflow runtime durations. Lease settings and Dispatcher reads enforce the
same limit; zero execution timeout still disables that deadline.

## State and transactions

`Store.Do` keeps lease/generation checks, idempotency, mutations, revisions and
receipts in one immediate write transaction. Ordinary composed Board writes share
one private savepoint helper; handled errors and failed releases roll back writes
and clear results. Decision previews always roll back. Server calls Board and
execution use cases without running SQL. Use cases accept the caller's `board.Tx`;
its public API exposes domain operations only. Store owns
commit and rollback. Composed use-case failures and savepoint cleanup errors must
propagate to Store for transaction rollback.
Registration rolls back retry-grant consumption, successor insertion and review
transition together even when its caller handles a failure.
Execution outcomes keep status, revocation, failure events and review transitions
atomic. Resume updates only its role's lease and recovery allowance, together.

The private query adapter forwards the caller's context to every SQL statement,
so cancellation can interrupt active database work as well as pool waiting.

Fact and Candidate publication appends one immutable metadata row and updates
the header atomically, without rewriting historical records. It reuses the
updated graph and metadata, refreshing execution and assets before projection.
State reads and control version checks still process the full state. Short
benchmarks do not establish a latency or throughput guarantee.

Frozen snapshot chunks are read in ordered pages of at most 128 entries. Each
page bounds chunk payloads to 2 MiB while retaining repeated identities, exact
bytes, digest checks and the caller's read snapshot.

Failed Step diagnostics load in one project-scoped event query per state read,
preserving the latest reason for each lease. Reads without failed Steps skip it.

Curation reuses execution and Asset inputs within one transaction while checking
successive corrections. Pure projections recompute support for each correction;
no cache crosses transaction or generation boundaries. Frozen State bytes and
decision digests share an explicit canonical encoding with compatibility tests.

`Store.View` uses query-only WAL readers for consistent snapshots. Observational
reads account for lease expiry without clearing authority or reserving the
writer. Planner reads that pin a view and decision previews still need a writer.
Private in-memory stores enforce the same read-only contract on their single
connection, restoring writable mode after callback failure or cancellation.

Board owns authorization, retry eligibility, conflicts, quotas and public cursor
validation. Storage retains snapshots, executions, receipts, events and archives.
Scheduling uses a disposable paged projection, invalidated transactionally when
source state changes; cached results never replace admission checks.

Batch uploads spool network bytes before opening a database transaction. Board
owns batch identity, receipt replay and file/hint admission in one savepoint;
fresh submissions verify staged sizes and digests. HTTP owns staging descriptors,
and receipt replay never reads their bytes. Downloads
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
Support projection, correction validation and Execute updates share prerequisite
traversal. Corrections cannot invalidate their own premises, including through
accepted Step inputs or another correction in the same curation batch.

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

Reopening a resolved dispute fences reviews to the new input revision. New
effective retained evidence renews its bounded review allowance; rewording or
reusing known evidence does not. Prior review and producer identities remain in
history, and stale pending review runs lose authority.

Assets are discovery identities, not evidence, dependencies, permissions or
coverage. A completed Step's untagged result can be found through its declared
assets using projected anchors marked with `discovery_step_id`. Explicit Fact
assets take precedence; this fallback is not stored as an observed association
or propagated into a Finding's support. Exact asset lookup remains the default.
Optional neighborhood reads include existing syntax-defined ancestors and
descendants, without DNS resolution, identity merging or sibling expansion.
These projections and their provenance are bound to the read snapshot and round.
Asset reads expose syntax-derived parent IDs even in endpoint-only catalogs.
Finding membership follows its curated Candidate boundary and current effective
support; an uncurated replacement Candidate cannot silently erase it.
Step deduplication indexes Asset anchors once per lookup. Filtered record byte
continuations must retain asset selectors, neighborhood mode and both versions.

## Execution and recovery

Dispatcher recovers registered executions before admitting fresh work. It
reconciles project generations, maintains leases and delivers saved results.
Workers stop before dispatcher ownership is released.
Startup reclaims old containers before model readiness checks. Background
cleanup admits at most eight operations and rotates inactive projects so repeated
failures cannot monopolize admission.

Worker Session owns identity, transcript, checkpoints and atomic session
replacement. Its private `phase` package decides transitions without filesystem
or provider access. Its private `journal` package owns append-only bytes,
committed-prefix verification and crash-tail archival. Failed saves restore the
prior phase and cursor values.

Worker translates durable phases into generic Agent request policy: pinned
instructions and a tool-disable reason. The Agent loop does not know Worker
phases, and request policy does not change the session checkpoint schema.
Native parallel tool groups run at most 16 callbacks at once, preserving result
order and serial/terminal barriers. Cancellation settles queued calls without
starting their tools.
Worker progress treats trace reads as observations, so repeating an unchanged
trace cannot replenish the continuation allowance as a mutation would.

Journals and checkpoints precede side effects. Recovery checks durable receipts
before additional model or tool calls. Acceptance reads an identity-bound
committed transcript. Per-run tools and drafts stay separate from durable state.

The local DAG executes command and Agent nodes through `workergraph`, retaining
dependency files and verified artifact receipts. Worker supplies execution
identity and the Agent callback; DAG does not import Worker or a provider.
Ordinary outputs stay in run/node directories. Declared path resources are
compared after lexical normalization, including parent/child overlaps; conflicting
nodes require dependency ordering. Declarations overlapping the shared workspace,
including its ancestors, must fit the Step's registered `write_paths`. These
declarations coordinate cooperating work; they do not constrain arbitrary shell
access or resolve filesystem aliases.

Reuse checks source structure and identity, then verifies retained bytes for the
selected dependency closure. Original nodes in repair graphs remain eligible;
closures containing imported nodes cannot be imported again. An interrupted graph
can supply a verified successful subgraph only when it is independent of every
unresolved node through dependencies and declared resources. Uncertain effects
are never replayed. Evidence reports verification time separately from the
original execution window; reuse does not observe a mutable target again.
An Agent node's bound terminal session can close a graph-checkpoint gap without
model or tool calls. Receipt imports reconcile by rechecking their retained
source; unfinished Agents and commands remain ineligible for replay.
Snapshot inspection and resume share retained-node validation. All saved input
bindings and dependencies are checked before recovery callbacks or checkpoint
writes, including when a graph adds new nodes.
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

Canvas details expose exact Asset associations and their provenance alongside
Fact derivations and support warnings. Discovery associations never establish
evidence validity.

Project commands own writes and upload continuation. Client round checks do not
replace server generation fences. Results, details and exports share evidence
projections. The graph feature owns its DOM and scopes view state to project and
generation. Model settings have an independent lifecycle; secret drafts clear
when their page is disposed.

Server settings coordination owns persistence and saved subscription credentials;
Kimi device authorization owns only pending login sessions. Ordinary API-key
saves do not acquire the login-session lock. Authorized drafts remain in memory
until save, and retries preserve credentials already rotated by Dispatcher.

View transitions expose their leaving state; feature containers own interaction
blocking so navigation can interrupt a fade. Cached inactive views settle without
starting animations. View and disclosure transitions share bounded playback and
cancellation while retaining separate state machines.

Files and colocated tests follow feature ownership. Components use local state
and props/events; server state has one resource owner. See [protocol](protocol.md)
for wire identities and [development](development.md) for focused checks.
