# Dual Graph Architecture

PwnMesh separates shared project reasoning from local execution. The browser
projects the shared graph and links execution summaries; it does not
currently expose the topology of a Worker's local command/agent DAG.

## Execution Flow

1. Project creation stores the original `origin` and `goal` inputs. The current
   orchestration protocol runs three roles: Decide (`reason`), Execute
   (`explore`), and Curate (`curate`). The [scheduler](../internal/dispatcher/scheduler.go)
   selects work from the server's readiness view and claims a role or Step lease.
2. [Execution registration](../internal/server/execution_input.go) resolves the
   assignment on the server, captures an immutable input snapshot, and binds
   accepted upstream results. [Registration checks](../internal/board/executions.go)
   enforce project generation, lease identity, protocol, and dependencies.
3. Decide drafts goals and Steps, then publishes a version-checked atomic
   [decision batch](../internal/board/decision_batch.go). A commit can wake the
   dispatcher immediately. Execute runs ready Steps; ordinary evidence can
   return directly to planning without an intervening curation turn.
4. Execute owns an [Agent session](../internal/worker/session_linux.go). It can
   use ordinary tools or [run_graph](../internal/worker/command_graph_linux.go)
   for a local DAG of commands and child Agents. Intermediate observations are
   published explicitly through `graph_action`; local tool success alone does
   not publish a Fact or complete a Step.
5. The Worker verifies its retained evidence and records a durable local
   [acceptance receipt](../internal/worker/graph_run_linux.go). The dispatcher
   delivers the result as `result_pending`. The [server applies it](../internal/server/executions.go)
   in one transaction with [Step conclusion](../internal/board/fact_completion.go),
   rechecking current prerequisites before accepting the result Fact.
6. Curate reconciles candidates, disputes, and corrective Fact relations from
   its immutable input. [Curation](../internal/board/curation.go) validates
   relations in order and commits the batch and cursor atomically. Newly invalid
   support reaches Decide; completion requires the separate root-goal contract.

## Graph Roles

| Surface | Purpose | Authoritative records |
| --- | --- | --- |
| Shared graph | Explain goals, authorized work, observations, and supported conclusions | Goals, Steps, Facts, Findings, candidates, disputes, and Fact relations in [board state](../internal/board/state.go) |
| Local execution DAG | Schedule commands and child Agents within one Execute attempt | Definitions, dependencies, node states, artifacts, and durable checkpoints in [workergraph](../internal/workergraph/graph.go) |
| Asset associations | Find records about the same normalized host, service, or endpoint | [Assets and anchors](../internal/board/assets.go); associations are neither evidence nor execution dependencies |

The shared graph contains two distinct Step inputs. `from` identifies evidence
premises; `depends_on` identifies Steps whose accepted results must be available.
[Dependency projection](../internal/board/step_dependencies.go) computes
`blocked_by`, `invalid_sources`, and transitive `support_valid` without rewriting
original observations. A completed Step can later lose support while retaining
its historical completion status.

Finding support requires nonempty effective Fact sources. Retained evidence
excerpts alone do not make a Finding supported; both state projection and
curation preserve this distinction.

Local DAG dependencies instead order local callbacks and declared artifact
handoffs. A DAG may succeed while the enclosing Step result is rejected because
shared evidence changed. [Dual-graph tests](../internal/dispatcher/dual_graph_linux_test.go)
cover that distinction, same-asset independence, and explicit abandonment.

## Connection and Lifecycle Rules

- `Fact.source_step_id` identifies the publishing Step, including intermediate
  observations. `Step.result` identifies its accepted result. A producer edge
  must not imply acceptance or inherit all of the Step's failed prerequisites.
- Execution IDs identify attempts. The registered lease is `backend@id`, and
  Fact provenance can use that lease while retained evidence uses the execution
  ID. Match explicit identities; never attach the latest attempt merely because
  it shares a Step ID.
- The [graph bridge](../internal/dispatcher/executions.go) supplies identity from
  the registered job. `read_graph` reads shared evidence, `read_snapshot` retains
  the original input, and writes require the execution fence. The curator's
  reads always use its immutable snapshot.
- A corrected `from` premise can leave a running Execute alive to retain
  independent observations; [heartbeat readiness](../internal/board/step_ready.go)
  does not waive final acceptance checks. Explicit abandonment, invalid accepted
  execution dependencies, and project lifecycle changes have separate effects.
- [Restart](../internal/board/restart.go) archives the previous round, revokes
  leases, clears current projections, and advances generation. Browser selection
  and execution summaries must remain bound to that project and round.

## Browser Projection Contract

[Graph mapping](../web/static/graph-data.js) turns explicit record references into
typed edges. Missing endpoints remain diagnostics; layout and navigation must
not manufacture evidence. Producer edges connect intermediate Facts to their
Step; accepted-result edges remain distinct and must not be duplicated.

Initial and explicit flow arrangement and upstream/downstream selection operate on these
typed connections through [layout](../web/static/layout.js),
[graph view](../web/static/graph-view.js), and [graph controls](../web/static/graph.js).
Waiting dependencies, invalid support, and pause state have separate filters.
Arrangement is a view operation and does not alter server scheduling. Live
refreshes preserve existing coordinates, including manual positions; the
arrange control recomputes the layout on request. Cycles remain finite, while
ownership and corrective relations do not impose execution order.

The [inspector](../web/static/inspector.js) can associate a selected Step or Fact
with public execution summaries. The [UI execution projection](../internal/board/ui.go)
contains bounded status and result text, not local DAG nodes or private jobs.
[Refresh](../web/static/service.js) checks generation around parallel reads;
archived executions instead carry their generation inside the saved job.
Inspectors must filter both forms and retain truncation markers when displaying
results. A failed or rejected attempt's text is not accepted evidence.

Direct relationship links navigate to either endpoint and clear incompatible
status filters. Step inspection lists its Explore attempts; Fact and Finding
inspection uses explicit producer or evidence run identities. Output readers
preserve successful public conclusions and error text without exposing saved
jobs. Narrow screens stack the canvas and inspector without hiding controls.
