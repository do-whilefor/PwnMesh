# PwnMesh development

Use Linux. CI uses Go 1.26, Node 22 (22.12 or newer), Python 3.13 and ripgrep. Go declares a 1.23
minimum in `go.mod`; use the CI toolchain for matching checks. Go race detection
also needs a working C compiler. Offline tests use local processes and loopback
HTTP fixtures. They do not require Docker or model credentials after dependencies
have been downloaded.

## Build and check

Run from the repository checkout:

```sh
go mod download
go mod verify
npm --prefix web ci
bash scripts/build.sh
bin/pwnmesh --help
bash scripts/check.sh
```

`build.sh [output]` defaults to `bin/pwnmesh`, type checks and builds the Vue
workspace, then embeds `web/dist` in a static Linux executable using read-only Go
dependencies. `check.sh` builds the frontend and runs its Vitest regressions,
the Go race suite and vet, Python HTTP replay and command regressions.
Select `go`, `web`, or `python` to run one check group. CI uses these same entries
and separately builds/smoke-tests `serve`, `dispatch`, and `worker`.

`web/src` contains Vue single-file components and TypeScript modules, with global
styles in the `App.vue` style blocks. `web/browser` contains TypeScript acceptance
tests; `web/public` contains fixed-path assets. No standalone JavaScript or CSS
source is tracked in these directories. `web/dist`, `web/.browser` and
`web/node_modules` are ignored generated directories. Browsers still receive the
compiled JavaScript and CSS bundles. A clean checkout must build the frontend before
raw `go build` or `go test` commands can import the embedded UI. The Linux build,
Go check and integration scripts perform this step automatically. Install the
locked npm dependencies once before running them.

CI and Docker set `PWNMESH_WEB_PREBUILT=1` after building the same frontend source
to avoid rebuilding it for each Go command. This explicit mode requires a
nonempty `web/dist/index.html` and JavaScript bundle; it does not check source
freshness. Ordinary builds regenerate the output. The Docker build uses a Node
stage for the frontend; the runtime image contains no Node installation.

For UI development, start the Go service on port 8000 and run
`npm --prefix web run dev`. Vite proxies the existing API paths while preserving
the browser Host and Origin. Keep the server's same-origin and trusted-host
checks enabled. Vue Router uses hash navigation, including existing project and
model links; production static resources continue to use `/static/`.

The default Docker build keeps the Go release checks. For faster local image
iteration, `docker build --target development -t pwnmesh:dev .` compiles without
running that test stage; it does not replace the default build before release.
See [container builds](../container/README.md) for the pinned Worker dependencies.

For a focused change, run its package and test names first. For example:

```sh
go test ./internal/board -run 'TestStore' -count=1
go test ./internal/worker -run '^TestAcceptance' -count=1
go test ./internal/worker/dag -count=1
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
| `bash scripts/test-integration.sh live smoke` | Model settings/output plus a basic worker image and Docker daemon | One local calculation through scheduling, execution, evidence and completion |
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
Browser acceptance in `web/browser/` is independently opt-in and serves the
production `web/dist` bundle. After building, install Chromium with
`npm --prefix web exec -- playwright install chromium` and run all isolated
fixture suites with `npm --prefix web run test:browser`. This compiles the strict
TypeScript browser tests into ignored `web/.browser` output and runs them with
Node's test runner. To select a suite by its source filename:

```sh
npm --prefix web run test:browser -- models_browser_test
```

Without an explicit dedicated `PWNMESH_WEB_URL` and the relevant opt-in flags,
live service/model tests stay skipped. `PLAYWRIGHT_MODULE` can override the
installed Playwright module. Default `check.sh web` runs TypeScript checks for both
application and browser tests, a production build and colocated `web/src/**/*.test.ts` regressions;
it does not start a browser or contact a model.

Use `container/README.md` for image/runtime ownership. Delete only containers and
images created for an acceptance run once they are no longer needed; do not prune
unrelated Docker resources or remove persistent project evidence volumes.

## Repository hygiene

The tracked top-level source layout is `cmd/`, `internal/`, `web/`, `tests/`,
`container/`, `docs/`, and `scripts/`. Root build/configuration files remain at the
root. Only the five maintained guides are allowed into `docs/`; historical local
notes and acceptance output remain ignored. `.local/` groups historical plans,
local acceptance tools and retained evidence outside the source directories.
Keep related local tools together when archiving them; generated Python bytecode
can be deleted. `refer/`, local development plans,
credentials, build outputs and temporary test artifacts are not source inputs.

Use `PwnMesh` in product text, `pwnmesh` in executable/module/storage names and
`PWNMESH_*` for project settings. Historical names belong only in migration or
rejection boundaries, their fixtures, and artifact exclusions. Keep comments that
explain constraints or failure modes; omit narration of the code itself.

Keep regression tests beside the owning Go package or in the existing integration
package. Preserve transaction, wire/hash and recovery boundaries described in
[architecture](architecture.md) and [supported formats](formats.md). Before a
schema change, test empty initialization, each supported incremental upgrade,
a current versioned reopen, unsupported input rejection and rollback. Preserve
canonical object names, public State/frozen input bytes, foreign keys, triggers
and query behavior for current databases; retired schema
versions must be rejected without mutation. Before a protocol move, retain exact wire fixtures.
Commit reviewable changes with their focused regressions, without generated
evidence or local plans. The Docker context includes the five maintained guides
because the release checks validate them, while excluding local archives.

Inspect fixed prompt costs without model calls using
`go test ./internal/worker -run '^TestFixedPromptOverhead$' -count=1 -v`.
This reports UTF-8 bytes separately for system instructions, fixed role/scenario
guidance, tool declarations and task data; it does not estimate provider tokens.
