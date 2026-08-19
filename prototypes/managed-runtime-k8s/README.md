# PROTOTYPE: TypeClaw Managed Runtime on Kubernetes

This throwaway prototype answers one question from
[`Prove the Kubernetes managed-runtime contract`](https://github.com/fml09/typeclaw-operator/issues/4):

> Can the proposed upstream TypeClaw Managed Runtime run as a single-active
> Kubernetes workload with durable Agent Folder state, writable credentials,
> meaningful health probes, and an externally owned restart handoff—without
> hostd or Kubernetes credentials in the runtime?

It is intentionally kept off `main`. The prototype is evidence for the
architecture decision, not operator implementation.

## Run

Prerequisites are Docker, kind, kubectl, jq, and a locally built Linux image
from TypeClaw PR
[`typeclaw/typeclaw#1373`](https://github.com/typeclaw/typeclaw/pull/1373) at
commit `d02c0892b2467717bd0ed12d5fac4dba6c7aa4c7`.

```sh
TYPECLAW_PROTOTYPE_IMAGE=typeclaw-runtime:managed-final ./run.sh
```

The script creates an ephemeral kind cluster, loads the image, and deletes the
cluster when it exits. Set `TYPECLAW_PROTOTYPE_KEEP_CLUSTER=1` to retain it for
inspection.

## What it exercises

- a single-replica StatefulSet with separate Agent Folder and runtime-home PVCs;
- init-container ownership handoff to UID/GID `65532`;
- a non-root runtime with read-only root, no added capabilities, no privilege
  escalation, and the documented unconfined seccomp requirement for bubblewrap;
- the immutable dependency graph and default GWS plugin despite a stale
  Agent Folder `node_modules` copy;
- `/health/live` and `/health/ready` as real Kubernetes probes, including a
  live/ready-to-stopping transition where readiness returns `503`;
- writable-secret update and unrelated provider-data preservation on the Agent
  Folder PVC;
- a restart accepted through the live TypeClaw WebSocket server and observed
  through the shared file spool while the runtime remains alive;
- externally initiated Pod replacement, followed by PVC-backed secret and
  runtime-home continuity, a surviving TUI restart-handoff record, and a
  graceful exit bounded well below Kubernetes force termination;
- preservation of an executable workspace file across both init-container
  ownership handoffs; and
- absence of a mounted Kubernetes service-account token in both runtime and
  relay containers.

## Expected decision boundary

This can prove the proposed contract is mechanically viable. It cannot make
TypeClaw `0.48.7` an official Managed Runtime release. Until the upstream PR is
accepted and released, the operator must treat the runtime image/version as an
unmet release prerequisite rather than ship a downstream fork.

## Observed result

Observed on 2026-08-19 with kind/Kubernetes `v1.36.1` and local arm64 image
`sha256:2f2dbd8f139df29d6a3551e20843ff0e3268a1f21b9bf4a11f147174b51a66b0`.
That image came from the PR worktree immediately before the pinned commit.
Post-run source comparison found the managed Dockerfile and health sources
byte-identical to the commit; `src/capabilities/runtime.ts` differed only in
formatter layout. This execution therefore establishes behavioral feasibility,
not byte-for-byte release provenance.

The complete script passed. In particular, the live server accepted the restart
frame, the request was visible to the relay while the original Pod UID remained
ready, and external actuation exposed `live=200` plus `ready=503/stopping`
before the process exited. The old Pod disappeared in 1 second; the executable
guard requires under 20 seconds, well below its 60-second Kubernetes grace
period. Deleting it produced a
different Pod UID with the updated `secrets.json`, provider sentinel,
runtime-home marker, TUI restart handoff, and workspace executable mode intact
on their PVCs. The replacement runtime returned a clean, non-degraded readiness
response.

Verdict: **yes for the proposed upstream contract; no for the current official
release**. The latest official release remains `0.48.7`. The upstream PR is
still open and has a requested canonical-source checkout repair in its release
workflow, so this prototype is feasibility evidence, not evidence that a
reproducible supported image is already published.

This remains a deliberately narrow proof. It ran one arm64 image on a
single-node kind `hostPath` PV; it does not prove amd64, a published multi-arch
manifest, CSI reattachment or restore, backup correctness, or cross-node
mobility. It proves only the baked/default dependency graph. Explicit arbitrary
plugins still need a separately decided hydration or derived-image policy.
