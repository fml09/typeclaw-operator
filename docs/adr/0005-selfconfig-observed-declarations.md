---
status: accepted
---

# Ship SelfConfig v1 as observed declarations with platform-enforced protected paths

ADR 0004 deferred agent-authored configuration changes: no upstream transport
exists for agent→operator submissions, and inventing a write path into the
single-writer Agent Folder would violate the ownership model. On 2026-08-25
the product owner asked to proceed anyway ("SelfConfig도 진행"). This ADR
records how v1 ships without breaking either constraint.

## Ground facts

- The TypeClaw runtime remains the authoritative editor of
  `<agentFolder>/typeclaw.json`. Its own file-edit guards already block
  privilege-promoting changes; those guards are workload authority.
- The relay sidecar is a platform component inside the Instance Pod sharing
  the Agent Folder volume's group ownership, and it is the only Pod-local
  process with both filesystem reach and (narrow) cluster authority.

## Decision

SelfConfig v1 is **observation plus policy projection**, not mediated writes:

1. The relay mounts `/agent` read-only, polls `typeclaw.json`, and on every
   content change records an observation into `status.selfConfig`: a SHA-256
   digest, the changed top-level JSON keys, a monotonically increasing
   revision, and whether any changed key intersects
   `spec.selfConfig.protectedPaths`. It evaluates against the protected-path
   list it reads from the same Instance object it patches, so one Get serves
   as both policy source and write target.
2. Field ownership is explicit: the operator never writes
   `status.selfConfig`; the relay never writes conditions or spec. The main
   reconciler projects `status.selfConfig.protectedViolation` into the
   `SelfConfigCompliant` condition and emits Events.
3. Protected paths are top-level keys of `typeclaw.json`. Deep-pointer
   matching stays out of v1.

## Rejected alternatives

- **Operator writes accepted config back into the Agent Folder** — breaks
  single-writer, collides with runtime reload semantics, and turns the
  operator into a second config authority. Rejected outright.
- **Request/approval CRD pipeline** (`TypeClawSelfConfig` resource) — waits
  on upstream defining which operations agents may request and how they are
  expressed. Revisit when the fork or upstream lands a submission contract;
  the observation layer below is compatible with adding it later.
- **Relay-owned conditions** — would give the sidecar authority over
  operator-owned status surface. Rejected; relay only fills its own fields.

## Consequences

- Operators gain drift visibility and compliance signaling for agent-edited
  config without any upstream change.
- `spec.selfConfig` absent ⇒ no observation, no condition: GitOps users who
  treat the whole folder as opaque keep today's behavior.
- Relay RBAC widens by exactly `patch/get typeclawinstances/status`
  restricted to its own Instance (resourceNames), alongside pod deletion.
