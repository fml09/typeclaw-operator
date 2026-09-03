# Personal Desktop

A **Personal Desktop** is a persistent KubeVirt virtual machine bound to one
TypeClaw Instance and to one human owner. The owner opens its graphical console
in a browser over Tailscale; the Instance's agent drives the same screen through
typed actions. The disk survives sessions, power cycles, and Instance restarts.

This guide is for a cluster administrator who has not worked in this repository
before. It covers prerequisites, enabling the feature, building golden images,
publishing the console, what the agent can do, where the security boundaries
are, migrating a desktop from the earlier proof of concept, and troubleshooting.

The vocabulary used below is defined in [CONTEXT.md](../CONTEXT.md). The
decisions behind the design are recorded in
[ADR 0007](adr/0007-personal-desktop.md).

## Personal Desktop versus Sandbox Lease

The operator already has a boundary for giving model-controlled tools an
execution environment: the **Sandbox Lease**, a session-scoped Tool Execution
Environment with isolated scratch state that is discarded when the session ends.
A Personal Desktop is deliberately not that.

| | Sandbox Lease | Personal Desktop |
|---|---|---|
| Lifetime | One session | Indefinite; survives sessions and power cycles |
| State | Scratch, discarded | Retained root disk (`Retain` by default) |
| Ownership | The Instance, per session | One named human owner plus the Instance |
| Human access | None | Desktop Console over Tailscale |
| Isolation claim | Certified Tool Execution Environment | KubeVirt VM on administrator-owned virtualization infrastructure |
| Concurrency | Independent per session | One Input Controller at a time, human or agent |

Because the two are different boundaries, enabling a Personal Desktop does not
change the Instance's Tool Execution Environment, and a Personal Desktop is not
a certified `RemoteSandbox`. Treat it as a durable workstation that both a
person and an agent share, not as a disposable sandbox.

## Cluster prerequisites

The operator provisions desktops; it does not install the virtualization stack
and will not upgrade it. An administrator installs and owns the following before
the feature is enabled.

- **KubeVirt v1.9.0** or compatible. The operator uses the `kubevirt.io/v1`
  `VirtualMachine` and `VirtualMachineInstance` kinds and the
  `subresources.kubevirt.io` VNC, screenshot, start, and stop subresources.
- **CDI (Containerized Data Importer) v1.66.0** or compatible, for
  `cdi.kubevirt.io/v1beta1` `DataVolume` import and cloning.
- **KVM-capable nodes.** Desktop VMs need hardware virtualization. Pin them with
  `spec.personalDesktop.nodeSelector` when only part of the cluster qualifies.
- **A StorageClass that supports cloning**, with `ReadWriteOnce` volumes. Every
  root disk the operator provisions is a CDI clone of a golden DataVolume in the
  same namespace. A disk adopted through `rootVolume.existingDataVolume` is never
  cloned, so that path alone does not need a clone-capable StorageClass.
- **A tailnet**, if the human console is to be published. In
  `access.tailscale.mode: Sidecar` the Gateway Pod runs its own `tailscaled` and
  the only prerequisite is a tailnet credential. In `mode: Ingress` you also
  need the Tailscale Kubernetes operator **and a CNI that enforces
  NetworkPolicy** — see [Reaching the console over
  Tailscale](#reaching-the-console-over-tailscale), where that requirement is
  load-bearing rather than advisory.

If KubeVirt or CDI is missing, the operator reports the condition
`PersonalDesktopReady=False` with reason `KubeVirtUnavailable` and provisions
nothing. It never falls back to some other execution path.

Two further constraints are worth knowing before choosing a namespace.

- KubeVirt relabels the namespace that hosts a VM to Pod Security
  `enforce=privileged`. The TypeClaw runtime itself remains a Restricted
  Workload, so most administrators put desktops in their own namespace with
  `spec.personalDesktop.namespace` rather than relaxing the Instance namespace.
- Cross-namespace desktop resources are cleaned up by a finalizer rather than by
  owner references, because owner references cannot cross namespaces.

## Enabling the feature

The desktop is declared on the TypeClaw Instance. The runtime must be new enough
to load Platform Extensions: an effective `spec.runtime.version` below **0.52.0**
leaves the condition `PersonalDesktopReady=False` with reason `RuntimeTooOld`
and nothing is provisioned. Setting an explicit `spec.runtime.image` skips the
version gate, on the assumption that the administrator knows what that image
contains.

### Linux desktop

```yaml
apiVersion: typeclaw.fml09.io/v1alpha1
kind: TypeClawInstance
metadata:
  name: alice-agent
  namespace: typeclaw-system
spec:
  runtime:
    version: 0.52.0
  storage:
    agentFolder:
      size: 5Gi
  personalDesktop:
    enabled: true
    os: Linux
    namespace: typeclaw-desktops
    owner:
      # Tailscale is the only console identity provider in v1. The subject is
      # the exact login name Tailscale asserts.
      subject: alice@example.com
    access:
      tailscale:
        hostname: alice-desktop
        tags:
          - tag:typeclaw-desktop
    image:
      goldenDataVolume: ubuntu-desktop-golden
      import:
        url: https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-amd64.img
        checksum: sha256:0000000000000000000000000000000000000000000000000000000000000000
        size: 32Gi
    rootVolume:
      size: 64Gi
      onInstanceDeletion: Retain
    resources:
      cpuCores: 4
      memory: 8Gi
    screen:
      width: 1440
      height: 900
    linux:
      username: desktop
      # Optional, and a second way into the guest. See below.
      sshAuthorizedKeys:
        - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleReplaceThisKey alice@example.com
```

`image.import` is optional. It creates the golden DataVolume from an HTTPS
source the first time, only if a DataVolume of that name does not already exist,
and the operator never deletes it afterwards. The checksum is mandatory and is
passed to CDI in its own `sha256:<hex>` form. If you build the golden image
yourself, drop the `import` block and point `goldenDataVolume` at the result.

`linux.username` names the autologin desktop user, and `linux.sshAuthorizedKeys`
installs public keys for that user. Those keys are an independent access path:
whoever holds a matching private key gets an interactive shell in the guest
without passing the console's identity check and without taking the input lease.
Leave the field unset unless you need a maintenance route into the desktop, and
review changes to it the way you would review changes to `owner.subject`.

### Windows desktop

Windows golden images are always administrator-built; `image.import` is rejected
for `os: Windows`.

```yaml
apiVersion: typeclaw.fml09.io/v1alpha1
kind: TypeClawInstance
metadata:
  name: bob-agent
  namespace: typeclaw-system
spec:
  runtime:
    version: 0.52.0
  storage:
    agentFolder:
      size: 5Gi
  personalDesktop:
    enabled: true
    os: Windows
    namespace: typeclaw-desktops
    owner:
      subject: bob@example.com
    access:
      tailscale:
        hostname: bob-desktop
        tags:
          - tag:typeclaw-desktop
    image:
      # Sealed with sysprep /generalize /oobe /shutdown /mode:vm.
      goldenDataVolume: windows11-desktop-golden
    rootVolume:
      size: 128Gi
      storageClassName: fast-rwo
      onInstanceDeletion: Retain
    resources:
      cpuCores: 4
      memory: 8Gi
    nodeSelector:
      kubevirt.io/schedulable: "true"
    windows:
      username: desktop
```

The operator renders a Windows VM with EFI (Secure Boot off), a TPM device, the
Hyper-V enlightenments and clock KubeVirt recommends, the virtio driver and
guest-tools containerDisk as a CD-ROM, and a Secret-backed sysprep CD-ROM. The
sysprep answer file creates the interactive automation user, enables autologon
with a generated password, fixes the display geometry, skips the OOBE screens,
and runs a first-logon script that installs the Guest Desktop Agent as a logon
scheduled task.

### What the operator creates

Enabling the feature renders, in the desktop namespace: the token Secret, the
root DataVolume (a clone of the golden one), the guest bootstrap Secret
(cloud-init on Linux, sysprep on Windows), the `VirtualMachine` with
`runStrategy: Manual`, a ClusterIP Service for the Guest Desktop Agent, the
Desktop Gateway Deployment with its ServiceAccount, Role, and RoleBinding, the
Gateway Service, and a NetworkPolicy around the Gateway. Publishing the console
adds either a console Ingress (`mode: Ingress`) or a `tailscaled` sidecar and
its Serve-config ConfigMap (`mode: Sidecar`). In the Instance namespace it adds
a ConfigMap holding the computer-use Platform Extension and extends the
Instance's own NetworkPolicy with egress to the Gateway.

The runtime StatefulSet gains the extension mount plus the
`TYPECLAW_PLATFORM_EXTENSIONS`, `PERSONAL_DESKTOP_GATEWAY_URL`, and
`PERSONAL_DESKTOP_AGENT_TOKEN` environment variables, and
`PERSONAL_DESKTOP_CONSOLE_URL` in `mode: Sidecar`, where the address is known
from the spec. Nothing is ever written into the Agent Folder.

That last distinction is deliberate. This Pod template is a pure function of the
Instance spec and never of observed status. An address the operator can only
learn by watching an Ingress would, if it reached the template, re-render the
StatefulSet the moment it was observed — restarting the agent minutes after the
sync reported healthy and outside whatever quiesce the deploying chart arranged.
In `mode: Ingress` the extension learns the console address from the Gateway
instead.

Setting `enabled: false`, or removing the whole block, deletes the VM, the
Gateway, the console publication, and the extension mount, but keeps the root
DataVolume and the token Secret, so re-enabling resumes the same disk with the
same tokens.

## Golden images

A golden image is a sealed root disk in the desktop namespace that every
Personal Desktop is cloned from. The operator reads it and never deletes it.

### Linux

The simplest path is the `image.import` block above: CDI imports an Ubuntu cloud
image over HTTPS into the named DataVolume, and the first desktop clones it. The
guest is then finished at first boot from the cloud-init Secret the operator
renders, which installs XFCE, LightDM with autologin, `qemu-guest-agent`, and
the `xdotool`, `scrot`, and `wmctrl` tools the X11 backend of the Guest Desktop
Agent uses, writes the agent and its token, registers the session autostart
entry, and reboots once into the graphical target.

Building the golden DataVolume by hand is equally valid, and is faster to boot
because the packages are already there. Import a cloud image, boot it once,
install the same packages, shut down cleanly, and point `goldenDataVolume` at
the resulting DataVolume with no `import` block. Do not bake credentials into
it: the agent token and the desktop user's configuration are delivered per
desktop through the cloud-init Secret.

### Windows

Windows golden images are built once by an administrator and never imported by
the operator, because nothing here may redistribute Windows media.

1. Import your licensed Windows ISO into a CDI DataVolume and create a blank OS
   DataVolume of the intended size.
2. Build a temporary VM with the ISO, the blank disk, and the virtio-win
   containerDisk attached. Use `runStrategy: RerunOnFailure` so a controlled
   shutdown is respected.
3. Install Windows with the virtio storage and network drivers, the QEMU Guest
   Agent from the virtio guest tools, and Python 3 if you do not want the
   first-logon script to download it.
4. Verify the guest boots cleanly and shuts down cleanly.
5. Seal it with `sysprep /generalize /oobe /shutdown /mode:vm`.
6. Publish the sealed DataVolume as `image.goldenDataVolume`.

Everything the operator needs afterwards arrives through the sysprep Secret: the
answer file, the first-logon setup script, the Guest Desktop Agent source, and
its token. If the golden image has no Python 3, the setup script downloads the
pinned python.org installer, verifies its SHA-256, and installs it silently; you
can override that installer with `windows.pythonInstaller`.

## Reaching the console over Tailscale

Identity comes from the `Tailscale-User-Login` header, which Tailscale strips
from client requests and overwrites with the authenticated login. The Gateway
accepts a console request only if that header equals `owner.subject`, or one of
`access.tailscale.allowedLogins`, and only if `X-Forwarded-Proto` is `https`.

That check is only as good as the guarantee that nothing else can reach the
listener. `access.tailscale.mode` chooses what provides it, and the choice is
not cosmetic — pick the wrong one for your cluster and the console is open to
anything that can send an HTTP request.

**`Sidecar` — correct on every cluster.** The operator runs `tailscaled` as a
second container in the Gateway Pod and binds the console to `127.0.0.1`. No
Ingress is created, the Service publishes no console port, and no other Pod can
open the socket, because it is not on the Pod network at all. Tailscale Serve
terminates tailnet TLS and attaches the identity headers. The console address is
known from the spec, so `status.personalDesktop.consoleURL` is set immediately
rather than after the tailnet device appears.

It needs one tailnet credential, named by `access.tailscale.authSecret` — a
Secret in the desktop namespace holding either `TS_AUTHKEY` (a reusable,
pre-authorized, **ephemeral** key) or `TS_CLIENT_ID` and `TS_CLIENT_SECRET`. The
key must be ephemeral: the sidecar keeps its node state on an `emptyDir` rather
than in a Secret, deliberately, so that publishing a console does not widen the
Gateway's Kubernetes credential. An ephemeral device is removed from the tailnet
shortly after the Pod goes away and gives its MagicDNS name back; a
non-ephemeral one lingers and the next Pod gets `<hostname>-1`.

```sh
kubectl -n <ns> create secret generic tailscale-console \
  --from-literal=TS_AUTHKEY='tskey-auth-...'
```

**`Ingress` — only where the CNI enforces NetworkPolicy.** The operator creates
an Ingress with `ingressClassName: tailscale` pointing at the Gateway's console
port, and the Tailscale Kubernetes operator turns that into a device on your
tailnet. The console listener is then on the Pod network, and the only thing
keeping other Pods off it is a NetworkPolicy admitting
`access.tailscale.operatorNamespace` (default `tailscale`).

Confirm your cluster enforces NetworkPolicy before choosing this. Several common
CNIs — flannel among them — accept NetworkPolicy objects and enforce none of
them, and there is no error to notice: the object exists, `kubectl describe`
shows the rule, and every Pod in the cluster can still reach the port. Any of
them can then send the owner's login in a header and take the console's
exclusive input lease on a desktop that is auto-logged-in with passwordless
sudo. That includes the agent's own Runtime Pod, which would let a model reach
the desktop through `bash` without passing the
`security.bypass.personalDesktopControl` check.

```sh
# No output here means nothing enforces NetworkPolicy; use Sidecar mode.
kubectl get pods -A -o name | grep -Ei 'cilium|calico|kube-router|antrea|weave'
```

Tailscale Funnel is never enabled. Funnel traffic carries no user identity, so
the operator will not set the annotation that would expose the console to the
public Internet.

### Restricting who may open a console

Tag the proxy device and grant access to the tag. With
`tags: [tag:typeclaw-desktop]`, the operator annotates the Ingress with
`tailscale.com/tags`, and your tailnet policy decides who may reach it:

```jsonc
{
  "tagOwners": {
    // The Tailscale operator must be allowed to own the tag it applies.
    "tag:typeclaw-desktop": ["tag:k8s-operator"]
  },
  "grants": [
    {
      "src": ["alice@example.com"],
      "dst": ["tag:typeclaw-desktop"],
      "ip":  ["tcp:443"]
    }
  ]
}
```

Two independent checks then have to pass: the tailnet grant decides who can open
a TCP connection to the device at all, and the Gateway decides whose login may
use the console behind it. Keep both narrow. Widening `allowedLogins` gives
every listed login full control of the desktop, including its files and its
signed-in browser sessions.

## Slash commands

The Platform Extension contributes one channel command to the Instance,
available in whatever chat channel the Instance is connected to.

It appears only while the feature is enabled. The command is contributed by the
extension, the extension is loaded only when the operator injects
`TYPECLAW_PLATFORM_EXTENSIONS`, and the operator injects that only when
`spec.personalDesktop.enabled` is true, the spec validates, and the runtime is
new enough to load Platform Extensions. If `/desktop` is missing from `/help`
while `/steer` and `/queue` are present, the feature is off — those two are
built into the runtime and are always there.

- `/desktop` (aliases `/vnc`, `/pc`) — with no argument, or with `status`,
  replies with the console URL (or `Console not published`), the VM power state,
  who currently holds input control, and whether the desktop is quarantined
  after an uncertain power operation.
- `/desktop start` — starts the VM.
- `/desktop stop` — releases the agent's input lease and then shuts the VM down.
- `/desktop release` — releases the agent's input lease without touching power.

Replies are short plain text, one fact per line.

The command requires the `session.admin` permission and additionally
`security.bypass.personalDesktopControl` for the invoking origin, so a chat
participant cannot power-cycle or seize a desktop unless you granted them that
explicitly.

## What the agent can and cannot do

Through the extension's tools the agent can:

- read the desktop status, including power state, control ownership, and the
  console URL;
- acquire and release the agent input lease (idle TTL 120 seconds by default,
  `gateway.agentLeaseTTLSeconds`);
- take a bounded screenshot of the whole screen;
- send a batch of at most 16 ordered typed actions — click, move, type, key,
  scroll — that can be decided from one frame;
- launch an application by alias (`browser`, `terminal`, `files`, `editor`) or
  by a bare executable name resolved on the guest's PATH;
- list the guest's open windows;
- start and stop the VM.

It cannot:

- open, watch, or authenticate to the Desktop Console; the console listener
  never accepts the agent's bearer token;
- act while the human holds control — the Input Controller is exclusive, and the
  human can take control from the agent at any time;
- reach the Guest Desktop Agent directly; every action goes through the Desktop
  Gateway, which holds the only credential the guest accepts. That boundary is
  the token and the runtime's egress policy, not a policy on the VM — see the
  note below on the desktop VM having no NetworkPolicy of its own;
- talk to the Kubernetes API; neither the runtime nor the model-controlled
  plugin holds a Kubernetes credential;
- write anything into the Agent Folder — the extension is mounted read-only from
  a ConfigMap the operator owns;
- run an arbitrary command line in the guest: `launch` takes an executable name,
  not arguments and not a shell;
- silently retry an action whose outcome is unknown. A lost connection after
  dispatch is reported as `UnknownOutcome` and the desktop is quarantined until
  a human or the agent explicitly recovers it.

## Security boundaries, and their limits

What the design does enforce:

- **One owner for the console.** The console accepts only the Tailscale login in
  `owner.subject` plus any explicit `allowedLogins`, over HTTPS, with an
  `Origin` matching the request `Host` on every mutation and WebSocket.
- **Two separate listeners.** The agent API (port 8080) accepts only the agent
  bearer token, compared in constant time; the console (port 8081) accepts only
  human identities. Neither accepts the other's credential.
- **Network isolation around the Gateway.** A NetworkPolicy lets only the
  Instance's runtime Pod reach the agent port, and the Gateway's egress is
  limited to DNS, the API server, and the guest agent (plus the public Internet
  in `mode: Sidecar`, which `tailscaled` needs to register).
- **A console that is not on the Pod network at all**, in
  `access.tailscale.mode: Sidecar`. The listener is bound to `127.0.0.1` and
  reached only by the `tailscaled` sharing its network namespace. This is the
  one console guarantee that does not depend on the cluster enforcing
  NetworkPolicy — see the limits below.
- **One exclusive Input Controller.** Human and agent input are never
  concurrent, and an ownership change invalidates in-flight work rather than
  interleaving it.
- **Secrets stay in Secrets.** Guest credentials reach the VM through the
  cloud-init `secretRef` or the sysprep Secret, never inline in the VM spec, and
  never through the Agent Folder.
- **One narrow Kubernetes credential.** The Gateway's Role is restricted by
  `resourceNames` to exactly one VM and to the console-related subresources.

What it does not do, and you should plan around:

- **In `access.tailscale.mode: Ingress`, the console is only as isolated as your
  CNI makes it.** The console listener is on the Pod network and a NetworkPolicy
  admitting the Tailscale operator namespace is the entire boundary. A CNI that
  does not implement NetworkPolicy — flannel, for instance — accepts that object
  and enforces nothing, with no error and no signal anywhere: `kubectl describe`
  shows the rule either way. Every Pod in the cluster can then reach the console
  port and assert the owner's login in a header, which grants the exclusive
  input lease on a desktop that is auto-logged-in with passwordless sudo. The
  Instance's own Runtime Pod is one of those Pods, so a model with `bash` can
  take the console without passing the extension's
  `security.bypass.personalDesktopControl` check — the check the Platform
  Extension is mounted outside the Agent Folder specifically to enforce. Use
  `mode: Sidecar` unless you have confirmed your CNI enforces policy.
- **The Gateway is a data-plane pod that does hold a Kubernetes
  service-account token.** KubeVirt VNC, screenshot, start, and stop are
  Kubernetes-authenticated subresources, so there is no way to relay a console
  without one. This is a recorded, deliberate deviation from the "no Kubernetes
  credential in the data plane" rule; see [ADR 0007](adr/0007-personal-desktop.md).
  A compromised Gateway can start, stop, screenshot, and drive that one VM — and
  nothing else.
- **The desktop VM gets no NetworkPolicy.** The operator renders policies around
  the Gateway and around the runtime, never around the VM's launcher Pod, so the
  Guest Desktop Agent's port 9876 is reachable from anywhere your cluster's own
  default policy already permits, and the guest bearer token is the only thing
  defending it. KubeVirt's own guidance is that a VMI is reachable from other
  endpoints by default and therefore needs a NetworkPolicy. If you want that port
  closed, write one yourself selecting `kubevirt.io/domain=<instance>-desktop` in
  the desktop namespace.
- **The desktop is not a Restricted Workload.** KubeVirt relabels its namespace
  to privileged Pod Security and runs `virt-launcher` with privileges you do not
  control from here. The VM is administrator-owned virtualization infrastructure,
  not a certified sandbox.
- **The guest session is logged in and unlocked by design.** Anyone who reaches
  the console reaches a live desktop session with its files, browser profile,
  and signed-in accounts. Screen lock is deliberately disabled so the agent can
  work; the tailnet grant and `owner.subject` are the real access control.
- **Screenshots can contain anything on screen**, including passwords and
  personal data. They flow into the model's context. Do not sign the desktop
  into accounts whose exposure you would not accept.
- **Tokens are generated once and never rotated by the operator, and the
  guest's copies cannot be rotated in place in v1.** The three values in
  `<instance>-desktop-tokens` are 64 hex characters from a CSPRNG, and they are
  retained when the feature is disabled so that re-enabling resumes the same
  disk. The guest receives `guest-token`, and on Windows `windows-password`,
  only while it is being provisioned. On Linux that is the cloud-init
  `write_files` module, which runs once per instance ID. On Windows it is the
  sysprep answer file, which sets the account password and autologon during the
  specialize and oobeSystem passes, plus the `setup.ps1` it starts from
  `FirstLogonCommands`; Windows reads an answer file only on the first boot
  after sysprep, so restarting an already-specialized guest re-reads nothing.
  Deleting the Secret therefore regenerates all three values, the Gateway comes
  back holding the new `guest-token`, and the running guest goes on requiring
  the old one: every screenshot, action, launch, and window listing returns 401,
  and the desktop is recoverable only by rebuilding it from the golden image.
  The plugin's bearer is the one value you can replace, because it never reaches
  the guest — change only the `agent-token` key, then restart the Gateway
  Deployment and the runtime StatefulSet so both re-read it.
- **State is in memory in a single replica.** Input leases and the power
  quarantine live in the Gateway process. It runs one replica with the
  `Recreate` strategy; a restart changes `gatewayBootID`, which is how clients
  detect that earlier frames and leases are void. There is no durable ledger
  yet, so a restart during an uncertain power operation forgets that
  uncertainty. Re-read the VM state before assuming anything.
- **Nothing here is a backup.** The root disk is retained, not protected. Use
  KubeVirt or CSI snapshots if the desktop's contents matter.

## Migrating a desktop from the proof of concept

The earlier `experiments/personal-desktop-poc/` prototype provisioned its VM and
root DataVolume from shell-rendered manifests, naming resources after a digest
of the owner tuple: `pd-` followed by the first 20 lowercase hexadecimal
characters of an HMAC over `(issuer, subject, Instance UID)`, with `-root`
appended for the disk. Product resources are named after the Instance instead, so
the operator will not find that disk on its own. Adopt it explicitly:

```yaml
spec:
  personalDesktop:
    enabled: true
    namespace: personal-desktop-poc   # the namespace the PoC disk lives in
    macAddress: 72:23:c8:e8:e9:be     # the address the disk was provisioned with
    rootVolume:
      existingDataVolume: pd-9f2c1a4b7de03e51ab77-root
```

`macAddress` covers a PoC disk whose guest configured itself before the
operator existed. The operator now renders a `networkData` document that matches
the interface by name (`en*`) rather than by hardware address, so a desktop it
provisioned survives any address KubeVirt assigns — including across the power
cycles that are a desktop's normal lifecycle. A PoC disk predates that: its
netplan was generated by cloud-init's fallback and pins the address the guest
first saw. Bring such a disk back on a different address and the guest finds no
interface it recognizes and comes up with no network at all, which looks like a
broken desktop rather than a configuration mismatch. Read the address off the
PoC VM before deleting it:

```sh
kubectl -n <ns> get vm <poc-vm> \
  -o jsonpath='{.spec.template.spec.domain.devices.interfaces[0].macAddress}'
```

Leave `macAddress` unset for any desktop cloned from a golden image. The
operator's `networkData` matches by interface name, so the guest needs no
address to be stable, and pinning one only creates a value you must never
change.

With `existingDataVolume` set, the operator does not create, resize, or delete
the root disk. `onInstanceDeletion` is ignored for an adopted volume — an
adopted disk is never deleted by the operator, on the assumption that whoever
created it still owns its lifecycle.

Migration steps:

1. Record the PoC VM's interface MAC address, then stop the VM and confirm no
   VMI is running against the disk. Two VMs must never attach the same
   `ReadWriteOnce` volume, and once the VM object is gone the address it used is
   gone with it.
2. Delete the PoC `VirtualMachine`, Service, Deployment, and Ingress. Leave the
   DataVolume and its PVC alone. Verify the PoC VM was not created with
   `dataVolumeTemplates`, which would delete the disk along with the VM.
3. Set `image.goldenDataVolume` to any golden DataVolume that exists in the
   namespace — it is required by the API, and with `existingDataVolume` set it is
   not cloned.
4. Add the block above and apply the Instance.
5. Watch `status.personalDesktop.phase` reach `Ready`, open the console, and
   confirm your files and browser profile are there.
6. Only then remove the PoC's tokens and manifests.

The guest side is upgraded in place on first boot: the operator's cloud-init or
sysprep material replaces the PoC's agent and token. A Linux PoC guest keeps
whatever you installed in it, so expect the first boot after adoption to reboot
once.

## Troubleshooting

Start with the Instance's status:

```sh
kubectl get typeclawinstance alice-agent -o jsonpath='{.status.personalDesktop}' | jq
kubectl get typeclawinstance alice-agent \
  -o jsonpath='{.status.conditions[?(@.type=="PersonalDesktopReady")]}' | jq
```

`kubectl get typeclawinstance` also prints a `Desktop` column carrying
`status.personalDesktop.phase`.

### Phases

| Phase | Meaning | What to look at |
|---|---|---|
| `Disabled` | The feature is off, or the block is absent | Nothing is provisioned; the root disk and tokens are retained |
| `Pending` | Enabled, but the operator cannot proceed yet | The condition reason: usually `KubeVirtUnavailable` or `RuntimeTooOld` |
| `Provisioning` | Images or the VM are still coming up | `goldenImagePhase`, `rootVolumePhase`, `vmPrintableStatus` |
| `Ready` | Gateway ready and the root volume `Succeeded` | `consoleURL`, `gatewayReady` |
| `Degraded` | Provisioned, but something is not healthy | `message`, then the Gateway Pod's logs |
| `Deleting` | Cleanup is running under the finalizer | Cross-namespace objects are removed before the finalizer is dropped |

### Condition reasons

| Reason | Cause | Fix |
|---|---|---|
| `Disabled` | `enabled: false` or no block | Expected; nothing to do |
| `KubeVirtUnavailable` | The KubeVirt or CDI CRDs are not present | Install KubeVirt and CDI; the operator never installs them |
| `RuntimeTooOld` | Effective `spec.runtime.version` is below 0.52.0 | Raise the runtime version, or set an explicit `spec.runtime.image` |
| `Provisioning` | Images, VM, or Gateway are still converging | Wait, then read the phase fields |
| `Ready` | Gateway ready and root volume `Succeeded` | — |
| `Error` | A step failed | `status.personalDesktop.message`, then the operator logs |

### Common symptoms

**`goldenImagePhase` stays at `ImportInProgress` or fails.** CDI is fetching the
image. Check the importer Pod in the desktop namespace. A checksum mismatch, a
non-HTTPS URL, or a StorageClass without enough space are the usual causes.

**`rootVolumePhase` stays at `CloneScheduled` or `CloneInProgress`.** The
StorageClass has to support cloning the golden PVC. Confirm the golden
DataVolume reached `Succeeded` first, and that the target size is not smaller
than the source.

**`vmPrintableStatus` is `Provisioning` or `Starting` forever.** KubeVirt holds
VMI start until the referenced DataVolume finishes. After that, look for
scheduling failures: a desktop VM needs a KVM-capable node, and
`nodeSelector` may be excluding every candidate.

**`gatewayReady` is false.** Look at the Gateway Deployment in the desktop
namespace, in this order: whether the Pod pulled its image and was scheduled,
whether the ServiceAccount, Role, and RoleBinding have been reconciled yet, and
then the Pod's log. The Gateway refuses to start when required environment is
missing — the desktop name and namespace and the owner identity are mandatory —
and it names the missing variable on the way out. Its readiness probe is
`/healthz` on the agent port, so a Pod that runs but never becomes ready points
at that port rather than at configuration.

**`consoleURL` is empty although `access.tailscale` is set.** The Tailscale
operator has not published a hostname on the Ingress yet. Check that the
operator is installed, that its OAuth client is allowed to own the tags you
listed, and that the Ingress has `status.loadBalancer.ingress[0].hostname`.

**The console loads but rejects you.** The Gateway compares
`Tailscale-User-Login` against `owner.subject` and `allowedLogins`. Confirm you
are hitting the tailnet hostname rather than the Service directly, that
`X-Forwarded-Proto` is `https`, and that the login string matches exactly.

**The agent says the desktop is quarantined.** A start or stop ended with an
unknown outcome, so the Gateway refuses further power changes until someone
recovers explicitly. Read the VM's real state with `kubectl get vm`, then use the
console's "Start / recover" action or `/desktop start`. Definite rejections are
not quarantined: a `Forbidden` or `BadRequest` start or stop is reported as
`Rejected`, and stopping an already-stopped `Manual` VM is an idempotent success.

**The agent's actions are rejected.** Someone holds control. `/desktop status`
names the current Input Controller; `/desktop release` gives the lease back, and
the console's "Hand back" does the same from the human side. After any handover
the agent has to take a fresh screenshot before acting again.
