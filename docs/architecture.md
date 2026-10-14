# PwnMesh architecture

PwnMesh is a Linux application with three commands in `cmd/pwnmesh`: `serve`,
`dispatch`, and `worker`. The browser talks to Server; Dispatcher prepares and
schedules work through Server; Docker runs Workers and bridges their graph and
model requests. Server owns the authoritative SQLite state. Workers retain local
session journals, tool artifacts and acceptance receipts in their workspace.

```text
Browser -> Server -> Board transactions -> SQLite
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
| `internal/server/` | HTTP admission, authorization, read/write transaction choice |
| `internal/board/` | Transactional mutations, projections, SQLite and migrations |
| `internal/board/model/` | Storage-free canonical state, identity, evidence rules and reading views |
| `internal/protocol/` | Shared jobs, results, RPC validation, paging and trace binding |
| `internal/dispatcher/` | Ownership, reconciliation, scheduling, leases, delivery |
| `internal/docker/` | Container ownership, transport, graph/model bridges |
| `internal/worker/` | Session lifecycle, recovery, runtime tools, evidence acceptance |
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
curation values, evidence-support rules, history views, input identities and task
budgets. Board and Config retain aliases for their existing callers. SQL-backed
projections and mutations remain in Board, within the existing transaction.
`protocol` uses shared model values but still imports aggregate Board views, so
SQLite remains a transitive dependency. Do not introduce a second model merely
to hide that coupling.

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
transitions. `graph_run_linux.go` and `acceptance_linux.go` enforce the outer
evidence receipt boundary. Journals/checkpoints precede side effects; durable
receipts are checked before issuing more model or tool calls.

`worker/dag` adapts command and Agent nodes to the existing `workergraph` scheduler.
Worker supplies the canonical execution identity and an Agent callback; Agent
sessions, scenario policy and publication stay in Worker. DAG owns dependency
files and artifact receipts, including verification when Worker selects evidence.
The child package never imports Worker or a model provider.

Board's `Store.Do` keeps lease/generation validation, idempotency lookup, mutation,
revisions and receipts in one `BEGIN IMMEDIATE` transaction. `Store.View` uses
query-only WAL readers for consistent snapshots without reserving the writer.
Keep these operations together even when their source files are split. Health
and settings GET requests use observational reads and do not expire leases;
scheduling and settings mutations retain their write transaction maintenance.

The schema and migration sequence live in `schema.go` and `migrations.go`.
Schema version `2` uses `pwnmesh_*` names for branded SQL objects. A supported
version `1` database receives a transactional namespace migration on open; new
databases create the current names directly. Runtime queries use only the current
names. This migration preserves stored records and execution identities.
Current-format validation stays beside storage and execution boundaries.
Unsupported inputs fail before they can trigger fresh work or implicit upgrades.

See [protocol](protocol.md) for wire identities, [recovery](recovery.md) for
operation semantics, [supported formats](formats.md) for supported formats, and
[development](development.md) for executable checks.
