---
status: accepted
---

# Allow Unconfined seccomp per environment until a certified profile ships

ADR 0001 fixes the workload floor at the Localhost bubblewrap-admitting
seccomp profile and forbids silent degradation to RuntimeDefault or
Unconfined. The first real deployment target — the owner's Talos homelab
(`john-k8s-gitops`, flannel CNI) — cannot host node profiles without
machine-config surgery, which would have blocked any deployment
indefinitely.

## Decision

`spec.security.seccompProfile` exposes an explicit choice:
`Localhost` (default, unchanged baseline) or `Unconfined`. Unset keeps the
baseline; nothing degrades automatically. The john-k8s-gitops instance sets
`Unconfined` and carries a visible comment recording the deviation and its
expiry condition: swap back to Localhost when the certified profile lands
on the nodes.

## Consequences

- Deployment unblocked without touching Talos machine configs.
- The deviation is declared per-Instance in Git, reviewable like any other
  spec change, instead of being a hidden operator default.
- Model-controlled tools inside the runtime lose kernel syscall filtering
  while Unconfined is selected; the Tool Execution Environment boundary
  (bubblewrap itself) still applies, just without a deny-list behind it.
