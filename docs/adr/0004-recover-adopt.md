# 0004: Recover lost Orders from git and adopt hand-written XRs, on request

- Status: accepted
- Date: 2026-11-28

## Context

An Order CR is the service's only memory of an order. It can go: a new
infra cluster, a restored backup without the CRs, a `kubectl delete`.
Deleting an Order never tears anything down (the finalizer only closes an
open pull request), so after that the VM still runs and its file is still on
the base branch, but nobody follows it: no live status, no teardown through
the service, the orderer cannot see it.

Git still holds what the Order knew (stuttgart-things/stuttgart-things#4823):

- the XR file in the cluster's `spec.gitops.ordersPath`, with the
  `stuttgart-things.com/order-id` label and annotation, `source-path`,
  `management-cluster`, `requested-by` and `owner`;
- the commit with the `Order-Id:` trailer that brought it in (a squash merge
  keeps the trailer), the files that commit added, and its pull request;
- the live XR on the management cluster, through machinery.

What git did not hold is the ClaimTemplate the order was rendered from:
`spec.template` is required and immutable.

## Decision

### Recovery runs on request, for admins

`POST /api/v1/recover` (`{"managementCluster": "...", "dryRun": true}`; no
cluster = all) rebuilds the missing Orders. It is not a background scan: an
Order someone deleted on purpose would come back on its own, and the scan
would create Orders in other people's names without anyone asking. Only
admins may call it (it creates Orders for every orderer); `dryRun` lists
what would be created and why files are skipped.

### What a recovered Order is

For each XR document in the orders directory that names its own file
(`source-path`) and carries an order-id without an Order of that name:

- name and label = the order-id; `spec.requestedBy` = the file's
  `requested-by`; placement = the cluster.
- `spec.rendered.files` = the files as they are on the base branch: the XR
  file first, then every file the trailer commit added that carries the same
  order-id (a target-path file outside the directory, e.g. a host
  ScrapeConfig). Without a trailer commit: the files in the directory with
  that order-id.
- `spec.recovered` = path, trailer commit, merged pull request, who, when.
- `spec.template` = the XR's `stuttgart-things.com/template` annotation,
  which intake writes on every XR from now on. Older files get
  `recovered:<Kind>`: an honest placeholder, not a guess.

Skipped (listed with the reason): an order-id that is no valid Order name,
the same order-id in two files, a file that names another management
cluster. Hand-written XRs (no order-id) are left alone: that is Adopt.

### Phase Recovered, then the normal follow

The controller sees `spec.recovered` on a fresh Order, fills the status a
merged order would have (XR ref, files, delivery with the trailer commit as
merge SHA and the pull request) and sets phase `Recovered`. From there the
live XR decides, as after `Applied`: `Building`, `Ready`; an XR machinery
does not see stays `Recovered` ("not yet"). The Flux check is skipped: the
file is on the base branch already. A recovered order can be torn down like
any merged one (`Recovered` is teardownable); the teardown removes all its
files.

### Open pull requests

The base branch is not all git holds. Recovery also reads the open pull
requests against it whose commits carry an `Order-Id:` trailer (the service
writes one into every commit, the order's and the teardown's):

- **an order not merged yet** (its XR file goes into the cluster's orders
  directory, the order-id is not on the base branch): the Order gets the
  files as on the branch head and starts in `PROpen`, following that pull
  request (merged → `Merged`, closed → `Cancelled`) -- no second PR.
- **an open teardown** (branch `delete-claim/...`) of an order whose file is
  on the base branch: the Order starts in `TeardownPROpen` with
  `spec.teardown` attempt 1 and that pull request in `status.teardown`;
  merged → `TearingDown`, closed unmerged → back to `Recovered`.

The finalizer closes an open pull request when its Order is deleted, so
this matters when CRs vanish without it running (a lost or restored
cluster). Not recovered: orders already torn down (their files are gone),
pull requests without a trailer (opened by hand or by the claims CLI).

### Adopt: a hand-written XR becomes an order

`POST /api/v1/adopt` (`{"managementCluster", "path", "template",
"parameters", "reason", "dryRun"}`), admins only, takes over the XR in
`path` on the base branch: the one document there that names its own file
(`source-path`).

**Adopt is off until a cluster opens it.** `spec.adoptKinds` of the
ManagementCluster lists the kinds it may adopt (written like
`allowedKinds`); empty = adopt is off there (403). `spec.adoptPaths` lists
the directories of its hand-written XRs; adopt takes files only from
there. Nothing about which XRs or kinds is in the code. A kind goes into
`adoptKinds` only once the fleet repo can tear it down: **ClusterStack stays
out until `hack/claim-deletion.py` can remove one** (today it refuses them,
`hack/teardown-clusterstack.sh` is the way); an adopted ClusterStack could
not be torn down through the service. Starting values: LabDA `machinery`
`[MinioBucket]`, LabUL `machinery-labul` `[NativeProxmoxVM, MinioBucket]`. The service opens a pull request (branch `adopt/<xr>`,
`chore(machinery): adopt <Kind> <name> as order <id>`) that replaces the file
with **only three additions**: the `order-id` label, the `order-id`
annotation and the `template` annotation (one-line edits, the rest of the
file byte for byte). The kustomization is not touched. After the merge the
order follows the XR (`Merged` → `Applied` → `Ready`) and can be torn down
through the service. `dryRun` answers with the order ID and the lines it
would add, and creates nothing.

- **template** is optional. Given, the service test-renders it (name = the
  XR's name plus `parameters`) and refuses it when the rendered XR is of
  another kind or API group. Not given: `adopted:<Kind>`, so the origin
  stays visible (`recovered:<Kind>` is recovery's).
- **Refused**: a cluster with empty `adoptKinds` or a kind not in it (403);
  a file outside `adoptPaths`; XR names in the ManagementCluster's
  `spec.adoptExclude` (403) -- the exclusions live with the cluster, not in
  the code; a kind outside `spec.allowedKinds`; an XR with an `order-id` already (409:
  recover it); a file in the orders directory; a file another live Order
  adopts already (409); a file with no or several self-naming documents.
- The pull request carries the SHA-256 of the file as it was adopted: if
  the file changed on the base branch since, the order ends `Failed`
  instead of overwriting the change.

Recovery reads the `adoptPaths` besides the orders directory -- there only
documents with an `order-id`, i.e. adopted ones -- so an adopted order whose
CR is lost comes back with its ID (an open adopt pull request too, in
`PROpen`).

## Consequences

- Recovery needs only GitHub reads (contents, commits, commit pulls) the
  service's App already has; machinery#116 (find XRs by label) is not
  needed, git is the index.
- `Order-Id:` trailers and the `order-id` annotation are now load-bearing:
  a hand edit that drops them makes an order unrecoverable.
- A recovered Order with `recovered:<Kind>` matches no auto-merge rule that
  names templates; a teardown of it is merged by a human.
