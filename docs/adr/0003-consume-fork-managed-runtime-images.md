---
status: accepted
---

# Consume fork-published managed runtime images until upstream absorbs the contract

Issue #4 resolved with a version gate: implementation consumes the first official
upstream TypeClaw release carrying the managed runtime contract and ships no
downstream fork. On 2026-08-25 the product owner directed immediate deployment
from the owner-controlled TypeClaw fork (`fml09/typeclaw`, branch
`feat/managed-runtime-contract`) instead of waiting on upstream review. This ADR
records that pivot so it does not silently override the earlier decision.

The fork branch implements exactly the four items this repository's capability
baseline classified as **Upstream** blockers ([research](../research/typeclaw-hermes-capability-parity.md),
"Upstream contribution implications"): a complete immutable managed runtime image,
an executable managed deployment profile with writable-secret and file-spool
restart contracts, a Kubernetes-compatible non-root security mode, and dedicated
liveness/readiness health semantics. Its release workflow parameterizes the
registry by repository owner, so fork releases publish
`ghcr.io/fml09/typeclaw-runtime:<version>`.

## Decision

- The operator consumes `ghcr.io/fml09/typeclaw-runtime:<version>` images
  published by `fml09/typeclaw` releases as its managed runtime input.
- The operator gates compatibility on those fork releases, not on
  `typeclaw/typeclaw` releases, until upstream absorbs the contract.
- When an official upstream release carries the managed runtime contract, the
  registry default moves back upstream and this ADR is superseded.

## Consequences

- The "no downstream fork" posture of issue #4 is suspended for the runtime
  artifact while remaining binding for operator code: this repository still
  contains zero TypeClaw source and treats `typeclaw/typeclaw` plus the fork's
  documented contract ([managed runtime](https://github.com/fml09/typeclaw/blob/feat/managed-runtime-contract/docs/content/docs/internals/managed-runtime.mdx))
  as the workload authority.
- Fork release cadence becomes the operator's upgrade clock; image references
  stay version-pinned and digest-pinning work under issue #12 applies to fork
  artifacts identically.
- Security decisions remain anchored to [ADR 0001](0001-restricted-workload-and-tool-execution-boundaries.md):
  the Restricted Workload floor stands even though the fork's reference Pod
  sketch uses `seccompProfile: Unconfined`. The operator renders the
  `Native + LocalBwrap` baseline instead of copying that sketch, and the
  control-directory ownership detail deferred by issue #7 stays unresolved
  rather than reintroducing the root init container the fork's sketch shows.
