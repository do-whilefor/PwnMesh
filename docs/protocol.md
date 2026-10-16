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

HTTP failures retain `detail` and add an authoritative `code`. Graph bridge
responses retain legacy `error` text and add `failure: {code, status, detail}`.
Recovery uses codes such as `state_changed`, `dependency_invalidated` and
`recovery_exhausted`, with HTTP status checks. Unclassified failures use
`request_failed`; only the transport compatibility adapter parses old uncoded
responses. Worker role policy accepts a narrow Graph round-trip function, with
the existing container bridge as the default transport.

## Observations and coordinated checks

Fact publication and final Fact handoff accept optional immutable `derived_from`:
at most 32 unique, nonempty observation Fact IDs, each at most 256 bytes. The
sources must already exist and be effective; `origin`, forward references and
`null` are not accepted. Omit the field for independent raw observations.
Derivation participates in support invalidation, context/evidence traversal and
completion checks. Fact `sources` read pages return these explicit derivation IDs;
stored observations and frozen inputs are never rewritten by a later correction.

Execute `read_updates` follows registered inputs through derivations, accepted
Step prerequisites and correction sources. Beyond 1000 intervening events,
`rebased: true` returns their current dependency closure from one snapshot. The
Worker advances its bound cursor only after a complete response is saved with
its transcript. Immutable user inputs already bound in the Job are not resent.
Missing observations or byte/node limits retain a pending cursor; the original
Job and permissions remain unchanged.

Decide's Step-add payload accepts optional `assessment: {claim, scope}`. The claim
normalizes whitespace and scope trims outer whitespace. This contract is immutable
and is checked against every Candidate from that Step using the same identity
rule as Finding grouping. Opposing judgments use the same proposition and scope
with different `status` values; negating the claim creates a different proposition.
Steps without an assessment retain free-form Candidate publication.

## Explicit curation requests

When the bound input contains an unreviewed pending curation request, a Curate commit requires
`request_review` with `disposition`, `checked_sources` and a nonempty `reason`.
Checked sources must contain every requested Fact exactly once and no others.
The server binds the review to the request revision; an optional supplied revision
must match. An empty `groups` array alone cannot acknowledge the request.

`resolved` and justified `no_change` dispositions permit acknowledgement without
invented relations or Findings. The request clears only when all pending groups
are covered; a partial commit retains its original scan boundary and request.
`needs_review` retains the unresolved review, request and completion blocker while
allowing evidence-gathering work. Unrelated Curate commits may omit `request_review`
and must preserve that unresolved review. It remains visible in curation progress
and planner context.

Decide may explicitly request another review of an unresolved question. The new
request must include every original source ID and may add effective observation
sources. Original source identities may remain even if a later correction made
them ineffective, so the disagreement can still be reviewed. An unrelated request
cannot replace the unresolved question; a different unreviewed pending request
also cannot be replaced. Reviews, effects and receipts commit atomically.

Disputes expose optional server-owned `review_revision` and `review_step_offset`.
Reopened reviews require input at or after that revision. The offset preserves
historical `review_step_ids` while limiting the current evidence period to two
review Steps and two execution attempts, including retries. Only new effective
retained evidence renews that allowance; old runs cannot resolve the new period.

## Asset reads

Asset identity remains the exact normalized host, service or endpoint identity.
`asset_ids` filters match those identities by default. Add
`asset_neighborhood: true` to include existing ancestors and descendants:
host/service/endpoint containment is derived from URL syntax, never DNS or target
behavior. Selecting an endpoint includes its known service and host, not sibling
endpoints; selecting a service includes its endpoints and host, not other services.
The option requires `asset_ids` and is supported on the same asset-filterable
sections. It creates no catalog entries. Preserve both filters while paging.
Asset records expose read-only `ancestor_ids` keyed by `host` or `service`.
Neighborhood queries accept an absent parent ID only when it is derivable from
catalog children in the same snapshot; exact lookup still requires an entry.

Read `anchors` to distinguish provenance. An anchor with `discovery_step_id`
means an untagged result was discovered through that producing Step's declared
target. Explicit result assets override this fallback, even when different.
Discovery does not establish observed scope, support validity or permission;
inspect the Fact and retained evidence. These derived anchors are frozen with
snapshots and do not leak into a restarted round.

## Local DAG declarations and retained evidence

Every `run_graph` node declares `resources`; `[]` asserts independent effects.
Plain resource keys conflict by equality. Path-shaped keys and `path:NAME` use
lexically normalized paths relative to the workspace, with parent/child overlap
checks. Conflicting nodes must be explicitly ordered. Declared shared workspace
outputs must fall within the registered Step's `write_paths`; ordinary artifacts
remain private to the node directory. Declarations are advisory coordination,
not a filesystem sandbox or a guarantee against undeclared shell side effects.

`verified_evidence[].verified_at` records byte verification. The response's legacy
`observed_at` is an alias of verification time. `execution_started_at` and
`execution_finished_at` identify the original execution window, including in
`reused_from` provenance. Verification/reuse does not establish target freshness.
Reuse from an interrupted graph requires a successful, verified dependency
subgraph independent of all pending/running nodes and their declared resources.
Source locks and whole-source structure/identity checks still apply; artifact
verification covers the selected dependency closure. An unrelated damaged output
does not block reuse. Original nodes in repair graphs remain eligible, but any
imported node in the selected closure prevents recursive reuse. Uncertain effects
and unfinished outputs are not accepted.

## RPC and serialization

Graph RPC is limited to 128 KiB; pages reserve 1 KiB for the envelope. Requests
validate IDs, digests, offsets and operations against the Job. Read observations
are runtime metadata, not write authority or proof of success.

Preserve JSON field order, tags, omissions and null/empty distinctions for identity
hashes. Do not normalize stored Job/Result bytes during upgrades. Representation
changes require versioned migration and identity tests.
