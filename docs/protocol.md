# PwnMesh protocol

`internal/protocol/` defines `Job` (run, kind, project graph/generation, input,
budget, workspace) and `Result` (status, text, retry classification, optional
evidence checks). Wire roles are Decide=`reason`, Execute=`explore`, Curate=`curate`.
Canonical values live in `internal/board/model`.

## Independent versions

| Domain | Current contract |
| --- | --- |
| Project | `orchestration_version: 1` |
| Execution result | `result_contract_version: 2`, `graph_rpc: true` for all roles |
| Decide context | Version `2` |
| Immutable input | Version `1`, bound to project, generation and state digest |
| Worker session | `schema_version: 1`, immutable session identity |
| Acceptance receipt | `schema_version: 1` in `acceptance.json` |
| Agent checkpoint | Version `1`, independent of session and acceptance |
| SQLite | Schema `4`, `pwnmesh_*` objects |

Unsupported protocols are rejected for execution, resume, apply and retry.
Recovery accepts current bound receipts/checkpoints. See [supported formats](formats.md).

## Run lifecycle

1. Server registers immutable input, binding project, run, kind, backend,
   namespace and lease.
2. Dispatcher applies ownership/capacity limits; Docker binds Job transport and
   graph/model bridges to the launch.
3. Worker uses bounded input and graph operations under its run's authority.
4. Execute verifies retained evidence before acceptance. Decide/Curate recover
   committed mutations through authoritative server receipts.
5. Saved results can be redelivered without replaying model/tools. Settlement
   checks generation, lease and idempotency.

Leases use `backend@run_id`. Restart increments generation; revocation tombstones
fence late workers. Digests, revisions and expected versions reject stale input;
checks and mutation/receipt writes share a transaction.

Steps and project completion use decision batches; Step completion uses the
registered result contract. Retired direct intent/completion routes return `404`.

## RPC and serialization

Graph RPC is limited to 128 KiB; pages reserve 1 KiB for the envelope. Requests
validate IDs, digests, offsets and operations against the Job. Read observations
are runtime metadata, not write authority or proof of success.

Preserve JSON field order, tags, omissions and null/empty distinctions for identity
hashes. Do not normalize stored Job/Result bytes during upgrades. Representation
changes require versioned migration and identity tests.
