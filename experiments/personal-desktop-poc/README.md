# Persistent Linux Personal Desktop PoC

가능성을 실제 코드와 manifest로 검증하기 위한 **experimental PoC**입니다. Ubuntu 24.04 cloud image를 사용자별 whole-root DataVolume로 복제하고, KubeVirt `VirtualMachine`에서 XFCE를 실행합니다. 사람은 Surf와 비슷한 웹 화면에서 보고 조작할 수 있고, TypeClaw은 computer-use plugin으로 같은 화면을 관찰하고 입력할 수 있습니다.

이 디렉터리는 operator의 product API나 production install surface가 아닙니다. `config/`와 Helm chart에 연결하지 않았고 현재 `CONTEXT.md`의 `Sandbox Lease` 의미도 바꾸지 않습니다.

현재 구현 범위는 **관리자가 미리 provision한 owner 한 명 + 그 owner 전용 TypeClaw runtime**입니다. 로그인한 신규 사용자를 보고 VM/DataVolume을 자동 생성하는 controller와 여러 end user를 한 TypeClaw runtime에서 구분하는 account-linking contract는 아직 없습니다. 웹 화면도 현재는 desktop/control pane이며 Surf의 chat pane 통합은 다음 단계입니다.

## 결론부터: 사용자별 영속화 방식

Personal Desktop의 stable owner key는 다음 tuple입니다.

```text
(OIDC issuer, OIDC subject, TypeClawInstance UID)
```

Gateway와 render script는 정확히 다음 canonical bytes를 같은 HMAC key로 계산합니다.

```text
v1\n<issuer>\n<subject>\n<TypeClawInstance UID>\n
```

앞 20개 lowercase hex를 사용해 `pd-<owner-key>` VM 이름을 만듭니다. email, 표시 이름, raw OIDC subject, TypeClaw session ID는 Kubernetes 이름이나 label에 기록하지 않습니다.

이 tuple과 `OWNER_HASH_KEY`는 PoC에서 곧 storage address입니다. key를 잃거나 교체하거나, TypeClawInstance를 새 UID로 다시 만들거나, IdP의 `iss`/`sub`가 바뀌면 기존 PVC가 자동으로 이동하지 않습니다. 새 이름의 빈 desktop이 선택되고 기존 DataVolume은 orphan처럼 남습니다. 따라서 이 값들은 immutable/backup-critical로 취급해야 합니다. 변경 전에는 기존 tuple과 `pd-...`/DataVolume 이름을 기록하고 disk를 새 owner object로 clone/rebind해야 합니다. 이 수동 절차는 향후 durable owner registry와 migration으로 대체해야 합니다.

사용자별 DataVolume은 VM의 `spec.dataVolumeTemplates` 밖에 둡니다. 따라서 access session 종료와 VM stop은 disk를 삭제하지 않습니다.

- 유지되는 것: 저장한 파일, browser profile/cookie, 설치한 application, OS와 desktop 설정
- 유지되지 않는 것: RAM, 실행 중 process, 저장하지 않은 editor buffer
- 별도인 것: 이 desktop disk와 TypeClaw `Agent Folder`는 서로 다른 storage/lifecycle입니다.

## 구조

```text
Human browser ── OIDC reverse proxy ─┐
                 (+ private proxy key)│
                                    │ HTTPS / WSS
TypeClaw computer-use plugin ────────┤
  (owner-scoped HMAC bearer)         ▼
                           Personal Desktop Gateway
                           ├─ screenshot → view-only polling
                           ├─ one exclusive RFB controller
                           └─ narrow namespace ServiceAccount
                                      │ KubeVirt subresources
                                      ▼
                           KubeVirt VMI native VNC
                                      │
                              Ubuntu 24.04 + XFCE
                                      │
                         user-owned whole-root DataVolume/PVC
```

KubeVirt/QEMU native VNC를 사용하므로 guest에 public VNC server나 reusable VNC password를 넣지 않습니다. Browser와 TypeClaw은 Kubernetes token을 받지 않고 Gateway에만 연결합니다.

화면 읽기와 쓰기는 의도적으로 다릅니다.

- 여러 browser tab은 screenshot polling으로 view-only 관찰을 할 수 있습니다.
- Screenshot은 Gateway 전체에서 동시 3개까지만 upstream KubeVirt API에 전달하고 초과 요청은 `429 + Retry-After`로 즉시 거절합니다. Web UI는 hidden tab에서 polling을 멈추고 오류에는 jittered exponential backoff를 적용합니다.
- Gateway status 조회는 5초, screenshot capture는 12초, screenshot response write는 5초, VNC readiness 조회는 5초 안에 끝나야 합니다. VNC stream 자체는 장시간 session이므로 이 짧은 deadline을 적용하지 않습니다.
- noVNC/RFB connection은 입력 가능한 controller 하나만 엽니다.
- Human은 `Agent에서 제어권 가져오기`를 명시적으로 눌러 Agent를 revoke할 수 있습니다.
- Agent는 Human controller를 preempt할 수 없습니다.
- Gateway restart는 모든 in-memory grant를 잊고 socket을 닫습니다. 복구를 추측하지 않습니다.

## 파일

- [`state-model.html`](./state-model.html): Personal Desktop, Access Session, input ownership을 분리해 검증하는 throwaway prototype
- [`gateway/`](./gateway): KubeVirt screenshot/VNC subresource를 relay하고 Surf-like 웹 화면을 제공하는 Go service
- [`manifests/`](./manifests): namespace-scoped RBAC, Gateway, golden DataVolume, 사용자별 DataVolume/VM template
- [`scripts/render-platform.sh`](./scripts/render-platform.sh): platform/golden image YAML을 stdout으로 render
- [`scripts/render-personal-desktop.sh`](./scripts/render-personal-desktop.sh): owner tuple에서 사용자별 YAML을 stdout으로 render
- [`scripts/derive-agent-token.sh`](./scripts/derive-agent-token.sh): signing key에서 정확한 owner tuple에만 유효한 Agent bearer를 파생
- [`typeclaw-plugin/`](./typeclaw-plugin): acquire/observe/click/type/key/scroll/power/release tools를 제공하는 TypeClaw plugin
- [`docs/research/persistent-linux-desktop-poc.md`](../../docs/research/persistent-linux-desktop-poc.md): Surf, KubeVirt, CDI, noVNC 근거와 version-aware 조사

## 로컬 검증

Cluster 없이 실행 가능한 gate는 다음과 같습니다.

```bash
(cd gateway && go test -mod=readonly ./... && go vet ./... && go test -race ./...)
node --test state-model.test.mjs gateway/static/index.test.mjs
sh -n scripts/*.sh
sh scripts/render.test.sh
(cd typeclaw-plugin && bun install --frozen-lockfile && bunx tsc --noEmit && bun test)
```

이 검증은 상태 전이, 인증/Origin, exclusive control, power ambiguity, stalled KubeVirt/read/write와 `agent-browser` deadline 이후의 slot/queue 회수, plugin session cleanup, manifest render를 확인합니다. 실제 image clone, XFCE first boot, native VNC, browser/Agent 입력과 stop/start persistence는 아래 cluster prerequisite를 충족한 뒤 별도 E2E로 확인해야 합니다.

## 전제 조건

Cluster administrator가 다음 infrastructure를 먼저 운영해야 합니다.

- Kubernetes node에서 hardware virtualization/KVM 사용 가능
- KubeVirt `v1.9.0`
- CDI `v1.66.0` 또는 설치한 KubeVirt와 호환되는 release
- clone 가능한 RWO StorageClass. 이 cluster의 후보는 `longhorn`입니다.
- `virtctl`, `kubectl`, container build/push 도구
- Human access를 위한 TLS + OIDC reverse proxy

KubeVirt와 CDI는 namespace application이 아니라 cluster-wide privileged infrastructure입니다. 이 PoC가 설치하지 않습니다.

2026-08-30에 확인한 현재 `homelab-lan` context에는 `virtualmachines.kubevirt.io`와 `datavolumes.cdi.kubevirt.io` CRD가 없습니다. 따라서 이 작업에서는 cluster에 아무 resource도 apply하지 않았습니다. 먼저 administrator 승인을 받아 KubeVirt/CDI를 설치한 뒤 아래 절차를 실행해야 합니다.

## 1. Gateway image 빌드

Gateway는 `kubevirt.io/client-go v1.9.0`을 사용하고 container에 noVNC `v1.7.0` source를 고정해 넣습니다.

```bash
cd experiments/personal-desktop-poc/gateway
docker build --platform linux/amd64 -t registry.example.com/personal-desktop-gateway:poc .
docker push registry.example.com/personal-desktop-gateway:poc
```

## 2. Platform과 golden image render/apply

PoC key/token은 서로 다른 random value로 만듭니다. `OWNER_HASH_KEY`는 Gateway Secret과 사용자 desktop render 양쪽에서 **동일해야** 합니다. `POC_AGENT_TOKEN_KEY`는 Gateway에만 두고, TypeClaw에는 이 key에서 파생한 owner-scoped token만 줍니다. `POC_AUTH_PROXY_TOKEN`은 OIDC reverse proxy와 Gateway만 공유합니다. `POC_DEV_ACCESS_TOKEN`은 localhost port-forward smoke test에만 사용합니다.

```bash
export DESKTOP_NAMESPACE=personal-desktop-poc
export TYPECLAW_INSTANCE_UID='<actual TypeClawInstance metadata.uid>'
export OWNER_HASH_KEY="$(openssl rand -hex 32)"
export POC_AGENT_TOKEN_KEY="$(openssl rand -hex 32)"
export POC_AUTH_PROXY_TOKEN="$(openssl rand -hex 32)"
export POC_DEV_ACCESS_TOKEN="$(openssl rand -hex 32)"
export GATEWAY_IMAGE='registry.example.com/personal-desktop-gateway:poc'
export STORAGE_CLASS=longhorn

kubectl create namespace "$DESKTOP_NAMESPACE" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl -n "$DESKTOP_NAMESPACE" create secret generic personal-desktop-gateway-secrets \
  --from-literal=owner-hash-key="$OWNER_HASH_KEY" \
  --from-literal=agent-token-key="$POC_AGENT_TOKEN_KEY" \
  --from-literal=auth-proxy-token="$POC_AUTH_PROXY_TOKEN" \
  --from-literal=dev-access-token="$POC_DEV_ACCESS_TOKEN" \
  --dry-run=client -o yaml | kubectl apply -f -

sh scripts/render-platform.sh | kubectl apply -f -
```

기본 golden source URL은 Ubuntu의 mutable `noble/current` image입니다. 빠른 PoC에는 편하지만 재현 가능한 build가 아닙니다. 반복 가능한 환경에서는 `GOLDEN_IMAGE_URL`을 검증한 immutable release URL로 설정하고 별도로 digest/provenance를 기록해야 합니다.

CDI import가 끝났는지 확인합니다.

```bash
kubectl -n "$DESKTOP_NAMESPACE" wait \
  --for=jsonpath='{.status.phase}'=Succeeded \
  datavolume/ubuntu-2404-cloud-golden \
  --timeout=20m
```

## 3. 사용자 Personal Desktop 생성

`OWNER_ISSUER`와 `OWNER_SUBJECT`는 OIDC token의 exact `iss`/`sub`여야 합니다. email을 쓰면 account rename/reassignment 때 owner confusion이 생깁니다.

```bash
export OWNER_ISSUER='https://id.example.com'
export OWNER_SUBJECT='<stable OIDC sub>'
export DESKTOP_NODE_NAME='<KVM-capable node name>'
ssh-keygen -q -t ed25519 -N '' -C typeclaw-desktop-poc -f /tmp/typeclaw-desktop-poc
export DESKTOP_SSH_AUTHORIZED_KEY="$(cat /tmp/typeclaw-desktop-poc.pub)"

sh scripts/render-personal-desktop.sh | kubectl apply -f -

# 이 값만 해당 owner의 TypeClaw runtime Secret에 넣습니다.
export PERSONAL_DESKTOP_AGENT_TOKEN="$(sh scripts/derive-agent-token.sh)"
```

같은 owner tuple으로 명령을 다시 실행하면 같은 Kubernetes object 이름을 render합니다. 서로 다른 동시 요청이 blank desktop을 두 개 만드는 대신 Kubernetes create/update conflict로 수렴합니다. 이것은 아직 durable registry/controller가 아니라 script-level PoC입니다.

clone과 첫 boot를 확인합니다.

```bash
kubectl -n "$DESKTOP_NAMESPACE" get datavolume,vm,vmi \
  -l typeclaw.io/desktop-owner-key

# 위 출력의 pd-... VM 이름을 사용합니다.
virtctl -n "$DESKTOP_NAMESPACE" start pd-<owner-key>
```

`DESKTOP_SSH_AUTHORIZED_KEY`는 VNC만으로 guest readiness를 추측하지 않고 `cloud-init status --wait`와 stop/start 영속성을 자동 검증하기 위한 test 전용 key입니다. 실제 사용자 key를 재사용하지 말고 테스트가 끝난 뒤 private key를 폐기합니다. VM은 명시한 KVM node에만 schedule됩니다.

첫 boot는 Internet에서 XFCE, LightDM, Firefox, QEMU Guest Agent package를 설치하고 한 번 reboot합니다. Network와 mirror 속도에 따라 수 분 이상 걸릴 수 있습니다. `VMI Running`만으로 desktop ready를 판단하지 말고 screenshot endpoint가 실제 PNG/JPEG frame을 반환하는지 확인합니다.

## 4. 웹에서 직접 조작

Human용 route와 Agent용 route는 header policy가 다르므로 분리합니다.

Human용 OIDC-aware reverse proxy는 다음을 해야 합니다.

1. 외부 request의 `Authorization`, `X-Personal-Desktop-Issuer`, `X-Personal-Desktop-Subject`, `X-Personal-Desktop-Proxy-Token`을 반드시 제거합니다.
2. 로그인 session에서 검증한 exact `iss`와 `sub`를 새 identity header로 주입합니다.
3. Proxy-side Secret의 `POC_AUTH_PROXY_TOKEN`을 `X-Personal-Desktop-Proxy-Token`으로 주입합니다. 이 값은 browser에 반환하거나 client request에서 받으면 안 됩니다.
4. Browser에서 proxy까지는 HTTPS/WSS를 사용하고, proxy에서 manifest의 Gateway ClusterIP `:80`까지는 cluster 내부 HTTP/WS로 전달합니다. 이 PoC Gateway 자체에는 TLS listener가 없습니다.
5. 원래 browser request의 `Host`/HTTP/2 `:authority`와 `Origin`을 보존합니다. Gateway는 Human의 power POST와 VNC WebSocket에서 `Origin` scheme/authority를 확인합니다. 외부 route는 `https` origin이어야 하고 authority는 `Host`와 정확히 같아야 하며, `http`는 `ALLOW_INSECURE_DEV_AUTH=true`인 loopback(`localhost`/loopback IP) 실험에서만 허용합니다. Proxy가 header를 제거하거나 내부 Service 이름으로 바꾸면 `403`을 반환합니다. Owner-scoped bearer를 쓰며 `Origin`을 보내지 않는 non-browser Agent power request만 browser origin 요구에서 제외됩니다.
6. Browser와 일반 Pod가 Gateway를 우회하거나 Kubernetes API/VNC subresource에 직접 접근하지 못하게 합니다.

TypeClaw용 machine route는 반대로 owner-scoped `Authorization`과 exact issuer/subject header를 보존해야 합니다. Off-cluster라면 별도 internal host에서 TLS를 terminate하고 Gateway HTTP/WS로 전달합니다. In-cluster라면 NetworkPolicy로 제한한 ClusterIP URL을 직접 사용할 수 있습니다. Human route의 unconditional strip policy를 Agent route에 적용하면 plugin이 인증되지 않습니다. 두 route 모두 raw Kubernetes/VNC credential은 client에 주지 않습니다.

Owner-scoped Agent의 noVNC WebSocket은 bearer가 없는 Human과 다른 Origin policy를 사용합니다. Exact request authority와 일치하는 `http`/`https` Origin은 허용하므로 ClusterIP HTTP route가 동작하지만, plaintext HTTP는 반드시 namespace/network-policy로 제한된 in-cluster path에만 써야 합니다. Off-cluster machine route는 HTTPS/WSS가 필수입니다. Human은 계속 HTTPS same-origin 또는 explicit loopback dev mode만 허용됩니다.

이 PoC는 특정 IdP가 정해지지 않아 Ingress/OAuth2-proxy manifest를 포함하지 않습니다.

OIDC proxy 없이 local lab에서만 확인하려면 platform을 `ALLOW_INSECURE_DEV_AUTH=true`로 다시 render/apply하고 port-forward합니다. 이 mode는 request `Host`가 `localhost` 또는 loopback IP이고, query의 `devToken`이 Gateway Secret의 `dev-access-token`과 constant-time comparison으로 일치할 때만 identity query를 인정합니다. 따라서 ClusterIP 주소로 같은 query를 보내도 인증되지 않습니다.

```bash
kubectl -n "$DESKTOP_NAMESPACE" port-forward service/personal-desktop-gateway 8080:80
```

그 뒤 다음 URL을 엽니다.

```text
http://localhost:8080/?issuer=https%3A%2F%2Fid.example.com&subject=<stable-sub>&devToken=<POC_DEV_ACCESS_TOKEN>
```

이 query-string mode는 기본적으로 꺼져 있고 UI에 경고가 표시됩니다. `devToken`도 URL에 들어가므로 browser history, access log, 화면 공유에 노출될 수 있습니다. 테스트가 끝나면 token을 폐기하고 외부에 노출하면 안 됩니다.

## 5. TypeClaw computer-use plugin 연결

[`typeclaw-plugin/README.md`](./typeclaw-plugin/README.md)의 디렉터리를 Agent Folder `packages/personal-desktop-computer-use`로 복사하고 `typeclaw.json`에 등록합니다. 위에서 파생한 `PERSONAL_DESKTOP_AGENT_TOKEN`을 **그 owner의** TypeClaw runtime Secret으로 주입한 뒤 runtime을 restart합니다. 이 token을 다른 `(iss, sub)` header와 재사용하면 Gateway가 거절합니다.

Plugin은 다음 보장을 의도합니다.

- tool argument에 `userId`, VM name, VNC URL을 받지 않습니다.
- Gateway credential이 고정 owner tuple에 bind되므로 model이 다른 사용자를 선택할 수 없습니다.
- 모든 desktop tool은 TypeClaw의 `security.bypass.personalDesktopControl` permission을 확인합니다. 기본 owner에는 wildcard로 grant되고 그 밖의 role은 명시적으로 grant해야 합니다.
- `desktop_observe`는 Gateway가 adaptive JPEG로 축소합니다. TypeClaw의 `tool-result-cap.imageMaxBytes`는 최소 `4 × ceil(screenshotMaxBytes / 3)`이어야 하며, 최대 plugin 설정 190,000 bytes에서는 253,336 이상이어야 합니다.
- TypeClaw이 image result를 size cap placeholder로 바꾸면 plugin은 그 관찰을 fresh frame으로 인정하지 않습니다. 다음 input은 `FreshObservationRequired`로 실패하므로 cap을 올리고 다시 observe해야 합니다.
- `desktop_observe`가 반환한 예측 불가능한 `observationId`를 다음 inference의 input tool이 echo해야 합니다. 새 frame의 ID를 같은 assistant batch에서 blind reference하거나 한 ID로 input을 두 번 보내는 것은 거절합니다. 이전의 유효한 ID와 새 observe를 같은 parallel batch에 섞으면 이전 input이 먼저 실행될 수 있으므로 두 tool을 같은 batch에 넣지 않습니다.
- 정상 input 순서는 `desktop_acquire` → 별도 tool round의 `desktop_observe` → 다음 inference의 input 하나입니다. Input tool은 암묵적으로 control lease를 만들지 않습니다.
- click/type/key/scroll은 하나씩 serialize합니다.
- Agent control lease는 한 TypeClaw `sessionId`에만 귀속됩니다. 다른 session은 status/observe는 할 수 있지만 acquire/input/release/power로 현재 writer를 가로채지 못하며, controller session이 끝나면 in-flight input을 cancel하고 RFB release를 확인합니다. 이 access lease 종료는 VM이나 PVC를 삭제하지 않습니다.
- Browser/RFB cleanup을 확인하지 못하면 local lease를 `Orphaned` quarantine으로 유지합니다. 새 session이나 plugin lifecycle은 기존 Agent controller를 암묵적으로 승계하지 않으며, `desktop_release`가 browser close와 Gateway release를 확인한 뒤에만 새 acquire를 허용합니다.
- Agent controller가 free일 때만 연결합니다. Human controller는 preempt하지 않습니다.
- `observationId`, Gateway boot ID, control generation, VMI가 바뀌거나 input을 보낸 뒤에는 새 `desktop_observe` 없이는 다음 input을 거절합니다. Gateway 재시작 뒤 generation 숫자가 우연히 재사용돼도 이전 frame을 인정하지 않습니다.
- input dispatch 뒤 결과를 `Unconfirmed`로 반환합니다. 연결/ACK가 사라진 action은 `UnknownOutcome`이고 자동 replay하면 안 됩니다.

Permission은 caller admission을, owner-scoped Gateway token은 target binding을 담당합니다. 둘을 합쳐도 이 plugin은 한 owner 전용 TypeClaw runtime을 전제로 하므로 서로 다른 end user를 같은 runtime에 admission하는 구성에는 배포하면 안 됩니다.

현재 plugin은 noVNC/RFB 구현을 중복하지 않고 TypeClaw container에 포함된 `agent-browser`로 Gateway canvas를 구동합니다. `agent-browser 0.33.0`에서는 WebSocket에도 인증을 전달하기 위해 Gateway 전용 session에 global extra headers를 설정하므로 이 session을 다른 origin/도구와 공유하면 안 되며 controller session end, release, error, dispose 때 닫습니다. 이것은 feasibility를 빨리 검증하기 위한 adapter입니다. Production plugin은 typed action protocol과 durable action ledger를 가진 별도 Broker client로 바꾸는 편이 맞습니다.

## 6. 영속성 확인

1. 웹이나 TypeClaw으로 XFCE terminal을 엽니다.
2. `echo persisted > ~/persisted.txt`를 실행합니다.
3. 모든 controller를 release합니다.
4. 웹의 `PC 끄기` 또는 plugin의 `desktop_power(stop)`으로 VM을 graceful stop합니다.
5. 같은 Gateway surface에서 다시 start하고 `cat ~/persisted.txt`를 확인합니다.

Gateway가 stop을 수락한 뒤에는 control을 block하고 Gateway start 성공 때 해제합니다. 이 사이에 `virtctl start`를 섞지 않는 것이 PoC의 운영 계약입니다. 외부에서 이미 시작했다면 Gateway start를 한 번 호출하면 Running VMI를 idempotent success로 확인하고 block을 해제합니다.

여기서 idempotent start는 VMI가 순간적으로 `Running`인 것만으로 판단하지 않습니다. VM printable status가 `Running`이고 pending `stateChangeRequests`가 없으며 VMI가 deletion 중이 아닐 때만 성공으로 바꿉니다. Pending Stop이 있으면 기존 quarantine을 유지합니다. KubeVirt Start/Stop과 이 확인 GET은 Gateway-owned 15초 deadline 안에서 끝나야 하며, deadline을 넘기면 `UnknownOutcome`입니다.

Stop/start 요청이 timeout, transport loss, 5xx 또는 conflict로 끝나면 API server가 요청을 수락했는지 확정할 수 없습니다. Gateway가 응답을 반환할 수 있으면 Gateway와 plugin은 `UnknownOutcome`, `retrySafe:false`, `controlBlocked:true`를 전달합니다. 이 경우 같은 power action을 자동 재시도하지 않습니다. 상태를 관찰한 뒤 운영자가 의도한 상태를 확인하고, control을 다시 허용하려면 Gateway의 explicit start가 성공하거나 이미 Running인 VMI를 idempotent success로 확인해야 합니다.

TypeClaw에서 POST 뒤 응답 자체를 잃어 Gateway JSON을 받지 못한 경우에는 block이 설치됐는지도 확정할 수 없어 plugin이 `controlBlocked:"unknown"`을 반환합니다. 이것도 명백한 실패가 아니라 `UnknownOutcome`이며 자동 재시도 금지 규칙은 같습니다.

Plugin도 power `UnknownOutcome`을 받으면 `pluginPowerUncertain`를 남기고 acquire/input/추가 stop을 process-local로 차단합니다. 모델이 경고 문구를 무시해도 tool 자체가 fail closed하며, 사용자가 상태를 확인한 뒤 explicit `desktop_power start`를 성공시켜야 해제됩니다.

Web UI도 structured `UnknownOutcome` body를 상태 패널에 그대로 보존하고 stop/control/takeover를 비활성화합니다. 상태를 확인한 뒤 `PC 켜기 / 불확실 상태 복구`를 명시적으로 눌러 start가 성공해야 제어를 다시 허용합니다.

이 `controlBlocked`는 PoC Gateway process memory에만 있습니다. Gateway restart/`Recreate` rollout 뒤에는 block 자체가 사라지고 `gatewayBootID`가 바뀝니다. 따라서 power outcome이 불명확했던 기록이 있으면 restart를 recovery로 간주하지 말고 VMI 실제 상태를 다시 확인한 다음 explicit start recovery를 수행해야 합니다. Production에서 같은 보장을 하려면 durable power/action ledger가 필요합니다.

VM stop 뒤에도 `${DESKTOP_NAME}-root` DataVolume/PVC가 남아야 합니다. 반대로 stop 전에 저장하지 않은 editor buffer나 실행 중 process가 사라지는 것은 정상입니다.

## 삭제는 session 종료와 다릅니다

VM만 삭제해도 별도 DataVolume/PVC는 남습니다.

```bash
kubectl -n "$DESKTOP_NAMESPACE" delete virtualmachine pd-<owner-key>
kubectl -n "$DESKTOP_NAMESPACE" get datavolume pd-<owner-key>-root
```

disk 삭제는 복구하기 어려운 별도 관리 동작이어야 합니다.

```bash
# 실제 사용자 데이터가 영구 삭제될 수 있으므로 owner/admin 확인 뒤에만 실행합니다.
kubectl -n "$DESKTOP_NAMESPACE" delete datavolume pd-<owner-key>-root
```

Production에서는 account disable이 `access revoke + VM stop + disk retain`을 수행하고, account deletion은 retention/grace/finalizer를 거쳐야 합니다. 이 lifecycle은 [`state-model.html`](./state-model.html)에 구현했지만 아직 Kubernetes controller로 구현하지 않았습니다.

## 이 PoC가 의도적으로 주장하지 않는 것

- **SPIFFE/mTLS 없음:** user 요청대로 이번 PoC에서 제외했습니다. 하지만 plaintext/anonymous access를 허용한다는 뜻은 아닙니다. 외부 transport에는 server-authenticated TLS, Human에는 OIDC session + proxy credential, Agent에는 owner-scoped bearer가 필요합니다.
- **Production security profile 아님:** accepted ADR 0002의 production `RemoteSandbox` identity 계약을 충족하지 않습니다. unavailable production provider의 fallback으로 사용하면 안 됩니다.
- **Gateway에 Kubernetes token이 있음:** namespace-scoped Role로 VM/VMI get, start/stop, VNC/screenshot만 허용합니다. 그래도 production의 “data plane에 Kubernetes credential 없음” 경계에는 맞지 않는 명시적 experimental 예외입니다.
- **Header auth가 완성된 OIDC가 아님:** trusted reverse proxy가 client header를 strip하고 검증된 identity와 private proxy token을 inject한다는 전제가 깨지면 identity spoofing이 가능합니다.
- **단일 Gateway replica:** control lock은 memory에 있습니다. Deployment는 rollout 중 두 Pod가 겹치지 않도록 `Recreate`를 사용하므로 update 때 잠깐 중단됩니다. replicas를 2개로 늘리면 one-writer 보장이 깨집니다. Production에는 durable/linearizable grant store가 필요합니다.
- **RFB action acknowledgement 없음:** socket write는 guest application effect의 증거가 아닙니다.
- **Screenshot privacy:** framebuffer에는 cookie, password, personal data가 보일 수 있습니다. PoC는 recording/retention/redaction 정책을 제공하지 않습니다.
- **Persistent compromise:** 다운로드한 malware, browser cookie, startup entry도 정상 파일처럼 다음 session에 남습니다.
- **운영 기능 없음:** backup/restore, quota, encryption key lifecycle, patch/reimage/reset, orphan sweeper, idle TTL controller가 없습니다.
- **Network Authority 없음:** first-boot package install을 위해 guest egress가 필요합니다. production egress policy를 검증하지 않습니다.
- **Live migration/HA 없음:** RWO whole-root PVC와 `runStrategy: Manual`을 사용합니다.

## 상태 모델 먼저 검증하기

Browser에서 [`state-model.html`](./state-model.html)을 직접 열면 다음 시나리오를 guided walkthrough와 free play로 확인할 수 있습니다.

- concurrent first request → Personal Desktop 하나
- Agent control + Human view → 명시적 Human takeover
- 두 browser tab control race와 reconnect grace
- lost ACK → `UnknownOutcome`, VM restart → generation invalidation
- idle stop/start 뒤 saved file 유지
- 다른 principal 거절, disable과 delete 분리, cleanup 성공 → `Deleted`, 실패 → `DeletionBlocked`

Deletion model은 compute phase와 deletion phase를 따로 유지합니다. `VMI absence → VM storage reference 제거와 volume detach → DataVolume/PVC absence`를 각각 확인해야 `Deleted`가 되며, stop/VM/storage 실패는 어느 단계가 막혔는지 보존합니다. PVC object 부재가 storage backend의 secure erasure를 뜻하지는 않습니다.

여기서 쓰는 `Personal Desktop`과 `Desktop Access Session`은 PoC에서 검증 중인 후보 용어입니다. Managed Spec/ADR 결정 없이 repository glossary에 확정하지 않았습니다.

## 다음 production 단계

PoC가 실제 cluster에서 통과하면 다음 순서가 합리적입니다.

1. OIDC identity와 TypeClaw sender를 연결하는 account-linking 계약을 결정합니다.
2. Personal Desktop CRD/controller와 unique owner reconciliation을 추가합니다.
3. Gateway의 Kubernetes access를 별도 Reconciler로 옮기고 typed action Broker를 둡니다.
4. durable control generation/action ledger, idle stop, disable/delete retention을 구현합니다.
5. backup/reset/patching, screenshot retention, guest egress와 credential-bearing browser 정책을 별도 결정합니다.

Surf의 chat + live desktop UX는 참고했지만, client-held `sandboxId`, 5분 timeout, session 종료 시 sandbox 삭제 lifecycle은 복사하지 않았습니다. Personal Desktop은 access session보다 오래 살아야 하기 때문입니다.
