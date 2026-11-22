# 0003: Teardown is a transition of the order, delivered as a delete-claim pull request

- Status: accepted
- Date: 2026-11-22

## Context

An order builds something through a pull request. Removing it was manual (#19): someone opened a PR that deletes the file (`claims delete --create-pr`), merged it, and the fleet repo deleted the XR. That needs repo write access. Personas and other ordinary orderers have neither repo nor cluster access, on purpose. The service is their only write path, so scenarios with `cleanup: delete-claim` could not clean up after themselves (ai-personas#27).

The fleet repo (stuttgart-things/stuttgart-things) already has the cluster side of a deletion:

- `pr-claim-delete.yaml` gates every PR from a `delete-claim/*` branch. `hack/claim-deletion.py verify-pr` requires that the PR deletes exactly one claim file, plus the runs and the host ScrapeConfig that go with it and its kustomization entry, and nothing else.
- `delete-claim-xr.yaml` runs **only** for a merged PR from `delete-claim/*`. It reads the removed claim from git, waits until Flux applied the merge, runs `kubectl delete` on the XR on the plane's runner, and waits until it is gone. It reports as the check run `Delete-XR` on the PR's head commit. `workflow_dispatch` with `pr=<n>` retries it.
- The workflows read neither the label (`delete-claim`), the title nor the body. Only the branch prefix and the diff count. `ClusterStack` is refused, as is any directory not in `hack/planes.py`.

So a teardown needs neither Flux prune nor an agent on the cluster. The service opens the same kind of PR. The service still never writes to a cluster, and deleting the Order CR still tears nothing down.

## Decision

### A teardown is a transition of the original order

`POST /api/v1/orders/{id}/teardown` records the request in `spec.teardown` of the order itself, not in an Order of its own:

- The order already knows what to remove: its files, its XR, its cluster, its visibility (ADR 0001). A second Order would copy all of that and need a link back.
- One ID covers the VM's whole life: `claims orders watch <id>` follows it from `Ordered` to `Deleted`, and the history shows both PRs.
- The cost is a second delivery on the order: `status.teardown` holds the teardown PR the same way `status.delivery` holds the order's.

`spec.teardown` holds `attempt`, `requestedBy`, `requestedAt`, `reason` and, optionally, `autoMerge.rule`. `status.teardown.attempt` says which request the status belongs to.

### Who may tear down

The same callers who may see and cancel the order: the orderer, its owner (the user, or a member of the owner group), and admins. A persona may tear down what it ordered. A team member may tear down the team's orders, but a human merges that PR.

Without auth (local, tests) anyone may, like everything else.

### Auto-merge of teardown PRs

Every merged `delete-claim/*` PR deletes its object: **the merge is the review**. An order rule (ADR 0002) therefore does not cover teardowns. A rule has to opt in:

```yaml
rules:
  - name: persona-test-vms
    groups: [group:default/ai-personas]
    templates: [native-vm-labul-proxmox]
    parameters: {size: [small]}
    actions: [order, teardown]   # default: [order]
```

A teardown is auto-merged when all of the following hold:

1. the request asks for it (`"autoMerge": true`, `claims orders teardown --auto-merge`);
2. a rule lists `teardown` under `actions`, its users/groups match **the caller of the teardown**, and it covers the order's template and rendered parameters;
3. the caller is **the orderer** (`spec.requestedBy`). Teammates and admins get a human merge. With auto-merge, a persona can only remove what it ordered itself.

If any of these fails, the request is refused with 403, as at intake. Nothing falls back to a human merge silently. The rule is recorded in `spec.teardown.autoMerge.rule`. In the controller the same ADR 0002 rules apply: the rule must still exist and still allow `teardown`. The service waits for the checks, which include `Verify-Claim-Deletion`, and merges pinned to the head SHA.

### Pull request

- Branch `delete-claim/<XR name>`, title `Delete claim: <Kind>/<name>`, label `delete-claim`, as `claims delete` opens it.
- The commit removes every file of the order that is still on the base branch, and its entry in a `kustomization.yaml` where the directory has one. It carries the `Order-Id:` trailer, so `Open` stays idempotent. A teardown whose files are all gone already fails (nothing to remove).
- The body names the order, the orderer, the caller, the reason and the order's PR. It says that merging deletes the object.

### Phases

```
Merged|Applied|Building|Ready|Deleted ──teardown──▶ TeardownPROpen ──merge──▶ TearingDown ──XR gone──▶ Deleted
                       ▲                                 │
                       └────── PR closed unmerged ───────┘  (withdrawn: back to the phase before)
```

- **TeardownPROpen**: the teardown PR is open. If it is closed without a merge, the teardown is withdrawn and the order goes back to the phase it came from. It can be requested again.
- **TearingDown**: merged. The service asks machinery until the XR is gone, then moves to **Deleted**. A teardown of an order whose XR never appeared is Deleted at once.
- **delete-claim-xr fails** (check run `Delete-XR` failed): the order stays `TearingDown`. Condition `Teardown` (reason `DeleteFailed`) and the message point to the retry: `gh workflow run delete-claim-xr.yaml -f pr=<n>`. That needs someone with repo access. The service does not dispatch it itself, because that would need `actions: write`. Once the retry deletes the XR, the order moves to Deleted.
- A teardown that cannot be delivered (a foreign open PR on the branch, no files left) does not fail the order. Condition `Teardown` (reason `Failed`) says why, and the order stays where it was.
- **Deleted after a teardown is final.** The controller stops asking machinery, so a new order with the same name is not mistaken for this one. Deleted without a teardown (the XR got lost) stays as before: should the XR come back, the order follows it again.
- Teardown is refused before the merge (cancel instead), on Cancelled and Failed orders, while a teardown is in flight, and after a completed one.

### Branches (#20)

The service deletes its own branch once its PR is merged (by the service or by a person) or closed, for orders and teardowns alike. `Open` heals a leftover branch that is not the order's, has no open PR, and whose head is contained in the base branch: the branch is deleted and created again for the new order. It is not reused, because the commit is not the order's. A branch with an open PR or with commits that are not on the base branch is still a conflict.

### API and UI

- `POST /api/v1/orders/{id}/teardown` with `{"reason": "...", "autoMerge": false, "resourceVersion": "..."}` answers 202 with the order. 404 if the caller may not see the order. 409 if the phase does not allow it, or the order changed since `resourceVersion`. 403 if auto-merge is not allowed. 400 if the reason is too long.
- The UI's order page has a **Tear down** button for callers who may, in the phases that allow it, with a confirmation. The UI never asks for auto-merge.

## Consequences

- `cleanup: delete-claim` scenarios run without a git token: `claims orders teardown <id> --auto-merge` and `claims orders watch <id>` until `Deleted`.
- `persona-test-vms` on machinery-labul needs `actions: [order, teardown]` in the fleet repo before personas can tear down unattended.
- Not covered:
  - one-shot runs that a `claims change` added after the order (they are not order files). `verify-pr` then fails, auto-merge stops, and a human finishes the PR.
  - withdrawing a teardown through the API (close the PR on GitHub).
  - the service starting the `delete-claim-xr` retry itself.
- Open: `pr-claim-delete.yaml` says the Dapr worker merges green `delete-claim/*` PRs it dispatched. If it merged any green `delete-claim/*` PR, the service's teardown PRs would be merged without a human. Check before the first unattended use.
