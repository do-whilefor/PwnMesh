# PwnMesh protocol

The shared contract is defined in `internal/protocol/`. `Job` carries the run ID,
kind, project graph and generation, immutable input, task budget and workspace.
`Result` carries status, text, retry classification and optional runtime evidence
checks. Browser labels are not wire names: Decide uses `reason`, Execute uses
`explore`, and Curate uses `curate`.

Storage-free state, execution, scheduling and input values live in
`internal/board/model`, with aliases in Board and Config. The same package owns
pure bounded context/completion views and their validation. Type relocation must
preserve exact JSON representation. Protocol and its execution clients import the
canonical model directly, without aggregate Board, SQLite or configuration loading
inside Protocol.

## Independent versions

| Domain | Current contract |
| --- | --- |
| Project orchestration | `orchestration_version: 1` for current execution |
| Registered execution result | `result_contract_version: 2` with `graph_rpc: true` for all three roles |
| Decide input | Decision context version `2` for current execution batches |
| Immutable input snapshot | Version `1`, bound to project, generation and state digest |
| Worker session | `schema_version: 1` with immutable session identity |
| Acceptance receipt | `schema_version: 1` in `acceptance.json` |
| Agent context checkpoint | Version `1`, separate from session and acceptance |
| SQLite schema | Schema `4` with `pwnmesh_*` object names; older schemas are rejected |

These numbers are independent. Incrementing one does not authorize rewriting
another. Server rejects unsupported project protocols, and
`requireCurrentExecution` rejects historical execution protocols for
resume/apply/retry. Worker recovery accepts current bound receipts and checkpoints.
Current database and protocol validation preserve Job, State, Result and
checkpoint serialization; see [supported formats](formats.md).

## Prepare, execute, settle

1. Server prepares a Job from current project state and registers immutable run
   input. Execution identity binds project, run, kind, backend, namespace and lease.
2. Dispatcher admits the registered work under its ownership and capacity limits.
   Docker transports the Job and binds its graph/model bridges to that launch.
3. Worker reads bounded input, executes tools and submits graph operations through
   the bridge. A request cannot choose a different run's authority.
4. Execute retains and verifies evidence before acceptance. Decide/Curate commit
   their authorized mutations and recover through authoritative server receipts.
5. A saved result can be delivered again without replaying its model/tools.
   Settlement still checks current generation, lease and idempotency state.

The lease string is bound to backend and run (`backend@run_id`). Project restart
increments generation; revocation tombstones fence late workers. State digests,
input revisions and expected versions prevent applying a decision to stale input.
Version checks and mutation/receipt writes belong in the same transaction.

Steps and project completion are submitted through decision batches; Step
completion uses the registered execution result protocol. The old direct
`POST /projects/{pid}/intents`, `/projects/{pid}/complete` and
`/projects/{pid}/intents/{iid}/conclude` routes are absent and return `404`.

## RPC and byte stability

`GraphRequest`/`GraphResponse` carry bounded graph operations. Graph RPC is limited
to 128 KiB, with page responses leaving 1 KiB for their envelope. Request IDs,
record digests, paging offsets and permitted operations are validated against the
registered Job. Read observations are Worker runtime metadata, not model-authored
permissions or evidence that a write succeeded.

Typed Job JSON participates in session identity and trace hashes. Preserve field
order, tags, omitted fields and null/empty distinctions when moving types. Raw
stored jobs/results must not be normalized during database upgrades. Current
State serialization keeps its canonical field order for those same hashes.

`internal/protocol/compatibility_test.go` locks wire fixtures and byte/hash
boundaries; `shared_values_test.go` covers the extracted execution values.
`dependencies_test.go` guards transitive production dependencies. Board fixtures
and Worker protocol/acceptance regressions protect the corresponding persisted
identities. Add a versioned migration and identity test before changing a
persisted representation.
