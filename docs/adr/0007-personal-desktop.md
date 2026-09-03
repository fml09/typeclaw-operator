---
status: accepted
---

# Ship the Personal Desktop as a first-class operator capability

Two research memos explored driving a graphical desktop from a TypeClaw agent:
a [persistent Linux desktop on KubeVirt](../research/persistent-linux-desktop-poc.md)
and [Windows computer-use](../research/windows-computer-use.md). Both were
prototyped in `experiments/personal-desktop-poc/` — a Gateway, a guest agent, a
plugin, and shell-rendered manifests, all outside the product path. The
prototype proved the flow and left the hard parts open: identity, ownership,
cleanup, and where Kubernetes credentials live.

This ADR records the decisions taken when moving that prototype into the
product: `spec.personalDesktop` on the TypeClawInstance, a Desktop Gateway
Deployment, an embedded computer-use Platform Extension, and an OS-agnostic
Guest Desktop Agent. The prototype is deleted; the [administrator
guide](../personal-desktop.md) documents the shipped feature.

A **Personal Desktop** is not a `Sandbox Lease`. The Sandbox Lease contract in
[ADR 0001](0001-restricted-workload-and-tool-execution-boundaries.md) is
session-scoped scratch state in a certified Tool Execution Environment. A
Personal Desktop is durable, single-owner, human-visible, and runs on
administrator-owned virtualization infrastructure. Keeping the two terms
separate is what allows this feature to exist without weakening the Sandbox
Lease guarantees, and both terms are now defined in
[CONTEXT.md](../../CONTEXT.md).

## Decision

**1. One Personal Desktop per TypeClaw Instance, bound to one owner, named
after the Instance.** Resources are `<instance>-desktop`, `<instance>-desktop-gateway`,
and so on, rather than a keyed digest of the `(iss, sub, instanceUID)` tuple the
PoC hashed into names. The Instance is already the single-owner unit of this
operator, so a second identity layer bought nothing, and derived names are
predictable for GitOps and for `kubectl`. The PoC's naming had a concrete
failure mode: changing the platform key, the issuer, the subject, or the
Instance UID selected a new blank name and silently stranded the existing disk.
*Cost accepted:* one person who wants two desktops needs two Instances, and
renaming an Instance orphans its disk. The recovery path is deliberate rather
than automatic — `rootVolume.existingDataVolume` adopts an already provisioned
disk, including one created by the PoC, and the operator then never creates,
resizes, or deletes it.

**2. Human console identity comes from a Tailscale identity header, and Funnel
is never enabled.** The Desktop Console trusts `Tailscale-User-Login`, which
Tailscale strips from client requests and overwrites with the authenticated
login. Tailnet grants decide who may open a connection at all, so access control
is expressed where the administrator already manages it instead of in a bespoke
OIDC proxy per desktop. Funnel is never enabled because Funnel traffic carries
no identity header, and enabling it would publish an authenticated desktop to
the Internet with the identity check silently absent.

That header is trustworthy only if nothing else can reach the listener, and
`spec.personalDesktop.access.tailscale.mode` selects what enforces that.

`Sidecar` runs tailscaled in the Gateway Pod and binds the console to loopback.
Nothing outside that Pod's network namespace can open the socket at all, and
Tailscale Serve attaches the identity headers itself. The guarantee is the
kernel's.

`Ingress` publishes through the Tailscale Kubernetes operator. The console
listener is then on the Pod network and a NetworkPolicy admitting only
`operatorNamespace` is the sole thing keeping other Pods off it.

*Cost accepted:* a hard dependency on Tailscale and one identity provider in v1.
Administrators who do not run Tailscale get the agent path with no console.
Sidecar mode additionally needs a tailnet credential per desktop
(`access.tailscale.authSecret`) and makes the console device ephemeral, so a
Gateway restart re-registers it.

**2a. Amendment (2026-09-03): `Ingress` mode is unsafe on a cluster whose CNI
does not enforce NetworkPolicy, and is no longer the mode this deployment
uses.** The original decision above rested on a premise this repository never
checked: that a NetworkPolicy naming the Tailscale operator namespace actually
restricts anything. The cluster this operator runs on uses flannel as its only
CNI, and flannel implements no NetworkPolicy — the objects are accepted by the
API server and enforced by nothing.

The consequence is not a narrowed trust boundary but an absent one. Every Pod in
the cluster can reach the console port and set `Tailscale-User-Login` to the
owner's login, which grants the exclusive human input lease on a desktop that is
auto-logged-in with passwordless sudo. That includes the Runtime Pod: a model
with the `bash` tool can take the console with `curl`, bypassing the
`security.bypass.personalDesktopControl` permission that the out-of-Agent-Folder
Platform Extension exists to enforce. The stated cost — "anyone who can run a
pod there can forge the header" — understated this by one word: *there* is every
namespace, not one.

`Sidecar` mode is the fix and is what `enabled: true` uses here. `Ingress` mode
remains available and remains correct where the CNI enforces policy; the field
documentation says so at the point of use, because an administrator choosing a
mode is the only person positioned to know which cluster they are on. The PoC's
Caddy `personal-desktop-gateway-external-proxy` is not an alternative: it stamps
a fixed subject on every request without reading the Tailscale header, and it
listens on the Pod network, so it has the same hole one hop earlier.

**3. The Desktop Gateway keeps a narrowly scoped Kubernetes credential. This
contradicts three acceptance criteria of open ticket #18 and is recorded here
rather than silently overridden.** Ticket #18 (open, last updated 2026-08-31,
read live on 2026-09-02) requires that "The Personal Desktop Gateway data plane
and TypeClaw Runtime run without a Kubernetes service-account token", and
[ADR 0001](0001-restricted-workload-and-tool-execution-boundaries.md) states the
same rule for data-plane surfaces. The console path cannot satisfy it. KubeVirt
exposes the graphical console only as Kubernetes-authenticated subresources —
`virtualmachineinstances/vnc`, `virtualmachineinstances/vnc/screenshot`, and the
`virtualmachines/start` and `virtualmachines/stop` subresources. There is no
unauthenticated in-cluster path to a VM's framebuffer, and a separate
credential-holding reconciler could not relay a live bidirectional WebSocket
without becoming the data plane itself. Moving the credential elsewhere would
move the data plane with it.

What is preserved instead is the blast radius. The Gateway's Role is scoped by
`resourceNames` to exactly one VirtualMachine and its VirtualMachineInstance: it
may read them, open their VNC and screenshot subresources, and start and stop
them. It cannot create, delete, or list anything, and it cannot touch any other
desktop. The TypeClaw Runtime and the model-controlled plugin hold no Kubernetes
credential at all: the plugin reaches the Gateway with a bearer token from a
Secret and never learns a cluster address.

Three of ticket #18's four acceptance criteria are affected, and it is worth
being exact about which, because an amendment written against only the first
would leave two criteria the implementation cannot satisfy. Criterion 1 — no
Kubernetes service-account token in the Gateway data plane — is the one this
decision contradicts head-on. Criterion 2 asks that KubeVirt lifecycle
credentials and RBAC be "available only to a narrow reconciler with
resource-scoped permissions"; the permissions are resource-scoped, but `virtualmachines/start`
and `virtualmachines/stop` are lifecycle verbs and the Gateway is a data-plane
Deployment rather than a reconciler, so that criterion is met in scope and
missed in placement. Criterion 4 asks for conformance tests that fail when
Kubernetes credentials return to a data-plane pod; written literally, such a
test fails on the very Deployment this ADR accepts, so it has to exempt the
Gateway's `resourceNames`-bounded Role by name while still failing for the
runtime, the plugin, and every other data-plane pod. Criterion 4's second half —
tests that fail when unrestricted process execution returns to the plugin — and
criterion 3, no unrestricted subprocess or filesystem access from
model-controlled plugin code, are untouched by this decision and remain binding
as written.
*Cost accepted:* one data-plane pod per enabled desktop holds a service-account
token, so #18 cannot be closed as written: criteria 1, 2, and 4 each need an
amendment scoped to the Gateway's console path before the ticket can close. A
compromised Gateway can observe and power-cycle its one VM.

**4. The computer-use plugin ships as a Platform Extension.** It is embedded in
the operator binary, projected read-only into the Managed Runtime from an
operator-owned ConfigMap, and activated through `TYPECLAW_PLATFORM_EXTENSIONS`.
The Agent Folder is never written, which is what ticket #17 asks for: bootstrap
secrets must not enter Agent Folder Git history, and platform code must not sit
in a directory the model may edit. This is exactly the Platform Extension
boundary already defined in CONTEXT.md — administrator-owned, immutable, trusted.
*Cost accepted:* the runtime has to understand platform extensions, so the
feature is gated at runtime version 0.52.0 and refuses to provision below it.
The extension's version is tied to the operator image rather than installed
independently, and it is not yet the signed, digest-pinned Platform Bundle that
[ADR 0002](0002-spiffe-workload-identity-and-credential-execution.md) wants for
production extensions.

**5. The typed-action protocol is OS-agnostic; Windows uses pixel input in v1.**
One protocol — health, windows, screenshot, actions, launch — serves the X11,
Windows, and macOS backends, so the Gateway, the plugin, and the model see the
same contract regardless of guest. On Windows, input is injected with
`SendInput` using absolute coordinates, and screen capture uses GDI, both
through `ctypes` so that the guest agent stays a single stdlib-only file. UI
Automation, which would identify controls semantically rather than by pixel,
is a later enhancement behind the same protocol.
*Cost accepted:* pixel input is brittle where UIA would be exact — it depends on
fixed geometry, is invalidated by DPI or resolution drift, and fails outright on
the locked workstation and the UAC secure desktop. Failing there is intentional:
disabling UAC or automating the secure desktop would break a Windows security
boundary to gain convenience.

**6. Guest credentials are delivered through Secrets, never inline in the VM
spec.** The Linux guest receives its cloud-config through
`cloudInitNoCloud.secretRef`, and the Windows guest receives its answer file,
setup script, agent source, and token through a sysprep Secret. A VirtualMachine
object is readable by anyone with `get virtualmachines` in the namespace, and
inline user data would put the Windows autologon password and the guest agent
token directly into it.
*Cost accepted:* an extra Secret per desktop, and the token Secret lives in the
desktop namespace. When that differs from the Instance namespace, the operator
mirrors the agent token into the Instance namespace so the runtime's
`secretKeyRef` stays namespace-local.

**7. The Gateway is single-replica with in-memory leases; `gatewayBootID` lets
clients detect restarts.** Exclusive input ownership and the power quarantine
are only correct with exactly one authority. Two replicas would need a shared
store before they were safe, and a shared store is not worth building before the
single-desktop semantics are proven. Every response carries the boot ID, so a
client that sees it change knows its frames, its lease, and any quarantine
knowledge are void.
*Cost accepted:* no high availability — a Gateway restart drops the console
session and the agent's lease. More importantly, the power quarantine is
process-local, so a restart during an uncertain start or stop forgets that
uncertainty; the Linux research memo's acceptance gate 9 therefore remains
unmet. Durable power and action ledgers are future work, and until they exist a
human has to re-read the VM state after a Gateway restart.

## Consequences

- The operator gains a KubeVirt and CDI dependency that it declares but never
  installs. Both are administrator-owned infrastructure; when their CRDs are
  absent, `PersonalDesktopReady` is False with reason `KubeVirtUnavailable` and
  nothing is provisioned. KubeVirt and CDI kinds are handled as unstructured
  objects and are never watched, so the operator still runs on clusters that
  have never heard of either.
- The desktop namespace is not a Restricted Workload boundary. KubeVirt relabels
  it to Pod Security `enforce=privileged`, which is why the desktop can be placed
  in its own namespace and why cross-namespace cleanup runs through a finalizer
  rather than owner references. The runtime's own Restricted posture from
  [ADR 0001](0001-restricted-workload-and-tool-execution-boundaries.md) is
  untouched.
- Ticket #18 cannot be closed as written. Three of its four acceptance criteria
  need an amendment scoped to the Gateway's console path: no service-account
  token in the data plane, KubeVirt lifecycle credentials held only by a narrow
  reconciler, and a conformance test that fails when credentials reach a
  data-plane pod. Only the criterion on model-controlled plugin code is
  untouched. The conformance tests that keep Kubernetes credentials out of the
  runtime and the plugin, and unrestricted process execution out of the plugin,
  remain required.
- Retention is the default and deletion is explicit. `onInstanceDeletion: Retain`
  keeps the root disk when the Instance goes away, disabling the feature keeps
  the disk and the tokens so re-enabling resumes the same desktop, and an
  adopted `existingDataVolume` is never deleted at all. Retention is not backup;
  snapshots remain the administrator's job.
- The desktop's security depends on inputs this repository does not own: the
  tailnet policy, the golden image contents, and the accounts the owner signs
  into on that desktop. Screenshots reach the model, so the desktop's screen is
  effectively part of the agent's context.
- `experiments/personal-desktop-poc/` is deleted. Its Gateway, plugin, guest
  agent, and manifests now live in the product path, and an existing PoC disk is
  migrated by adoption rather than by copying.
