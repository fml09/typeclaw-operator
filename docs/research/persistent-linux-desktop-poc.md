# Persistent Linux desktop PoC on KubeVirt

Status: experimental feasibility and decision memo; not an accepted design
Observed: 2026-08-30

## 결론

가능합니다. 첫 PoC는 Ubuntu/Xfce VM 하나를 사용자마다 영속적으로 보유하게 하고, KubeVirt native VNC를 하나의 `Desktop Gateway`가 relay하는 구조가 가장 작습니다.

핵심 결정은 다음과 같습니다.

1. 이 desktop은 chat session마다 폐기하는 `Sandbox Lease`가 아닙니다. 사용자와 TypeClaw Instance에 귀속되고 여러 session을 넘어 유지되는 **Personal Desktop**입니다. 이 명칭은 아직 glossary나 ADR에 수용되지 않은 임시 용어입니다.
2. 사용자 binding key는 검증된 OIDC `(iss, sub)`와 `TypeClawInstanceUID`의 tuple입니다. Kubernetes resource 이름과 label에는 raw identity 대신 이 tuple의 keyed digest를 씁니다.
3. VM은 `runStrategy: Manual`로 start/stop하되, whole-root DataVolume/PVC는 유지합니다. 브라우저를 닫거나 VM을 stop해도 파일, browser profile과 설치된 앱은 남습니다.
4. Browser와 TypeClaw computer-use plugin은 같은 graphical console을 봅니다. 사람과 agent가 동시에 입력하지 않도록 `HumanOwned | AgentOwned | Unowned` 중 하나만 허용합니다.
5. SPIFFE mTLS는 이 PoC에서 제외합니다. 대신 browser는 OIDC session과 private proxy credential, TypeClaw extension은 exact owner tuple에 bind된 HMAC bearer를 사용합니다. 이 선택은 accepted production boundary를 충족하지 않으므로 기능을 `Experimental`로만 표시해야 합니다.

구현된 범위는 관리자가 script로 사전 provision한 owner 하나와 그 owner 전용 TypeClaw runtime입니다. 로그인 기반 self-service provisioning, shared runtime의 per-call principal, durable owner registry와 Surf식 chat 통합은 아직 없습니다. 현재 web page는 desktop/control pane만 제공합니다.

Operator의 product path에는 KubeVirt/CDI client, VM controller 또는 desktop resource가 없습니다. Manager는 TypeClaw, credential, network, backup과 update controller만 등록하고 있고 root module에도 KubeVirt dependency가 없습니다 ([manager registration](../../cmd/manager/main.go#L71-L125), [`go.mod`](../../go.mod#L5-L11)). KubeVirt client와 manifest는 `experiments/personal-desktop-poc/`에만 격리했습니다. 따라서 이것은 기존 plugin에 URL 하나를 추가하는 일이 아니라 새 experimental provider workstream입니다.

## Evidence policy

이 문서는 다음 표기를 사용합니다.

- **Observed fact**는 official source나 현재 repository에서 직접 확인한 사실입니다.
- **Inference**는 observed fact에서 도출한 설계 판단입니다.
- **Recommendation**은 이 PoC에 적용할 후보이며 accepted ADR이 아닙니다.
- **Assumption**은 구현 전에 고정하거나 PoC에서 검증할 조건입니다.

주요 upstream source는 다음 immutable revision을 기준으로 읽었습니다.

- E2B Surf [`d2a98aa9d0cd67db5146bec843a296f132d443f5`](https://github.com/e2b-dev/surf/tree/d2a98aa9d0cd67db5146bec843a296f132d443f5).
- E2B Desktop template [`89a545e22343aa1c40f28338bf3281a6c04b1d4a`](https://github.com/e2b-dev/desktop/tree/89a545e22343aa1c40f28338bf3281a6c04b1d4a)와 SDK [`5a56c87e9db0e221b138662805af7743e75f1082`](https://github.com/e2b-dev/E2B/tree/5a56c87e9db0e221b138662805af7743e75f1082).
- KubeVirt user guide [`bf1f3564e2a41eb059df5ab126724bb78cf15200`](https://github.com/kubevirt/user-guide/tree/bf1f3564e2a41eb059df5ab126724bb78cf15200)와 source [`a61d1001066c179e1703f28549abe0add45a1807`](https://github.com/kubevirt/kubevirt/tree/a61d1001066c179e1703f28549abe0add45a1807).
- noVNC [`ac861f9e280b015569c4b1c3999516d9c0fa35c3`](https://github.com/novnc/noVNC/tree/ac861f9e280b015569c4b1c3999516d9c0fa35c3).
- TypeClaw [`681f581793a0cbb98126e3c0288e7ea8d60206c3`](https://github.com/typeclaw/typeclaw/tree/681f581793a0cbb98126e3c0288e7ea8d60206c3).

## 권장 PoC architecture

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

**Inference.** Browser가 kube-apiserver에 직접 접속할 필요는 없습니다. Desktop Gateway가 user session을 검증한 뒤 자기 ServiceAccount로 해당 사용자의 VMI VNC subresource를 열고, browser에는 same-origin WebSocket만 제공하면 됩니다. Browser와 TypeClaw Runtime에는 kubeconfig나 Kubernetes token을 주지 않습니다.

**Recommendation.** PoC에서는 Gateway가 lifecycle control plane과 VNC data plane을 한 process에 담아도 됩니다. API와 package seam은 분리해 두고, production 승격 시 accepted ADR의 Broker/Reconciler process 분리를 적용합니다. 이 합침은 PoC 단축을 위한 의도적인 deviation입니다.

## Ubuntu/Xfce image와 persistent disk

### Golden image

초기 image는 Ubuntu cloud image를 CDI로 가져온 뒤 다음 항목을 설치한 golden DataVolume/PVC로 만듭니다.

- Xfce와 display manager
- 한 명의 standard desktop user와 PoC용 autologin
- `qemu-guest-agent`
- browser와 acceptance test에 필요한 최소 앱
- 고정 해상도와 screen-lock 비활성화 설정

KubeVirt는 기본적으로 VNC 연결이 가능한 VGA-compatible graphics device를 VMI에 붙이고, modern guest에는 `virtio` video도 선택할 수 있습니다 ([graphics device](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/compute/virtual_hardware.md#L449-L519)). 절대좌표 pointer를 위해 tablet input device를 명시할 수 있습니다 ([tablet input](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/compute/virtual_hardware.md#L738-L764)). 따라서 E2B처럼 guest 안에 `x11vnc`와 `websockify`를 다시 설치할 필요는 없습니다. Xfce는 guest display에만 실행하고, stream은 KubeVirt native VNC를 사용합니다.

E2B Desktop이 Ubuntu 22.04, Xvfb, Xfce, `xdotool`, `scrot`, `x11vnc`, noVNC/websockify를 조합한다는 것은 Linux desktop UX가 성립한다는 좋은 reference입니다 ([template](https://github.com/e2b-dev/desktop/blob/89a545e22343aa1c40f28338bf3281a6c04b1d4a/template/template.py#L3-L65)). 다만 KubeVirt VM에는 실제 virtual graphics device가 있으므로 Xvfb와 guest-side VNC 부분은 복사하지 않습니다.

### 사용자별 whole-root persistence

**Observed fact.** KubeVirt의 `containerDisk`와 ephemeral volume은 VM stop 뒤 write state를 보존하지 않습니다. Root disk가 stop/restart를 넘어 지속되어야 하면 PVC 또는 DataVolume을 사용해야 합니다 ([storage behavior](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/storage/disks_and_volumes.md#L655-L722)).

**Observed fact.** `Manual` run strategy는 `start`, `stop`, `restart` subresource 호출로만 power state를 바꾸며 그 호출 뒤에도 strategy를 `Manual`로 유지합니다 ([run strategy](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/compute/run_strategies.md#L14-L49), [command behavior](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/compute/run_strategies.md#L55-L105)). Force stop은 전원 코드를 뽑는 것과 같아 data loss를 일으킬 수 있으므로 idle shutdown에 쓰면 안 됩니다 ([lifecycle warning](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/user_workloads/lifecycle.md#L48-L62)).

**Recommendation.** 사용자 최초 생성 때 golden disk에서 whole-root DataVolume을 한 번 clone하고 이후에는 같은 volume만 attach합니다. Root DataVolume은 VM 안의 `dataVolumeTemplates`에 숨기지 않고 `PersonalDesktop` record가 별도 lifecycle resource로 추적하며, VM은 그 volume을 참조합니다.

- Browser disconnect와 chat 종료는 connection만 닫습니다.
- Idle timeout은 guest를 graceful shutdown한 뒤 VM을 stop합니다. PVC는 삭제하지 않습니다.
- 다음 web login이나 TypeClaw action은 같은 VM을 start하고 desktop readiness를 기다립니다.
- Account/Desktop 삭제는 별도의 확인된 destructive operation입니다. 이때만 optional final snapshot 뒤 PVC를 삭제합니다.

삭제 순서는 access revoke 뒤 `VMI absence 확인 → VM 삭제 또는 root DataVolume reference 제거와 volume detach 확인 → DataVolume/PVC absence 확인`입니다. API delete 요청의 ACK만으로 다음 단계로 넘어가면 안 됩니다. Compute stop, VM detach, storage cleanup 실패는 서로 다른 retry 단계로 남겨야 하며, PVC object 부재는 `Retain` PV나 backend data의 secure erasure를 증명하지 않습니다. 이 순서는 PoC state model에 compute phase와 직교하는 deletion phase로 표현했지만 Kubernetes controller/finalizer로는 아직 구현하지 않았습니다.

VM의 `dataVolumeTemplates`에서 만든 storage는 VM 삭제 시 함께 삭제됩니다 ([DataVolume ownership behavior](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/storage/disks_and_volumes.md#L538-L595)). 따라서 idle 처리에서 VM object를 delete/recreate하면 안 됩니다. PoC는 VM을 보존한 채 start/stop하고, controller repair 중에도 root volume identity를 변경하지 않아야 합니다.

PVC persistence는 backup이 아닙니다. KubeVirt VM snapshot은 CSI `VolumeSnapshotClass` 지원이 필요하며, online snapshot의 application consistency에는 QEMU Guest Agent가 참여합니다 ([snapshot prerequisites and quiescing](https://github.com/kubevirt/user-guide/blob/bf1f3564e2a41eb059df5ab126724bb78cf15200/docs/storage/snapshot_restore_api.md#L3-L38)). Snapshot/restore는 PoC core flow 다음 단계로 둡니다.

## 사용자 인식과 authorization

### Durable owner key

**Observed fact.** OpenID Connect는 `(iss, sub)` 조합만 RP가 장기간 stable user identifier로 의존할 수 있다고 정하며, email과 display name에는 uniqueness나 stability를 보장하지 않습니다 ([OIDC Core §5.7](https://openid.net/specs/openid-connect-core-1_0-18.html#ClaimStability)).

**Recommendation.** Desktop의 logical owner key는 다음 tuple입니다.

```text
(oidcIssuer, oidcSubject, typeClawInstanceUID)
```

Kubernetes object name은 raw value가 아니라 `HMAC-SHA256(platformKey, canonicalTuple)`의 앞 20개 lowercase hexadecimal 문자 표현을 씁니다. 이렇게 하면 email 변경과 resource-name injection을 피하고, 같은 사용자가 다른 TypeClaw Instance에 연결될 때 desktop이 섞이지 않습니다. HMAC key와 owner mapping record는 PVC보다 먼저 복구되어야 하는 platform state입니다. 이 PoC에는 alias registry가 없으므로 platform key, `iss`/`sub`, TypeClawInstance UID 중 하나라도 바뀌면 새 blank name이 선택되고 기존 PVC는 자동 이전되지 않습니다. 변경 전 old/new binding을 기록하고 disk를 clone/rebind하는 migration이 필요합니다.

**Assumption.** 첫 PoC는 한 TypeClaw Instance가 한 OIDC subject에 귀속된다고 가정합니다. 여러 사람이 같은 TypeClaw Instance를 사용하면서 각자의 PC를 원하면 현재 plugin context만으로는 충분하지 않습니다. TypeClaw tool context가 plugin에 주는 identity는 `sessionId`뿐이며 verified user principal은 없습니다 ([TypeClaw `ToolContext`](https://github.com/typeclaw/typeclaw/blob/681f581793a0cbb98126e3c0288e7ea8d60206c3/src/plugin/types.ts#L21-L26)). 이 경우 platform이 호출마다 verified subject binding을 추가하는 별도 contract가 필요합니다.

### PoC authentication without SPIFFE

PoC에서 다음 세 credential domain을 섞지 않습니다.

1. Browser는 HTTPS/WSS에서 OIDC session cookie를 사용합니다. OIDC reverse proxy는 client-supplied identity/Authorization/proxy headers를 제거하고 검증한 `iss`/`sub`와 private proxy credential을 Gateway에 주입합니다.
2. TypeClaw Platform Extension은 model argument가 아닌 mounted Secret에서 owner-scoped bearer를 읽습니다. Gateway는 `HMAC(agentTokenKey, issuer, subject, TypeClawInstanceUID)`를 검증하므로 한 owner token을 다른 subject에 재사용할 수 없습니다. Signing key 자체는 TypeClaw에 주지 않습니다.
3. Gateway만 `virtualmachineinstances/vnc`, `virtualmachineinstances/vnc/screenshot`과 VM read/start/stop에 대한 narrow Kubernetes RBAC를 가집니다. DataVolume/PVC provisioning은 별도 render/apply 단계이며 Gateway에는 그 write 권한을 주지 않습니다.

Browser가 보내는 VM name, namespace나 `desktopId`를 그대로 trust하지 않습니다. `/desktop/me`처럼 authenticated owner에서 target을 server-side resolve합니다. VNC URL이나 Kubernetes bearer token을 iframe URL query에 넣지 않습니다.

이 방식은 SPIFFE workload identity를 대체하는 production 설계가 아닙니다. Static bearer theft, Gateway compromise와 long-lived desktop credential rotation을 해결하지 못하므로 accepted [ADR 0001](../adr/0001-restricted-workload-and-tool-execution-boundaries.md)과 [ADR 0002](../adr/0002-spiffe-workload-identity-and-credential-execution.md)의 production conformance를 주장할 수 없습니다.

## Browser control과 agent control

### noVNC web surface

향후 product UI는 Surf처럼 desktop pane과 chat pane을 함께 보여 줄 수 있지만, 현재 PoC page는 desktop/control pane만 구현합니다. 두 경우 모두 raw upstream URL을 iframe에 전달하지 않고 project-owned noVNC component를 same-origin WSS endpoint에 연결합니다. noVNC 자체는 static client library이고 가장 단순한 deployment도 web server와 VNC용 WebSocket proxy를 요구합니다 ([embedding guide](https://github.com/novnc/noVNC/blob/ac861f9e280b015569c4b1c3999516d9c0fa35c3/docs/EMBEDDING.md#L1-L25)).

이 PoC Gateway는 WebSocket `Origin`의 scheme/authority와 request `Host`를 검사합니다. 외부 browser origin은 `https`여야 하고 authority는 `Host`와 정확히 일치해야 하며, explicit insecure-dev mode의 loopback만 `http`를 허용합니다. 따라서 앞단 reverse proxy는 외부 `Host`/HTTP/2 `:authority`와 `Origin`을 보존해야 하며, 내부 Service 이름으로 rewrite하면 same-origin browser VNC 연결도 거부됩니다.

noVNC의 `viewOnly`는 keyboard와 pointer event를 **client가 보내지 않게 하는 boolean**입니다 ([property contract](https://github.com/novnc/noVNC/blob/ac861f9e280b015569c4b1c3999516d9c0fa35c3/docs/API.md#L80-L83)). 따라서 authorization boundary로 사용할 수 없습니다. 이 PoC는 view-only browser에 RFB socket을 주지 않고 KubeVirt screenshot endpoint를 polling합니다. noVNC/RFB socket은 Gateway가 exclusive input controller 하나에만 발급합니다.

### One input owner

Gateway는 desktop마다 다음 state를 atomic하게 관리합니다.

```text
Unowned ── agent acquire ──> AgentOwned(epoch=N)
Unowned ── user takes control ──> HumanOwned(epoch=N)
AgentOwned ── revoke + drain + fresh frame ──> HumanOwned(epoch=N+1)
HumanOwned ── disconnect/hand back + fresh frame ──> AgentOwned(epoch=N+1)
```

- Browser는 어느 state에서도 화면을 볼 수 있지만 `HumanOwned`일 때만 input-capable WebSocket을 받습니다.
- Agent tool은 `AgentOwned`일 때만 action을 보낼 수 있습니다.
- Ownership 전환은 in-flight action을 drain하거나 `UnknownOutcome`으로 닫고 epoch를 증가시킵니다.
- Agent는 hand-back 뒤 새 screenshot과 geometry를 받아야 하며 오래된 좌표를 재사용하지 않습니다.
- 같은 action을 timeout 뒤 자동 replay하지 않습니다.

KubeVirt VNC handler의 기본값은 새 connection이 기존 VNC session을 drop하는 동작이며, `preserveSession` option이 이를 바꿉니다 ([handler default](https://github.com/kubevirt/kubevirt/blob/a61d1001066c179e1703f28549abe0add45a1807/pkg/virt-api/rest/vnc.go#L43-L60)). Browser viewer와 agent connection을 동시에 유지할 때는 `preserveSession=true`가 실제 target version에서 두 client를 안정적으로 유지하는지 먼저 검증합니다. 그렇지 않으면 Gateway가 단일 upstream connection을 소유하고 frame을 fan-out해야 합니다.

### Minimal TypeClaw tool surface

TypeClaw extension은 lifecycle과 raw VNC address를 model에 노출하지 않고 다음 두 conceptual capability를 제공합니다.

- `computer_observe`: Gateway의 bounded screenshot을 vision profile이 읽을 수 있는 artifact와 `frameId`, dimensions, ownership state로 반환합니다.
- `computer_act`: `expectedFrameId`, `epoch`, `actionId`와 click, move, type, key, scroll action을 보냅니다.

구현된 PoC plugin은 모델 사용성을 위해 이 둘을 `desktop_status`, `desktop_acquire`, `desktop_observe`, `desktop_click`, `desktop_type`, `desktop_key`, `desktop_scroll`, `desktop_power`, `desktop_release`로 나눴습니다. Text-only main model을 지원하기 위해 `acquire → observe → first-party look_at(models.vision) → observationId를 echo한 input`을 서로 다른 model/tool round로 실행해 blind parallel dispatch를 막습니다. 아직 durable `frameId`/`actionId` ledger는 없으며 각 input 결과를 `Unconfirmed` 또는 연결 유실 시 `UnknownOutcome`로 취급합니다.

Plugin은 `sessionId`를 ephemeral input lease의 owner와 cancellation correlation에 쓰지만 durable desktop owner identity로 쓰지 않습니다. Controller session이 끝나면 RFB authority만 release하고 VM/PVC는 유지합니다. 현재 PoC의 power transition은 웹 또는 `desktop_power` tool로 명시적으로 요청해야 하며 lazy start와 idle power policy는 구현하지 않았습니다. 두 기능은 controller 단계의 recommendation입니다.

## Surf에서 가져올 UX와 가져오면 안 되는 lifecycle

Surf는 좋은 interaction prototype이지만 persistent multi-user architecture의 reference는 아닙니다.

| 항목 | Surf source에서 관찰한 사실 | 이 PoC의 결정 |
|---|---|---|
| 화면 | Backend가 만든 `vncUrl`을 browser state에 저장하고 iframe `src`로 렌더링합니다 ([state](https://github.com/e2b-dev/surf/blob/d2a98aa9d0cd67db5146bec843a296f132d443f5/app/page.tsx#L31-L40), [iframe](https://github.com/e2b-dev/surf/blob/d2a98aa9d0cd67db5146bec843a296f132d443f5/app/page.tsx#L384-L390)). | PoC는 desktop/control pane만 만들고, 향후 chat layout에도 client-held raw URL 대신 same-origin authenticated Gateway와 embedded noVNC component를 사용합니다. |
| Sandbox identity | Browser가 `sandboxId`를 다음 chat request에 다시 보내고 backend가 그 ID로 reconnect합니다 ([route](https://github.com/e2b-dev/surf/blob/d2a98aa9d0cd67db5146bec843a296f132d443f5/app/api/chat/route.ts#L21-L52)). | Client-provided ID가 아니라 server-side `(iss, sub, TypeClawInstanceUID)` mapping으로 resolve합니다. |
| Lifetime | Timeout은 5분이고, visible tab에서 10초가 남으면 연장하며 0이 되면 client state를 지웁니다 ([timeout](https://github.com/e2b-dev/surf/blob/d2a98aa9d0cd67db5146bec843a296f132d443f5/lib/config.ts#L1-L14), [renewal](https://github.com/e2b-dev/surf/blob/d2a98aa9d0cd67db5146bec843a296f132d443f5/app/page.tsx#L167-L192)). Stop action은 sandbox를 kill합니다 ([actions](https://github.com/e2b-dev/surf/blob/d2a98aa9d0cd67db5146bec843a296f132d443f5/app/actions.ts#L6-L25)). | Session expiry는 VNC connection만 닫고, idle timeout은 VM만 halt하며 PVC를 보존합니다. |
| Stream auth | Checked source는 `stream.start()`를 option 없이 호출하고 interactive `getUrl()`을 반환합니다 ([creation](https://github.com/e2b-dev/surf/blob/d2a98aa9d0cd67db5146bec843a296f132d443f5/app/api/chat/route.ts#L37-L49)). E2B SDK는 auth가 opt-in이고 `viewOnly`도 URL option입니다 ([SDK](https://github.com/e2b-dev/E2B/blob/5a56c87e9db0e221b138662805af7743e75f1082/packages/desktop-js/src/sandbox.ts#L596-L669), [server startup](https://github.com/e2b-dev/E2B/blob/5a56c87e9db0e221b138662805af7743e75f1082/packages/desktop-js/src/sandbox.ts#L672-L731)). | OIDC authorization과 server-side input filter를 필수로 둡니다. |
| Human/agent handoff | Checked Surf source에는 durable owner mapping이나 exclusive input-owner state machine이 없습니다. | `HumanOwned`/`AgentOwned` 전환을 Gateway가 집행합니다. |

Surf live deployment 앞단에 source에 없는 access control이 있을 가능성은 배제할 수 없습니다. 위 비교는 checked repository가 제공하는 contract만 대상으로 합니다.

## 최소 구현 순서

1. **Cluster prerequisite.** KubeVirt와 CDI를 administrator-owned infrastructure로 설치합니다. Operator가 임의로 설치하거나 upgrade하지 않습니다.
2. **One persistent VM.** Ubuntu/Xfce golden image와 whole-root DataVolume/PVC를 만들고 `Manual` VM에서 `start → file/browser state 변경 → stop → start` 뒤 상태가 남는지 검증합니다.
3. **VNC Gateway.** Narrow ServiceAccount로 KubeVirt VNC/screenshot subresource를 relay하고, OIDC-authenticated noVNC page에서 동일 console을 조작합니다.
4. **Durable mapping.** provisional `PersonalDesktop` record와 `(iss, sub, TypeClawInstanceUID)` binding, lazy start, graceful idle halt를 구현합니다.
5. **Computer-use extension.** `computer_observe`와 `computer_act`를 Gateway에 연결합니다. Screenshot size와 fixed coordinate space를 TypeClaw의 actual model path에서 측정합니다.
6. **Handoff.** Browser의 Take control/Hand back과 Gateway의 single-writer epoch를 구현합니다.
7. **Only then harden.** Backup/restore, quota, per-user deletion, audit, token rotation, NetworkPolicy와 production identity/separation을 추가합니다.

## Acceptance gates

PoC는 다음 항목이 모두 재현되어야 성공입니다.

1. Alice와 Bob으로 로그인했을 때 서로 다른 VM/PVC가 선택되고, URL/ID를 바꿔도 상대 desktop에 접근할 수 없습니다.
2. 같은 Alice라도 다른 `TypeClawInstanceUID`는 다른 desktop에 bind됩니다.
3. Alice가 파일과 browser profile을 변경한 뒤 browser를 닫고 VM을 idle halt해도, 다시 로그인했을 때 같은 데이터가 보입니다.
4. Browser와 `computer_observe`가 같은 Xfce screen, 해상도와 cursor coordinate space를 봅니다.
5. Agent가 입력 중일 때 browser는 live view만 가능하며, Take control 뒤 agent action은 거부됩니다. Hand back 뒤에는 fresh frame 없이는 agent action을 받지 않습니다.
6. 새 VNC viewer가 기존 agent/viewer connection을 끊지 않으며, 끊는다면 Gateway single-upstream fan-out으로 전환합니다.
7. Browser와 TypeClaw Pod 어디에도 kubeconfig나 ServiceAccount token이 없고, Gateway RBAC은 target namespace의 필요한 subresource와 lifecycle verb로 제한됩니다.
8. Tab close, chat end와 bearer timeout은 PVC를 삭제하지 않습니다. PVC 삭제는 explicit Personal Desktop deletion에서만 일어납니다.
9. Gateway restart와 VM restart 뒤에도 owner mapping, power state와 input ownership이 fail closed로 복구됩니다. **현재 PoC는 이 gate를 완전히 통과하지 못합니다.** `gatewayBootID`로 이전 frame은 무효화하지만 Gateway와 plugin의 power/control quarantine은 process-local이므로 process restart가 uncertainty를 잊습니다. Live PoC에서 restart 후 VMI 상태를 다시 조회하고 explicit start로 recovery하는 것을 검증해야 하며, production gate를 통과하려면 durable power/action ledger가 필요합니다.
10. 대표 해상도에서 VNC latency, screenshot bytes, boot-to-Xfce P50/P95와 kube-apiserver load를 기록합니다. KubeVirt VNC가 API subresource이므로 측정 없이 다수 concurrent viewer로 확장하지 않습니다.

Gateway의 localhost smoke-test mode는 loopback `Host`와 별도 random `devToken`이 모두 확인될 때만 query identity를 인정합니다. ClusterIP Host에서 같은 query를 보내면 거절합니다. 구현된 Gateway는 screenshot 동시 upstream 요청을 3개로 제한하고, UI는 hidden-tab polling을 중단하며 오류에 backoff를 적용하지만, 이것은 gate 10의 실제 부하 측정을 대신하지 않습니다.

## 결정이 아직 필요한 부분

- Ubuntu release, golden image promotion과 patch/update 정책
- user당 CPU, memory, root disk quota와 idle timeout
- 한 사용자가 여러 TypeClaw Instance 사이에서 PC를 공유해야 하는지 여부. 현재 memo는 공유하지 않습니다.
- browser profile과 credential-bearing state의 backup, retention, encryption 및 account deletion 정책
- OIDC provider와 Gateway session implementation
- PoC 이후 SPIFFE identity와 Broker/Reconciler process 분리를 언제 복원할지

이 PoC가 성공해도 current `Sandbox Lease`를 persistent하게 바꾸면 안 됩니다. `Sandbox Lease`는 session-scoped scratch state라는 현재 domain contract를 유지하고 ([glossary](../../CONTEXT.md)), Personal Desktop의 durable ownership, retention, deletion과 backup semantics는 별도 domain/ADR 결정으로 승격해야 합니다.
