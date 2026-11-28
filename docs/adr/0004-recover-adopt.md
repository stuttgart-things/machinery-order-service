# 0004: Recover lost Orders from git, on request

- Status: accepted (Recovered); Adopt follows in its own change
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

Not recovered: orders whose pull request was never merged (close the PR and
order again) and orders already torn down (their files are gone).

## Consequences

- Recovery needs only GitHub reads (contents, commits, commit pulls) the
  service's App already has; machinery#116 (find XRs by label) is not
  needed, git is the index.
- `Order-Id:` trailers and the `order-id` annotation are now load-bearing:
  a hand edit that drops them makes an order unrecoverable.
- A recovered Order with `recovered:<Kind>` matches no auto-merge rule that
  names templates; a teardown of it is merged by a human.
