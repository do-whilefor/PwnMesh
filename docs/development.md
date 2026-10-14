# PwnMesh development

Use Linux. CI uses Go 1.26, Node 22, Python 3.13 and ripgrep. Go declares a 1.23
minimum in `go.mod`; use the CI toolchain for matching checks. Go race detection
also needs a working C compiler. Offline tests use local processes and loopback
HTTP fixtures. They do not require Docker or model credentials after dependencies
have been downloaded.

## Build and check

Run from the repository checkout:

```sh
go mod download
go mod verify
bash scripts/build.sh
bin/pwnmesh --help
bash scripts/check.sh
```

`build.sh [output]` defaults to `bin/pwnmesh`, uses read-only Go dependencies and
builds a static Linux executable. `check.sh` runs the Go race suite and vet,
browser JavaScript syntax/Node tests, Python HTTP replay and command regressions.
Select `go`, `web`, or `python` to run one check group. CI uses these same entries
and separately builds/smoke-tests `serve`, `dispatch`, and `worker`.

For a focused change, run its package and test names first. For example:

```sh
go test ./internal/board -run 'TestStore' -count=1
go test ./internal/worker -run '^TestAcceptance' -count=1
go test ./internal/protocol -count=1
```

Do not run real-model or full-image acceptance for a documentation-only change.
Default checks clear inherited acceptance controls and model credentials, so a
developer's live-test shell settings cannot silently enable paid/network suites.
The raw `go test` command still honors explicit environment gates; use the scripts
when isolation from those inherited settings is needed.

## Integration and acceptance

| Entry | Requirements | Scope |
| --- | --- | --- |
| `bash scripts/test-integration.sh offline` | Linux, Go/C toolchain, Python, ripgrep | Cross-component offline fixtures and acceptance-oracle regressions |
| `bash scripts/test-integration.sh docker` | Existing worker image, Linux Docker daemon | Scripted-model Docker acceptance plus Docker input delivery |
| `bash scripts/test-integration.sh live readiness` | Explicit model endpoint, authentication, model and evidence output directory | Real-model readiness probe |
| `bash scripts/test-integration.sh live <scenario>` | Model settings/output plus worker image and Docker daemon | One selected real-model acceptance scenario |

Docker example, after building an image deliberately:

```sh
docker build -f container/Dockerfile -t pwnmesh-worker:dev .
PWNMESH_DOCKER_TEST_IMAGE=pwnmesh-worker:dev bash scripts/test-integration.sh docker
```

Set `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`, and `ANTHROPIC_MODEL` (or
`ANTHROPIC_DEFAULT_FABLE_MODEL`) in the environment before a live run. Set
`PWNMESH_LIVE_OUTPUT` to the evidence directory and `PWNMESH_DOCKER_TEST_IMAGE`
for container scenarios. Scripts do not load `.env` or choose credentials for you.
Live runs may incur provider charges and are outside ordinary offline checks.

See [integration suites](../tests/integration/README.md) for named scenarios,
Docker socket constraints, retained-artifact replay and runtime knobs. External
entrypoints live in `docker_acceptance_test.go`, `live_acceptance_test.go` and
`replay_acceptance_test.go`. Shared helpers and offline oracle tests stay in the
same Go package; no export-only fixture framework or duplicated fixtures is needed.
Browser acceptance in `web/browser/` is independently opt-in.

Use `container/README.md` for image/runtime ownership. Delete only containers and
images created for an acceptance run once they are no longer needed; do not prune
unrelated Docker resources or remove persistent project evidence volumes.

## Repository hygiene

The tracked top-level source layout is `cmd/`, `internal/`, `web/`, `tests/`,
`container/`, `docs/`, and `scripts/`. Root build/configuration files remain at the
root. Only the five maintained guides are allowed into `docs/`; historical local
notes and acceptance output remain ignored. `refer/`, local development plans,
credentials, build outputs and temporary test artifacts are not source inputs.

Use `PwnMesh` in product text, `pwnmesh` in executable/module/storage names and
`PWNMESH_*` for project settings. Historical names belong only in migration or
rejection boundaries, their fixtures, and artifact exclusions. Keep comments that
explain constraints or failure modes; omit narration of the code itself.

Keep regression tests beside the owning Go package or in the existing integration
package. Preserve transaction, wire/hash and recovery boundaries described in
[architecture](architecture.md) and [supported formats](formats.md). Before a
schema change, test empty initialization, a current versioned reopen, unsupported
input rejection and rollback. Namespace changes also need a populated schema `1`
upgrade fixture that checks canonical object names, stored bytes, foreign keys,
triggers and query behavior. Before a protocol move, retain exact wire fixtures.
Commit reviewable changes with their focused regressions, without generated
evidence or local plans.
