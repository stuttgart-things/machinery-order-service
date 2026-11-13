# CLAUDE.md

Guidance for Claude Code in this repository.

## Commands

```bash
export GOTOOLCHAIN=go1.27.4
task generate                       # after changing api/v1alpha1 or kubebuilder markers
task test                           # needs envtest binaries (task envtest)
KUBEBUILDER_ASSETS=$(setup-envtest use 1.37.0 -p path) go test ./...
go test ./internal/controller -run TestOrderLifecycle -v
go build -o ./bin/machinery-order-service .
```

## Architecture

`main.go` → `cmd.Execute()` (cobra: `serve` default, `version`). `cmd/serve.go`
wires a controller-runtime manager (controller, leader-elected) and the HTTP
API (`apiRunnable`, every replica).

- `api/v1alpha1` - Order (namespaced, name = order ID) and ManagementCluster
  (cluster-scoped). Generated: `zz_generated.deepcopy.go`, `config/crd`,
  `config/rbac` (controller-gen v0.23.0; CI fails on a stale diff).
- `internal/intake` - order → parameters (adds `order_id`,
  `management_cluster`, `source_path`, `environment_config`; refuses them from
  the orderer), render, XR ref, PR convention (copied from claims
  `cmd/render_layout.go` `conventionFor`: branch `vm-<name>`, create-vm title).
- `internal/layout` - copy of claims `internal/layout` (source-path/target-path
  placement, kustomization append); keep both in sync.
- `internal/render` - claim-machinery-api client; forwards the caller's token,
  else the service's (`CLAIM_API_*`).
- `internal/delivery` - GitHub API only (git data API: tree, commit, ref; no
  clone). Idempotent `Open` recognises its own branch/PR by the `Order-Id:`
  commit trailer. `ConflictError` = taken file/branch (order Failed).
- `internal/machinery` - gRPC client for machinery `GetResourceDetail`.
- `internal/controller` - the phase machine. `step` loops `advance` until the
  phase stops moving. Errors that retrying cannot fix end in `Failed`
  (`fail`), others go back to controller-runtime's backoff.
- `internal/api` - REST + SSE (`Hub`, fed by the Order informer). `Create`,
  `List`, `Get`, `Cancel`, `Stream` are shared with the UI.
- `internal/auth` - copy of claim-machinery-api `internal/auth` (same env
  vars); keep in sync.
- `internal/web` - UI stage 1, clusterbook style (`pageHead`, `--hr-*`,
  relative URLs against `<base href>`), htmx vendored in `static/`.
- `internal/ghauth` - GitHub App installation token or static token.

## Rules

- The service never writes to a management cluster; git is its only write path.
- Deleting an Order never tears anything down.
- Intake renders with the orderer's token and stores it in `spec.rendered`
  (immutable); the controller only renders when that is empty.
- UI URLs must stay relative (web tests check for absolute paths).

## Tests

- `cmd/serve_test.go` runs `serve` end to end (envtest, fake GitHub, fake
  claim-machinery-api, in-process machinery gRPC).
- `internal/controller` uses envtest with `githubfake`, `rendertest`,
  `machinerytest`; tests skip without envtest binaries.
- `internal/api`, `internal/web` use the controller-runtime fake client and
  `internal/auth/authtest` (a TLS OIDC issuer).
