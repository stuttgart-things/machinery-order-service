# Architecture Decision Records

One decision per file, numbered. A changed decision is a new ADR; the old one gets `Status: superseded by NNNN`.

| ADR | Title | Status |
|---|---|---|
| [0001](0001-order-visibility.md) | Callers see their own and their teams' orders, admins all | accepted |
| [0002](0002-auto-merge.md) | Auto-merge only when the order asks for it and a rule allows it | accepted |
| [0003](0003-teardown.md) | Teardown is a transition of the order, delivered as a delete-claim pull request | accepted |
| [0004](0004-recover-adopt.md) | Recover lost Orders from git and adopt hand-written XRs, on request | accepted |
