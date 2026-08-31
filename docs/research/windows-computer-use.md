# KubeVirt Windows computer-use for TypeClaw

Status: feasibility and architecture research; not an accepted design  
Observed: 2026-08-30

## 결론

구현할 수 있습니다. 다만 범위를 “TypeClaw plugin 하나”로 잡으면 안 됩니다.

TypeClaw plugin은 모델에 `observe`와 `act` 같은 typed tool을 노출하는 **Platform Extension**이어야 합니다. Windows 설치, VM 생성·폐기, 인증, 네트워크 격리, 화면 캡처와 입력 실행은 plugin 밖의 새로운 Windows-backed **RemoteSandbox** provider가 맡아야 합니다. 현재 repository에 이 provider는 없으며, 현행 production `RemoteSandbox` 계약은 gVisor/Kata-class `RuntimeClass`를 전제로 하므로 KubeVirt VM을 production provider로 인정하려면 별도 ADR과 보안 certification이 필요합니다 ([ADR 0001](../adr/0001-restricted-workload-and-tool-execution-boundaries.md)).

권장 구조는 다음 네 부분입니다.

1. TypeClaw의 signed, digest-pinned `computer-use` Platform Extension이 모델에 작은 typed tool surface를 제공합니다.
2. Kubernetes credential이 없는 Sandbox Broker가 Sandbox Lease, per-invocation authorization, protocol fencing, output bound를 집행합니다.
3. 별도 Kubernetes-facing Sandbox Reconciler만 KubeVirt/CDI/Service/NetworkPolicy/PVC/Secret API를 사용할 수 있습니다.
4. 각 Windows VM 안에는 QEMU Guest Agent와 별도로, 실제 사용자 desktop session에서 실행되는 Windows Computer Agent가 있습니다. PoC에서는 이 agent가 version/hash-pinned Microsoft `winapp ui` 명령만 allowlist로 감싸고, production에서는 그 구현을 project-owned stable protocol 뒤에 숨깁니다.

이 판단의 확실성은 단계별로 다릅니다.

| 질문 | 판단 | 근거와 한계 |
|---|---|---|
| TypeClaw가 screenshot을 보고 Windows action을 호출할 수 있는가? | **가능** | TypeClaw plugin tool은 Zod-typed argument, cancellation signal, text/image result를 지원합니다. |
| KubeVirt에 unattended Windows image를 만들고 lease마다 복제할 수 있는가? | **가능** | CDI, DataVolume, Sysprep, virtio driver, QEMU Guest Agent, PVC clone/snapshot 경로가 공식 문서에 있습니다. |
| E2B Desktop 구현을 그대로 port할 수 있는가? | **불가능** | E2B Desktop은 Ubuntu/X11의 `scrot`, `xdotool`, `x11vnc`, noVNC에 직접 결합되어 있습니다. 재사용 대상은 API shape와 control/viewing 분리 원칙입니다. |
| 잠금 화면, LogonUI, UAC secure desktop까지 일반 tool처럼 조작할 수 있는가? | **지원하지 않아야 함** | 실제 input injection은 unlocked interactive desktop을 요구하고 secure desktop에서 실패합니다. UAC를 끄거나 이를 우회하는 방식은 보안 경계를 무너뜨립니다. |
| 지금 이 repository가 production-ready KubeVirt provider를 제공하는가? | **아니요** | KubeVirt API/RBAC, Sandbox Lease, VM/PVC lifecycle, Windows guest protocol, status, NetworkPolicy와 E2E가 모두 새 scope입니다. |
| E2B와 같은 startup latency를 보장할 수 있는가? | **미확정** | E2B는 pre-booted Firecracker snapshot을 사용하지만 KubeVirt Windows clone은 storage와 OOBE에 좌우됩니다. KubeVirt 예제도 clone 후 OOBE에 최대 약 5분이 걸릴 수 있다고 경고합니다. |

따라서 권장 목표는 먼저 **single-VM experimental PoC**를 만드는 것입니다. 이 PoC에서도 plugin에는 Kubernetes credential을 주지 않고, 실제 외부 account credential은 사용하지 않습니다. 그 뒤 lease/reconciler와 SPIFFE identity를 붙이고, failure injection과 provider certification을 통과한 경우에만 production 승격을 검토해야 합니다.

## Evidence policy와 관찰 기준

이 문서는 다음 표기를 사용합니다.

- **Observed fact**는 pinned official source나 이 repository의 accepted ADR/code에서 직접 확인한 사실입니다.
- **Inference**는 여러 observed fact에서 도출한 설계 판단입니다.
- **Recommendation**은 이 project가 채택할 후보 설계입니다. accepted ADR이 아닙니다.
- **Assumption**은 PoC에서 검증하거나 사람이 결정해야 하는 조건입니다.

Upstream source는 다음 revision을 기준으로 읽었습니다.

- TypeClaw upstream `main`: [`9439953bcc117c207dde3b0047730b7398457787`](https://github.com/typeclaw/typeclaw/tree/9439953bcc117c207dde3b0047730b7398457787).
- Owner-managed runtime fork: [`fml09/typeclaw@c95fede9cbf54598179b2c00723507207039ea29`](https://github.com/fml09/typeclaw/tree/c95fede9cbf54598179b2c00723507207039ea29).
- E2B Desktop template: [`89a545e22343aa1c40f28338bf3281a6c04b1d4a`](https://github.com/e2b-dev/desktop/tree/89a545e22343aa1c40f28338bf3281a6c04b1d4a), SDK: [`5a56c87e9db0e221b138662805af7743e75f1082`](https://github.com/e2b-dev/E2B/tree/5a56c87e9db0e221b138662805af7743e75f1082), infrastructure: [`d73e2b1f51cbd7e4d477452ee152571a9d7d08fd`](https://github.com/e2b-dev/infra/tree/d73e2b1f51cbd7e4d477452ee152571a9d7d08fd).
- Target virtualization stack: [KubeVirt `v1.9.0`](https://github.com/kubevirt/kubevirt/releases/tag/v1.9.0), [CDI `v1.66.0`](https://github.com/kubevirt/containerized-data-importer/releases/tag/v1.66.0). Windows/storage documentation은 pinned user-guide revision [`bf1f3564e2a41eb059df5ab126724bb78cf15200`](https://github.com/kubevirt/user-guide/tree/bf1f3564e2a41eb059df5ab126724bb78cf15200)을 사용했습니다.
- Microsoft `winapp` release [`v0.5.0`](https://github.com/microsoft/winappCli/releases/tag/v0.5.0), commit [`fd7cb6f235fa54dd2c6e26386e65e967a2c8797a`](https://github.com/microsoft/winappCli/tree/fd7cb6f235fa54dd2c6e26386e65e967a2c8797a). 이 release는 스스로 Public Preview이자 experimental이라고 명시하므로 production stability 증거가 아닙니다 ([README](https://github.com/microsoft/winappCli/blob/fd7cb6f235fa54dd2c6e26386e65e967a2c8797a/README.md#L1-L7)).
- SPIRE Windows identity 후보는 current release [`v1.15.3`](https://github.com/spiffe/spire/releases/tag/v1.15.3)를 기준으로 했습니다.

## TypeClaw에서 가능한 extension seam

### Plugin tool은 computer-use facade를 표현할 수 있습니다

**Observed fact.** TypeClaw plugin은 agent loop와 같은 Bun process에 로드되는 trusted TypeScript module이며, tool, skill, subagent, hook 등을 기여할 수 있습니다 ([plugin model](https://github.com/typeclaw/typeclaw/blob/9439953bcc117c207dde3b0047730b7398457787/docs/content/docs/concepts/plugins-and-stages.mdx#L7-L17), [contributions](https://github.com/typeclaw/typeclaw/blob/9439953bcc117c207dde3b0047730b7398457787/docs/content/docs/concepts/plugins-and-stages.mdx#L31-L45)). Tool contract에는 Zod parameters, `AbortSignal`, session ID와 `{type: "text"}` 또는 `{type: "image", mimeType, data}` result가 있습니다 ([Plugin API](https://github.com/typeclaw/typeclaw/blob/9439953bcc117c207dde3b0047730b7398457787/docs/content/docs/reference/plugin-api.mdx#L75-L101), [source type](https://github.com/typeclaw/typeclaw/blob/9439953bcc117c207dde3b0047730b7398457787/src/plugin/types.ts#L8-L47)).

**Inference.** Model-facing `observe`와 `act`는 새 upstream agent loop 없이 plugin tool로 만들 수 있습니다. Screenshot도 image result로 직접 모델에 돌려줄 수 있습니다. Lease acquire/release는 model tool로 노출하지 않고 TypeClaw session lifecycle에 bind하는 편이 권한 surface가 작습니다.

**Constraint.** Plugin은 sandbox가 아닙니다. 같은 process, filesystem, network, workload identity를 공유하므로 KubeVirt token이나 cluster-admin credential을 plugin에 주면 TypeClaw 전체에 그 권한을 주는 것과 같습니다. accepted ADR도 runtime, broker, sandbox가 Kubernetes credential을 받지 않고 reconciler만 narrow RBAC를 사용하도록 정합니다 ([ADR 0001](../adr/0001-restricted-workload-and-tool-execution-boundaries.md), [ADR 0002](../adr/0002-spiffe-workload-identity-and-credential-execution.md)).

**Constraint.** Current managed-runtime image는 임의 plugin을 동적으로 설치하지 않습니다. Platform은 plugin을 boot 전에 hydrate하거나 derived image를 만들어야 하며, TypeClaw process는 cluster API credential을 받지 않습니다 ([managed-runtime image contract](https://github.com/fml09/typeclaw/blob/c95fede9cbf54598179b2c00723507207039ea29/docs/content/docs/internals/managed-runtime.mdx#L7-L23)). Production extension은 mutable Agent Folder plugin이 아니라 ADR 0002의 signed, digest-pinned OCI Platform Bundle이어야 합니다.

**Recommendation.** 첫 구현은 HTTP MCP보다 direct TypeClaw plugin이 낫습니다. Direct plugin은 각 operation을 named tool로 노출하고 `AbortSignal`, session ID, custom mTLS/auth client와 per-tool permission/result cap을 사용할 수 있습니다. 현재 HTTP MCP surface는 model이 `mcp_list_tools`/`mcp_describe`/`mcp_call` dispatcher를 거치며 ([MCP contract](https://github.com/typeclaw/typeclaw/blob/9439953bcc117c207dde3b0047730b7398457787/docs/content/docs/reference/mcp.mdx#L9-L15)), HTTP config에는 arbitrary static header field가 없습니다 ([config source](https://github.com/typeclaw/typeclaw/blob/9439953bcc117c207dde3b0047730b7398457787/src/config/config.ts#L90-L122)). 이미 OAuth-enabled MCP service가 있을 때는 MCP도 가능하지만, project-owned Broker adapter에는 direct plugin의 contract가 더 작습니다.

**Constraint.** Plugin의 `permissions` 선언은 permission string을 등록할 뿐 자동 authorization을 수행하지 않습니다 ([permission registry](https://github.com/typeclaw/typeclaw/blob/9439953bcc117c207dde3b0047730b7398457787/src/permissions/permissions.ts#L102-L122)). `computer-use.control`을 role에 명시적으로 grant하고, `tool.before`에서 `event.origin`과 `ctx.permissions.has(...)`를 검사해 모든 control tool을 fail closed해야 합니다.

### Screenshot size는 별도의 acceptance gate입니다

**Observed fact.** 모든 TypeClaw agent에 auto-loaded되는 `tool-result-cap`은 기본적으로 image result의 base64 string을 262,144 bytes로 제한하고, 초과한 image를 text placeholder로 바꿉니다. 이는 decoded image로 약 190 KiB 수준입니다 ([tool-result-cap contract](https://github.com/typeclaw/typeclaw/blob/9439953bcc117c207dde3b0047730b7398457787/src/bundled-plugins/tool-result-cap/README.md#L1-L18), [defaults](https://github.com/typeclaw/typeclaw/blob/9439953bcc117c207dde3b0047730b7398457787/src/bundled-plugins/tool-result-cap/README.md#L24-L38)).

**Practical consequence.** `1024x768` Windows PNG를 그대로 반환하면 screenshot 대신 placeholder만 모델에 전달될 수 있습니다. PoC는 다음 순서로 처리해야 합니다.

1. Fixed resolution에서 PNG, JPEG 또는 WebP의 실제 크기와 model readability를 측정합니다.
2. 먼저 downscale, adaptive compression, region capture로 bound를 지킵니다.
3. 그래도 부족하면 `computer_observe`의 fully-qualified tool name만 cap exemption에 넣습니다. 전체 cap을 끄면 매 turn transcript와 model context가 커지므로 금지합니다.
4. 원본 frame은 짧은 TTL의 broker-side object로 보관하고, 모델에는 bounded rendition만 보냅니다. 원본 object ID가 임의 file read나 cross-lease access로 이어져서는 안 됩니다.

Screenshot tool이 image를 반환해도 configured model이 image input을 지원하지 않으면 computer-use loop는 성립하지 않습니다. Observed TypeClaw default model은 text와 image input을 선언하지만 ([provider registry](https://github.com/typeclaw/typeclaw/blob/9439953bcc117c207dde3b0047730b7398457787/src/config/providers.ts#L68-L80), [default selection](https://github.com/typeclaw/typeclaw/blob/9439953bcc117c207dde3b0047730b7398457787/src/config/providers.ts#L1338-L1342)), custom profile까지 일반화할 수는 없습니다. Lease acquire 전에 model capability를 확인하고 text-only model이면 tool을 unavailable로 둡니다.

## E2B Desktop에서 가져올 것과 버릴 것

### 실제 E2B architecture

**Observed fact.** E2B Desktop image는 Ubuntu 22.04에 Xorg/Xvfb/Xfce, `xdotool`, `scrot`, `x11vnc`, noVNC/websockify와 desktop application을 설치합니다 ([template](https://github.com/e2b-dev/desktop/blob/89a545e22343aa1c40f28338bf3281a6c04b1d4a/template/template.py#L3-L78)). Runtime은 container가 아니라 pre-booted snapshot에서 resume되는 isolated Firecracker Linux microVM이며 control plane과 sandbox data plane이 분리됩니다 ([E2B architecture](https://github.com/e2b-dev/infra/blob/d73e2b1f51cbd7e4d477452ee152571a9d7d08fd/docs/ARCHITECTURE.md#L13-L28), [runtime path](https://github.com/e2b-dev/infra/blob/d73e2b1f51cbd7e4d477452ee152571a9d7d08fd/docs/ARCHITECTURE.md#L152-L220)).

**Observed fact.** E2B Desktop SDK의 screenshot은 `scrot`로 PNG file을 만든 뒤 E2B filesystem API로 읽고, mouse와 keyboard action은 `xdotool` shell command를 실행합니다 ([screenshot](https://github.com/e2b-dev/E2B/blob/5a56c87e9db0e221b138662805af7743e75f1082/packages/desktop-js/src/sandbox.ts#L241-L274), [input](https://github.com/e2b-dev/E2B/blob/5a56c87e9db0e221b138662805af7743e75f1082/packages/desktop-js/src/sandbox.ts#L276-L458)). Human stream은 guest의 `x11vnc:5900`과 noVNC/websockify `:6080`을 별도로 시작합니다 ([VNC server](https://github.com/e2b-dev/E2B/blob/5a56c87e9db0e221b138662805af7743e75f1082/packages/desktop-js/src/sandbox.ts#L596-L752)).

**Inference.** E2B source의 Linux/X11 implementation은 Windows에 port할 수 없습니다. 대신 다음 contract shape는 재사용할 가치가 있습니다.

- sandbox identity와 model action API를 분리합니다.
- screenshot과 action은 동일한 native-pixel coordinate space를 사용합니다.
- action batch 뒤에 settle/wait하고 새 screenshot으로 effect를 확인합니다.
- agent control path와 사람용 live viewer path를 분리합니다.
- golden image/snapshot에서 새 isolated session을 시작합니다.

E2B의 noVNC `viewOnly` flag는 browser-side `RFB.viewOnly` 설정일 뿐 server authorization이 아닙니다 ([noVNC fork](https://github.com/e2b-dev/noVNC/blob/461b7f1ccb20755037d8995612e5fb08ed16f9e4/app/ui.js#L1732-L1740)). 이 UI option을 TypeClaw authorization boundary로 복사하면 안 됩니다.

## KubeVirt가 제공하는 것과 제공하지 않는 것

### Provisioning과 lifecycle substrate

**Observed fact.** CDI는 HTTP/registry import, existing PVC clone, local image upload를 DataVolume으로 제공하고 raw, qcow2와 ISO를 지원합니다 ([CDI overview](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/storage/containerized_data_importer.md#L1-L17), [formats](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/storage/containerized_data_importer.md#L53-L58)). KubeVirt는 referenced DataVolume의 import나 clone이 끝날 때까지 VMI start를 보류합니다 ([DataVolume behavior](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/storage/disks_and_volumes.md#L530-L609)).

**Observed fact.** Windows는 virtio storage/network driver가 필요하고, KubeVirt는 virtio driver와 QEMU Guest Agent가 들어 있는 containerDisk를 CD-ROM으로 attach하는 경로를 문서화합니다 ([drivers](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/compute/windows_virtio_drivers.md#L6-L29), [installation and containerDisk](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/compute/windows_virtio_drivers.md#L53-L69), [guest tools](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/compute/windows_virtio_drivers.md#L104-L127)). Sysprep answer file은 ConfigMap이나 Secret-backed CD-ROM으로 붙일 수 있고, generalized image는 `/generalize /shutdown /oobe /mode:vm` flow로 만들 수 있습니다 ([Sysprep flow](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/user_workloads/startup_scripts.md#L40-L73), [Secret volume example](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/user_workloads/startup_scripts.md#L847-L903)).

**Observed fact.** QEMU Guest Agent는 `AgentConnected` condition, OS/interface/user/filesystem information과 ping readiness를 제공합니다 ([guest information](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/user_workloads/guest_agent_information.md#L1-L24), [API surface](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/user_workloads/guest_agent_information.md#L63-L77), [probe](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/user_workloads/liveness_and_readiness_probes.md#L277-L294)). 이 API에는 screenshot, mouse, keyboard 또는 UI Automation이 없습니다. 따라서 QEMU Guest Agent를 Windows Computer Agent 대신 쓸 수 없습니다.

### VNC는 production model-control path가 아닙니다

**Observed fact.** KubeVirt graphical console은 `virtctl vnc`/proxy로 접근하며, VNC와 screenshot은 Kubernetes-authenticated subresource입니다 ([user guide](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/user_workloads/accessing_virtual_machines.md#L28-L46), [RBAC](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/user_workloads/accessing_virtual_machines.md#L362-L407)). KubeVirt v1.9.0 source도 `virtualmachineinstances/vnc`와 PNG `virtualmachineinstances/vnc/screenshot` route를 구분하며, 기본 VNC request가 기존 session을 drop할 수 있음을 보여 줍니다 ([routes](https://github.com/kubevirt/kubevirt/blob/v1.9.0/pkg/virt-api/api.go#L351-L369), [handler](https://github.com/kubevirt/kubevirt/blob/v1.9.0/pkg/virt-api/rest/vnc.go#L37-L85)). Built-in `edit` role은 VNC/port-forward를 포함하지만 screenshot을 포함하지 않으므로 PoC screenshot에는 explicit `get` RBAC가 필요합니다 ([v1.9.0 RBAC](https://github.com/kubevirt/kubevirt/blob/v1.9.0/pkg/virt-operator/resource/generate/rbac/cluster.go#L421-L446), [screenshot resource](https://github.com/kubevirt/kubevirt/blob/v1.9.0/pkg/virt-operator/resource/generate/rbac/cluster.go#L82-L85)). Port-forward traffic도 Kubernetes control plane을 통과하며, 고정적인 high-traffic에는 dedicated Service를 쓰라고 문서가 권고합니다 ([access guide](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/user_workloads/accessing_virtual_machines.md#L249-L258), [API-server pressure](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/user_workloads/accessing_virtual_machines.md#L324-L327)).

**PoC option.** 가장 빠른 feasibility spike는 narrow helper가 authenticated `/vnc` WebSocket을 열고 RFB framebuffer, `KeyEvent`, `PointerEvent`를 같은 connection에서 처리하는 방식입니다 ([RFB protocol](https://datatracker.ietf.org/doc/html/rfc6143)). `/vnc/screenshot`은 diagnostic proof로만 사용합니다. VNC로 본 console frame에 RDP session input을 보내면 서로 다른 Windows session/display를 조작할 수 있으므로 screen과 input transport를 섞지 않습니다 ([Microsoft console/session distinction](https://learn.microsoft.com/en-us/windows/win32/termserv/consoles-vs-terminals)). 이 spike는 Kubernetes-authenticated helper이므로 production TypeClaw data path가 아닙니다.

**Recommendation.** `virtctl vnc`와 KubeVirt screenshot은 image bootstrap, administrator debugging, break-glass recovery에만 사용합니다. Production model loop는 guest Computer Agent의 private Service를 사용합니다. 이렇게 해야 TypeClaw plugin이나 high-throughput broker에 Kubernetes credential을 주지 않고, API server를 framebuffer data path로 만들지 않을 수 있습니다.

**Recommendation.** Human viewer는 model control과 별도 capability로 미룹니다. 필요해지면 one-time, short-TTL authorization과 server-enforced view-only를 갖춘 Viewer Gateway를 설계해야 합니다. RDP/VNC `LoadBalancer`를 직접 public exposure하거나 URL query에 reusable password를 넣어서는 안 됩니다.

## 권장 architecture

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

이 구조에서 component별 권한은 다음과 같습니다.

| Component | 가져야 하는 권한 | 가져서는 안 되는 권한 |
|---|---|---|
| TypeClaw Platform Extension | 자신의 TypeClaw permission, Broker 호출, current lease handle | Kubernetes API, VNC subresource, arbitrary VM address, Windows admin credential |
| Sandbox Broker | lease 검증, bounded action forwarding, outcome record | ServiceAccount token, PVC/VM create, Windows shell |
| Sandbox Reconciler | namespace-scoped KubeVirt/CDI/PVC/Service/NetworkPolicy/Secret lifecycle | model prompt, screenshot bytes, arbitrary tool execution |
| Windows Computer Agent | 자신의 interactive desktop에서 allowlisted observe/action | Kubernetes token, arbitrary PowerShell/command execution, another lease의 disk/session |
| Viewer Gateway, 향후 | 별도 viewer grant와 frame stream | agent input 권한; client-side `viewOnly`에 의존하는 authorization |

### Accepted architecture와의 관계

**Observed fact.** ADR 0001은 RemoteSandbox를 Sandbox Broker data plane과 Kubernetes-facing Sandbox Reconciler control plane으로 분리하고, session-scoped Sandbox Lease, default-deny Network Authority, no live Agent Folder, no Kubernetes credential, durable outcome와 unknown-outcome fail-closed를 요구합니다. 이 문서의 component split은 그 방향을 따릅니다.

**Gap.** 같은 ADR의 production certification 문구는 sandbox Pod의 gVisor/Kata-class `RuntimeClass`를 요구합니다. KubeVirt Windows는 hardware VM과 `virt-launcher` infrastructure를 사용하므로 이 문구를 충족한다고 간주할 수 없습니다. 다음 중 하나를 accepted ADR로 결정해야 합니다.

1. `RemoteSandbox` security contract를 provider-neutral requirements와 provider-specific certification profile로 일반화합니다.
2. `WindowsVM`을 별도 Tool Execution Environment로 추가하되 Sandbox Lease와 data/control-plane invariants를 재사용합니다.

이 문서는 1번을 권장하지만 결정하지 않습니다. Certification에는 cluster-wide privileged KubeVirt components, `virt-launcher` Pod Security/admission tuple, KubeVirt/CDI/CRI version, KVM availability, QEMU/libvirt hardening, node eligibility, storage isolation, CNI NetworkPolicy, guest image/protocol digest, cleanup과 failure evidence가 포함되어야 합니다. TypeClaw Restricted Workload의 PSS 결과와 privileged virtualization infrastructure의 결과를 섞거나 재사용해서는 안 됩니다.

**Gap.** 현재 `NetworkSpec.PublicWeb`는 cluster/private/control-plane destination을 허용하지 않습니다 ([current API](../../api/v1alpha1/typeclawinstance_types.go#L93-L109), [ADR 0002](../adr/0002-spiffe-workload-identity-and-credential-execution.md)). Runtime에서 Broker `ClusterIP`로 통신하려면 `Unrestricted`로 넓히거나 PublicWeb를 오용하지 말고, 정확한 Broker identity/port만 허용하는 dedicated platform-capability egress를 Network Authority에 추가해야 합니다.

## Windows Computer Agent

### Service와 interactive user process를 분리합니다

**Observed fact.** Microsoft는 Windows Vista 이후 service가 사용자와 직접 상호작용할 수 없으며, interactive user session의 별도 GUI application과 IPC를 사용하라고 안내합니다 ([Interactive Services](https://learn.microsoft.com/en-us/windows/win32/services/interactive-services)).

**Recommendation.** VM에는 두 process role을 둡니다.

- Windows service는 boot health, SPIRE Agent coordination, session-agent supervision, update/termination 신호만 처리합니다.
- 실제 screenshot, UI Automation과 input injection은 restricted automation user의 interactive session에서 실행되는 Computer Agent가 처리합니다.
- 둘 사이는 ACL이 걸린 named pipe 같은 local IPC를 사용합니다. Service를 `LocalSystem` interactive process로 만들거나 session 0에서 UI를 조작하지 않습니다.

**Assumption requiring proof.** 각 lease에 unlocked interactive desktop을 어떻게 안전하게 만들고 유지할지는 아직 결정되지 않았습니다. PoC는 disposable local automation account의 controlled autologon을 사용할 수 있지만, production은 password storage, rotation, session unlock, crash recovery와 human takeover policy를 별도로 threat-model해야 합니다. Golden image에 shared password를 bake해서는 안 됩니다.

### PoC implementation으로 `winapp ui`를 감쌉니다

**Observed fact.** Microsoft `winapp ui`는 UI Automation을 사용해 WPF, WinForms, Win32, Electron, WinUI 3 application을 inspect/search하고, `invoke`, `set-value`, `wait-for`, screenshot과 input action을 제공합니다 ([overview](https://github.com/microsoft/winappCli/blob/fd7cb6f235fa54dd2c6e26386e65e967a2c8797a/docs/ui-automation.md#L4-L15), [screenshot](https://github.com/microsoft/winappCli/blob/fd7cb6f235fa54dd2c6e26386e65e967a2c8797a/docs/ui-automation.md#L225-L241), [keyboard and values](https://github.com/microsoft/winappCli/blob/fd7cb6f235fa54dd2c6e26386e65e967a2c8797a/docs/ui-automation.md#L399-L452)). Screenshot은 기본적으로 Windows.Graphics.Capture를 사용하고 unavailable하면 PrintWindow로 fallback하며, screen capture mode도 있습니다.

**Observed fact.** Mouse/keyboard injection verb는 unlocked foreground desktop을 요구합니다. Locked workstation, LogonUI/UAC secure desktop에서는 `no_interactive_desktop`으로 실패하고, animation 중 target이 움직이면 `target_moved`로 거절할 수 있습니다. UIA pattern verb는 가능한 경우 real input injection보다 안전합니다 ([interactive requirement](https://github.com/microsoft/winappCli/blob/fd7cb6f235fa54dd2c6e26386e65e967a2c8797a/docs/ui-automation.md#L11-L15), [click/drag safety](https://github.com/microsoft/winappCli/blob/fd7cb6f235fa54dd2c6e26386e65e967a2c8797a/docs/ui-automation.md#L304-L337)). Windows `SendInput` 자체도 UIPI 제약을 받습니다 ([Microsoft API](https://learn.microsoft.com/en-us/windows/win32/api/winuser/nf-winuser-sendinput)).

**Recommendation.** PoC Computer Agent는 arbitrary command line을 받아 실행하지 않습니다. Stable RPC action을 allowlisted `winapp ui --json` command로 compile하고, timeout, output schema, target process/window, image size를 검증합니다. UIA `invoke`/`set-value`를 우선하고 pixel click/type은 fallback으로 둡니다.

**Production gate.** `winapp`은 Public Preview입니다. Golden image에는 actual `v0.5.0` release artifact의 version, SHA-256, signature를 pin해야 하고, pinned release documentation과 installed binary의 verb/error behavior가 같은지 conformance test로 증명해야 합니다. External contract는 project-owned versioned RPC여야 하므로 향후 native UIA/WGC agent로 교체해도 TypeClaw tool contract가 바뀌지 않아야 합니다.

## Tool과 wire protocol contract

### Model-facing tools

권장 model-facing surface는 두 tool뿐입니다. TypeClaw `ToolContext.sessionId`를 Broker의 Sandbox Lease에 lazy-bind하면 model이 lease ID, VM ID, TTL, persistence나 release semantics를 조작할 이유가 없습니다.

| Tool | 주요 input | result |
|---|---|---|
| `computer_observe` | optional window/region과 bounded rendition preference | model-visible text metadata + bounded image content |
| `computer_act` | `expectedFrameId`, bounded ordered `actions[]`, deadline | model-visible terminal outcome, completed action index, warnings와 새 frame metadata |

첫 `computer_observe`에서 plugin은 자신의 `sessionId`로 Broker의 internal `ensureSession`을 호출합니다. Broker는 policy가 허용하면 lease를 만들거나 기존 session binding을 반환합니다. `computer_act`도 같은 binding만 사용할 수 있으므로 model-supplied identifier로 다른 VM을 선택할 수 없습니다. Session end/idle TTL이 release를 시작하고, reset/delete/retain은 policy 또는 administrator control-plane operation으로 분리합니다. Destructive desktop reset을 ordinary model tool로 두지 않습니다.

TypeClaw의 `ToolResult.details`와 MCP `structuredContent`를 model-visible contract로 사용하면 안 됩니다. `frameId`, width, height, DPI, screen revision, active window와 bounded UIA summary는 image 앞의 **text content에도 반드시 포함**해야 합니다. `details`에는 adapter diagnostics를 중복 기록할 수 있지만 model action이 그것에 의존해서는 안 됩니다.

첫 PoC의 action allowlist는 이 정도면 충분합니다.

- `uia.invoke`, `uia.setValue`, `uia.waitFor`.
- `mouse.move`, `mouse.click`, `mouse.drag`, `mouse.scroll`.
- `keyboard.key`, `keyboard.chord`, `keyboard.type`.
- `wait` with a strict maximum.

Clipboard, arbitrary process launch, PowerShell, file upload/download, microphone, camera, USB, multi-monitor, registry와 Windows setting 변경은 첫 PoC에서 제외합니다. Application launch가 꼭 필요하면 administrator-configured allowlist의 application ID만 별도 action으로 추가합니다.

Model이 plugin에 보내는 예시 argument는 다음과 같습니다.

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

Plugin은 이 call을 Broker에 보낼 때 bound `leaseId`와 새 `invocationId`를 내부적으로 추가합니다. Pixel fallback은 fixed native coordinate를 사용하며 `screenRevision`, width, height, DPI를 model-visible text metadata에 넣습니다. `expectedFrameId`는 stale-frame action을 줄이는 optimistic fence이지, 비동기 UI에 대한 exact serialization 보장은 아닙니다.

### Outcome와 retry

Wire protocol은 최소한 다음 terminal outcome을 구분해야 합니다.

| Outcome | 의미 | Caller behavior |
|---|---|---|
| `Succeeded` | Guest가 action과 bounded verification을 완료함 | 새 frame을 observe하고 진행 |
| `Rejected` | lease/frame/action/policy가 dispatch 전에 거절됨 | request를 수정할 수 있음 |
| `FailedBeforeDispatch` | guest에 side effect를 보내기 전에 실패함 | 같은 logical intent를 새 invocation으로 재평가 가능 |
| `CancelledBeforeDispatch` | TypeClaw `AbortSignal`이 dispatch 전에 취소함 | side effect 없음 |
| `UnknownOutcome` | dispatch 뒤 connection loss, timeout, guest crash 또는 cancellation이 발생함 | 동일 click/type을 자동 replay하지 말고 re-observe와 사용자/policy 결정을 요구 |

`invocationId`는 guest가 받은 exact duplicate를 deduplicate하는 데 사용합니다. 하지만 crash가 side effect와 durable record 사이에 발생할 수 있으므로 exactly-once를 주장하지 않습니다. 이는 ADR 0001/0002의 unknown-outcome와 no-replay 원칙을 Windows UI action에도 적용한 것입니다.

### Wire-level requirements

- Protocol은 versioned HTTPS/HTTP2, Connect 또는 gRPC 중 하나로 구현할 수 있지만, schema와 error enum은 transport와 독립적으로 versioning합니다.
- Broker와 guest는 SPIFFE X.509-SVID 기반 mTLS를 production minimum으로 사용합니다.
- 모든 request는 lease ID, invocation ID, deadline, maximum action count와 maximum response bytes를 가집니다.
- Broker는 expected VM SPIFFE ID, Sandbox Lease, Security Epoch와 guest image/protocol digest를 함께 검증합니다.
- Screenshot, UI tree와 error output은 bounded합니다. Guest stdout/stderr나 `winapp` raw output을 그대로 model transcript에 넣지 않습니다.
- Health와 `Capabilities`는 `winapp`/agent version, supported verbs, display geometry, active session state를 보고하지만 secret이나 Windows username을 불필요하게 노출하지 않습니다.

## Golden image와 per-lease bootstrap

### Cluster prerequisites

KubeVirt와 CDI는 TypeClaw operator가 자동 설치할 application dependency가 아니라 cluster-admin이 설치하고 certify하는 infrastructure로 취급해야 합니다. 최소 prerequisite는 다음과 같습니다.

- Worker BIOS/nested virtualization과 `/dev/kvm` availability.
- KubeVirt와 compatible CDI version.
- ISO import를 위한 scratch-capable StorageClass.
- golden PVC clone 또는 CSI VolumeSnapshot/clone을 지원하는 storage.
- VMI pod에 실제로 적용되는 CNI NetworkPolicy.
- Windows VM resource quota와 충분한 node capacity.

Talos v1.13의 공식 guide도 BIOS virtualization, CDI scratch storage, 그리고 live migration을 사용할 경우 shared storage를 별도 prerequisite로 둡니다 ([Talos KubeVirt guide](https://docs.siderolabs.com/talos/v1.13/advanced-guides/install-kubevirt)). 따라서 Talos 자체는 blocker가 아니지만, ADR 0006의 homelab target에서 이 항목은 canary로 실측해야 합니다.

### Image build sequence

1. Customer-supplied, licensed Windows ISO를 CDI DataVolume로 upload/import합니다. Operator repository나 public registry가 Windows golden disk를 재배포하지 않습니다.
2. Blank OS DataVolume, Windows ISO, digest-pinned virtio-win containerDisk, Secret-backed Sysprep media를 attach합니다.
3. Image-builder VM은 `RerunOnFailure`를 사용합니다. KubeVirt에서 `Always`는 정상 guest shutdown도 respawn하지만 `RerunOnFailure`는 controlled shutdown을 존중합니다 ([run strategies](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/compute/run_strategies.md#L14-L49)).
4. Unattended setup은 matching storage/network driver, QEMU Guest Agent, signed Computer Agent, pinned `winapp`, SPIRE Agent, browser/application과 fixed display/DPI setting을 설치합니다.
5. Computer Agent protocol conformance, Defender scan, signature/hash, OS patch level과 clean-shutdown을 검증합니다.
6. `sysprep /generalize /oobe /shutdown /mode:vm`으로 seal합니다. KubeVirt official sample은 driver와 Guest Agent install 뒤 이 flow를 보여 줍니다 ([post-install example](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/user_workloads/startup_scripts.md#L1062-L1080)).
7. Sealed PVC 또는 VolumeSnapshot을 golden source로 publish하고 Windows build, update level, virtio image digest, Computer Agent digest, `winapp` version/hash, protocol version과 Security Epoch metadata를 기록합니다.

KubeVirt sample의 `<EnableLUA>false>`와 plaintext Administrator password는 production template로 복사하면 안 됩니다 ([insecure sample lines](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/user_workloads/startup_scripts.md#L1000-L1045)). UAC는 유지하고, image build에 필요한 elevation은 model session 전에 끝냅니다.

KubeVirt Tekton task repository에는 EFI Windows installer와 customization pipeline 예제가 있으므로 manual bootstrap이 안정되면 image pipeline의 출발점으로 사용할 수 있습니다 ([official task guide](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/cluster_admin/tekton_tasks.md#L45-L57)).

### Per-lease clone

Sandbox Reconciler는 lease마다 다음 resource를 만듭니다.

1. Golden source에서 새 OS DataVolume/PVC를 clone합니다.
2. Fresh VM name, lease labels, one automation user/session, ephemeral identity bootstrap material을 생성합니다.
3. `Manual` run strategy VM, private ClusterIP Service, default-deny ingress/egress NetworkPolicy와 resource quota를 만듭니다. KubeVirt는 VMI label을 launcher Pod에 전달하므로 Service와 NetworkPolicy selector로 사용할 수 있습니다 ([Service](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/network/service_objects.md#L1-L12), [ClusterIP example](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/network/service_objects.md#L15-L82), [NetworkPolicy](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/network/networkpolicy.md#L1-L18)).
4. VM을 explicit start하고 multi-signal readiness를 기다립니다.
5. Release에서는 먼저 action authority와 network grant를 revoke하고 VM을 stop한 뒤 Service, bootstrap Secret과 disposable PVC를 cleanup합니다. Durable outcome 기록은 cleanup과 독립적으로 남깁니다.

Default persistence는 `Disposable`이어야 합니다. `RetainedDesktop`이 필요하면 disk를 같은 TypeClaw Instance에 영구 귀속하고 다른 tenant/lease에 재할당하지 않습니다. 중단된 VM의 PVC를 보존하는 것은 E2B memory snapshot resume와 같은 semantics가 아니며 startup latency를 다시 측정해야 합니다.

KubeVirt ephemeral volume은 VM stop 시 local COW를 버리지만 backing PVC와 node-local behavior에 제약이 있습니다 ([ephemeral volumes](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/storage/disks_and_volumes.md#L655-L669)). 첫 implementation은 lifecycle이 명확한 per-lease DataVolume clone을 권장합니다.

### Readiness state machine

`VMI Running`만으로 lease를 Active로 바꾸면 안 됩니다. 권장 state는 다음과 같습니다.

```text
Requested → Provisioning → Booting → GuestAgentConnected
          → InteractiveDesktopReady → ComputerAgentReady → Active
          → Revoking → Stopping → Cleaning → Released
                                  ↘ Failed
```

`Active` 조건은 모두 충족해야 합니다.

- VMI가 Running이고 expected golden-image identity를 사용합니다.
- QEMU `AgentConnected`와 guest ping이 성공합니다.
- Windows interactive session이 unlocked이며 fixed display geometry가 적용됐습니다.
- Computer Agent가 expected SPIFFE identity로 mTLS handshake하고 protocol/capability version이 맞습니다.
- Test observe가 bounded image를 반환하고 no-op/UIA health action이 성공합니다.

Windows update처럼 Guest Agent가 잠시 사라지는 작업 중 liveness probe가 launcher Pod를 재시작하면 VM 자체가 파괴될 수 있습니다. KubeVirt도 long Windows update에서 probe pause가 필요하다고 명시합니다 ([probe maintenance warning](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/user_workloads/liveness_and_readiness_probes.md#L371-L392)). Production은 in-session uncontrolled update 대신 golden image rebuild와 maintenance policy를 사용해야 합니다.

## SPIFFE identity 후보

**Observed fact.** SPIRE Agent v1.15.3는 Windows service로 실행할 수 있고 Windows에서 Workload API named pipe를 제공합니다. Join token file과 bootstrap trust bundle path도 구성할 수 있습니다 ([agent configuration](https://github.com/spiffe/spire/blob/v1.15.3/doc/spire_agent.md#L53-L87), [Windows service](https://github.com/spiffe/spire/blob/v1.15.3/doc/spire_agent.md#L324-L335)). `windows` workload attestor는 caller process access token에서 user/group SID를 만들며, 선택적으로 executable path와 SHA-256 selector를 제공합니다 ([Windows workload attestor](https://github.com/spiffe/spire/blob/v1.15.3/doc/plugin_agent_workloadattestor_windows.md#L1-L28)).

**Candidate design, not an established fact.** Golden image에 private key, node identity나 reusable join token을 bake하지 않습니다. Reconciler가 lease별 one-use, 최대 600초 bootstrap material과 trust bundle을 Secret-backed setup media로 주입하고, explicit expected agent ID로 registration한 뒤 Windows service의 SPIRE Agent가 attestation하도록 prototype합니다. User-session Computer Agent는 named-pipe Workload API에서 X.509-SVID를 받아 Broker와 mTLS를 맺습니다. Registration은 attested VM parent identity와 signed agent path/hash 및 restricted user SID를 bind해야 합니다.

SPIRE `join_token`은 one-time pre-shared key입니다 ([join-token attestor](https://github.com/spiffe/spire/blob/v1.15.3/doc/plugin_agent_nodeattestor_jointoken.md#L1-L8)). 그 token의 possession은 특정 KubeVirt VMI에서 실행 중이라는 증거가 아닙니다. 따라서 join token만으로 production VMI identity를 주장할 수 없고, KubeVirt guest용 node-attestation profile과 explicit VMI/lease binding은 새 ADR와 certification에서 해결해야 합니다.

다음 항목은 반드시 실증해야 합니다.

- KubeVirt Windows guest에서 선택한 node attestor와 SPIRE Server route가 정상 동작하는가.
- Sysprep/clone 뒤 달라지는 machine/user SID를 registration에 어떻게 bind하는가.
- Interactive user process가 named pipe를 통해 기대한 selectors와 X.509-SVID를 받는가.
- Higher-integrity process attestation에 필요한 Windows privilege가 최소화되는가.
- Join material이 한 번만 소비되고 Secret, setup media, guest file와 logs에서 제거되는가.
- Kubernetes Secret 삭제가 이미 생성된 setup ISO나 guest cache의 secure erasure를 보장하지 않으므로, token consumption 뒤 guest file 삭제, media detach, token revoke와 lease disk destruction을 각각 검증하는가.
- Lease release 시 SPIRE node/registration entry와 issued authority를 revoke하고, stale agent ID가 reconnect하지 못하는가.
- Guest clone이나 stolen disk가 다른 Sandbox Lease에서 재-attest할 수 없는가.

이 검증 전에는 “SPIRE를 설치했으므로 guest identity가 해결됐다”고 기록하면 안 됩니다. PoC 초기는 short-lived, per-lease test certificate를 쓸 수 있지만 production 승격에는 ADR 0002의 SPIFFE identity와 no-fallback contract가 적용됩니다.

## Security model

### Network와 Kubernetes authority

- TypeClaw plugin, Broker, Windows guest에는 ServiceAccount token이나 kubeconfig를 넣지 않습니다.
- Sandbox Reconciler는 dedicated ServiceAccount와 namespace/provider-specific minimal RBAC를 사용합니다. Current manager RBAC에 KubeVirt cluster-wide authority를 단순 추가하기보다 separate deployment/identity가 blast radius를 줄입니다.
- VMI에는 default-deny NetworkPolicy를 적용하고 Broker→Computer Agent, guest→SPIRE와 administrator-approved public destinations만 허용합니다. KubeVirt도 VMI가 기본적으로 다른 endpoint에서 접근 가능하므로 NetworkPolicy가 필요하다고 명시합니다 ([KubeVirt NetworkPolicy](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/network/networkpolicy.md#L1-L18)).
- Domain allowlist, DNS rebinding, redirect와 CONNECT/SNI enforcement는 NetworkPolicy가 아니라 ADR 0002의 Network Authority가 담당합니다. Adapter가 없거나 inexact하면 networked capability를 unavailable로 둡니다.
- RDP 3389, KubeVirt VNC와 Computer Agent port를 NodePort/LoadBalancer로 public expose하지 않습니다.

### Action과 desktop authority

- 한 Active Sandbox Lease만 한 Windows interactive desktop의 input authority를 가집니다.
- Human takeover가 필요하면 agent input grant를 먼저 revoke합니다. Concurrent agent/human input은 지원하지 않습니다.
- Lock workstation, UAC secure desktop, Ctrl+Alt+Delete/SAS와 elevation prompt는 `Unavailable`로 실패시킵니다. UAC disable이나 secure desktop automation fallback을 두지 않습니다.
- UIA selector를 pixel coordinate보다 우선하지만, selector와 active window도 action 직전에 re-resolve합니다.
- Fixed resolution/DPI를 Security Epoch-compatible image contract로 다루고 drift가 생기면 기존 frame을 거절합니다.
- Guest RPC에는 raw `cmd.exe`, PowerShell, arbitrary binary path나 arbitrary `winapp` arguments를 노출하지 않습니다.

### Data와 credential

- Screenshot과 UI tree에는 password, token, personal data가 들어갈 수 있습니다. Raw frame은 짧은 TTL, encryption at rest, lease-scoped access, bounded audit metadata와 explicit retention policy가 필요합니다.
- Agent Folder를 VM에 mount하지 않습니다. 향후 file transfer가 필요하면 ADR 0001의 Authorized Workspace View와 validated output delta를 별도 typed capability로 구현합니다.
- 첫 PoC는 real account credential을 사용하지 않습니다.
- 이후 model이 password를 보고 직접 type하는 방식은 Opaque Credential Use가 아닙니다. Raw Credential Disclosure로 분류하고 confirmation/audit를 적용하거나, approved Credential Consumer가 guest의 정확한 field에 credential을 주입하되 bytes를 model/plugin에 반환하지 않는 별도 protocol이 필요합니다.
- Clipboard와 browser password manager는 default off입니다. 활성화하면 별도의 Credential Consumer와 destination allowlist가 필요합니다.

### Supply chain, Windows licensing과 image state

- Platform Extension, Computer Agent, `winapp`, virtio image와 golden disk metadata를 version/digest-pin하고 Security Epoch에 포함합니다.
- `winapp` development certificate나 Developer Mode를 production guest에 남기지 않습니다. Project-owned bridge는 production signing identity로 서명합니다.
- Windows 11 virtual desktop 권리는 edition, user/device와 access model에 따라 달라지므로 Microsoft licensing guidance를 deployment gate로 검토해야 합니다 ([Microsoft licensing guidance](https://www.microsoft.com/licensing/guidance/Windows-11-Licensing-for-Virtual-Desktops)). Evaluation media는 PoC 범위에서만 Microsoft의 terms에 맞게 사용합니다 ([Windows 11 Enterprise evaluation](https://www.microsoft.com/en-us/evalcenter/evaluate-windows-11-enterprise)). 이 문서는 법률 자문이나 redistribution 권한을 제공하지 않습니다.
- Windows 11 installer는 TPM device를 요구할 수 있지만 persistent vTPM은 필수가 아닙니다. KubeVirt의 persistent TPM/EFI backend-state snapshot은 같은 VM restore만 지원하며 다른 VM clone은 지원하지 않습니다 ([persistent state](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/compute/persistent_tpm_and_uefi_state.md#L1-L33), [TPM notes](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/compute/persistent_tpm_and_uefi_state.md#L35-L76)). Cloneable golden baseline에서는 persistent backend state를 피하고, BitLocker나 persistent Secure Boot state가 필요하면 per-VM provisioning을 별도 검증합니다.

## 현재 repository와의 gap

**Observed fact.** 2026-08-30 repository scan에서 API kind는 `TypeClawInstance`, `CredentialRequest`, `CredentialApproval`뿐입니다. Main instance reconciler는 StatefulSet, Service와 relay RBAC를 만들고 ([controller](../../internal/controller/typeclawinstance_controller.go#L43-L138)), 별도 controller가 NetworkPolicy를 관리합니다 ([network controller](../../internal/controller/networkpolicy_controller.go#L34-L100)). KubeVirt, CDI, Windows, VNC, RDP dependency나 resource controller는 없습니다.

따라서 production implementation에는 최소한 다음 work package가 새로 필요합니다.

- Generic Sandbox Lease API와 durable per-invocation outcome model.
- Credentialless Sandbox Broker API/Deployment와 TypeClaw Platform Extension.
- KubeVirt Windows Sandbox Reconciler, dedicated RBAC와 provider status.
- DataVolume clone, VM, Service, NetworkPolicy, bootstrap Secret, PVC cleanup/finalizer lifecycle.
- Windows golden-image pipeline와 signed Computer Agent artifact.
- SPIFFE Windows guest identity and registration lifecycle.
- Network Authority에 exact Broker/guest capability path.
- Image/result bounds, unknown-outcome, cancellation, no-replay와 cleanup failure injection tests.
- KubeVirt/CDI/storage/CNI/Talos canary와 provider certification suite.

이 scope를 기존 `TypeClawInstanceReconciler`에 직접 끼워 넣기 전에 ADR과 API ownership을 결정해야 합니다. 특히 generic `RemoteSandbox` contract를 generalize할지 Windows-specific Tool Execution Environment를 만들지, `SandboxLease`를 CRD로 expose할지 internal reconciler record로 둘지는 사람의 결정입니다.

## Failure modes와 fail-closed behavior

| Failure | Detection | Required behavior |
|---|---|---|
| `/dev/kvm`, CDI scratch, clone/snapshot 또는 NetworkPolicy 지원이 없음 | Provider canary와 StorageClass/CNI probe | Provider `Unavailable`; 다른 isolation으로 silent fallback 금지 |
| ISO import/DataVolume clone이 지연·실패 | CDI/DataVolume condition과 deadline | Lease `ProvisioningFailed`; VM start 금지; partial PVC cleanup |
| VMI는 Running이지만 Windows OOBE/session이 준비되지 않음 | QEMU agent + Computer Agent + observe readiness | Lease를 Active로 만들지 않음; bounded timeout 후 failure |
| QEMU Guest Agent만 연결됨 | Computer Agent mTLS health 불일치 | Lifecycle metadata만 ready로 보고 tool은 unavailable |
| Computer Agent만 연결되고 expected image/protocol이 아님 | SVID, Security Epoch, capability digest mismatch | Connection reject, VM quarantine/cleanup |
| Desktop lock, LogonUI, UAC secure desktop | Agent session-state와 `no_interactive_desktop` | Action fail closed; UAC disable이나 VNC fallback 금지 |
| Window animation, DPI/resolution 변화, stale screenshot | `frameId`, display revision, target re-resolution | `StaleFrame`/`target_moved`; re-observe 요청 |
| Click/type dispatch 후 response loss | Invocation journal은 dispatch를 기록했지만 completion 없음 | `UnknownOutcome`; same action automatic retry 금지 |
| TypeClaw cancellation이 dispatch 뒤 도착 | Guest cancellation acknowledgement 없음 | Remaining action revoke; current action은 `UnknownOutcome` 가능 |
| Windows update/reboot와 liveness restart loop | maintenance state, probe/status transition | Lease drain; probe pause/bounded maintenance; image rebuild 정책 |
| Human VNC/RDP와 agent가 같은 desktop을 조작 | input-owner lease와 session event | Agent grant revoke 또는 viewer-only; concurrent input 금지 |
| Screenshot이 TypeClaw cap을 초과 | `tool-result-cap` marker와 byte metric | bounded re-encode/region retry; blank success로 취급 금지 |
| `winapp` release behavior drift | Golden-image conformance suite | Image promotion block; arbitrary latest install 금지 |
| Snapshot이 crash-consistent이거나 backend TPM/EFI state를 포함 | Snapshot indication과 volume inventory | Golden promotion/clone block; offline/quiesced rebuild |
| Release 후 VM/PVC/Secret/Service가 남음 | Finalizer/GC audit와 TTL sweeper | Authority는 이미 revoke한 채 cleanup 독립 재시도; durable leak status |
| Disposable disk가 재사용되어 cross-lease data가 남음 | owner/lease UID and storage provenance check | Attach reject, sanitize가 아니라 delete-by-default |
| License/activation 조건 불명확 | Image provenance/licensing review | Image publish와 production lease block |

Online VM snapshot은 QEMU Guest Agent가 있으면 filesystem freeze를 시도하지만, 없거나 실패하면 crash-consistent일 수 있습니다. Snapshot status는 이를 indication으로 표시하며 restore target은 stopped 상태여야 합니다 ([snapshot consistency](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/storage/snapshot_restore_api.md#L30-L38), [indications](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/storage/snapshot_restore_api.md#L86-L99)). Golden image publication은 offline clean shutdown을 기본으로 해야 합니다.

## Phased PoC

### Phase 0 — architecture와 environment gate

- New ADR에서 KubeVirt provider와 ADR 0001의 관계, Sandbox Lease ownership, production certification criteria를 결정합니다.
- Windows licensing과 ISO provenance를 승인합니다.
- Talos node의 virtualization, KubeVirt/CDI version, scratch/clone storage, CNI NetworkPolicy를 canary로 검증합니다.
- 첫 target은 `amd64`, single display, disposable disk, **plugin/guest의 Kubernetes credential 없음**, **real external account credential 없음**, no public viewer로 고정합니다.

Exit criterion: cluster-admin prerequisites와 security exceptions가 명시되고 silent fallback이 없습니다.

### Phase 1 — manually managed Windows VM

- CDI + Sysprep + virtio/QEMU Guest Agent로 한 개의 Windows golden VM을 만듭니다.
- Fixed `1024x768` 또는 선택한 해상도와 DPI를 설정합니다.
- Administrator가 `virtctl vnc`와 KubeVirt screenshot으로 bootstrap을 확인합니다.
- Notepad, Calculator, browser 같은 deterministic test application을 설치합니다.

Exit criterion: 10회 clean clone/boot에서 동일한 display/session readiness와 no shared machine credential을 확인합니다.

### Phase 2 — Computer Agent와 development plugin

- Static VM에 private test certificate를 넣고, Computer Agent가 pinned `winapp ui` allowlist만 실행하게 합니다.
- Development-only TypeClaw plugin으로 `observe`, `act`를 노출합니다. KubeVirt lifecycle은 아직 수동이며 plugin에는 kubeconfig가 없습니다.
- UIA-first와 pixel fallback task를 각각 수행합니다.
- Screenshot compression/cap behavior를 측정하고 model이 실제 frame을 받는지 검증합니다.

Example tasks:

1. Notepad에 지정 text를 입력하고 Save dialog를 열되 실제 file write 직전에 멈춥니다.
2. Calculator에서 UIA와 pixel path로 같은 계산을 수행하고 결과를 inspect합니다.
3. Browser의 local test page에서 checkbox/form/navigation을 수행합니다.
4. Lock screen과 UAC prompt에서 action이 실패하는지 확인합니다.

Exit criterion: 100 consecutive observe/action/observe loops, stale-frame rejection, bounded output, cancellation과 post-dispatch connection-loss `UnknownOutcome` test가 통과합니다.

### Phase 3 — Broker, Reconciler와 Sandbox Lease

- TypeClaw Platform Extension → credentialless Broker → separate Sandbox Reconciler path를 구현합니다.
- Lease마다 DataVolume clone, VM, ClusterIP Service, NetworkPolicy, Secret, TTL, release cleanup을 자동화합니다.
- Runtime NetworkPolicy에 exact Broker capability route만 추가합니다.
- No ServiceAccount token in runtime, Broker, guest를 admission/E2E에서 검사합니다.

Exit criterion: 50 concurrent/sequential disposable leases에서 cross-lease network/data access가 없고, crash 뒤 zombie resource가 TTL 내 cleanup됩니다.

### Phase 4 — SPIFFE와 hardening

- SPIRE Windows service, Windows workload attestor, one-shot bootstrap, per-lease registration과 X.509-SVID rotation을 검증합니다.
- Platform Extension과 guest artifacts를 signed/digest-pinned Platform Bundle/golden image로 release합니다.
- Network Authority, resource quotas, screenshot retention/redaction, security audit와 failure injection을 추가합니다.
- KubeVirt provider canary와 Security Epoch invalidation을 구현합니다.

Exit criterion: identity replay, stolen clone, stale Security Epoch, CNI failure, SPIRE outage와 cleanup failure가 모두 fail closed이며 new ADR의 certification suite가 통과합니다.

### Phase 5 — optional product capabilities

- Human viewer gateway, server-enforced view-only와 agent takeover handoff.
- Authorized Workspace View 기반 file transfer.
- Approved credential-field injection.
- Retained desktop, backup/restore와 measured restart SLO.

이 기능들은 core computer-use success와 security boundary가 증명되기 전에는 PoC scope에 넣지 않습니다.

## Production acceptance checklist

- [ ] Provider-specific ADR와 threat model이 accepted 상태입니다.
- [ ] KubeVirt/CDI/CNI/storage/Windows/virtio/Computer Agent version matrix가 pinned되어 있습니다.
- [ ] TypeClaw Runtime, Platform Extension, Broker와 guest에 Kubernetes credential이 없습니다.
- [ ] Plugin은 signed Platform Bundle이며 mutable Agent Folder에서 load되지 않습니다.
- [ ] Exact Broker route 외 cluster/private egress가 차단됩니다.
- [ ] One lease가 one desktop input authority와 one disk owner에 bind됩니다.
- [ ] VMI Running, QEMU AgentConnected, interactive desktop, Computer Agent mTLS를 모두 readiness에 사용합니다.
- [ ] `UnknownOutcome` 뒤 click/type이 replay되지 않습니다.
- [ ] Lock/UAC/secure desktop에서 fail closed하고 UAC를 끄지 않습니다.
- [ ] Screenshot이 실제 model input에 들어가며 byte/context/retention bound를 지킵니다.
- [ ] Golden image에 reusable password, join token, private key, kubeconfig가 없습니다.
- [ ] Disposable release 뒤 VM/PVC/Service/Secret와 guest identity가 제거됩니다.
- [ ] Windows licensing, activation, update와 redistribution policy가 승인됐습니다.
- [ ] Startup time, action latency와 screenshot quality SLO는 E2B 수치를 가정하지 않고 실제 cluster에서 측정됐습니다.

## 남은 결정

1. KubeVirt를 generic RemoteSandbox provider로 generalize할지, 별도 Tool Execution Environment로 모델링할지 결정해야 합니다.
2. Sandbox Lease를 public namespaced CRD로 만들지, Broker/Reconciler 전용 API record로 둘지 결정해야 합니다.
3. Disposable desktop만 먼저 제공할지, TypeClaw Instance에 귀속된 retained desktop도 v1에 넣을지 결정해야 합니다.
4. Interactive Windows session을 production에서 어떻게 만들고 unlock 상태를 유지할지 결정해야 합니다.
5. Model-facing screenshot의 해상도, encoding, cap exemption과 retention을 결정해야 합니다.
6. `winapp`을 certification된 implementation으로 유지할지, native Windows Computer Agent로 교체할지 결정해야 합니다.
7. Human viewer와 agent takeover를 core scope와 분리할지 결정해야 합니다.
8. Credential-bearing UI automation을 Raw Credential Disclosure로만 제공할지, field-specific Opaque Credential Use protocol을 만들지 결정해야 합니다.

## Primary source index

- TypeClaw: [plugin trust model](https://github.com/typeclaw/typeclaw/blob/9439953bcc117c207dde3b0047730b7398457787/docs/content/docs/concepts/plugins-and-stages.mdx#L7-L17), [Plugin API](https://github.com/typeclaw/typeclaw/blob/9439953bcc117c207dde3b0047730b7398457787/docs/content/docs/reference/plugin-api.mdx#L75-L101), [tool result cap](https://github.com/typeclaw/typeclaw/blob/9439953bcc117c207dde3b0047730b7398457787/src/bundled-plugins/tool-result-cap/README.md#L1-L38), [managed runtime](https://github.com/fml09/typeclaw/blob/c95fede9cbf54598179b2c00723507207039ea29/docs/content/docs/internals/managed-runtime.mdx#L7-L39), [platform ownership](https://github.com/fml09/typeclaw/blob/c95fede9cbf54598179b2c00723507207039ea29/docs/content/docs/internals/managed-runtime.mdx#L177-L183).
- E2B: [Desktop template](https://github.com/e2b-dev/desktop/blob/89a545e22343aa1c40f28338bf3281a6c04b1d4a/template/template.py#L3-L78), [Desktop SDK](https://github.com/e2b-dev/E2B/blob/5a56c87e9db0e221b138662805af7743e75f1082/packages/desktop-js/src/sandbox.ts#L241-L458), [VNC server](https://github.com/e2b-dev/E2B/blob/5a56c87e9db0e221b138662805af7743e75f1082/packages/desktop-js/src/sandbox.ts#L596-L752), [infrastructure architecture](https://github.com/e2b-dev/infra/blob/d73e2b1f51cbd7e4d477452ee152571a9d7d08fd/docs/ARCHITECTURE.md#L13-L28).
- KubeVirt: [`v1.9.0` release](https://github.com/kubevirt/kubevirt/releases/tag/v1.9.0), [Windows virtio drivers](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/compute/windows_virtio_drivers.md#L1-L127), [Sysprep](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/user_workloads/startup_scripts.md#L40-L73), [CDI](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/storage/containerized_data_importer.md#L1-L58), [VM access](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/user_workloads/accessing_virtual_machines.md#L28-L46), [Service](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/network/service_objects.md#L1-L82), [NetworkPolicy](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/network/networkpolicy.md#L1-L28), [snapshot/restore](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/storage/snapshot_restore_api.md#L1-L121), [KubeVirt VNC handler](https://github.com/kubevirt/kubevirt/blob/v1.9.0/pkg/virt-api/rest/vnc.go#L37-L85).
- Microsoft Windows: [`winapp v0.5.0`](https://github.com/microsoft/winappCli/releases/tag/v0.5.0), [`winapp ui` automation](https://github.com/microsoft/winappCli/blob/fd7cb6f235fa54dd2c6e26386e65e967a2c8797a/docs/ui-automation.md#L4-L15), [capture](https://github.com/microsoft/winappCli/blob/fd7cb6f235fa54dd2c6e26386e65e967a2c8797a/docs/ui-automation.md#L225-L241), [input](https://github.com/microsoft/winappCli/blob/fd7cb6f235fa54dd2c6e26386e65e967a2c8797a/docs/ui-automation.md#L304-L452), [Interactive Services](https://learn.microsoft.com/en-us/windows/win32/services/interactive-services), [`SendInput`](https://learn.microsoft.com/en-us/windows/win32/api/winuser/nf-winuser-sendinput), [virtual desktop licensing](https://www.microsoft.com/licensing/guidance/Windows-11-Licensing-for-Virtual-Desktops).
- SPIFFE/SPIRE: [SPIRE Agent configuration](https://github.com/spiffe/spire/blob/v1.15.3/doc/spire_agent.md#L53-L87), [Windows service](https://github.com/spiffe/spire/blob/v1.15.3/doc/spire_agent.md#L324-L335), [Windows workload attestor](https://github.com/spiffe/spire/blob/v1.15.3/doc/plugin_agent_workloadattestor_windows.md#L1-L64), [join token](https://github.com/spiffe/spire/blob/v1.15.3/doc/plugin_agent_nodeattestor_jointoken.md#L1-L8), [`v1.15.3` release](https://github.com/spiffe/spire/releases/tag/v1.15.3).
- Target environment: [Talos v1.13 KubeVirt installation guide](https://docs.siderolabs.com/talos/v1.13/advanced-guides/install-kubevirt).
