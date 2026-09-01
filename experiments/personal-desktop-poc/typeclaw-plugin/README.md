# Personal Desktop computer-use plugin (experimental)

이 플러그인은 TypeClaw `0.51.0`의 current plugin API를 대상으로 한 PoC입니다. 모델이 user ID나 VM 이름을 인자로 고르게 하지 않습니다. Runtime 설정에 고정된 `issuer + subject`와 Gateway가 알고 있는 `TypeClawInstance UID`가 대상 Personal Desktop을 결정합니다.

도구는 다음과 같습니다.

- `desktop_status`, `desktop_acquire`, `desktop_observe`
- `desktop_act`(bounded ordered click/type/key/scroll batch)
- `desktop_launch`, `desktop_windows`
- `desktop_power`, `desktop_release`

입력과 관찰은 VM 내부의 `desktop-agent`로 직접 전달됩니다. Plugin은 Gateway의 typed action endpoint(`/api/control/acquire|release`, `/api/agent/actions|launch|screenshot|windows`)를 호출하고, Gateway가 이를 VM의 agent Service로 프록시합니다. 더 이상 `agent-browser`, Chrome, noVNC canvas를 사용하지 않으므로 TypeClaw container에 browser 자동화 도구가 필요 없습니다. Plugin 자체는 Kubernetes token이나 VNC endpoint를 받지 않습니다.

이전 noVNC 우회 방식과 달리 `desktop_act`는 guest agent의 실행 결과를 받습니다. 전체 ordered batch가 끝나면 `Applied`, 실행 중 action이 실패하면 `completedActions`와 `failedActionIndex`를 포함한 `Partial`, 응답을 잃으면 `UnknownOutcome`입니다. 세 결과 모두 의도한 화면 효과는 다음 `desktop_observe`로 확인하며 partial/unknown batch는 자동 replay하면 안 됩니다.

## Agent Folder에 설치

이 디렉터리를 Agent Folder의 `packages/personal-desktop-computer-use`로 복사하고 `typeclaw.json`에 등록합니다.

```json
{
  "plugins": ["./packages/personal-desktop-computer-use"],
  "personal-desktop-computer-use": {
    "gatewayUrl": "https://desktop.example.internal",
    "issuer": "https://id.example.com",
    "subject": "stable-oidc-subject",
    "agentTokenEnv": "PERSONAL_DESKTOP_AGENT_TOKEN",
    "screenshotMaxWidth": 1024,
    "screenshotMaxBytes": 180000
  }
}
```

`PERSONAL_DESKTOP_AGENT_TOKEN`은 TypeClaw runtime의 환경 변수/Secret으로 주입하고 config 파일에는 쓰지 않습니다. 이 값은 platform의 signing key 자체가 아니라 [`../scripts/derive-agent-token.sh`](../scripts/derive-agent-token.sh)가 exact `(issuer, subject, TypeClawInstance UID)` tuple에 대해 파생한 token이어야 합니다. 다른 subject header와 함께 재사용하면 Gateway가 인증을 거절합니다. Plugin 등록은 restart-required입니다.

모든 desktop tool은 `security.bypass.personalDesktopControl` permission으로 fail closed합니다. 이 `security.bypass.*` permission은 plugin-level로 선언되어 기본 `owner` wildcard에 자동 grant됩니다. 다른 role에 허용하려면 그 role의 `permissions[]`에 exact string을 명시하고 TypeClaw을 restart해야 합니다. Built-in role의 explicit `permissions[]`는 default에 append하지 않고 전체를 대체하므로, 기존 effective permission을 모두 함께 적지 않으면 `channel.respond` 같은 기본 권한을 잃습니다.

이 role guard는 caller admission을 담당하고 owner-scoped Gateway credential은 대상 PC를 고정합니다. 그래도 plugin config는 한 subject에 고정되어 있으므로 runtime의 channel/role admission을 한 owner 전용으로 제한해야 하며, 서로 다른 end user가 한 TypeClaw runtime을 공유하는 구성에는 사용하면 안 됩니다.

`desktop_observe`는 adaptive JPEG를 TypeClaw tool의 `{type:"image", mimeType, data}` content로 image-capable main model에 직접 반환합니다. 별도 임시 파일 write와 first-party `look_at`/vision profile 호출이 없으므로 각 frame에서 model round-trip 하나를 줄입니다. Main model이 image input을 지원하지 않으면 이 plugin을 사용할 수 없습니다.

Screenshot raw byte cap은 최대 190,000 bytes입니다. Base64로 확장돼도 TypeClaw 기본 `tool-result-cap.imageMaxBytes=262,144` 아래인지 plugin이 확인합니다. `tool-result-cap.imageMaxBytes`를 기본값보다 낮췄다면 `4 × ceil(screenshotMaxBytes / 3)` 이상으로 맞춰야 합니다. 그렇지 않으면 cap plugin이 image를 text placeholder로 바꾸므로 computer-use loop가 성립하지 않습니다.

Input의 정상 순서는 `desktop_acquire` → 다음 tool round의 `desktop_observe`(image result) → image를 받은 다음 inference의 `desktop_act` 하나입니다. `desktop_act`에는 같은 관찰에서 결정할 수 있는 click/type/key/scroll을 1~16개 넣을 수 있으며 total typed text는 4,000자까지입니다. 예를 들어 보이는 input을 click하고 text를 type한 뒤 Enter를 누르는 동작은 한 batch로 보냅니다. 앞선 action 뒤에 새로 나타나거나 움직일 target의 좌표는 같은 screenshot에서 결정할 수 없으므로 batch를 거기서 끝내고 다시 observe합니다. `desktop_acquire`는 제어권만 얻고 화면을 관찰하지 않으며, input tool은 암묵적으로 lease를 만들지 않습니다. `desktop_launch`는 좌표가 필요 없으므로 observation 없이 lease만으로 호출할 수 있지만, 실행 결과는 새 image observation으로 확인합니다.

Local Agent control lease는 `desktop_acquire`를 호출한 한 TypeClaw `sessionId`에 귀속됩니다. Gateway의 agent lease는 idle TTL(기본 120초)이 있고 observe/action 호출이 갱신합니다. 다른 session은 `desktop_status`/`desktop_observe`/`desktop_windows`는 사용할 수 있지만 control-mutating tool로 writer를 가로채지 못합니다. Controller session이 끝나면 plugin이 pending input을 cancel하고 Gateway의 Agent release를 bounded wait로 확인합니다. View-only session의 종료는 controller를 끊지 않습니다. 어느 경우든 VM/PVC는 삭제하지 않습니다.

Close 또는 Gateway release 확인이 실패하면 lease를 지우지 않고 `pluginControlCleanupRequired`가 있는 orphan quarantine으로 남깁니다. 새 session과 plugin restart 뒤의 fresh lease는 Gateway에 남은 Agent controller를 암묵적으로 자기 것으로 채택하지 않습니다. 이때는 `desktop_release`로 Gateway release를 확인한 뒤 새로 `desktop_acquire`해야 합니다. 같은 session의 idempotent acquire도 저장한 `gatewayBootID + controlGeneration`과 현재 Gateway status가 일치할 때만 기존 lease를 재사용합니다.

Gateway status, action batch, screenshot, power request의 client deadline은 각각 8초, 32초, 15초, 20초입니다. Desktop tool queue의 각 active operation은 45초로 제한됩니다. Read-only deadline은 일반 실패로 끝나지만, dispatch된 power/input의 응답을 잃은 경우에는 기존 `UnknownOutcome`과 recovery 규칙을 유지합니다.

`desktop_observe`는 예측할 수 없는 `observationId`와 image content를 함께 반환합니다. `desktop_act`는 모델이 실제로 받은 최신 ID를 필수로 되돌려 보내며, batch를 한 번 사용하면 모든 session의 관찰이 무효화됩니다. Guest는 batch 전체를 먼저 검증한 뒤 ordered execution을 시작합니다. 같은 assistant tool batch에서 만들어지는 **새** ID를 input이 미리 참조할 수는 없습니다. 이전의 유효한 ID와 새 observe를 같은 parallel batch에 섞으면 이전 input이 먼저 실행될 수 있으므로 두 tool을 같은 batch에 넣지 않습니다.

`desktop_windows`는 view-only입니다. Control lease를 요구하지도 만들지도 않습니다.

## 한계

- Gateway→agent 구간은 `OWNER_HASH_KEY`에서 파생한 별도 bearer로 인증합니다. Token은 Gateway Secret과 VM cloud-init에만 존재하며 TypeClaw에게 주지 않습니다. Cloud-init 기록의 노출 한계는 [PoC README](../README.md)의 한계 목록을 참고합니다.
- `Applied`는 batch의 xdotool 실행 성공까지만 증명합니다. 실행 중 실패한 `Partial`과 응답을 잃은 `UnknownOutcome`은 모두 `retrySafe:false`이며 같은 batch를 자동 replay하면 안 됩니다.
- Power API의 timeout/transport/5xx/conflict도 accepted 여부를 알 수 없는 `UnknownOutcome`입니다. Gateway JSON까지 받으면 plugin은 `retrySafe:false`와 확인된 `controlBlocked:true`를 보존합니다. POST 뒤 transport/proxy 응답 자체가 사라지면 block 여부도 알 수 없으므로 `controlBlocked:"unknown"`을 반환합니다. Plugin은 어느 경우든 process-local `pluginPowerUncertain`를 남기고 acquire/input/stop을 fail closed합니다. 상태를 확인한 뒤 explicit `desktop_power({action:"start"})`가 성공해야 plugin quarantine과 Gateway의 process-local control block이 풀립니다. 어느 power action도 자동 재시도하면 안 됩니다. Gateway나 TypeClaw plugin process restart는 in-memory 상태를 잊으므로 recovery 증거가 아니며, boot ID가 바뀌면 VMI 상태를 다시 확인해야 합니다.
- 사람은 Agent를 명시적으로 preempt할 수 있지만 Agent는 사람을 preempt하지 않습니다.
- Browser와 TypeClaw 계정의 linking은 config에 고정된 OIDC identity로 대신합니다. 실제 multi-user account-linking은 아직 구현하지 않았습니다.
- 입력 좌표는 직전 `desktop_observe.details.framebufferWidth/Height` 기준입니다(VM 화면과 1:1). 최신 `observationId`가 일치하지 않거나 Gateway boot ID, VM/control generation, 해상도가 바뀌거나 input을 보낸 뒤에는 `FreshObservationRequired`로 다음 input을 막습니다. Gateway 재시작으로 generation이 다시 시작돼도 boot ID가 달라 이전 frame을 재사용하지 않습니다.
