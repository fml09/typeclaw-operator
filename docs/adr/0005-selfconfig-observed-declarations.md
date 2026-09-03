---
status: accepted
---

# Ship SelfConfig v1 as observed declarations with platform-enforced protected paths

ADR 0004 deferred agent-authored configuration changes: no mediated write
transport exists from the runtime to the operator, and inventing a write path
into the single-writer Agent Folder would violate the ownership model. On
2026-08-25 the product owner asked to proceed anyway — in their words,
translated from Korean: "go ahead with SelfConfig too".
This ADR records v1 observation through a sanitized runtime contract without
adding an operator-owned write authority.

## Ground facts

- The TypeClaw runtime remains the authoritative editor of
  `<agentFolder>/typeclaw.json`. Its own file-edit guards already block
  privilege-promoting changes; those guards are workload authority.
- The Managed Runtime emits a `ConfigObservationDocument` to the
  operator-provided runtime-to-relay channel. The document contains only the
  full-file digest and per-top-level-key digests; it never contains raw config
  or credential values.
- The relay sidecar is a platform component with narrow cluster authority, but
  it has no Agent Folder mount and cannot traverse the runtime's filesystem.

## Decision

SelfConfig v1 is **observation plus policy projection**, not mediated writes:

1. The Managed Runtime writes a sanitized `ConfigObservationDocument` to the
   dedicated runtime-to-relay channel. The relay polls that document and on
   every digest change records an observation into `status.selfConfig`: a
   SHA-256 digest, the changed top-level JSON keys, a monotonically increasing
   revision, and whether any changed key intersects
   `spec.selfConfig.protectedPaths`. Raw `typeclaw.json` and credential files
   never cross the channel.
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
  config through the Managed Runtime's sanitized observation contract; the
  operator still never writes the Agent Folder.
- `spec.selfConfig` absent ⇒ no observation, no condition: GitOps users who
  treat the whole folder as opaque keep today's behavior.
- Relay RBAC widens by exactly `patch/get typeclawinstances/status`
  restricted to its own Instance (resourceNames), alongside pod deletion.
