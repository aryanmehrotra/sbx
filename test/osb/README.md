# OpenSandbox conformance

sbx claims to be OpenSandbox-compatible. This directory is how that claim is checked: by
**OpenSandbox's own Go e2e suite** (`tests/go` in `github.com/alibaba/OpenSandbox`), at a
pinned commit, **run unmodified** against `sbx serve --osb-addr`.

```sh
scripts/osb-conformance.sh                          # the v0.9.0 tier, against a throwaway sbx
scripts/osb-conformance.sh --tier v0.10.0           # a later tier (tiers are cumulative)
scripts/osb-conformance.sh --files sandbox,command  # just these upstream files
scripts/osb-conformance.sh -run 'Renew|Endpoint'    # and only tests whose name matches
scripts/osb-conformance.sh --external http://host:8080 --key K   # any server - a real
                                                    # OpenSandbox, to compare against
scripts/osb-bench.sh [--compare http://host:8080]   # the same lifecycle, timed
```

## What is here

| | |
|---|---|
| `UPSTREAM` | repo, tag and **commit**. The script fetches the tag and refuses to run if it no longer resolves to that commit. |
| `expectations` | which upstream files each release must pass, and the only skips allowed - each by test name *and* skip message. |
| `cmd/osbharness` | the Go half of the scripts: lists upstream tests, reads `go test -json`, prints the table and decides the exit status. Its tests are the gate's own gate. |
| `bench` | the benchmark, through the upstream Go SDK (required from the module proxy at `v1.1.0`, the same commit). |

Nothing from upstream is vendored or copied. The suite is fetched once per pinned commit into
`${SBX_OSB_CACHE:-${XDG_CACHE_HOME:-~/.cache}/sbx/osb}/<commit>` (a sparse checkout of
`tests/go`, `sdks/sandbox/go` and `specs`, ~2.4 MB) and `go test` runs in its `tests/go`, where
upstream's own `replace ../../sdks/sandbox/go` resolves. A reused checkout is re-verified: same
commit, no modified files. This is its own Go module so the root `go.mod` keeps no dependencies.

## How a run is judged

Every top-level test in the selected files is listed first, `go test` is asked for exactly
those, and each one must report. The run is red if any test **fails**, **never reports**, or
**skips** without an allowance for that test and that message in `expectations`; if the
package fails outside a test (build error, panic, timeout); or if `go test` exits non-zero
while the report looked green. A skip is not a pass.

## Not touching your own sandboxes

`sbx serve` fronts and reaps **every** sandbox on its docker endpoint, so a second daemon next
to your live one would bind its ports and could put its sandboxes to sleep. The throwaway
daemon is isolated two ways, both enforced:

- **Its own `HOME`** (`$work/home`): the presence file, slot lock and history in `~/.sbx` are
  private, so the one-per-machine guard is not tripped and your daemon's presence record is
  never removed. `DOCKER_HOST`, `DOCKER_CONFIG`, `GOPATH`, `GOMODCACHE` and `GOCACHE` are
  resolved under your real `HOME` first and passed explicitly; `SBX_NO_UPDATE_CHECK=1`.
- **A docker endpoint with no sbx sandboxes on it.** Checked before the daemon starts; if the
  endpoint has any, the script refuses and lists them. CI runners are empty. On a laptop with
  a live daemon, give it a second engine: `colima start osb` then
  `--docker-host unix://$HOME/.colima/osb/docker.sock`.

Teardown (on exit, `INT` and `TERM`) deletes leftovers through the API - only ids starting
`osb-` - then stops the daemon, then `sbx rm`s any `osb-*` sandbox that was not on the
endpoint when the run began. `--external` starts and removes nothing.

## Environment the suite reads

Every `Getenv` in `tests/go` and the SDK at the pinned commit, and what the script sets:

| variable | read by | set to |
|---|---|---|
| `OPENSANDBOX_TEST_DOMAIN` | `base_e2e_test.go` connection config (default `localhost:8080`) | `127.0.0.1:<port>` |
| `OPENSANDBOX_TEST_PROTOCOL` | same (default `http`) | `http` |
| `OPENSANDBOX_TEST_API_KEY` | same (default `e2e-test`) | the random `--osb-key`; unset with `--no-key` |
| `OPENSANDBOX_TEST_USE_SERVER_PROXY` | same; `true` routes execd through the server, header `X-API-Key` | `false` unless already set (v0.11.0 adds server proxy) |
| `OPENSANDBOX_SANDBOX_DEFAULT_IMAGE` | image for every test (default `python:3.11-slim`; `e2e_test.go`: `opensandbox/code-interpreter:latest`) | passed through |
| `OPENSANDBOX_E2E_SANDBOX_CPU` / `_MEMORY` | resource limits in pool and credential-vault tests (default `1` / `2Gi`) | passed through |
| `OPENSANDBOX_URL` | `e2e_test.go` (default `http://localhost:8080`) | the server URL |
| `OPENSANDBOX_API_KEY` | `e2e_test.go`, two of its three tests - `TestE2E_FullLifecycle` sends **no key** | the key |
| `RUN_CODE_INTERPRETER_E2E` | `code_interpreter` and one `scenario_agent` test skip unless `true` | `true` unless already set |
| `OPENSANDBOX_TEST_HOST_VOLUME_DIR` / `OPENSANDBOX_TEST_PVC_NAME` | `volume` | passed through |
| `OPENSANDBOX_TEST_REDIS_URL` | `pool` (Redis-backed tests skip without it) | passed through |
| `OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_HOST` / `_TARGET_IP` / `_SANDBOX_IMAGE` / `_LABEL_KEY` / `_LABEL_VALUE` | `credential_vault` (skips without `_TARGET_IP`) | passed through |
| `LLM_ENDPOINT` / `LLM_MODEL` | `scenario_agent` (skips without an endpoint) | passed through |
| `OPEN_SANDBOX_DOMAIN` / `_PROTOCOL` / `_API_KEY` | SDK fallbacks when a config field is empty | same as the `OPENSANDBOX_TEST_*` values |
| `OPENSANDBOX_DISABLE_METRICS` | SDK; `1` stops lifecycle-metric events, which `lifecycle_metrics` asserts on | never set |

Upstream's CI runs the same suite with `OPENSANDBOX_INSECURE_SERVER=YES` for its server
(`scripts/go-e2e.sh`); that is a server setting, not read by the tests.

## Known before the first run

- `TestSandbox_PauseAndResume` and `TestManager_PauseAndResume` call `t.Skip` unconditionally
  at the pinned commit; `expectations` allows exactly that message.
- `TestE2E_FullLifecycle` (`e2e_test.go`, tier v0.12.0) creates its client with an empty key,
  so it needs a daemon started with `--no-key` (which passes sbx `--osb-insecure-no-key`).
