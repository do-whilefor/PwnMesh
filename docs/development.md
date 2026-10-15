# PwnMesh development

Use Linux. Match CI with Go 1.26, Node 22.12+, Python 3.13 and ripgrep;
Go race detection also needs a C compiler. Offline checks need no Docker or model
credentials once dependencies are installed.

## Build and check

Run from the repository root:

```sh
go mod download
go mod verify
npm --prefix web ci
bash scripts/build.sh
bin/pwnmesh --help
```

`build.sh [output]` defaults to `bin/pwnmesh`. It type checks and builds the Vue
frontend, then embeds `web/dist` in a static Linux binary. A clean checkout must
build the frontend before raw Go commands can import the embedded UI.

| Command | Scope |
| --- | --- |
| `bash scripts/check.sh web` | Type checks, production build and Vitest |
| `bash scripts/check.sh go` | Frontend build, Go race suite and vet |
| `bash scripts/check.sh python` | HTTP replay and command regressions |
| `bash scripts/check.sh` | All three groups |

Start with the affected package and test names, for example
`go test ./internal/board -run 'TestStore' -count=1`. Documentation changes do not
need real-model or full-image acceptance. Check scripts clear inherited model
credentials and acceptance controls; raw `go test` still honors explicit gates.

CI and Docker use `PWNMESH_WEB_PREBUILT=1` to reuse a frontend built from the same
source. This checks output presence, not freshness; ordinary builds regenerate it.
For faster local image builds, use
`docker build --target development -t pwnmesh:dev .`; the default release target
retains Go checks. See [container builds](../container/README.md).

## UI and integration work

With the Go service on port 8000, run `npm --prefix web run dev`. Vite proxies the
API; keep the server's trusted-host and same-origin checks enabled. Edit Vue and
TypeScript in `web/src`, fixed assets in `web/public`, and browser tests in
`web/browser`. Generated bundles and dependencies stay ignored.

| Integration command | Additional requirements |
| --- | --- |
| `bash scripts/test-integration.sh offline` | Local toolchain only |
| `bash scripts/test-integration.sh docker` | Linux Docker and an existing `PWNMESH_DOCKER_TEST_IMAGE` |
| `bash scripts/test-integration.sh live readiness` | Explicit model settings and `PWNMESH_LIVE_OUTPUT` |
| `bash scripts/test-integration.sh live <scenario>` | Model settings, evidence output, Linux Docker and a Worker image |

Live runs use `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`, and `ANTHROPIC_MODEL`
(or `ANTHROPIC_DEFAULT_FABLE_MODEL`). Scripts do not load `.env`; live calls may
incur charges. See [integration suites](../tests/integration/README.md) for
scenarios, opt-in controls and retained-artifact replay.

Browser acceptance is separate from ordinary checks. After building the frontend,
install Chromium with `npm --prefix web exec -- playwright install chromium`,
then run `npm --prefix web run test:browser`. Append `-- models_browser_test` to
select one suite. Live service/model cases require their explicit opt-ins.

## Repository hygiene

- Keep only the five maintained guides in `docs/`. Local plans, diagnostics and
  evidence belong in ignored `.local/`; credentials, generated output and `refer/`
  are not source inputs.
- Use `PwnMesh` for product text, `pwnmesh` for code/storage names and `PWNMESH_*`
  for settings. Keep comments that explain constraints rather than narrate code.
- Keep focused regressions beside their owning package, with required fixtures.
  Remove obsolete diagnostics and temporary test artifacts after validation.
- For schema changes, check initialization, current-version reopen, rejection
  without mutation and atomic rollback. Preserve wire fixtures and the boundaries
  in [architecture](architecture.md), [formats](formats.md) and [recovery](recovery.md).
- Commit changes with their focused checks. Remove only unneeded Docker resources
  created for the work; preserve project evidence volumes and unrelated resources.
