---
status: accepted
---

# Implement v1 operator capabilities with TypeClaw-native shapes ahead of full spec completion

The approved Wayfinding Map scoped its destination at a decision-ready handoff
and listed implementation as out of scope until every open ticket resolved. On
2026-08-25 the product owner directed full implementation of the
Hermes-equivalent capability set now ("hermes operator처럼 기능들을 모두
구현"). This ADR records that pivot and the conservative defaults taken for
tickets that remain open, so code does not silently override the tracker.

Scope follows the Capability Parity rule in CONTEXT.md: the same operational
outcome as Hermes, expressed through TypeClaw-native concepts — not copied
schemas.

## Decisions taken for open tickets

- **#3 resource surface**: one namespaced `TypeClawInstance` owns all declared
  policy. No raw pod escape hatch in v1; workload shape is fully
  operator-rendered from the managed runtime contract.
- **#6 lifecycle**: single-active StatefulSet, suspend-to-zero, rolling image
  replacement through the restart relay or controller rollout, retained PVCs
  by default (`onInstanceDeletion: Retain`, mapped to the native StatefulSet
  persistentVolumeClaimRetention policy — no finalizer, no root repair).
- **#7 persistence/backup**: scheduled tar snapshots to a dedicated snapshot
  volume, count-based retention, and guarded restore into a suspended
  Instance with an empty-target check. v1 captures snapshots live; crash
  consistency relies on SQLite journal resilience rather than an unproven
  scale-to-zero dance whose controller-death recovery semantics are unsafe.
  Object-storage destinations stay future work.
- **#8 networking**: default-deny ingress allowing same namespace plus
  optional CIDRs on port 8973; egress `PublicWeb` (the CONTEXT.md destination
  universe) rendered with explicit ipBlock exclusions, DNS excepted;
  `Unrestricted` opt-out.
- **#9 SelfConfig/GitOps**: mediated config writes remain **deferred** — no
  upstream operation contract exists and inventing a write path into the
  single-writer Agent Folder would violate the ownership model. Sanitized
  SelfConfig observation is shipped separately under ADR 0005.
  AutoUpdate deliberately never rewrites spec.runtime.version so GitOps stays
  authoritative about intent.
- **#10 license**: Apache-2.0, matching the existing source header template.
- **#11 status**: conditions describe TypeClaw-owned milestones
  (ResourcesReady, RuntimeReady, BackupReady, AutoUpdateReady); bounded
  cardinality metrics; Kubernetes Events on transitions.
- **#12 distribution**: signed multi-arch operator image, plain manifests,
  Helm chart. OLM bundle and SBOM attestations land with first tagged release.
- **Restart relay** implements the upstream file-spool contract as a same-Pod
  sidecar holding the only Kubernetes credential in the data plane's Pod, via
  a projected service-account token scoped to deleting exactly its own Pod.
  Model-controlled tools never receive it (ADR 0001 boundary preserved).

## Consequences

- Implementation now leads the tracker; open tickets must converge onto
  recorded behavior instead of blocking it, and divergences get new ADRs.
- SelfConfig mediated writes remain deferred; its sanitized observation and
  policy projection are implemented under ADR 0005.
