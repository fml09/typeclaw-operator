# Personal Desktop computer-use extension

This is the Platform Extension that lets a TypeClaw Instance see and drive its
owner's Personal Desktop. The model never chooses a user ID or a VM name: the
Desktop Gateway this extension talks to fronts exactly one desktop and already
knows who owns it, so the only thing the extension supplies is the bearer token
the operator projected into the Managed Runtime.

Tools:

- `desktop_status`, `desktop_acquire`, `desktop_observe`
- `desktop_act` (one bounded, ordered click/type/key/scroll batch)
- `desktop_launch`, `desktop_windows`
- `desktop_power`, `desktop_release`

Channel command: `desktop` (aliases `vnc`, `pc`).

Input and observation travel to the Guest Desktop Agent inside the VM. The
extension calls the Desktop Gateway's typed action endpoints
(`/api/control/acquire|release`, `/api/agent/actions|launch|screenshot|windows`,
`/api/power/start|stop`) and the Gateway proxies them to the guest agent
Service. There is no browser automation, no Chrome, and no noVNC canvas in the
runtime container, and the extension itself never receives a Kubernetes
credential or a VNC endpoint.

`desktop_act` reports what the guest actually did. `Applied` means the whole
ordered batch ran, `Partial` carries `completedActions` and `failedActionIndex`
for a batch that failed mid-execution, and `UnknownOutcome` means the response
was lost. All three are confirmed by the next `desktop_observe`, and a partial
or unknown batch is never replayed automatically.

## Runtime requirement

This extension targets a TypeClaw fork build that supports Platform Extensions
(`TYPECLAW_PLATFORM_EXTENSIONS`) and plugin-contributed channel commands. It
type-checks against the published `typeclaw` 0.48.9 package, whose
`PluginExports` predates `channelCommands`; `index.ts` therefore declares the
channel-command types locally and returns an exports object typed as a widened
`PluginExports`. On a runtime without channel-command support the tools still
work and the extra key is ignored.

## Installation

The operator owns installation. It embeds `index.ts` (see `../embed.go`),
renders it into the `<instance>-desktop-extension` ConfigMap, mounts that
read-only at `/opt/typeclaw/extensions/personal-desktop-computer-use`, and sets
`TYPECLAW_PLATFORM_EXTENSIONS` to the mounted `index.ts`. Nothing is copied into
the Agent Folder, so the agent cannot modify the extension that governs its own
desktop access.

## Configuration

Every setting is optional and resolved once at plugin start. An explicit
`typeclaw.json` block wins over the environment, because the operator injects
these variables into every Managed Runtime and an administrator's deliberate
override must not be silently replaced by the platform default.

| Setting | Environment fallback | Default |
|---|---|---|
| `gatewayUrl` | `PERSONAL_DESKTOP_GATEWAY_URL` | none — required |
| `agentTokenEnv` | — | `PERSONAL_DESKTOP_AGENT_TOKEN` |
| `screenshotMaxWidth` | `PERSONAL_DESKTOP_SCREENSHOT_MAX_WIDTH` | `1024` |
| `screenshotMaxBytes` | `PERSONAL_DESKTOP_SCREENSHOT_MAX_BYTES` | `180000` |
| `consoleUrl` | `PERSONAL_DESKTOP_CONSOLE_URL` | unset |

`gatewayUrl` must be an origin with no path, query, or fragment. The bearer
token itself is read from the environment variable named by `agentTokenEnv` at
request time and is never written to a config file, a log line, or a channel
reply.

When no usable Gateway URL is resolved the extension still loads — one bad
environment variable must not stop the whole runtime from booting — and every
tool fails with `PluginUnavailable: PERSONAL_DESKTOP_GATEWAY_URL is not set`.
A malformed value is reported the same way, naming the source it came from.

## Permissions

All desktop tools fail closed on the `security.bypass.personalDesktopControl`
permission. Because it is declared by the plugin, it is auto-granted to the
default `owner` wildcard. Granting it to another role means writing the exact
string into that role's `permissions[]` and restarting TypeClaw. Note that an
explicit `permissions[]` on a built-in role replaces the defaults instead of
appending to them, so an incomplete list silently drops ordinary permissions
such as `channel.respond`.

The `desktop` channel command requires both the `session.admin` tier and
`security.bypass.personalDesktopControl` for the invoking origin: the tier gates
who may operate the agent at all, while the bypass permission gates who may
drive this particular machine.

This role guard handles caller admission; the owner-scoped Gateway credential
pins the target desktop. The Gateway serves one owner, so a runtime whose
channel and role admission is shared by several end users must not be given this
extension.

## The observation loop

`desktop_observe` defaults to saving an adaptive JPEG in runtime-private
temporary storage and returning its absolute path. The agent must immediately
pass that path to `look_at`; only the vision profile receives the image bytes,
and the text-only main model receives the visual description. Observation files
never enter the Agent Folder and are removed when the observation is
invalidated or its session ends.

An image-capable main model can request `deliver: "image"` to receive the JPEG
as a `{type:"image", ...}` tool content part directly and save one model round
trip. Do not use that mode with a text-only main model: it cannot forward image
bytes it never received to `look_at`.

The raw screenshot cap is at most 190,000 bytes, and the extension checks that
the base64 expansion stays under TypeClaw's default
`tool-result-cap.imageMaxBytes` of 262,144. If that cap has been lowered, raise
it to at least `4 × ceil(screenshotMaxBytes / 3)`; otherwise the cap plugin
replaces the image with a text placeholder and the computer-use loop cannot
work.

The normal order is `desktop_acquire`, then `desktop_observe`, then `look_at`
with the returned path, then exactly one `desktop_act` after the visual result.
With `deliver: "image"`, the separate `look_at` step is omitted.
A batch holds 1 to 16 actions and at most 4,000 typed characters in total — for
example click an input, type into it, press Enter. Stop the batch before any
target whose position cannot be read off the same screenshot, and observe again.
`desktop_acquire` only takes control; it does not observe, and input tools never
create a lease implicitly. `desktop_launch` needs no coordinates, so it needs
only the lease, but its effect is still confirmed by a new observation.

`desktop_observe` returns an unpredictable `observationId` alongside the image path or image.
`desktop_act` must echo the latest ID the model actually received, and using a
batch invalidates the observations of every session. Do not put an observe and
an act in the same parallel tool batch: input cannot reference an ID that the
same batch is still producing, and mixing an older valid ID with a new observe
can execute the input first.

`desktop_windows` is view-only. It neither requires nor creates a control lease.

## Control and recovery

The local agent control lease belongs to the one TypeClaw `sessionId` that
called `desktop_acquire`. The Gateway's agent lease has an idle TTL (120 seconds
by default) that observe and action calls renew. Other sessions may still call
`desktop_status`, `desktop_observe`, and `desktop_windows`, but no
control-mutating tool can take the writer from them. When the controlling
session ends, the extension cancels pending input and confirms the Gateway
release with a bounded wait; a view-only session ending does not disturb the
controller. Neither path ever deletes the VM or its disk.

If the close or the release confirmation fails, the lease is not dropped. It
stays as an orphan quarantine reported as `pluginControlCleanupRequired`. A new
session, or a fresh lease after a plugin restart, never implicitly adopts an
Agent controller that is still registered at the Gateway: recovery is
`desktop_release` (or `desktop release` in a channel) followed by a new
`desktop_acquire`. Even an idempotent re-acquire from the same session only
reuses the existing lease when the stored `gatewayBootID` and
`controlGeneration` still match the Gateway's current status.

Client deadlines are 8 s for Gateway status, 32 s for an action batch, 15 s for
a screenshot, and 20 s for a power request; each active operation in the
serialized desktop queue is bounded at 45 s. A read-only deadline is an ordinary
failure, but a dispatched power or input request whose response was lost keeps
the `UnknownOutcome` and recovery rules below.

Coordinates are interpreted in the framebuffer space reported by the previous
`desktop_observe` (`details.framebufferWidth`/`framebufferHeight`, 1:1 with the
VM screen). Input is refused with `FreshObservationRequired` when the presented
`observationId` is not the latest one, when the Gateway boot ID, VMI UID,
control generation, or resolution changed, or when input has already been sent.
A Gateway restart resets the generation counter, but the boot ID also changes,
so an older frame is never reused.

## Limits

- The Gateway-to-guest hop uses a separate bearer that lives only in the Gateway
  Secret and the guest bootstrap material. TypeClaw never sees it.
- `Applied` proves only that the guest executed the batch. A `Partial` batch and
  a lost-response `UnknownOutcome` are both `retrySafe: false` and must not be
  replayed automatically.
- A power request that times out or answers with a transport error, 5xx, or a
  conflict has an unknown accepted state. When the Gateway's own JSON body
  arrives, the extension preserves `retrySafe: false` and the confirmed
  `controlBlocked: true`; when the response itself is lost it reports
  `controlBlocked: "unknown"`. Either way it records a process-local
  `pluginPowerUncertain` and fails acquire, input, and stop closed. Only an
  explicit, successful `desktop_power({action:"start"})` clears the quarantine
  and the Gateway's control block. No power action is ever retried
  automatically, and a Gateway or runtime restart forgets in-memory state, so it
  is not evidence of recovery — a changed boot ID means the VM state must be
  inspected again.
- A human can preempt the agent. The agent never preempts a human.

## Development

```sh
bun install
bunx tsc --noEmit
bun test
```
