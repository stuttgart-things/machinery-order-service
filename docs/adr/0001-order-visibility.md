# 0001: Callers see their own and their teams' orders, admins all

- Status: accepted
- Date: 2026-11-18

## Context

`GET /api/v1/orders` returned every order to every logged-in user; `?mine=true` narrowed it to the caller's. A developer login therefore saw other users' orders: names, parameters, requester, PR links (#12, found by the AI persona pilot, which saw three foreign orders before placing one).

Orders already carry who placed them (`spec.requestedBy`) and for whom (`owner` parameter: a user or a Backstage group). Group membership comes from the ZITADEL project roles, mapped the same way Backstage maps them (`internal/auth`). Cancelling already followed that rule: the orderer, the owner group or an admin.

## Decision

- With auth on, a caller sees an order when it placed it, when the order's `owner` is the caller or one of its groups, or when it is an admin (`AUTH_ADMIN_ROLES`). Without auth, everything is visible (local and test setups).
- This applies to list, watch, get, cancel and the UI's detail page alike. An order the caller may not see answers 404, not 403, so its existence does not leak.
- `?mine=true` stays and narrows further to the caller's own orders; the UI's "My orders" uses it.
- The management clusters are not secret: `GET /api/v1/management-clusters` lists name, lab, cordoned and where the orders land in git to every logged-in user, without how the service reaches machinery.

## Consequences

- A team sees its team's orders, which matches Backstage ownership (the catalog shows an order under Owned for that user or group).
- An order without `owner` is visible only to its orderer and admins.
- Clients that relied on seeing all orders (none known) need an admin role.
