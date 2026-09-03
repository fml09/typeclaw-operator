# Persistent Linux desktop PoC on KubeVirt

Status: experimental feasibility and decision memo; not an accepted design
Observed: 2026-08-30

## Conclusion

It is feasible. The smallest first PoC gives every user one persistent Ubuntu/Xfce VM and lets a single `Desktop Gateway` relay KubeVirt's native VNC.

The core decisions are as follows.

1. This desktop is not a `Sandbox Lease` discarded per chat session. It is a **Personal Desktop**: bound to a user and a TypeClaw Instance, and kept across many sessions. That name is a provisional term, not yet accepted into the glossary or an ADR.
2. The user binding key is the tuple of the verified OIDC `(iss, sub)` and the `TypeClawInstanceUID`. Kubernetes resource names and labels use a keyed digest of that tuple instead of the raw identity.
3. The VM is started and stopped under `runStrategy: Manual` while the whole-root DataVolume/PVC is kept. Closing the browser or stopping the VM still leaves files, the browser profile, and installed applications in place.
4. The browser and the TypeClaw computer-use plugin look at the same graphical console. So that a human and the agent never type at the same time, only one of `HumanOwned | AgentOwned | Unowned` is allowed.
5. SPIFFE mTLS is out of scope for this PoC. Instead the browser uses an OIDC session plus a private proxy credential, and the TypeClaw extension uses an HMAC bearer bound to the exact owner tuple. That choice does not satisfy the accepted production boundary, so the capability must be marked `Experimental` only.

The implemented scope is one owner pre-provisioned by an administrator with a script, plus a TypeClaw runtime dedicated to that owner. Login-based self-service provisioning, a per-call principal for a shared runtime, a durable owner registry, and Surf-style chat integration do not exist yet. The current web page offers only the desktop/control pane.

The operator's product path has no KubeVirt/CDI client, VM controller, or desktop resource. The manager registers only the TypeClaw, credential, network, backup, and update controllers, and the root module has no KubeVirt dependency either ([manager registration](../../cmd/manager/main.go#L71-L125), [`go.mod`](../../go.mod#L5-L11)). The KubeVirt client and the manifests were isolated in `experiments/personal-desktop-poc/` alone. This is therefore not a matter of adding one URL to an existing plugin but a new experimental provider workstream.

## Evidence policy

This document uses the following labels.

- **Observed fact** is something confirmed directly in an official source or in the current repository.
- **Inference** is a design judgement derived from observed facts.
- **Recommendation** is a candidate to apply to this PoC, not an accepted ADR.
- **Assumption** is a condition to fix before implementation or to verify in the PoC.

The main upstream sources were read at the following immutable revisions.

- E2B Surf [`d2a98aa9d0cd67db5146bec843a296f132d443f5`](https://github.com/e2b-dev/surf/tree/d2a98aa9d0cd67db5146bec843a296f132d443f5).
- E2B Desktop template [`89a545e22343aa1c40f28338bf3281a6c04b1d4a`](https://github.com/e2b-dev/desktop/tree/89a545e22343aa1c40f28338bf3281a6c04b1d4a) and SDK [`5a56c87e9db0e221b138662805af7743e75f1082`](https://github.com/e2b-dev/E2B/tree/5a56c87e9db0e221b138662805af7743e75f1082).
- KubeVirt user guide [`bf1f3564e2a41eb059df5ab126724bb78cf15200`](https://github.com/kubevirt/user-guide/tree/bf1f3564e2a41eb059df5ab126724bb78cf15200) and source [`a61d1001066c179e1703f28549abe0add45a1807`](https://github.com/kubevirt/kubevirt/tree/a61d1001066c179e1703f28549abe0add45a1807).
- noVNC [`ac861f9e280b015569c4b1c3999516d9c0fa35c3`](https://github.com/novnc/noVNC/tree/ac861f9e280b015569c4b1c3999516d9c0fa35c3).
- TypeClaw [`681f581793a0cbb98126e3c0288e7ea8d60206c3`](https://github.com/typeclaw/typeclaw/tree/681f581793a0cbb98126e3c0288e7ea8d60206c3).

## Recommended PoC architecture

```text
Authenticated browser                       TypeClaw Runtime
  OIDC session                                computer-use Platform Extension
  noVNC over same-origin WSS                   owner-scoped HMAC bearer
         │                                               │
         └──────────────────┬────────────────────────────┘
                            ▼
                  Desktop Gateway (PoC)
                  - owner binding and authorization
                  - VM start/stop/readiness
                  - VNC WebSocket relay
                  - screenshot and RFB input adapter
                  - one input-owner state machine
                  - narrow KubeVirt ServiceAccount
                            │
             Kubernetes-authenticated VNC/screenshot subresources
                            ▼
                  KubeVirt VirtualMachineInstance
                  Ubuntu + Xfce + display manager
                            │
                     whole-root DataVolume/PVC
                     retained while VM is halted
```

**Observed fact.** KubeVirt exposes VNC as an authenticated WebSocket subresource and exposes a separate PNG screenshot endpoint ([API routes](https://github.com/kubevirt/kubevirt/blob/a61d1001066c179e1703f28549abe0add45a1807/pkg/virt-api/api.go#L363-L376)). Its VNC handler is a raw bidirectional streamer and rejects stopped VMIs or VMIs without a graphics device ([handler](https://github.com/kubevirt/kubevirt/blob/a61d1001066c179e1703f28549abe0add45a1807/pkg/virt-api/rest/vnc.go#L37-L86)). `virtctl vnc --proxy-only` proves that this WebSocket stream can be adapted to an ordinary VNC client connection ([proxy implementation](https://github.com/kubevirt/kubevirt/blob/a61d1001066c179e1703f28549abe0add45a1807/pkg/virtctl/vnc/vnc.go#L101-L185)).

**Observed fact.** noVNC accepts a WebSocket carrying a standard RFB stream, renders it in an element, and sends keyboard and pointer events ([noVNC API](https://github.com/novnc/noVNC/blob/ac861f9e280b015569c4b1c3999516d9c0fa35c3/docs/API.md#L3-L10), [connection contract](https://github.com/novnc/noVNC/blob/ac861f9e280b015569c4b1c3999516d9c0fa35c3/docs/API.md#L175-L228)).

**Inference.** The browser does not need to reach kube-apiserver directly. It is enough for the Desktop Gateway to verify the user session, open that user's VMI VNC subresource with its own ServiceAccount, and hand the browser only a same-origin WebSocket. Neither the browser nor the TypeClaw Runtime is given a kubeconfig or a Kubernetes token.

**Recommendation.** In the PoC the Gateway may hold the lifecycle control plane and the VNC data plane in one process. Keep the API and package seams separate, and apply the accepted ADR's Broker/Reconciler process split when promoting to production. This merge is a deliberate deviation taken to shorten the PoC.

## Ubuntu/Xfce image and persistent disk

### Golden image

The initial image is made into a golden DataVolume/PVC by importing an Ubuntu cloud image with CDI and then installing the following.

- Xfce and a display manager
- One standard desktop user, with autologin for the PoC
- `qemu-guest-agent`
- The browser and the minimum applications the acceptance tests need
- Fixed resolution and disabled screen lock

By default KubeVirt attaches a VGA-compatible graphics device that VNC can connect to, and `virtio` video can also be selected for modern guests ([graphics device](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/compute/virtual_hardware.md#L449-L519)). A tablet input device can be declared for an absolute-coordinate pointer ([tablet input](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/compute/virtual_hardware.md#L738-L764)). There is therefore no need to install `x11vnc` and `websockify` inside the guest again the way E2B does. Xfce runs only on the guest display, and the stream uses KubeVirt's native VNC.

That E2B Desktop combines Ubuntu 22.04, Xvfb, Xfce, `xdotool`, `scrot`, `x11vnc`, and noVNC/websockify is a good reference for the Linux desktop UX being achievable ([template](https://github.com/e2b-dev/desktop/blob/89a545e22343aa1c40f28338bf3281a6c04b1d4a/template/template.py#L3-L65)). Because a KubeVirt VM has a real virtual graphics device, however, the Xvfb and guest-side VNC parts are not copied.

### Per-user whole-root persistence

**Observed fact.** KubeVirt's `containerDisk` and ephemeral volumes do not preserve written state after the VM stops. A root disk that must persist across stop/restart has to use a PVC or a DataVolume ([storage behavior](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/storage/disks_and_volumes.md#L655-L722)).

**Observed fact.** The `Manual` run strategy changes power state only through `start`, `stop`, and `restart` subresource calls, and keeps the strategy at `Manual` even after those calls ([run strategy](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/compute/run_strategies.md#L14-L49), [command behavior](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/compute/run_strategies.md#L55-L105)). A force stop is the equivalent of pulling the power cord and can cause data loss, so it must not be used for idle shutdown ([lifecycle warning](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/user_workloads/lifecycle.md#L48-L62)).

**Recommendation.** Clone a whole-root DataVolume from the golden disk once when the user is first created, and attach only that same volume afterwards. The root DataVolume is not hidden in the VM's `dataVolumeTemplates`; a `PersonalDesktop` record tracks it as a separate lifecycle resource and the VM references that volume.

- A browser disconnect or the end of a chat closes only the connection.
- An idle timeout shuts the guest down gracefully and then stops the VM. The PVC is not deleted.
- The next web login or TypeClaw action starts the same VM and waits for desktop readiness.
- Deleting an account or a desktop is a separate, confirmed destructive operation. Only then is the PVC deleted, after an optional final snapshot.

The deletion order, after revoking access, is `confirm VMI absence → delete the VM or remove the root DataVolume reference and confirm the volume detached → confirm DataVolume/PVC absence`. An ACK for the API delete request alone must not advance to the next step. Compute stop, VM detach, and storage cleanup failures have to stay separate retry stages, and the absence of a PVC object does not prove secure erasure of a `Retain` PV or of the backend data. This order is expressed in the PoC state model as a deletion phase orthogonal to the compute phase, but it is not implemented as a Kubernetes controller/finalizer yet.

Storage created from a VM's `dataVolumeTemplates` is deleted together with the VM ([DataVolume ownership behavior](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/storage/disks_and_volumes.md#L538-L595)). Idle handling must therefore never delete and recreate the VM object. The PoC starts and stops the VM while preserving it, and must not change the root volume identity even during controller repair.

PVC persistence is not backup. A KubeVirt VM snapshot requires CSI `VolumeSnapshotClass` support, and the QEMU Guest Agent takes part in the application consistency of an online snapshot ([snapshot prerequisites and quiescing](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/storage/snapshot_restore_api.md#L3-L38)). Snapshot/restore is left for the step after the PoC core flow.

## User recognition and authorization

### Durable owner key

**Observed fact.** OpenID Connect states that only the `(iss, sub)` combination may be relied on by an RP as a long-term stable user identifier, and guarantees neither uniqueness nor stability for email and display name ([OIDC Core §5.7](https://openid.net/specs/openid-connect-core-1_0-18.html#ClaimStability)).

**Recommendation.** The desktop's logical owner key is the following tuple.

```text
(oidcIssuer, oidcSubject, typeClawInstanceUID)
```

The Kubernetes object name uses the first 20 lowercase hexadecimal characters of `HMAC-SHA256(platformKey, canonicalTuple)` rather than the raw values. This avoids both email changes and resource-name injection, and keeps desktops from being mixed up when the same user connects to a different TypeClaw Instance. The HMAC key and the owner mapping record are platform state that must be recovered before the PVC. This PoC has no alias registry, so if the platform key, the `iss`/`sub`, or the TypeClawInstance UID changes, a new blank name is chosen and the existing PVC is not carried over automatically. A migration that records the old and new binding before the change and clones/rebinds the disk is required.

**Assumption.** The first PoC assumes that one TypeClaw Instance belongs to one OIDC subject. If several people use the same TypeClaw Instance and each want their own PC, the current plugin context is not sufficient. The only identity the TypeClaw tool context gives a plugin is `sessionId`; there is no verified user principal ([TypeClaw `ToolContext`](https://github.com/typeclaw/typeclaw/blob/681f581793a0cbb98126e3c0288e7ea8d60206c3/src/plugin/types.ts#L21-L26)). That case needs a separate contract in which the platform adds a verified subject binding to every call.

### PoC authentication without SPIFFE

The PoC does not mix the following three credential domains.

1. The browser uses an OIDC session cookie over HTTPS/WSS. The OIDC reverse proxy strips client-supplied identity/Authorization/proxy headers and injects the verified `iss`/`sub` and a private proxy credential into the Gateway.
2. The TypeClaw Platform Extension reads an owner-scoped bearer from a mounted Secret, not from a model argument. The Gateway verifies `HMAC(agentTokenKey, issuer, subject, TypeClawInstanceUID)`, so one owner's token cannot be reused for another subject. The signing key itself is never given to TypeClaw.
3. Only the Gateway holds narrow Kubernetes RBAC for `virtualmachineinstances/vnc`, `virtualmachineinstances/vnc/screenshot`, and VM read/start/stop. DataVolume/PVC provisioning is a separate render/apply step, and the Gateway is not given write permission for it.

A VM name, namespace, or `desktopId` sent by the browser is never trusted as-is. The target is resolved server-side from the authenticated owner, as in `/desktop/me`. A VNC URL or a Kubernetes bearer token is never placed in an iframe URL query.

This approach is not a production design that replaces SPIFFE workload identity. It solves neither static bearer theft, nor Gateway compromise, nor rotation of long-lived desktop credentials, so it cannot claim production conformance with the accepted [ADR 0001](../adr/0001-restricted-workload-and-tool-execution-boundaries.md) and [ADR 0002](../adr/0002-spiffe-workload-identity-and-credential-execution.md).

## Browser control and agent control

### noVNC web surface

A future product UI may show a desktop pane and a chat pane together the way Surf does, but the current PoC page implements only the desktop/control pane. In both cases a raw upstream URL is never handed to an iframe; a project-owned noVNC component connects to a same-origin WSS endpoint. noVNC itself is a static client library, and even the simplest deployment requires a web server and a WebSocket proxy for VNC ([embedding guide](https://github.com/novnc/noVNC/blob/ac861f9e280b015569c4b1c3999516d9c0fa35c3/docs/EMBEDDING.md#L1-L25)).

This PoC Gateway inspects the WebSocket `Origin`'s scheme and authority against the request `Host`. An external browser origin must be `https` and its authority must match `Host` exactly; only loopback in the explicit insecure-dev mode may use `http`. The reverse proxy in front must therefore preserve the external `Host`/HTTP-2 `:authority` and the `Origin`; rewriting them to an internal Service name makes even a same-origin browser VNC connection fail.

noVNC's `viewOnly` is **a boolean that stops the client from sending** keyboard and pointer events ([property contract](https://github.com/novnc/noVNC/blob/ac861f9e280b015569c4b1c3999516d9c0fa35c3/docs/API.md#L80-L83)). It therefore cannot be used as an authorization boundary. This PoC gives a view-only browser no RFB socket and polls the KubeVirt screenshot endpoint instead. The Gateway issues the noVNC/RFB socket only to the single exclusive input controller.

### One input owner

The Gateway manages the following state atomically per desktop.

```text
Unowned ── agent acquire ──> AgentOwned(epoch=N)
Unowned ── user takes control ──> HumanOwned(epoch=N)
AgentOwned ── revoke + drain + fresh frame ──> HumanOwned(epoch=N+1)
HumanOwned ── disconnect/hand back + fresh frame ──> AgentOwned(epoch=N+1)
```

- The browser can watch the screen in any state, but receives an input-capable WebSocket only while `HumanOwned`.
- An agent tool may send actions only while `AgentOwned`.
- An ownership transition drains in-flight actions or closes them as `UnknownOutcome`, and increments the epoch.
- After a hand-back the agent must take a new screenshot and geometry, and must not reuse stale coordinates.
- The same action is never replayed automatically after a timeout.

The KubeVirt VNC handler's default behavior is that a new connection drops the existing VNC session; the `preserveSession` option changes that ([handler default](https://github.com/kubevirt/kubevirt/blob/a61d1001066c179e1703f28549abe0add45a1807/pkg/virt-api/rest/vnc.go#L43-L60)). Before holding a browser viewer and an agent connection open at the same time, first verify that `preserveSession=true` really keeps both clients stable on the actual target version. Otherwise the Gateway must own a single upstream connection and fan the frames out.

### Minimal TypeClaw tool surface

The TypeClaw extension does not expose lifecycle or the raw VNC address to the model; it provides the following two conceptual capabilities.

- `computer_observe`: returns the Gateway's bounded image result together with `frameId`, dimensions, and ownership state to an image-capable main model.
- `computer_act`: sends `expectedFrameId`, `epoch`, `actionId`, and a bounded ordered `actions[]` of click, move, type, key, and scroll that can be decided from one frame.

The implemented PoC plugin exposes these capabilities as `desktop_observe` and `desktop_act`, and keeps `desktop_status`, `desktop_acquire`, `desktop_launch`, `desktop_windows`, `desktop_power`, and `desktop_release` separate for lifecycle and status. `desktop_observe` returns the bounded image result directly to the image-capable main model, so there is no separate `look_at(models.vision)` tool/model round. The order is `acquire → observe(image) → desktop_act(actions[]) echoing the observationId of the next inference`, and up to 16 ordered actions that can be decided from the same frame execute at once, so a screenshot/model round is not repeated for every action. There is no durable `frameId`/`actionId` ledger yet; a failure during guest execution is treated as `Partial` and a lost connection as `UnknownOutcome`, and no batch is ever replayed automatically.

The plugin uses `sessionId` as the owner of the ephemeral input lease and for cancellation correlation, but not as the durable desktop owner identity. When the controlling session ends, only the RFB authority is released; the VM and PVC stay. In the current PoC a power transition must be requested explicitly from the web page or the `desktop_power` tool; lazy start and an idle power policy are not implemented. Both are recommendations for the controller stage.

## UX to take from Surf, and lifecycle not to take

Surf is a good interaction prototype, but it is not a reference for a persistent multi-user architecture.

| Item | Observed in the Surf source | Decision for this PoC |
|---|---|---|
| Screen | The `vncUrl` the backend produces is stored in browser state and rendered as an iframe `src` ([state](https://github.com/e2b-dev/surf/blob/d2a98aa9d0cd67db5146bec843a296f132d443f5/app/page.tsx#L31-L40), [iframe](https://github.com/e2b-dev/surf/blob/d2a98aa9d0cd67db5146bec843a296f132d443f5/app/page.tsx#L384-L390)). | The PoC builds only a desktop/control pane, and a future chat layout also uses a same-origin authenticated Gateway with an embedded noVNC component instead of a client-held raw URL. |
| Sandbox identity | The browser sends `sandboxId` back on the next chat request and the backend reconnects with that ID ([route](https://github.com/e2b-dev/surf/blob/d2a98aa9d0cd67db5146bec843a296f132d443f5/app/api/chat/route.ts#L21-L52)). | Resolved from the server-side `(iss, sub, TypeClawInstanceUID)` mapping, not from a client-provided ID. |
| Lifetime | The timeout is 5 minutes, is extended when 10 seconds remain in a visible tab, and clears client state when it reaches 0 ([timeout](https://github.com/e2b-dev/surf/blob/d2a98aa9d0cd67db5146bec843a296f132d443f5/lib/config.ts#L1-L14), [renewal](https://github.com/e2b-dev/surf/blob/d2a98aa9d0cd67db5146bec843a296f132d443f5/app/page.tsx#L167-L192)). The stop action kills the sandbox ([actions](https://github.com/e2b-dev/surf/blob/d2a98aa9d0cd67db5146bec843a296f132d443f5/app/actions.ts#L6-L25)). | Session expiry closes only the VNC connection; an idle timeout halts only the VM and preserves the PVC. |
| Stream auth | The checked source calls `stream.start()` with no options and returns an interactive `getUrl()` ([creation](https://github.com/e2b-dev/surf/blob/d2a98aa9d0cd67db5146bec843a296f132d443f5/app/api/chat/route.ts#L37-L49)). In the E2B SDK auth is opt-in, and `viewOnly` is a URL option as well ([SDK](https://github.com/e2b-dev/E2B/blob/5a56c87e9db0e221b138662805af7743e75f1082/packages/desktop-js/src/sandbox.ts#L596-L669), [server startup](https://github.com/e2b-dev/E2B/blob/5a56c87e9db0e221b138662805af7743e75f1082/packages/desktop-js/src/sandbox.ts#L672-L731)). | OIDC authorization and a server-side input filter are mandatory. |
| Human/agent handoff | The checked Surf source has no durable owner mapping and no exclusive input-owner state machine. | The Gateway enforces the `HumanOwned`/`AgentOwned` transitions. |

The possibility that the live Surf deployment has access control in front of it that is absent from the source cannot be ruled out. The comparison above covers only the contract the checked repository provides.

## Minimum implementation order

1. **Cluster prerequisite.** Install KubeVirt and CDI as administrator-owned infrastructure. The operator never installs or upgrades them on its own.
2. **One persistent VM.** Build the Ubuntu/Xfce golden image and the whole-root DataVolume/PVC, and verify on a `Manual` VM that state survives `start → change file/browser state → stop → start`.
3. **VNC Gateway.** Relay the KubeVirt VNC/screenshot subresources with a narrow ServiceAccount, and drive the same console from an OIDC-authenticated noVNC page.
4. **Durable mapping.** Implement the provisional `PersonalDesktop` record, the `(iss, sub, TypeClawInstanceUID)` binding, lazy start, and graceful idle halt.
5. **Computer-use extension.** Connect `computer_observe` and `computer_act` to the Gateway. Measure screenshot size and the fixed coordinate space on TypeClaw's actual model path.
6. **Handoff.** Implement Take control/Hand back in the browser and the Gateway's single-writer epoch.
7. **Only then harden.** Add backup/restore, quota, per-user deletion, audit, token rotation, NetworkPolicy, and production identity/separation.

## Acceptance gates

The PoC succeeds only if every item below reproduces.

1. Logging in as Alice and as Bob selects different VMs/PVCs, and changing the URL or ID gives no access to the other person's desktop.
2. Even for the same Alice, a different `TypeClawInstanceUID` binds to a different desktop.
3. After Alice changes files and the browser profile, closes the browser, and the VM is idle-halted, the same data is visible on the next login.
4. The browser and `computer_observe` see the same Xfce screen, resolution, and cursor coordinate space.
5. While the agent is providing input the browser can only view live, and after Take control the agent's actions are rejected. After Hand back, no agent action is accepted without a fresh frame.
6. A new VNC viewer does not drop an existing agent/viewer connection; if it does, switch to a Gateway single-upstream fan-out.
7. Neither the browser nor the TypeClaw Pod holds a kubeconfig or a ServiceAccount token, and the Gateway RBAC is limited to the required subresources and lifecycle verbs in the target namespace.
8. Closing the tab, ending the chat, and a bearer timeout do not delete the PVC. PVC deletion happens only on an explicit Personal Desktop deletion.
9. Owner mapping, power state, and input ownership recover fail-closed after a Gateway restart and after a VM restart. **The current PoC does not fully pass this gate.** `gatewayBootID` invalidates earlier frames, but the power/control quarantine in the Gateway and the plugin is process-local, so a process restart forgets the uncertainty. The live PoC has to verify that VMI state is re-read after a restart and that recovery goes through an explicit start; passing the production gate requires a durable power/action ledger.
10. Record VNC latency, screenshot bytes, boot-to-Xfce P50/P95, and kube-apiserver load at a representative resolution. Because KubeVirt VNC is an API subresource, do not scale to many concurrent viewers without measurement.

The Gateway's localhost smoke-test mode accepts a query identity only when both a loopback `Host` and a separate random `devToken` are confirmed. The same query sent to a ClusterIP host is rejected. The implemented Gateway limits concurrent upstream screenshot requests to three, and the UI stops polling on a hidden tab and backs off on errors, but none of that substitutes for the real load measurement of gate 10.

## Decisions still required

- Ubuntu release, golden image promotion, and the patch/update policy
- Per-user CPU, memory, and root disk quota, and the idle timeout
- Whether one user has to share a PC across several TypeClaw Instances. This memo does not share it.
- Backup, retention, and encryption of the browser profile and credential-bearing state, and the account deletion policy
- The OIDC provider and the Gateway session implementation
- When to restore SPIFFE identity and the Broker/Reconciler process split after the PoC

Even if this PoC succeeds, the current `Sandbox Lease` must not be turned into something persistent. Keep the current domain contract that a `Sandbox Lease` is session-scoped scratch state ([glossary](../../CONTEXT.md)), and promote the durable ownership, retention, deletion, and backup semantics of a Personal Desktop through a separate domain or ADR decision.
