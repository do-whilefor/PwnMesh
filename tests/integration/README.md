# PwnMesh integration and acceptance suites

Run these commands on Linux from the repository root. Dependencies must already be
available: Go, a C compiler for race detection, Python 3 and ripgrep. Offline
tests use local fixtures, loopback HTTP servers and temporary files. They do
not require Docker or a model service.

| Suite | Entry points | Command | Additional prerequisites |
| --- | --- | --- | --- |
| Offline integration | All unconditional tests, including scenario validators and observation helpers | `bash scripts/test-integration.sh offline` | None |
| Docker acceptance | `docker_acceptance_test.go`; input transfer in `internal/docker/inputs_integration_test.go` | `bash scripts/test-integration.sh docker` | Docker CLI, Linux daemon at `/var/run/docker.sock`, existing `PWNMESH_DOCKER_TEST_IMAGE` |
| Real model acceptance | `live_acceptance_test.go` | `bash scripts/test-integration.sh live <scenario>` | Explicit model environment and `PWNMESH_LIVE_OUTPUT`; worker image and Docker except for `readiness` |
| Retained evidence / timing | `replay_acceptance_test.go` | Select an exact test with `go test -run '^TestName$' ./tests/integration` | The corresponding replay directory or timing output environment variable declared by that test |

The Go package stays shared because Docker workloads, live workloads and offline
oracle tests use the same evidence validators and fixture helpers. External
entrypoints are separated into the files above; helper tests remain enabled in
normal `go test ./...` and CI. Filenames containing `live` describe the evidence
being checked and do not by themselves imply a model call. Never exclude all
`TestLive*` tests from offline CI.

`scripts/check.sh` and the offline command clear inherited Docker, live, replay
and timing opt-ins as well as model credentials. The Docker command enables only
Docker acceptance and uses scripted local model servers. Live commands choose
one exact external test and clear other inherited run-selection gates. Existing
per-test environment gates remain supported for direct Go test invocation.

Real model prerequisites are `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`, and
`ANTHROPIC_MODEL` or `ANTHROPIC_DEFAULT_FABLE_MODEL`. Supply credentials through
the process environment. Scripts do not read `.env`, build or pull images, or
provision the separate efficiency fixture service. Live acceptance can incur model usage and has a longer
timeout; it is outside default CI.

Available live scenarios:

- `readiness`: production readiness handshake only; no Docker.
- `smoke`: one Worker calculates a local sum and input hash, retains evidence,
  publishes one Fact and completes the project; bash/coreutils are sufficient.
- `contention`, `orchestration`: concurrent execution and orchestration.
- `asset-graph`, `worker-command-graph`, `worker-mixed-graph`: graph execution and provenance.
- `curation-relations`, `curation-support`: evidence correction and current support.
- `release-audit`, `release-audit-reuse`: release acquisition and verified reuse.
- `scenario-skills`, `scenario-review`, `stale-repair`: scenario-specific behavior.
- `efficiency`, `efficiency-orchestration`: require the separately provisioned
  local fixture at `http://127.0.0.1:18080/health`. Its local analysis scripts are
  not part of this repository.

Acceptance tests use scoped container namespaces and clean up their containers.
The supplied image is an existing developer resource and is not deleted by these
commands. Remove any image created only for a one-off validation after use.
Retained evidence replays read original artifacts without calling the model;
`TestWorkerCommandGraphTiming` is a local scheduling measurement, not a replay.
