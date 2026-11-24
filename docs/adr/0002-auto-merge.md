# 0002: Auto-merge only when the order asks for it and a rule allows it

- Status: accepted
- Date: 2026-11-19

## Context

Every order ends in a pull request that a human merges ("MERGE = BAU"). For people ordering production-like VMs that is the point: the merge is the review.

The AI persona tests (stuttgart-things/ai-personas) order small throwaway VMs to check the whole path: order, PR, build, login. A human has to sit by and merge the persona's PR within the run. Otherwise the run times out, and the SSH certificate (30 minutes, ai-personas ADR 0003) expires. Teams will want the same later for small test VMs.

Merging must never become the default by accident. A persona or a person must not be able to switch it on for themselves either.

## Decision

Auto-merge needs two things. Without both, a human merges as before.

1. **The order asks for it.** `POST /api/v1/orders` takes `"autoMerge": true`; claims has `claims order --auto-merge`. Without it nothing changes, whoever orders.
2. **A rule of the management cluster allows it.** `ManagementCluster.spec.autoMerge.rules` lists who may auto-merge what:

   ```yaml
   spec:
     autoMerge:
       method: merge            # merge (default), squash or rebase
       rules:
         - name: persona-test-vms
           groups: [group:default/ai-personas]
           users: []                          # user:default/<name>
           templates: [native-vm-labul-proxmox]
           parameters:                        # rendered values, defaults included
             size: [small]
           parameterPrefixes:                 # optional, rendered values by prefix
             name: [persona-]
   ```

   A rule matches when:
   - the orderer is listed under `users`, or one of its groups is listed under `groups`. A rule without users and groups matches nobody.
   - the template is listed under `templates`, or `templates` is empty.
   - every listed parameter has one of the allowed values in the *rendered* order. Defaults count, so `size` left out means `medium` and does not match `[small]`.
   - every parameter under `parameterPrefixes` starts with one of its prefixes, e.g. `name: [persona-]`, so test VMs on shared hardware are recognisable by name (stuttgart-things/stuttgart-things#4828).

The rules live in git with the cluster's other settings. Changing them is a reviewed pull request; no API changes them.

**At intake:**
- An order that asks for auto-merge and matches no rule is refused with **403**, and the message names the rules that exist. It does not fall back to a human merge silently: whoever asked learns that they may not.
- A matching order records the rule in `spec.autoMerge.rule`, which is immutable. Its PR body says that it will be merged automatically, under which rule and for whom.

**In the controller, while the PR is open:**
- The rule must still exist, and the order's template and parameters must still match it. Identity is not checked again (groups come from the token at intake). A rule that was removed or narrowed stops the auto-merge, and a human decides.
- It waits for the PR's checks on the head commit: check runs and commit statuses. `success`, `neutral` and `skipped` count as passed.
  - Pending checks: it waits.
  - Any failed check: it does not merge, the order stays `PROpen` for a human, and the condition says which check failed.
  - No checks at all after one minute: it merges. A repository without checks would otherwise never merge.
- Merging uses the cluster's `method` and is pinned to the head SHA, so a commit pushed in between is not merged unseen.
- Condition `AutoMerge` (`Waiting`, `ChecksFailed`, `RuleGone`, `NotMergeable`, `Merged`) and the history show what happened. After the merge the order follows the usual path (Merged, Applied, …).
- A human can still merge or close the PR at any time. Cancelling works as before.

**Maximum lifetime (#27):** a rule may set `maxLifetime` (a duration such as `1h`), so test VMs whose runner died do not stay on shared hardware:

```yaml
       rules:
         - name: persona-test-vms
           groups: [group:default/ai-personas]
           actions: [order, teardown]   # maxLifetime needs teardown
           maxLifetime: 1h
```

- The CRD refuses `maxLifetime` on a rule without the `teardown` action: the expiry teardown is merged under the same rule, nobody watches it.
- At intake an order under such a rule gets `spec.autoMerge.expiresAt` = intake time + `maxLifetime`. It is fixed with the rule name (immutable); changing the rule later does not move it. `kubectl get orders -o wide` shows it (`Expires`), the UI shows it in the order detail, and `status.message` ends in `(expires …)` until then.
- The controller requeues the order to `expiresAt`. Then, whatever phase the order is in:
  - before the merge (`Ordered`, `Placed`, `PROpen`): it is cancelled, the pull request closed. The message starts with `expired:`.
  - after the merge (`Merged`, `Applied`, `Building`, `Ready`, `Deleted`): the service asks for a teardown itself (`spec.teardown.requestedBy: service:machinery-order-service`, reason `expired: …`), auto-merged under the order's rule while it still allows teardowns, else a human merges. An order still `Building` is torn down as well; waiting for `Ready` would leave a VM that never becomes Ready.
  - an order whose teardown was asked for already (by the runner, a person or a previous expiry) is left alone, also when that teardown was withdrawn: a human closed it on purpose.

`GET /api/v1/management-clusters` lists each cluster's rules. Personas and people can see what they may auto-merge without guessing.

## Consequences

- Persona scenarios run in one go when their persona's group has a rule, and the scenario asks for it (ai-personas: `merge: auto`). Scenarios with `merge: human` still test the human path.
- The service's GitHub identity needs to read check runs and statuses, and to merge (`contents: write`, `checks: read`, `statuses: read`).
- No rule is configured by default. Turning it on for a cluster is a change in the fleet repo (stuttgart-things).
- Out of scope: auto-merge of changes (only orders and teardowns exist here), and time windows or quotas per rule. A limit on open auto-merge orders per orderer can follow if needed.
