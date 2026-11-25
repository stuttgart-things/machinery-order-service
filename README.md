# machinery-order-service

Takes orders for machinery resources (VMs first), renders them through
[claim-machinery-api](https://github.com/stuttgart-things/claim-machinery-api)
and delivers them to the GitOps repository as pull requests. It follows each
order through git, Flux and machinery until the resource is Ready.

Design: stuttgart-things/stuttgart-things#4296. This repository is the
skeleton, stuttgart-things/stuttgart-things#4297.

```
claims CLI / Backstage ──REST + ZITADEL token──▶ machinery-order-service (infra cluster)
   1. Order CR (Ordered), requestedBy from the token
   2. placement: pinned to a ManagementCluster (Placed); waits while the
      cluster is not ready (health through machinery, below)
   3. render through claim-machinery-api with the orderer's token, plus
      order_id, management_cluster, source_path, environment_config
   4. git: XR file + kustomization entry (+ extra files) on a branch,
      commit trailer "Order-Id: <id>", pull request (PROpen)
                         │
            git (stuttgart-things) ── Flux ──▶ machinery / machinery-labul
                         │                        Crossplane builds the XR
   5. status: PR merged (Merged) → Flux lastAppliedRevision at or past the
      merge (Applied) → machinery sees the XR (Building) → Ready
```

**The service never applies anything to a management cluster.** Its only
write path is git. Deleting an Order never tears anything down: before the
merge its pull request is closed, after the merge only the record goes.
Removing what an order built is a **teardown** ([ADR 0003](docs/adr/0003-teardown.md)):
the service opens a `delete-claim/<xr>` pull request that removes the order's
file, and after its merge the fleet repo's `delete-claim-xr.yaml` deletes the
XR. The service deletes its own branches once their pull request is merged or
closed, and replaces a leftover branch of the same name whose commits are all
on the base branch.

## Phases

| Phase | Meaning |
|---|---|
| `Ordered` | accepted |
| `Placed` | pinned to `spec.placement.managementCluster` |
| `PROpen` | files on branch `vm-<name>`, pull request open |
| `Merged` | merged; `status.delivery.mergeSHA` |
| `Applied` | the cluster's Flux Kustomization is at or past the merge |
| `Building` | machinery sees the XR, not Ready yet |
| `Ready` | machinery reports the XR Ready (`status.live`: address, info) |
| `TeardownPROpen` | teardown requested, `delete-claim/<xr>` pull request open (`status.teardown`); closed unmerged = withdrawn, back to the phase before |
| `TearingDown` | teardown merged; waiting until `delete-claim-xr.yaml` deleted the XR. A failed `Delete-XR` check shows in condition `Teardown` with the retry command |
| `Deleted` | machinery no longer sees the XR. After a teardown this is final; otherwise (the XR was Ready and got lost) the order follows it again should it come back. `status.live` keeps the last address and info, `lastSeenAt` says when |
| `Cancelled` | cancelled before the merge, or the PR was closed unmerged |
| `Failed` | cannot go on as is: invalid parameters, a taken name or branch, cordoned cluster. A cluster that is **not ready** does not fail an order: it waits in `Ordered`/`Placed` with the reason in `status.message` |

## API

| Method | Path | |
|---|---|---|
| `POST` | `/api/v1/orders` | `{"template", "managementCluster", "parameters", "reason", "autoMerge"}` → 201 Order, already `Ordered` with a message on what follows, `Location` header. 400 with `fields` when claim-machinery-api rejects parameters, 400 with the valid names for an unknown cluster, 409 for a cordoned or not ready cluster, 400 (field `managementCluster`) when the cluster does not offer the rendered XR's kind/version, 403 when `autoMerge` matches no rule of the cluster |
| `GET` | `/api/v1/orders` | `?mine=true`, `?phase=`, `?cluster=`; without the rendered files. Only the orders the caller may see (below) |
| `GET` | `/api/v1/orders/watch` | Server-Sent Events, `event: order`, same filters and visibility |
| `GET` | `/api/v1/orders/{id}` | one order with its files; 404 when the caller may not see it |
| `POST` | `/api/v1/orders/{id}/cancel` | `{"resourceVersion"}` optional: 409 when the order moved. Only before the merge, by whoever may see it |
| `POST` | `/api/v1/orders/{id}/teardown` | `{"reason", "autoMerge", "resourceVersion"}` → 202. Only merged orders, by whoever may see them; 409 before the merge, while a teardown runs or after one completed; 403 when `autoMerge` is asked by someone other than the orderer or matches no rule with `actions: [teardown]` |
| `GET` | `/api/v1/management-clusters` | `{"items": [{"name", "lab", "cordoned", "ready", "readyMessage", "conditions", "checkedAt", "capabilities", "repository", "ordersPath", "autoMerge"}]}`: where orders can go, whether the cluster is healthy, what it can build, and who may auto-merge what |
| `GET` | `/ui/` | **My orders**, live through SSE |
| `GET` | `/ui/?scope=all` | **All orders** (admins) or **All I may see** (own + the teams', ADR 0001), with orderer and group; `group=<group>` (or `-` for none) keeps one group, `sort=group` groups the rows. Group = the order's `owner`, else the orderer's groups at order time |
| `GET` | `/ui/orders/{id}` | **Order detail**: timeline, files, PR/commit, order ID, Cancel, Tear down (a human merges) |
| `GET` | `/ui/manifest.json` | sthings-portal manifest (no login): title `Orders`, one fragment |
| `GET` | `/ui/fragment/orders` | Portal overview fragment: the caller's orders open/ready/failed and the newest five, linked to their detail |
| `GET` | `/health`, `/ready`, `/version` | |

**Visibility** ([ADR 0001](docs/adr/0001-order-visibility.md)): with auth on, a
caller sees the orders it placed and those whose `owner` parameter is itself or
one of its groups (from the ZITADEL project roles, as for Backstage); admins
(`AUTH_ADMIN_ROLES`) see all. Without auth everything is visible.

Everything answers at the root and under `BASE_PATH` (behind sthings-portal).
The UI has no ordering form on purpose: orders come in through the API.

On sthings-portal's overview the Orders card shows the fragment (portal
README, "Fragments"): the portal reads the manifest with its health check and
loads the fragment through its proxy every 10 s, so it is the logged-in
user's orders. Unlike the pages, the fragment carries `BASE_PATH` in every URL,
since it runs on the portal's overview without `<base href>`.

```bash
curl -X POST "$URL/api/v1/orders" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{
  "template": "native-vm-labda-vsphere",
  "managementCluster": "machinery",
  "parameters": {"name": "demo", "use_case": "baseos", "size": "small", "owner": "group:default/platform-team"}
}'
```

## gRPC API

`order.v1.OrderService` ([`api/proto/order/v1/order.proto`](api/proto/order/v1/order.proto))
on its own port, `GRPC_PORT` (default `9090`), on every replica. It calls the
same service layer as REST: validation and rendering with the caller's token,
visibility (ADR 0001), auto-merge rules (ADR 0002), teardown (ADR 0003).

| RPC | REST |
|---|---|
| `CreateOrder` | `POST /api/v1/orders` |
| `GetOrder` | `GET /api/v1/orders/{id}` (with the rendered files) |
| `ListOrders` | `GET /api/v1/orders` (`filter.mine`, `.phase`, `.cluster`) |
| `WatchOrders` (server stream) | `GET /api/v1/orders/watch`; `send_initial` sends the current orders and `SYNCED` first, `resource_version` resumes (orders changed after it); `order_id` + `until_phases` follows one order and ends there (Cancelled/Failed/deleted always end it) |
| `CancelOrder`, `TeardownOrder` | `POST /api/v1/orders/{id}/cancel`, `/teardown` |
| `ListManagementClusters` | `GET /api/v1/management-clusters` |

Auth as REST: the ZITADEL token in the `authorization` metadata
(`Bearer <token>`), `requested_by` from the token. No token or a bad one:
`Unauthenticated`; no order role: `PermissionDenied`; an order the caller may
not see: `NotFound`. Errors: 400 → `InvalidArgument` (claim-machinery-api's
fields as `google.rpc.BadRequest` field violations), 403 → `PermissionDenied`,
404 → `NotFound`, 409 → `FailedPrecondition`, or `Aborted` when the order
moved since `resource_version`. Health (`grpc.health.v1`) and reflection need
no token.

A separate port instead of h2c on the HTTP port: no HTTP middleware or SSE
buffering in the way, and the gateway gets a plain gRPC backend
(`GRPCRoute`, `grpcRouteEnabled`/`grpcRouteHostname` in the KCL module).

```bash
grpcurl -H "authorization: Bearer $TOKEN" -d '{
  "template": "native-vm-labda-vsphere", "management_cluster": "machinery",
  "parameters": {"name": "demo", "size": "small"}
}' $GRPC_HOST:443 order.v1.OrderService/CreateOrder

grpcurl -H "authorization: Bearer $TOKEN" -d '{"order_id": "ord-…", "until_phases": ["PROpen"]}' \
  $GRPC_HOST:443 order.v1.OrderService/WatchOrders
```

## Custom resources

Group `order.machinery.stuttgart-things.com/v1alpha1`, CRDs in
[`config/crd`](config/crd), examples in [`config/samples`](config/samples).

- **Order** (namespaced, name = order ID `ord-<12 hex>`): `template`,
  `parameters`, `placement.managementCluster`, `requestedBy`,
  `requestedByGroups` (the orderer's groups at intake, for grouping in the UI), `reason`,
  `cancel`, `rendered` (set by the API at intake), `autoMerge.{rule, expiresAt}` (set at
  intake when the order asked for auto-merge and a rule allowed it;
  `expiresAt` when the rule has a `maxLifetime`) and
  `teardown.{attempt, requestedBy, requestedAt, reason, autoMerge.rule}` (set
  by the teardown endpoint).
  Template, placement, rendered and autoMerge are immutable.
- **ManagementCluster** (cluster-scoped): `gitops.{repository, baseBranch,
  ordersPath}`, `render.{clusterName, environmentConfig, claimApiUrl}`
  (`claimApiUrl`: this cluster's own claim-machinery-api for rendering and
  templates; empty = `CLAIM_API_URL`, e.g. for a staging cluster),
  `machinery.{endpoint, insecure, tokenSecretRef}`,
  `flux.{name, namespace, kind, revisionField}`,
  `autoMerge.{method, rules[].{name, users, groups, templates, parameters, parameterPrefixes, actions, maxLifetime}}`
  (`actions`: `order`, `teardown`; empty = `[order]`; `maxLifetime` needs `teardown`),
  `cordoned`. Status: the health conditions below, `checkedAt`,
  `observedGeneration`, `capabilities` (below).

### Cluster health

Every minute the service checks each ManagementCluster through its
machinery (read-only, no kubeconfig) and writes conditions to its status:

| Condition | False when |
|---|---|
| `MachineryReachable` | machinery does not answer (or its token Secret is missing) |
| `FluxReady` | the Kustomization in `spec.flux` is missing or `Ready=False` (`Ready=Unknown` while it reconciles is not a failure) |
| `CrossplaneHealthy` | a `Configuration`, `Provider` or `Function` has `Healthy=False` (the Lock collisions in crossplane-configurations take them all down at once) |
| `Ready` | any of the above is False |

`Ready=False` refuses new orders (409) like `cordoned`, and orders that have
no pull request yet wait. Orders past their PR go on. `Unknown` never
blocks: no `spec.machinery`, `spec.flux` unset, the package kinds not in
machinery's watch set, or a check older than 5 minutes (`checkedAt`; the
service is not checking). The service never cordons a cluster itself.

`kubectl get mc` shows `Ready`; `kubectl get mc <name> -o yaml` the reasons.

### Capabilities

With the same check the service reads what the cluster can build
(`status.capabilities`, #32): `kinds[]` from its XRDs (`kind`, `group`,
`versions`, and the `configuration`/`configurationVersion` that installed it,
through the XRD's owning ConfigurationRevision) and the names of its
`environmentConfigs`. An order whose rendered XR (apiVersion + kind) the
cluster does not offer is refused at intake with 400, and an order created
with kubectl ends `Failed` before its pull request. No data never refuses:
machinery does not watch `CompositeResourceDefinition`, reports none, or
`observedAt` is older than 15 minutes.

An XR selects its EnvironmentConfig by a **label value**, not by its name:
`spec.environmentConfig` against the label `<configuration>.<group>/environment`
(e.g. `proxmox-vm.resources.stuttgart-things.com/environment: labul`). A
missing one does not fail, the XR builds with the defaults. With machinery
rendering labels (machinery#117, info field `Labels` on `EnvironmentConfig`)
the service keeps the values of every `…/environment` label in
`capabilities.environments` and refuses an XR whose `spec.environmentConfig`
none of them has (400). The label key comes from the XR's Configuration;
the package manager's long name (`stuttgart-things-crossplane-configurations-proxmox-vm`)
matches its short prefix (`proxmox-vm`). No check when the XR sets no
`environmentConfig`, when no EnvironmentConfig carries that key at all, or
without labels from machinery.
`CrossplaneHealthy` needs machinery to watch the three package kinds
(`pkg.crossplane.io`, the shared watch config in stuttgart-things/flux
`cicd/machinery`); without them it stays Unknown.

The live side comes from machinery's gRPC `GetResourceDetail` (kind + name of
the rendered XR). The Applied check reads the Flux Kustomization through
machinery too, so machinery has to watch it with an info field labelled
`Revision` for `status.lastAppliedRevision` (the shared watch config in
stuttgart-things/flux `cicd/machinery` does; another label goes in
`spec.flux.revisionField`). Without `spec.flux` an
order goes from Merged to Building once machinery sees its XR.

## Configuration

| Variable | Purpose | Default |
|---|---|---|
| `CLAIM_API_URL` | claim-machinery-api base URL | required |
| `CLAIM_API_TOKEN` or `CLAIM_API_OIDC_CLIENT_ID`/`_CLIENT_SECRET`/`_TOKEN_URL`/`_SCOPE` | the service's own identity, used for Orders created with kubectl | - |
| `GITHUB_APP_ID`/`_INSTALLATION_ID`/`_PRIVATE_KEY_FILE` | bot identity for branches and PRs (preferred) | - |
| `GITHUB_TOKEN` | alternative to the App | - |
| `GITHUB_API_URL` | GitHub API | `https://api.github.com` |
| `AUTH_OIDC_ISSUER`, `AUTH_OIDC_AUDIENCE`, `AUTH_OIDC_CA_FILE`, `AUTH_ORDER_ROLES`, `AUTH_ROLES_CLAIM`, `AUTH_ADMIN_ROLES`, `AUTH_ROLE_GROUPS`, `AUTH_TRUST_FORWARDED_USER` | ZITADEL auth, as in claim-machinery-api | off |
| `ORDER_NAMESPACE` | where Orders live | the pod's namespace |
| `PORT`, `BASE_PATH` | API/UI | `8080`, - |
| `GRPC_PORT` | gRPC API | `9090` |
| `METRICS_ADDR`, `PROBE_ADDR` | controller-runtime | `:8081`, `:8082` |
| `LEADER_ELECT` | leader election for the controller (the API runs on every replica) | off |
| `PR_POLL_INTERVAL`, `LIVE_POLL_INTERVAL` | how often PR/Flux and machinery are checked | `1m`, `30s` |
| `LOG_FORMAT`, `DEBUG` | `json`; debug logging | text, off |

The GitHub App needs `contents: write` and `pull_requests: write` on the
GitOps repository; for auto-merge also `checks: read` and `statuses: read`.

## Auto-merge

An order normally waits for a human to merge its pull request. It is merged
by the service only when the order asks for it (`"autoMerge": true`,
`claims order --auto-merge`) and a rule of its ManagementCluster allows it:

```yaml
spec:
  autoMerge:
    method: merge                 # merge (default), squash, rebase
    rules:
      - name: persona-test-vms
        groups: [group:default/ai-personas]   # or users: [user:default/<name>]
        templates: [native-vm-labul-proxmox]  # empty = any
        parameters: {size: [small]}           # rendered values, defaults included
        parameterPrefixes: {name: [persona-]} # optional: rendered value starts with one
```

No matching rule: the order is refused with 403. A matching order merges once
the checks on the PR's head commit passed (`success`, `neutral`, `skipped`;
no checks at all after a minute counts as passed). A failed check, a rule
that was removed or narrowed, or a PR GitHub will not merge leaves it to a
human; condition `AutoMerge` says why. While the PR is open, `status.message`
shows the same state (`pull request <url>; auto-merge: waiting for checks: …`),
so clients that only read the message (claims) see it too.

A rule with `maxLifetime: 1h` (and `actions: [order, teardown]`) limits how
long its orders live: the intake sets `spec.autoMerge.expiresAt`, and when it
is reached the service cancels the order (before the merge) or tears it down
itself (after it, reason `expired: …`, auto-merged under the rule). So test VMs
whose runner died do not stay on shared hardware. Decision and limits:
[ADR 0002](docs/adr/0002-auto-merge.md).

## Deployment

KCL module in [`deployment`](deployment): namespace, the two CRDs, service
account, ClusterRole/-Binding, ConfigMap, Deployment, Service, optional
Ingress or HTTPRoute, optional GRPCRoute for the gRPC API, and
ManagementCluster objects. CRDs and RBAC come from
`deployment/generated` (copied from `config/` by `task generate`; CI fails
when they drift).

```bash
task render     # Dagger KCL module with tests/kcl-deploy-profile.yaml
```

Secrets stay outside the module: `githubAppSecretName` (key
`private-key.pem`, mounted as a file) and `existingSecretName` (environment,
e.g. `CLAIM_API_OIDC_CLIENT_SECRET`), each made by External Secrets or by
hand. The machinery tokens are the Secrets the ManagementClusters name.

A release (semantic-release on `main`) publishes the binaries
(GoReleaser), `ghcr.io/stuttgart-things/machinery-order-service:<version>`
(ko, distroless static) and the kustomize base
`ghcr.io/stuttgart-things/machinery-order-service-kustomize:<version>` pinned
to that image.

## Development

```bash
task generate          # deepcopy, CRDs, RBAC, deployment/generated, gRPC code (needs protoc)
task test              # unit tests + envtest (controller and serve end to end)
task build-local       # ./bin/machinery-order-service
task run               # against the current kubeconfig
task lint              # golangci-lint (Dagger)
task build-test        # build + all tests incl. envtest (Dagger)
task build-scan-image  # ko image to ttl.sh + Trivy (Dagger)
```

Tests use fakes for everything outside: `internal/delivery/githubfake`
(GitHub), `internal/render/rendertest` (claim-machinery-api),
`internal/machinery/machinerytest` and an in-process gRPC server (machinery).

## Not here yet

- teardown after the merge, recovering Orders from git + live (`Recovered`),
  `adopt` for hand-written XRs; finding XRs by order-id in machinery needs
  stuttgart-things/stuttgart-things#4557
- the scheduler (InfraTarget, PlacementPolicy, capacity): #4298, #4299
- UI stages 2–4 (#4300); a direct ZITADEL browser login (today: the portal's
  forwarded user or a bearer token)
