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
   2. placement: pinned to a ManagementCluster (Placed)
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
| `Cancelled` | cancelled before the merge, or the PR was closed unmerged |
| `Failed` | cannot go on as is: invalid parameters, a taken name or branch, cordoned cluster |

## API

| Method | Path | |
|---|---|---|
| `POST` | `/api/v1/orders` | `{"template", "managementCluster", "parameters", "reason"}` → 201 Order. 400 with `fields` when claim-machinery-api rejects parameters |
| `GET` | `/api/v1/orders` | `?mine=true`, `?phase=`, `?cluster=`; without the rendered files |
| `GET` | `/api/v1/orders/watch` | Server-Sent Events, `event: order`, same filters |
| `GET` | `/api/v1/orders/{id}` | one order with its files |
| `POST` | `/api/v1/orders/{id}/cancel` | `{"resourceVersion"}` optional: 409 when the order moved. Only before the merge |
| `GET` | `/ui/` | **My orders**, live through SSE |
| `GET` | `/ui/orders/{id}` | **Order detail**: timeline, files, PR/commit, order ID, Cancel |
| `GET` | `/health`, `/ready`, `/version` | |

Everything answers at the root and under `BASE_PATH` (behind sthings-portal).
The UI has no ordering form on purpose: orders come in through the API.

```bash
curl -X POST "$URL/api/v1/orders" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{
  "template": "native-vm-labda-vsphere",
  "managementCluster": "machinery",
  "parameters": {"name": "demo", "use_case": "baseos", "size": "small", "owner": "group:default/platform-team"}
}'
```

## Custom resources

Group `order.machinery.stuttgart-things.com/v1alpha1`, CRDs in
[`config/crd`](config/crd), examples in [`config/samples`](config/samples).

- **Order** (namespaced, name = order ID `ord-<12 hex>`): `template`,
  `parameters`, `placement.managementCluster`, `requestedBy`, `reason`,
  `cancel`, and `rendered` (set by the API at intake). Template, placement and
  rendered are immutable.
- **ManagementCluster** (cluster-scoped): `gitops.{repository, baseBranch,
  ordersPath}`, `render.{clusterName, environmentConfig}`,
  `machinery.{endpoint, insecure, tokenSecretRef}`,
  `flux.{name, namespace, kind, revisionField}`, `cordoned`.

The live side comes from machinery's gRPC `GetResourceDetail` (kind + name of
the rendered XR). The Applied check reads the Flux Kustomization through
machinery too, so machinery has to watch it with an info field
`lastAppliedRevision` (`status.lastAppliedRevision`). Without `spec.flux` an
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
| `METRICS_ADDR`, `PROBE_ADDR` | controller-runtime | `:8081`, `:8082` |
| `LEADER_ELECT` | leader election for the controller (the API runs on every replica) | off |
| `PR_POLL_INTERVAL`, `LIVE_POLL_INTERVAL` | how often PR/Flux and machinery are checked | `1m`, `30s` |
| `LOG_FORMAT`, `DEBUG` | `json`; debug logging | text, off |

The GitHub App needs `contents: write` and `pull_requests: write` on the
GitOps repository.

## Deployment

KCL module in [`deployment`](deployment): namespace, the two CRDs, service
account, ClusterRole/-Binding, ConfigMap, Deployment, Service, optional
Ingress or HTTPRoute, and ManagementCluster objects. CRDs and RBAC come from
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
task generate          # deepcopy, CRDs, RBAC, deployment/generated
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

- a gRPC API next to REST
- teardown after the merge, recovering Orders from git + live (`Recovered`),
  `adopt` for hand-written XRs; finding XRs by order-id in machinery needs
  stuttgart-things/stuttgart-things#4557
- the scheduler (InfraTarget, PlacementPolicy, capacity): #4298, #4299
- UI stages 2–4 (#4300); a direct ZITADEL browser login (today: the portal's
  forwarded user or a bearer token)
