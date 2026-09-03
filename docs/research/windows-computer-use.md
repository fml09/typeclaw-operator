# KubeVirt Windows computer-use for TypeClaw

Status: feasibility and architecture research; not an accepted design  
Observed: 2026-08-30

## Conclusion

It can be built. The scope must not be framed as “one TypeClaw plugin”, however.

The TypeClaw plugin has to be a **Platform Extension** that exposes typed tools such as `observe` and `act` to the model. Windows installation, VM creation and disposal, authentication, network isolation, screen capture, and input execution have to be owned by a new Windows-backed **RemoteSandbox** provider outside the plugin. That provider does not exist in the current repository, and because the current production `RemoteSandbox` contract presumes a gVisor/Kata-class `RuntimeClass`, accepting a KubeVirt VM as a production provider requires a separate ADR and security certification ([ADR 0001](../adr/0001-restricted-workload-and-tool-execution-boundaries.md)).

The recommended structure has the following four parts.

1. TypeClaw's signed, digest-pinned `computer-use` Platform Extension gives the model a small typed tool surface.
2. A Sandbox Broker with no Kubernetes credential enforces the Sandbox Lease, per-invocation authorization, protocol fencing, and output bounds.
3. Only a separate Kubernetes-facing Sandbox Reconciler may use the KubeVirt/CDI/Service/NetworkPolicy/PVC/Secret APIs.
4. Inside every Windows VM there is, separately from the QEMU Guest Agent, a Windows Computer Agent that runs in the real user desktop session. In the PoC that agent wraps only version- and hash-pinned Microsoft `winapp ui` commands behind an allowlist; in production its implementation hides behind a project-owned stable protocol.

The certainty of this judgement differs from step to step.

| Question | Judgement | Evidence and limits |
|---|---|---|
| Can TypeClaw look at a screenshot and invoke Windows actions? | **Yes** | A TypeClaw plugin tool supports Zod-typed arguments, a cancellation signal, and text/image results. |
| Can an unattended Windows image be built on KubeVirt and cloned per lease? | **Yes** | CDI, DataVolume, Sysprep, virtio drivers, the QEMU Guest Agent, and PVC clone/snapshot paths are all in the official documentation. |
| Can the E2B Desktop implementation be ported as-is? | **No** | E2B Desktop is coupled directly to `scrot`, `xdotool`, `x11vnc`, and noVNC on Ubuntu/X11. What is reusable is the API shape and the principle of separating control from viewing. |
| Can the lock screen, LogonUI, and the UAC secure desktop be driven like ordinary tools? | **Must not be supported** | Real input injection requires an unlocked interactive desktop and fails on the secure desktop. Turning UAC off, or working around it, tears down the security boundary. |
| Does this repository provide a production-ready KubeVirt provider today? | **No** | KubeVirt API/RBAC, the Sandbox Lease, VM/PVC lifecycle, the Windows guest protocol, status, NetworkPolicy, and E2E are all new scope. |
| Can startup latency equal to E2B's be guaranteed? | **Undetermined** | E2B uses pre-booted Firecracker snapshots, while a KubeVirt Windows clone depends on storage and OOBE. The KubeVirt examples themselves warn that OOBE after a clone can take up to roughly 5 minutes. |

The recommended goal is therefore to build a **single-VM experimental PoC** first. Even in that PoC the plugin receives no Kubernetes credential and no real external account credential is used. Attach the lease/reconciler and SPIFFE identity afterwards, and consider promotion to production only once failure injection and provider certification pass.

## Evidence policy and observation basis

This document uses the following labels.

- **Observed fact** is something confirmed directly in a pinned official source or in this repository's accepted ADRs and code.
- **Inference** is a design judgement derived from several observed facts.
- **Recommendation** is a candidate design for this project to adopt. It is not an accepted ADR.
- **Assumption** is a condition to verify in the PoC or for a human to decide.

The upstream sources were read at the following revisions.

- TypeClaw upstream `main`: [`9439953bcc117c207dde3b0047730b7398457787`](https://github.com/typeclaw/typeclaw/tree/9439953bcc117c207dde3b0047730b7398457787).
- Owner-managed runtime fork: [`fml09/typeclaw@c95fede9cbf54598179b2c00723507207039ea29`](https://github.com/fml09/typeclaw/tree/c95fede9cbf54598179b2c00723507207039ea29).
- E2B Desktop template: [`89a545e22343aa1c40f28338bf3281a6c04b1d4a`](https://github.com/e2b-dev/desktop/tree/89a545e22343aa1c40f28338bf3281a6c04b1d4a), SDK: [`5a56c87e9db0e221b138662805af7743e75f1082`](https://github.com/e2b-dev/E2B/tree/5a56c87e9db0e221b138662805af7743e75f1082), infrastructure: [`d73e2b1f51cbd7e4d477452ee152571a9d7d08fd`](https://github.com/e2b-dev/infra/tree/d73e2b1f51cbd7e4d477452ee152571a9d7d08fd).
- Target virtualization stack: [KubeVirt `v1.9.0`](https://github.com/kubevirt/kubevirt/releases/tag/v1.9.0), [CDI `v1.66.0`](https://github.com/kubevirt/containerized-data-importer/releases/tag/v1.66.0). The Windows/storage documentation was read at the pinned user-guide revision [`bf1f3564e2a41eb059df5ab126724bb78cf15200`](https://github.com/kubevirt/user-guide/tree/bf1f3564e2a41eb059df5ab126724bb78cf15200).
- Microsoft `winapp` release [`v0.5.0`](https://github.com/microsoft/winappCli/releases/tag/v0.5.0), commit [`fd7cb6f235fa54dd2c6e26386e65e967a2c8797a`](https://github.com/microsoft/winappCli/tree/fd7cb6f235fa54dd2c6e26386e65e967a2c8797a). That release states of itself that it is a Public Preview and experimental, so it is not evidence of production stability ([README](https://github.com/microsoft/winappCli/blob/fd7cb6f235fa54dd2c6e26386e65e967a2c8797a/README.md#L1-L7)).
- The SPIRE Windows identity candidate was read at the current release [`v1.15.3`](https://github.com/spiffe/spire/releases/tag/v1.15.3).

## Extension seams available in TypeClaw

### A plugin tool can express the computer-use facade

**Observed fact.** A TypeClaw plugin is a trusted TypeScript module loaded into the same Bun process as the agent loop, and it can contribute tools, skills, subagents, hooks, and more ([plugin model](https://github.com/typeclaw/typeclaw/blob/9439953bcc117c207dde3b0047730b7398457787/docs/content/docs/concepts/plugins-and-stages.mdx#L7-L17), [contributions](https://github.com/typeclaw/typeclaw/blob/9439953bcc117c207dde3b0047730b7398457787/docs/content/docs/concepts/plugins-and-stages.mdx#L31-L45)). The tool contract carries Zod parameters, an `AbortSignal`, a session ID, and a `{type: "text"}` or `{type: "image", mimeType, data}` result ([Plugin API](https://github.com/typeclaw/typeclaw/blob/9439953bcc117c207dde3b0047730b7398457787/docs/content/docs/reference/plugin-api.mdx#L75-L101), [source type](https://github.com/typeclaw/typeclaw/blob/9439953bcc117c207dde3b0047730b7398457787/src/plugin/types.ts#L8-L47)).

**Inference.** Model-facing `observe` and `act` can be built as plugin tools without a new upstream agent loop. A screenshot can also be returned to the model directly as an image result. Binding lease acquire/release to the TypeClaw session lifecycle instead of exposing them as model tools keeps the permission surface smaller.

**Constraint.** A plugin is not a sandbox. It shares the same process, filesystem, network, and workload identity, so handing a plugin a KubeVirt token or a cluster-admin credential is the same as handing that authority to all of TypeClaw. The accepted ADRs likewise require that the runtime, broker, and sandbox receive no Kubernetes credential and that only the reconciler uses narrow RBAC ([ADR 0001](../adr/0001-restricted-workload-and-tool-execution-boundaries.md), [ADR 0002](../adr/0002-spiffe-workload-identity-and-credential-execution.md)).

**Constraint.** The current managed-runtime image does not install arbitrary plugins dynamically. The platform has to hydrate the plugin before boot or build a derived image, and the TypeClaw process receives no cluster API credential ([managed-runtime image contract](https://github.com/fml09/typeclaw/blob/c95fede9cbf54598179b2c00723507207039ea29/docs/content/docs/internals/managed-runtime.mdx#L7-L23)). A production extension has to be ADR 0002's signed, digest-pinned OCI Platform Bundle rather than a mutable Agent Folder plugin.

**Recommendation.** For the first implementation a direct TypeClaw plugin is better than HTTP MCP. A direct plugin exposes each operation as a named tool and can use `AbortSignal`, the session ID, a custom mTLS/auth client, and per-tool permission and result caps. The current HTTP MCP surface routes the model through the `mcp_list_tools`/`mcp_describe`/`mcp_call` dispatcher ([MCP contract](https://github.com/typeclaw/typeclaw/blob/9439953bcc117c207dde3b0047730b7398457787/docs/content/docs/reference/mcp.mdx#L9-L15)), and the HTTP config has no arbitrary static header field ([config source](https://github.com/typeclaw/typeclaw/blob/9439953bcc117c207dde3b0047730b7398457787/src/config/config.ts#L90-L122)). MCP is also viable where an OAuth-enabled MCP service already exists, but for a project-owned Broker adapter the direct plugin's contract is smaller.

**Constraint.** A plugin's `permissions` declaration only registers permission strings; it performs no automatic authorization ([permission registry](https://github.com/typeclaw/typeclaw/blob/9439953bcc117c207dde3b0047730b7398457787/src/permissions/permissions.ts#L102-L122)). `computer-use.control` must be granted to a role explicitly, and every control tool must fail closed by checking `event.origin` and `ctx.permissions.has(...)` in `tool.before`.

### Screenshot size is a separate acceptance gate

**Observed fact.** `tool-result-cap`, auto-loaded into every TypeClaw agent, limits an image result's base64 string to 262,144 bytes by default and replaces an oversized image with a text placeholder. That is roughly 190 KiB of decoded image ([tool-result-cap contract](https://github.com/typeclaw/typeclaw/blob/9439953bcc117c207dde3b0047730b7398457787/src/bundled-plugins/tool-result-cap/README.md#L1-L18), [defaults](https://github.com/typeclaw/typeclaw/blob/9439953bcc117c207dde3b0047730b7398457787/src/bundled-plugins/tool-result-cap/README.md#L24-L38)).

**Practical consequence.** Returning a `1024x768` Windows PNG as-is can deliver a placeholder to the model instead of the screenshot. The PoC has to work through the following order.

1. Measure the actual size and the model readability of PNG, JPEG, or WebP at a fixed resolution.
2. Stay inside the bound first through downscaling, adaptive compression, and region capture.
3. If that is still not enough, add only `computer_observe`'s fully-qualified tool name to the cap exemption. Turning the whole cap off grows the transcript and model context every turn and is forbidden.
4. Keep the original frame as a short-TTL broker-side object and send the model only a bounded rendition. The original object ID must never lead to arbitrary file reads or cross-lease access.

Even when the screenshot tool returns an image, the computer-use loop does not hold if the configured model does not support image input. The observed TypeClaw default model declares text and image input ([provider registry](https://github.com/typeclaw/typeclaw/blob/9439953bcc117c207dde3b0047730b7398457787/src/config/providers.ts#L68-L80), [default selection](https://github.com/typeclaw/typeclaw/blob/9439953bcc117c207dde3b0047730b7398457787/src/config/providers.ts#L1338-L1342)), but that cannot be generalized to custom profiles. Check the model capability before acquiring a lease and leave the tool unavailable for a text-only model.

## What to take from E2B Desktop and what to discard

### The actual E2B architecture

**Observed fact.** The E2B Desktop image installs Xorg/Xvfb/Xfce, `xdotool`, `scrot`, `x11vnc`, noVNC/websockify, and desktop applications on Ubuntu 22.04 ([template](https://github.com/e2b-dev/desktop/blob/89a545e22343aa1c40f28338bf3281a6c04b1d4a/template/template.py#L3-L78)). The runtime is not a container but an isolated Firecracker Linux microVM resumed from a pre-booted snapshot, with the control plane separated from the sandbox data plane ([E2B architecture](https://github.com/e2b-dev/infra/blob/d73e2b1f51cbd7e4d477452ee152571a9d7d08fd/docs/ARCHITECTURE.md#L13-L28), [runtime path](https://github.com/e2b-dev/infra/blob/d73e2b1f51cbd7e4d477452ee152571a9d7d08fd/docs/ARCHITECTURE.md#L152-L220)).

**Observed fact.** The E2B Desktop SDK's screenshot creates a PNG file with `scrot` and reads it back through the E2B filesystem API, and mouse and keyboard actions run `xdotool` shell commands ([screenshot](https://github.com/e2b-dev/E2B/blob/5a56c87e9db0e221b138662805af7743e75f1082/packages/desktop-js/src/sandbox.ts#L241-L274), [input](https://github.com/e2b-dev/E2B/blob/5a56c87e9db0e221b138662805af7743e75f1082/packages/desktop-js/src/sandbox.ts#L276-L458)). The human stream separately starts the guest's `x11vnc:5900` and noVNC/websockify `:6080` ([VNC server](https://github.com/e2b-dev/E2B/blob/5a56c87e9db0e221b138662805af7743e75f1082/packages/desktop-js/src/sandbox.ts#L596-L752)).

**Inference.** The Linux/X11 implementation in the E2B source cannot be ported to Windows. The following contract shapes are worth reusing instead.

- Separate sandbox identity from the model action API.
- Screenshots and actions use the same native-pixel coordinate space.
- Settle and wait after an action batch, then confirm the effect with a new screenshot.
- Separate the agent control path from the human live-viewer path.
- Start each new isolated session from a golden image or snapshot.

E2B's noVNC `viewOnly` flag is only a browser-side `RFB.viewOnly` setting, not server authorization ([noVNC fork](https://github.com/e2b-dev/noVNC/blob/461b7f1ccb20755037d8995612e5fb08ed16f9e4/app/ui.js#L1732-L1740)). This UI option must not be copied into the TypeClaw authorization boundary.

## What KubeVirt provides and what it does not

### Provisioning and lifecycle substrate

**Observed fact.** CDI provides HTTP/registry import, cloning of an existing PVC, and local image upload as DataVolumes, and supports raw, qcow2, and ISO ([CDI overview](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/storage/containerized_data_importer.md#L1-L17), [formats](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/storage/containerized_data_importer.md#L53-L58)). KubeVirt holds VMI start until the import or clone of a referenced DataVolume finishes ([DataVolume behavior](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/storage/disks_and_volumes.md#L530-L609)).

**Observed fact.** Windows needs virtio storage and network drivers, and KubeVirt documents attaching a containerDisk that carries the virtio drivers and the QEMU Guest Agent as a CD-ROM ([drivers](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/compute/windows_virtio_drivers.md#L6-L29), [installation and containerDisk](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/compute/windows_virtio_drivers.md#L53-L69), [guest tools](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/compute/windows_virtio_drivers.md#L104-L127)). A Sysprep answer file can be attached as a ConfigMap- or Secret-backed CD-ROM, and a generalized image can be produced with the `/generalize /shutdown /oobe /mode:vm` flow ([Sysprep flow](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/user_workloads/startup_scripts.md#L40-L73), [Secret volume example](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/user_workloads/startup_scripts.md#L847-L903)).

**Observed fact.** The QEMU Guest Agent provides the `AgentConnected` condition, OS/interface/user/filesystem information, and ping readiness ([guest information](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/user_workloads/guest_agent_information.md#L1-L24), [API surface](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/user_workloads/guest_agent_information.md#L63-L77), [probe](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/user_workloads/liveness_and_readiness_probes.md#L277-L294)). That API has no screenshot, mouse, keyboard, or UI Automation. The QEMU Guest Agent therefore cannot stand in for the Windows Computer Agent.

### VNC is not the production model-control path

**Observed fact.** The KubeVirt graphical console is reached through `virtctl vnc`/proxy, and VNC and screenshot are Kubernetes-authenticated subresources ([user guide](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/user_workloads/accessing_virtual_machines.md#L28-L46), [RBAC](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/user_workloads/accessing_virtual_machines.md#L362-L407)). The KubeVirt v1.9.0 source likewise separates the `virtualmachineinstances/vnc` and PNG `virtualmachineinstances/vnc/screenshot` routes, and shows that a default VNC request can drop an existing session ([routes](https://github.com/kubevirt/kubevirt/blob/v1.9.0/pkg/virt-api/api.go#L351-L369), [handler](https://github.com/kubevirt/kubevirt/blob/v1.9.0/pkg/virt-api/rest/vnc.go#L37-L85)). The built-in `edit` role includes VNC and port-forward but not screenshot, so the PoC screenshot needs explicit `get` RBAC ([v1.9.0 RBAC](https://github.com/kubevirt/kubevirt/blob/v1.9.0/pkg/virt-operator/resource/generate/rbac/cluster.go#L421-L446), [screenshot resource](https://github.com/kubevirt/kubevirt/blob/v1.9.0/pkg/virt-operator/resource/generate/rbac/cluster.go#L82-L85)). Port-forward traffic also passes through the Kubernetes control plane, and the documentation recommends a dedicated Service for sustained high traffic ([access guide](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/user_workloads/accessing_virtual_machines.md#L249-L258), [API-server pressure](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/user_workloads/accessing_virtual_machines.md#L324-L327)).

**PoC option.** The fastest feasibility spike is a narrow helper that opens the authenticated `/vnc` WebSocket and handles the RFB framebuffer, `KeyEvent`, and `PointerEvent` on the same connection ([RFB protocol](https://datatracker.ietf.org/doc/html/rfc6143)). `/vnc/screenshot` is used only as diagnostic proof. Sending RDP session input against a console frame seen over VNC can drive a different Windows session or display, so screen and input transports are never mixed ([Microsoft console/session distinction](https://learn.microsoft.com/en-us/windows/win32/termserv/consoles-vs-terminals)). Because this spike is a Kubernetes-authenticated helper, it is not the production TypeClaw data path.

**Recommendation.** Use `virtctl vnc` and the KubeVirt screenshot only for image bootstrap, administrator debugging, and break-glass recovery. The production model loop uses the guest Computer Agent's private Service. Only that way is no Kubernetes credential handed to the TypeClaw plugin or to a high-throughput broker, and the API server is not turned into a framebuffer data path.

**Recommendation.** Defer the human viewer as a capability separate from model control. When it does become necessary, design a Viewer Gateway with one-time short-TTL authorization and server-enforced view-only. An RDP/VNC `LoadBalancer` must never be exposed publicly, and a reusable password must never be placed in a URL query.

## Recommended architecture

```text
TypeClaw Runtime Pod
  └─ computer-use Platform Extension
       │  typed tools; no Kubernetes credential
       │  SPIFFE mTLS + Sandbox Lease
       ▼
Sandbox Broker / Capability Gateway                 data plane
  ├─ lease and invocation authorization
  ├─ bounded image/action protocol
  ├─ no Kubernetes credential
  └─ private mTLS connection to one registered VM
       │
       │ request only; separate authenticated API
       ▼
Windows Sandbox Reconciler                          control plane
  ├─ narrow KubeVirt/CDI/PVC/Service/NetworkPolicy RBAC
  ├─ creates and observes per-lease resources
  └─ never proxies screenshots or input
       │
       ▼
KubeVirt Windows VM
  ├─ QEMU Guest Agent: lifecycle/readiness metadata only
  ├─ SPIRE Agent Windows service: workload identity candidate
  ├─ bootstrap/health Windows service: no GUI interaction
  └─ per-user-session Windows Computer Agent
       ├─ stable, allowlisted RPC server
       ├─ UI Automation and screenshot adapter
       └─ PoC implementation: pinned `winapp ui`
```

The per-component authority in this structure is as follows.

| Component | Authority it must have | Authority it must not have |
|---|---|---|
| TypeClaw Platform Extension | Its own TypeClaw permissions, Broker calls, the current lease handle | Kubernetes API, VNC subresource, arbitrary VM address, Windows admin credential |
| Sandbox Broker | Lease verification, bounded action forwarding, outcome records | ServiceAccount token, PVC/VM creation, Windows shell |
| Sandbox Reconciler | Namespace-scoped KubeVirt/CDI/PVC/Service/NetworkPolicy/Secret lifecycle | Model prompts, screenshot bytes, arbitrary tool execution |
| Windows Computer Agent | Allowlisted observe/action on its own interactive desktop | Kubernetes token, arbitrary PowerShell/command execution, another lease's disk or session |
| Viewer Gateway, future | A separate viewer grant and the frame stream | Agent input authority; authorization that relies on client-side `viewOnly` |

### Relationship to the accepted architecture

**Observed fact.** ADR 0001 splits RemoteSandbox into a Sandbox Broker data plane and a Kubernetes-facing Sandbox Reconciler control plane, and requires a session-scoped Sandbox Lease, default-deny Network Authority, no live Agent Folder, no Kubernetes credential, durable outcomes, and fail-closed unknown outcomes. The component split in this document follows that direction.

**Gap.** The production certification wording in the same ADR requires a gVisor/Kata-class `RuntimeClass` for the sandbox Pod. KubeVirt Windows uses a hardware VM and the `virt-launcher` infrastructure, so it cannot be counted as satisfying that wording. One of the following has to be decided in an accepted ADR.

1. Generalize the `RemoteSandbox` security contract into provider-neutral requirements plus a provider-specific certification profile.
2. Add `WindowsVM` as a separate Tool Execution Environment while reusing the Sandbox Lease and the data/control-plane invariants.

This document recommends option 1 but does not decide it. Certification has to include the cluster-wide privileged KubeVirt components, the `virt-launcher` Pod Security/admission tuple, the KubeVirt/CDI/CRI versions, KVM availability, QEMU/libvirt hardening, node eligibility, storage isolation, CNI NetworkPolicy, guest image/protocol digests, and cleanup and failure evidence. The PSS result of a TypeClaw Restricted Workload and the result for privileged virtualization infrastructure must never be mixed or reused for each other.

**Gap.** The current `NetworkSpec.PublicWeb` does not allow cluster, private, or control-plane destinations ([current API](../../api/v1alpha1/typeclawinstance_types.go#L93-L109), [ADR 0002](../adr/0002-spiffe-workload-identity-and-credential-execution.md)). To let the runtime reach the Broker `ClusterIP`, do not widen it to `Unrestricted` and do not misuse PublicWeb; add to the Network Authority a dedicated platform-capability egress that allows only the exact Broker identity and port.

## Windows Computer Agent

### Separate the service from the interactive user process

**Observed fact.** Microsoft states that since Windows Vista a service cannot interact with the user directly, and advises using a separate GUI application in the interactive user session together with IPC ([Interactive Services](https://learn.microsoft.com/en-us/windows/win32/services/interactive-services)).

**Recommendation.** Keep two process roles in the VM.

- The Windows service handles only boot health, SPIRE Agent coordination, session-agent supervision, and update/termination signals.
- The actual screenshot, UI Automation, and input injection are handled by the Computer Agent running in the interactive session of a restricted automation user.
- Between the two, use local IPC such as an ACL-protected named pipe. Never make the service a `LocalSystem` interactive process and never drive the UI from session 0.

**Assumption requiring proof.** How to create and hold an unlocked interactive desktop safely for each lease is not decided yet. The PoC may use a controlled autologon of a disposable local automation account, but production has to threat-model password storage, rotation, session unlock, crash recovery, and the human takeover policy separately. A shared password must never be baked into the golden image.

### Wrap `winapp ui` as the PoC implementation

**Observed fact.** Microsoft `winapp ui` uses UI Automation to inspect and search WPF, WinForms, Win32, Electron, and WinUI 3 applications, and provides `invoke`, `set-value`, `wait-for`, screenshot, and input actions ([overview](https://github.com/microsoft/winappCli/blob/fd7cb6f235fa54dd2c6e26386e65e967a2c8797a/docs/ui-automation.md#L4-L15), [screenshot](https://github.com/microsoft/winappCli/blob/fd7cb6f235fa54dd2c6e26386e65e967a2c8797a/docs/ui-automation.md#L225-L241), [keyboard and values](https://github.com/microsoft/winappCli/blob/fd7cb6f235fa54dd2c6e26386e65e967a2c8797a/docs/ui-automation.md#L399-L452)). Screenshots use Windows.Graphics.Capture by default, fall back to PrintWindow when it is unavailable, and a screen capture mode also exists.

**Observed fact.** The mouse and keyboard injection verbs require an unlocked foreground desktop. On a locked workstation or the LogonUI/UAC secure desktop they fail with `no_interactive_desktop`, and they may refuse with `target_moved` when the target moves during an animation. Where they apply, UIA pattern verbs are safer than real input injection ([interactive requirement](https://github.com/microsoft/winappCli/blob/fd7cb6f235fa54dd2c6e26386e65e967a2c8797a/docs/ui-automation.md#L11-L15), [click/drag safety](https://github.com/microsoft/winappCli/blob/fd7cb6f235fa54dd2c6e26386e65e967a2c8797a/docs/ui-automation.md#L304-L337)). Windows `SendInput` itself is also subject to UIPI restrictions ([Microsoft API](https://learn.microsoft.com/en-us/windows/win32/api/winuser/nf-winuser-sendinput)).

**Recommendation.** The PoC Computer Agent never accepts and runs an arbitrary command line. It compiles stable RPC actions into allowlisted `winapp ui --json` commands and validates the timeout, output schema, target process/window, and image size. Prefer UIA `invoke`/`set-value` and keep pixel click and type as the fallback.

**Production gate.** `winapp` is a Public Preview. The golden image has to pin the version, SHA-256, and signature of the actual `v0.5.0` release artifact, and a conformance test has to prove that the verb and error behavior of the installed binary matches the pinned release documentation. The external contract has to be a project-owned versioned RPC, so that replacing it later with a native UIA/WGC agent does not change the TypeClaw tool contract.

## Tool and wire protocol contract

### Model-facing tools

The recommended model-facing surface is only two tools. Lazy-binding TypeClaw's `ToolContext.sessionId` to the Broker's Sandbox Lease leaves the model no reason to manipulate the lease ID, VM ID, TTL, persistence, or release semantics.

| Tool | Main input | Result |
|---|---|---|
| `computer_observe` | Optional window/region and a bounded rendition preference | Model-visible text metadata plus bounded image content |
| `computer_act` | `expectedFrameId`, a bounded ordered `actions[]`, a deadline | Model-visible terminal outcome, completed action index, warnings, and new frame metadata |

On the first `computer_observe` the plugin calls the Broker's internal `ensureSession` with its own `sessionId`. If policy allows, the Broker creates a lease or returns the existing session binding. `computer_act` can use only that same binding, so a model-supplied identifier cannot select a different VM. Session end or an idle TTL starts the release, while reset, delete, and retain are separated into policy or administrator control-plane operations. A destructive desktop reset is never left as an ordinary model tool.

TypeClaw's `ToolResult.details` and MCP `structuredContent` must not be used as the model-visible contract. `frameId`, width, height, DPI, screen revision, the active window, and a bounded UIA summary **must also be included in the text content** ahead of the image. `details` may duplicate adapter diagnostics, but no model action may depend on them.

The following action allowlist is enough for the first PoC.

- `uia.invoke`, `uia.setValue`, `uia.waitFor`.
- `mouse.move`, `mouse.click`, `mouse.drag`, `mouse.scroll`.
- `keyboard.key`, `keyboard.chord`, `keyboard.type`.
- `wait` with a strict maximum.

Clipboard, arbitrary process launch, PowerShell, file upload/download, microphone, camera, USB, multi-monitor, registry, and Windows setting changes are excluded from the first PoC. If application launch is genuinely required, add only application IDs from an administrator-configured allowlist as a separate action.

An example of the arguments the model sends to the plugin follows.

```json
{
  "expectedFrameId": "frm_184",
  "deadlineMs": 10000,
  "actions": [
    {
      "kind": "uia.invoke",
      "window": { "process": "notepad", "hwnd": "0x2049A" },
      "target": { "automationId": "SaveButton" }
    }
  ]
}
```

When the plugin forwards this call to the Broker it internally adds the bound `leaseId` and a new `invocationId`. The pixel fallback uses fixed native coordinates and puts `screenRevision`, width, height, and DPI into the model-visible text metadata. `expectedFrameId` is an optimistic fence that reduces stale-frame actions, not a guarantee of exact serialization against an asynchronous UI.

### Outcome and retry

The wire protocol has to distinguish at least the following terminal outcomes.

| Outcome | Meaning | Caller behavior |
|---|---|---|
| `Succeeded` | The guest completed the action and the bounded verification | Observe a new frame and continue |
| `Rejected` | The lease, frame, action, or policy was refused before dispatch | The request may be corrected |
| `FailedBeforeDispatch` | It failed before any side effect reached the guest | The same logical intent may be re-evaluated as a new invocation |
| `CancelledBeforeDispatch` | The TypeClaw `AbortSignal` cancelled before dispatch | No side effect |
| `UnknownOutcome` | Connection loss, timeout, guest crash, or cancellation occurred after dispatch | Do not replay the same click or type automatically; require a re-observe and a user or policy decision |

`invocationId` is used to deduplicate an exact duplicate that reaches the guest. Because a crash can occur between the side effect and the durable record, however, exactly-once is not claimed. This applies the unknown-outcome and no-replay principles of ADR 0001/0002 to Windows UI actions as well.

### Wire-level requirements

- The protocol may be implemented over versioned HTTPS/HTTP2, Connect, or gRPC, but the schema and error enum are versioned independently of the transport.
- The Broker and the guest use SPIFFE X.509-SVID-based mTLS as the production minimum.
- Every request carries a lease ID, an invocation ID, a deadline, a maximum action count, and maximum response bytes.
- The Broker verifies the expected VM SPIFFE ID, the Sandbox Lease, the Security Epoch, and the guest image/protocol digest together.
- Screenshots, the UI tree, and error output are bounded. Guest stdout/stderr and raw `winapp` output never go into the model transcript as-is.
- Health and `Capabilities` report the `winapp`/agent version, supported verbs, display geometry, and active session state, but do not expose secrets or the Windows username unnecessarily.

## Golden image and per-lease bootstrap

### Cluster prerequisites

KubeVirt and CDI have to be treated as infrastructure that a cluster admin installs and certifies, not as an application dependency the TypeClaw operator installs automatically. The minimum prerequisites are as follows.

- Worker BIOS/nested virtualization and `/dev/kvm` availability.
- KubeVirt and a compatible CDI version.
- A scratch-capable StorageClass for ISO import.
- Storage that supports golden PVC cloning or CSI VolumeSnapshot/clone.
- A CNI NetworkPolicy that actually applies to the VMI pod.
- Windows VM resource quota and sufficient node capacity.

The official Talos v1.13 guide likewise lists BIOS virtualization, CDI scratch storage, and — when live migration is used — shared storage as separate prerequisites ([Talos KubeVirt guide](https://docs.siderolabs.com/talos/v1.13/advanced-guides/install-kubevirt)). Talos itself is therefore not a blocker, but on the homelab target of ADR 0006 these items have to be measured with a canary.

### Image build sequence

1. Upload or import a customer-supplied, licensed Windows ISO into a CDI DataVolume. Neither the operator repository nor a public registry redistributes a Windows golden disk.
2. Attach a blank OS DataVolume, the Windows ISO, a digest-pinned virtio-win containerDisk, and Secret-backed Sysprep media.
3. The image-builder VM uses `RerunOnFailure`. In KubeVirt, `Always` respawns even a normal guest shutdown, while `RerunOnFailure` respects a controlled shutdown ([run strategies](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/compute/run_strategies.md#L14-L49)).
4. The unattended setup installs matching storage and network drivers, the QEMU Guest Agent, the signed Computer Agent, the pinned `winapp`, the SPIRE Agent, the browser and applications, and fixed display/DPI settings.
5. Verify Computer Agent protocol conformance, a Defender scan, signatures and hashes, the OS patch level, and a clean shutdown.
6. Seal with `sysprep /generalize /oobe /shutdown /mode:vm`. The official KubeVirt sample shows this flow after the driver and Guest Agent installation ([post-install example](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/user_workloads/startup_scripts.md#L1062-L1080)).
7. Publish the sealed PVC or VolumeSnapshot as the golden source and record the Windows build, update level, virtio image digest, Computer Agent digest, `winapp` version and hash, protocol version, and Security Epoch metadata.

The KubeVirt sample's `<EnableLUA>false>` and plaintext Administrator password must not be copied into a production template ([insecure sample lines](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/user_workloads/startup_scripts.md#L1000-L1045)). Keep UAC enabled and finish whatever elevation the image build needs before the model session.

The KubeVirt Tekton task repository has EFI Windows installer and customization pipeline examples, so once the manual bootstrap is stable it can serve as the starting point for an image pipeline ([official task guide](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/cluster_admin/tekton_tasks.md#L45-L57)).

### Per-lease clone

The Sandbox Reconciler creates the following resources per lease.

1. Clone a new OS DataVolume/PVC from the golden source.
2. Generate a fresh VM name, lease labels, one automation user and session, and ephemeral identity bootstrap material.
3. Create a `Manual` run strategy VM, a private ClusterIP Service, a default-deny ingress/egress NetworkPolicy, and a resource quota. KubeVirt propagates VMI labels to the launcher Pod, so they can be used as Service and NetworkPolicy selectors ([Service](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/network/service_objects.md#L1-L12), [ClusterIP example](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/network/service_objects.md#L15-L82), [NetworkPolicy](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/network/networkpolicy.md#L1-L18)).
4. Start the VM explicitly and wait for multi-signal readiness.
5. On release, first revoke the action authority and the network grant, then stop the VM, then clean up the Service, the bootstrap Secret, and the disposable PVC. Durable outcome records are kept independently of the cleanup.

The default persistence has to be `Disposable`. If `RetainedDesktop` is needed, the disk is bound permanently to the same TypeClaw Instance and never reassigned to another tenant or lease. Preserving the PVC of a halted VM does not carry the same semantics as an E2B memory snapshot resume, and startup latency has to be measured again.

A KubeVirt ephemeral volume discards its local COW when the VM stops, but it has constraints around the backing PVC and node-local behavior ([ephemeral volumes](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/storage/disks_and_volumes.md#L655-L669)). The first implementation is recommended to use a per-lease DataVolume clone whose lifecycle is unambiguous.

### Readiness state machine

A lease must not be moved to Active on `VMI Running` alone. The recommended states are as follows.

```text
Requested → Provisioning → Booting → GuestAgentConnected
          → InteractiveDesktopReady → ComputerAgentReady → Active
          → Revoking → Stopping → Cleaning → Released
                                  ↘ Failed
```

Every `Active` condition has to be met.

- The VMI is Running and uses the expected golden-image identity.
- QEMU `AgentConnected` and the guest ping succeed.
- The Windows interactive session is unlocked and the fixed display geometry is applied.
- The Computer Agent completes the mTLS handshake with the expected SPIFFE identity and the protocol and capability versions match.
- A test observe returns a bounded image and a no-op/UIA health action succeeds.

During work in which the Guest Agent disappears briefly, such as a Windows update, a liveness probe that restarts the launcher Pod can destroy the VM itself. KubeVirt states explicitly that a probe pause is needed during a long Windows update ([probe maintenance warning](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/user_workloads/liveness_and_readiness_probes.md#L371-L392)). Production has to use golden image rebuilds and a maintenance policy instead of uncontrolled in-session updates.

## SPIFFE identity candidate

**Observed fact.** SPIRE Agent v1.15.3 can run as a Windows service and provides the Workload API named pipe on Windows. A join token file and a bootstrap trust bundle path are configurable as well ([agent configuration](https://github.com/spiffe/spire/blob/v1.15.3/doc/spire_agent.md#L53-L87), [Windows service](https://github.com/spiffe/spire/blob/v1.15.3/doc/spire_agent.md#L324-L335)). The `windows` workload attestor derives user and group SIDs from the caller process access token, and optionally provides executable path and SHA-256 selectors ([Windows workload attestor](https://github.com/spiffe/spire/blob/v1.15.3/doc/plugin_agent_workloadattestor_windows.md#L1-L28)).

**Candidate design, not an established fact.** No private key, node identity, or reusable join token is baked into the golden image. Prototype it so that the Reconciler injects per-lease one-use bootstrap material valid for at most 600 seconds, together with the trust bundle, through Secret-backed setup media, registers with an explicit expected agent ID, and then lets the SPIRE Agent running as a Windows service attest. The user-session Computer Agent receives an X.509-SVID from the named-pipe Workload API and establishes mTLS with the Broker. The registration has to bind the attested VM parent identity, the signed agent path and hash, and the restricted user SID.

A SPIRE `join_token` is a one-time pre-shared key ([join-token attestor](https://github.com/spiffe/spire/blob/v1.15.3/doc/plugin_agent_nodeattestor_jointoken.md#L1-L8)). Possession of that token is not evidence of running on a particular KubeVirt VMI. A join token alone therefore cannot assert a production VMI identity, and both a node-attestation profile for KubeVirt guests and an explicit VMI/lease binding have to be settled in a new ADR and certification.

The following items must be demonstrated.

- Whether the chosen node attestor and the SPIRE Server route work correctly on a KubeVirt Windows guest.
- How the machine and user SIDs that change after Sysprep/clone are bound into the registration.
- Whether the interactive user process receives the expected selectors and X.509-SVID through the named pipe.
- Whether the Windows privileges needed for higher-integrity process attestation are minimized.
- Whether the join material is consumed exactly once and removed from the Secret, the setup media, guest files, and logs.
- Whether — given that deleting a Kubernetes Secret does not guarantee secure erasure of an already-created setup ISO or guest cache — guest file deletion, media detach, token revocation, and lease disk destruction after token consumption are each verified.
- Whether lease release revokes the SPIRE node/registration entry and the issued authority, and a stale agent ID cannot reconnect.
- Whether a guest clone or a stolen disk cannot re-attest under a different Sandbox Lease.

Before that verification nothing may be recorded as “guest identity is solved because SPIRE is installed”. The early PoC may use short-lived per-lease test certificates, but promotion to production is governed by ADR 0002's SPIFFE identity and no-fallback contract.

## Security model

### Network and Kubernetes authority

- No ServiceAccount token and no kubeconfig goes into the TypeClaw plugin, the Broker, or the Windows guest.
- The Sandbox Reconciler uses a dedicated ServiceAccount and namespace/provider-specific minimal RBAC. A separate deployment and identity reduces the blast radius more than simply adding cluster-wide KubeVirt authority to the current manager RBAC.
- Apply a default-deny NetworkPolicy to the VMI and allow only Broker→Computer Agent, guest→SPIRE, and administrator-approved public destinations. KubeVirt itself states that a VMI is reachable from other endpoints by default and therefore needs a NetworkPolicy ([KubeVirt NetworkPolicy](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/network/networkpolicy.md#L1-L18)).
- Domain allowlisting, DNS rebinding, redirects, and CONNECT/SNI enforcement are the job of ADR 0002's Network Authority, not of NetworkPolicy. When the adapter is absent or inexact, the networked capability is left unavailable.
- RDP 3389, KubeVirt VNC, and the Computer Agent port are never exposed publicly through NodePort or LoadBalancer.

### Action and desktop authority

- Only one Active Sandbox Lease holds the input authority of one Windows interactive desktop.
- If a human takeover is needed, the agent input grant is revoked first. Concurrent agent and human input is not supported.
- Lock workstation, the UAC secure desktop, Ctrl+Alt+Delete/SAS, and elevation prompts fail as `Unavailable`. There is no UAC-disable and no secure-desktop automation fallback.
- UIA selectors take priority over pixel coordinates, but the selector and the active window are still re-resolved immediately before the action.
- Fixed resolution and DPI are treated as a Security Epoch-compatible image contract, and existing frames are rejected once drift appears.
- The guest RPC never exposes raw `cmd.exe`, PowerShell, arbitrary binary paths, or arbitrary `winapp` arguments.

### Data and credentials

- Screenshots and the UI tree can contain passwords, tokens, and personal data. Raw frames need a short TTL, encryption at rest, lease-scoped access, bounded audit metadata, and an explicit retention policy.
- The Agent Folder is never mounted into the VM. If file transfer becomes necessary later, implement ADR 0001's Authorized Workspace View and validated output delta as a separate typed capability.
- The first PoC uses no real account credentials.
- Having the model read a password and type it itself is not Opaque Credential Use. It has to be classified as Raw Credential Disclosure with confirmation and audit applied, or else a separate protocol is needed in which an approved Credential Consumer injects the credential into the exact guest field without returning the bytes to the model or the plugin.
- The clipboard and the browser password manager are off by default. Enabling them requires a separate Credential Consumer and a destination allowlist.

### Supply chain, Windows licensing, and image state

- Version- and digest-pin the Platform Extension, the Computer Agent, `winapp`, the virtio image, and the golden disk metadata, and include them in the Security Epoch.
- No `winapp` development certificate and no Developer Mode is left in a production guest. The project-owned bridge is signed with the production signing identity.
- Windows 11 virtual desktop rights vary with edition, user/device, and access model, so Microsoft's licensing guidance has to be reviewed as a deployment gate ([Microsoft licensing guidance](https://www.microsoft.com/licensing/guidance/Windows-11-Licensing-for-Virtual-Desktops)). Evaluation media is used only within the PoC scope and in line with Microsoft's terms ([Windows 11 Enterprise evaluation](https://www.microsoft.com/en-us/evalcenter/evaluate-windows-11-enterprise)). This document provides neither legal advice nor redistribution rights.
- The Windows 11 installer may require a TPM device, but a persistent vTPM is not mandatory. KubeVirt's persistent TPM/EFI backend-state snapshot supports restoring the same VM only, not cloning into another VM ([persistent state](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/compute/persistent_tpm_and_uefi_state.md#L1-L33), [TPM notes](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/compute/persistent_tpm_and_uefi_state.md#L35-L76)). Avoid persistent backend state in a cloneable golden baseline, and verify per-VM provisioning separately if BitLocker or persistent Secure Boot state is required.

## Gap against the current repository

**Observed fact.** In the 2026-08-30 repository scan the only API kinds are `TypeClawInstance`, `CredentialRequest`, and `CredentialApproval`. The main instance reconciler creates the StatefulSet, the Service, and the relay RBAC ([controller](../../internal/controller/typeclawinstance_controller.go#L43-L138)), and a separate controller manages the NetworkPolicy ([network controller](../../internal/controller/networkpolicy_controller.go#L34-L100)). There is no KubeVirt, CDI, Windows, VNC, or RDP dependency and no resource controller for them.

A production implementation therefore needs at least the following new work packages.

- A generic Sandbox Lease API and a durable per-invocation outcome model.
- A credentialless Sandbox Broker API/Deployment and the TypeClaw Platform Extension.
- A KubeVirt Windows Sandbox Reconciler, dedicated RBAC, and provider status.
- The DataVolume clone, VM, Service, NetworkPolicy, bootstrap Secret, and PVC cleanup/finalizer lifecycle.
- A Windows golden-image pipeline and a signed Computer Agent artifact.
- SPIFFE Windows guest identity and registration lifecycle.
- An exact Broker/guest capability path in the Network Authority.
- Image/result bounds, unknown-outcome, cancellation, no-replay, and cleanup failure-injection tests.
- A KubeVirt/CDI/storage/CNI/Talos canary and a provider certification suite.

Before wedging this scope directly into the existing `TypeClawInstanceReconciler`, the ADR and the API ownership have to be decided. In particular, whether to generalize the generic `RemoteSandbox` contract or to build a Windows-specific Tool Execution Environment, and whether to expose `SandboxLease` as a CRD or keep it an internal reconciler record, are human decisions.

## Failure modes and fail-closed behavior

| Failure | Detection | Required behavior |
|---|---|---|
| No `/dev/kvm`, CDI scratch, clone/snapshot, or NetworkPolicy support | Provider canary and StorageClass/CNI probe | Provider `Unavailable`; no silent fallback to another isolation |
| ISO import or DataVolume clone is delayed or fails | CDI/DataVolume conditions and deadline | Lease `ProvisioningFailed`; no VM start; partial PVC cleanup |
| The VMI is Running but Windows OOBE/session is not ready | QEMU agent + Computer Agent + observe readiness | Do not move the lease to Active; fail after a bounded timeout |
| Only the QEMU Guest Agent is connected | Computer Agent mTLS health mismatch | Report only lifecycle metadata as ready and leave the tools unavailable |
| The Computer Agent connects but is not the expected image or protocol | SVID, Security Epoch, capability digest mismatch | Reject the connection, quarantine or clean up the VM |
| Desktop lock, LogonUI, UAC secure desktop | Agent session state and `no_interactive_desktop` | Fail the action closed; no UAC disable and no VNC fallback |
| Window animation, DPI/resolution change, stale screenshot | `frameId`, display revision, target re-resolution | `StaleFrame`/`target_moved`; require a re-observe |
| Response lost after a click/type dispatch | The invocation journal recorded the dispatch but no completion | `UnknownOutcome`; no automatic retry of the same action |
| A TypeClaw cancellation arrives after dispatch | No guest cancellation acknowledgement | Revoke the remaining actions; the current action may be `UnknownOutcome` |
| Windows update/reboot and a liveness restart loop | Maintenance state, probe/status transitions | Drain the lease; probe pause or bounded maintenance; image rebuild policy |
| A human VNC/RDP session and the agent drive the same desktop | Input-owner lease and session events | Revoke the agent grant or make it viewer-only; no concurrent input |
| A screenshot exceeds the TypeClaw cap | `tool-result-cap` marker and byte metrics | Bounded re-encode or region retry; never treat it as a blank success |
| `winapp` release behavior drift | Golden-image conformance suite | Block image promotion; no installing an arbitrary latest version |
| The snapshot is crash-consistent or includes backend TPM/EFI state | Snapshot indications and volume inventory | Block golden promotion and cloning; rebuild offline or quiesced |
| VM/PVC/Secret/Service remain after release | Finalizer/GC audit and TTL sweeper | Retry cleanup independently with authority already revoked; durable leak status |
| A disposable disk is reused and cross-lease data remains | Owner/lease UID and storage provenance check | Reject the attach; delete by default rather than sanitize |
| License or activation conditions are unclear | Image provenance and licensing review | Block image publication and the production lease |

An online VM snapshot attempts a filesystem freeze when the QEMU Guest Agent is present, but it can be crash-consistent when the agent is missing or the freeze fails. The snapshot status marks this as an indication, and the restore target has to be stopped ([snapshot consistency](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/storage/snapshot_restore_api.md#L30-L38), [indications](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/storage/snapshot_restore_api.md#L86-L99)). Golden image publication has to default to an offline clean shutdown.

## Phased PoC

### Phase 0 — architecture and environment gate

- Decide, in a new ADR, the relationship between the KubeVirt provider and ADR 0001, the Sandbox Lease ownership, and the production certification criteria.
- Approve the Windows licensing and the ISO provenance.
- Verify the Talos nodes' virtualization, the KubeVirt/CDI versions, scratch/clone storage, and the CNI NetworkPolicy with a canary.
- Fix the first target at `amd64`, a single display, a disposable disk, **no Kubernetes credential in the plugin or the guest**, **no real external account credential**, and no public viewer.

Exit criterion: the cluster-admin prerequisites and the security exceptions are stated explicitly and there is no silent fallback.

### Phase 1 — manually managed Windows VM

- Build one Windows golden VM with CDI + Sysprep + virtio/QEMU Guest Agent.
- Set a fixed `1024x768`, or the chosen resolution, and the DPI.
- An administrator confirms the bootstrap with `virtctl vnc` and the KubeVirt screenshot.
- Install deterministic test applications such as Notepad, Calculator, and a browser.

Exit criterion: ten clean clone/boot runs show identical display and session readiness and no shared machine credential.

### Phase 2 — Computer Agent and development plugin

- Put a private test certificate on the static VM and let the Computer Agent run only the pinned `winapp ui` allowlist.
- Expose `observe` and `act` through a development-only TypeClaw plugin. The KubeVirt lifecycle is still manual and the plugin holds no kubeconfig.
- Run UIA-first and pixel-fallback tasks separately.
- Measure screenshot compression and cap behavior, and verify that the model actually receives the frame.

Example tasks:

1. Type given text into Notepad and open the Save dialog, stopping just before the actual file write.
2. Perform the same calculation in Calculator through the UIA path and the pixel path, and inspect the result.
3. Work through checkboxes, a form, and navigation on a local test page in the browser.
4. Confirm that actions fail on the lock screen and at a UAC prompt.

Exit criterion: 100 consecutive observe/action/observe loops, stale-frame rejection, bounded output, cancellation, and the post-dispatch connection-loss `UnknownOutcome` test all pass.

### Phase 3 — Broker, Reconciler, and Sandbox Lease

- Implement the TypeClaw Platform Extension → credentialless Broker → separate Sandbox Reconciler path.
- Automate the DataVolume clone, VM, ClusterIP Service, NetworkPolicy, Secret, TTL, and release cleanup per lease.
- Add only the exact Broker capability route to the runtime NetworkPolicy.
- Check in admission and E2E that there is no ServiceAccount token in the runtime, the Broker, or the guest.

Exit criterion: across 50 concurrent and sequential disposable leases there is no cross-lease network or data access, and zombie resources after a crash are cleaned up within the TTL.

### Phase 4 — SPIFFE and hardening

- Verify the SPIRE Windows service, the Windows workload attestor, the one-shot bootstrap, per-lease registration, and X.509-SVID rotation.
- Release the Platform Extension and the guest artifacts as a signed, digest-pinned Platform Bundle and golden image.
- Add Network Authority, resource quotas, screenshot retention and redaction, security audit, and failure injection.
- Implement the KubeVirt provider canary and Security Epoch invalidation.

Exit criterion: identity replay, a stolen clone, a stale Security Epoch, CNI failure, a SPIRE outage, and cleanup failure all fail closed, and the new ADR's certification suite passes.

### Phase 5 — optional product capabilities

- Human viewer gateway, server-enforced view-only, and agent takeover handoff.
- File transfer based on the Authorized Workspace View.
- Approved credential-field injection.
- Retained desktop, backup/restore, and a measured restart SLO.

None of these enter the PoC scope before core computer-use success and the security boundary are proven.

## Production acceptance checklist

- [ ] The provider-specific ADR and threat model are accepted.
- [ ] The KubeVirt/CDI/CNI/storage/Windows/virtio/Computer Agent version matrix is pinned.
- [ ] The TypeClaw Runtime, Platform Extension, Broker, and guest hold no Kubernetes credential.
- [ ] The plugin is a signed Platform Bundle and is not loaded from a mutable Agent Folder.
- [ ] Cluster and private egress other than the exact Broker route is blocked.
- [ ] One lease binds to one desktop input authority and one disk owner.
- [ ] VMI Running, QEMU AgentConnected, the interactive desktop, and Computer Agent mTLS are all used for readiness.
- [ ] No click or type is replayed after an `UnknownOutcome`.
- [ ] Lock, UAC, and secure desktop fail closed, and UAC is never turned off.
- [ ] Screenshots really enter the model input and respect the byte, context, and retention bounds.
- [ ] The golden image holds no reusable password, join token, private key, or kubeconfig.
- [ ] After a disposable release the VM, PVC, Service, Secret, and guest identity are removed.
- [ ] The Windows licensing, activation, update, and redistribution policy is approved.
- [ ] Startup time, action latency, and screenshot quality SLOs were measured on a real cluster rather than assumed from E2B's numbers.

## Remaining decisions

1. Decide whether to generalize KubeVirt into a generic RemoteSandbox provider or to model it as a separate Tool Execution Environment.
2. Decide whether to make the Sandbox Lease a public namespaced CRD or to keep it an API record private to the Broker and Reconciler.
3. Decide whether to ship only the disposable desktop first, or to include a retained desktop bound to a TypeClaw Instance in v1.
4. Decide how a production interactive Windows session is created and kept unlocked.
5. Decide the resolution, encoding, cap exemption, and retention of the model-facing screenshot.
6. Decide whether to keep `winapp` as the certified implementation or to replace it with a native Windows Computer Agent.
7. Decide whether to separate the human viewer and agent takeover from the core scope.
8. Decide whether to offer credential-bearing UI automation only as Raw Credential Disclosure, or to build a field-specific Opaque Credential Use protocol.

## Primary source index

- TypeClaw: [plugin trust model](https://github.com/typeclaw/typeclaw/blob/9439953bcc117c207dde3b0047730b7398457787/docs/content/docs/concepts/plugins-and-stages.mdx#L7-L17), [Plugin API](https://github.com/typeclaw/typeclaw/blob/9439953bcc117c207dde3b0047730b7398457787/docs/content/docs/reference/plugin-api.mdx#L75-L101), [tool result cap](https://github.com/typeclaw/typeclaw/blob/9439953bcc117c207dde3b0047730b7398457787/src/bundled-plugins/tool-result-cap/README.md#L1-L38), [managed runtime](https://github.com/fml09/typeclaw/blob/c95fede9cbf54598179b2c00723507207039ea29/docs/content/docs/internals/managed-runtime.mdx#L7-L39), [platform ownership](https://github.com/fml09/typeclaw/blob/c95fede9cbf54598179b2c00723507207039ea29/docs/content/docs/internals/managed-runtime.mdx#L177-L183).
- E2B: [Desktop template](https://github.com/e2b-dev/desktop/blob/89a545e22343aa1c40f28338bf3281a6c04b1d4a/template/template.py#L3-L78), [Desktop SDK](https://github.com/e2b-dev/E2B/blob/5a56c87e9db0e221b138662805af7743e75f1082/packages/desktop-js/src/sandbox.ts#L241-L458), [VNC server](https://github.com/e2b-dev/E2B/blob/5a56c87e9db0e221b138662805af7743e75f1082/packages/desktop-js/src/sandbox.ts#L596-L752), [infrastructure architecture](https://github.com/e2b-dev/infra/blob/d73e2b1f51cbd7e4d477452ee152571a9d7d08fd/docs/ARCHITECTURE.md#L13-L28).
- KubeVirt: [`v1.9.0` release](https://github.com/kubevirt/kubevirt/releases/tag/v1.9.0), [Windows virtio drivers](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/compute/windows_virtio_drivers.md#L1-L127), [Sysprep](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/user_workloads/startup_scripts.md#L40-L73), [CDI](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/storage/containerized_data_importer.md#L1-L58), [VM access](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/user_workloads/accessing_virtual_machines.md#L28-L46), [Service](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/network/service_objects.md#L1-L82), [NetworkPolicy](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/network/networkpolicy.md#L1-L28), [snapshot/restore](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/storage/snapshot_restore_api.md#L1-L121), [KubeVirt VNC handler](https://github.com/kubevirt/kubevirt/blob/v1.9.0/pkg/virt-api/rest/vnc.go#L37-L85).
- Microsoft Windows: [`winapp v0.5.0`](https://github.com/microsoft/winappCli/releases/tag/v0.5.0), [`winapp ui` automation](https://github.com/microsoft/winappCli/blob/fd7cb6f235fa54dd2c6e26386e65e967a2c8797a/docs/ui-automation.md#L4-L15), [capture](https://github.com/microsoft/winappCli/blob/fd7cb6f235fa54dd2c6e26386e65e967a2c8797a/docs/ui-automation.md#L225-L241), [input](https://github.com/microsoft/winappCli/blob/fd7cb6f235fa54dd2c6e26386e65e967a2c8797a/docs/ui-automation.md#L304-L452), [Interactive Services](https://learn.microsoft.com/en-us/windows/win32/services/interactive-services), [`SendInput`](https://learn.microsoft.com/en-us/windows/win32/api/winuser/nf-winuser-sendinput), [virtual desktop licensing](https://www.microsoft.com/licensing/guidance/Windows-11-Licensing-for-Virtual-Desktops).
- SPIFFE/SPIRE: [SPIRE Agent configuration](https://github.com/spiffe/spire/blob/v1.15.3/doc/spire_agent.md#L53-L87), [Windows service](https://github.com/spiffe/spire/blob/v1.15.3/doc/spire_agent.md#L324-L335), [Windows workload attestor](https://github.com/spiffe/spire/blob/v1.15.3/doc/plugin_agent_workloadattestor_windows.md#L1-L64), [join token](https://github.com/spiffe/spire/blob/v1.15.3/doc/plugin_agent_nodeattestor_jointoken.md#L1-L8), [`v1.15.3` release](https://github.com/spiffe/spire/releases/tag/v1.15.3).
- Target environment: [Talos v1.13 KubeVirt installation guide](https://docs.siderolabs.com/talos/v1.13/advanced-guides/install-kubevirt).
