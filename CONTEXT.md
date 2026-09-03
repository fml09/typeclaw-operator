# TypeClaw Operator

The domain for declaratively operating isolated TypeClaw agents on Kubernetes.

## Language

**TypeClaw Instance**:
An independently managed TypeClaw agent with its own runtime identity and Agent Folder.
_Avoid_: Bot, deployment

**Agent Folder**:
The self-contained per-agent directory that carries authored configuration and durable agent history across runtime replacement.
_Avoid_: Workspace, home directory

**Managed Runtime**:
A TypeClaw runtime whose lifecycle and durable platform capabilities are supplied by the hosting platform.
_Avoid_: Host mode, Kubernetes mode

**Capability Parity**:
The same operational outcome as the architectural reference, expressed through TypeClaw-native concepts rather than identical APIs or resources.
_Avoid_: Schema parity, feature copy

**Restricted Workload**:
A TypeClaw-owned Kubernetes workload that conforms to the Kubernetes Restricted Pod Security Standard. Separately administered node-security infrastructure is outside this boundary.
_Avoid_: Restricted cluster, sandbox

**Runtime Isolation**:
The boundary that separates a TypeClaw runtime from the node and from other workloads. It is independent of the Tool Execution Environment.
_Avoid_: Tool sandbox, execution backend

**Tool Execution Environment**:
The boundary through which model-controlled tools receive process, filesystem, credential, and network authority. It is independent of Runtime Isolation.
_Avoid_: Runtime sandbox, shell runner

**Security Binding**:
The durable assignment of a TypeClaw Instance to specific Runtime Isolation and Tool Execution Environment identities. It changes only through an explicit administrative transition.
_Avoid_: Runtime selection, automatic fallback

**Security Epoch**:
An immutable validation context for a Security Binding under one certified set of artifacts, environment evidence, and binding-preserving policy. A new epoch revalidates the existing binding rather than selecting different identities.
_Avoid_: Backend version, automatic reselection

**Sandbox Lease**:
The session-scoped lifetime of a remote Tool Execution Environment, including its isolated scratch state. It is not shared across TypeClaw Instances or sessions.
_Avoid_: Sandbox Pod, shared worker

**Authorized Workspace View**:
A revisioned, explicitly bounded view of Agent Folder state made available to a remote Tool Execution Environment. Changes return as a validated delta rather than through a live Agent Folder mount.
_Avoid_: Agent Folder mount, shared workspace

**Personal Desktop**:
A persistent, single-owner interactive desktop bound to one TypeClaw Instance. Its durable state and its ownership outlive every session, which is what separates it from a Sandbox Lease.
_Avoid_: Persistent sandbox, desktop lease

**Desktop Gateway**:
The single authority that mediates every access to one Personal Desktop, for the human owner and for the agent alike. No other party reaches that desktop's console or its typed-action surface.
_Avoid_: VNC proxy, desktop broker

**Desktop Console**:
The human owner's authenticated view of a Personal Desktop, published through an administrator-declared access provider that asserts the viewer's identity. It is never reachable without that assertion, and what makes the assertion unforgeable is that nothing can reach the listener except the provider itself. An identity header is only as good as that reachability guarantee.
_Avoid_: VNC URL, noVNC page

**Guest Desktop Agent**:
The boundary inside a Personal Desktop's interactive session that executes typed actions and captures the screen. Its protocol is OS-agnostic; whichever platform backend satisfies it is outside the boundary.
_Avoid_: Automation script, VNC server

**Input Controller**:
The one party, human or agent, that currently holds exclusive input authority over a Personal Desktop. Authority transfers through an explicit handover and is never held concurrently.
_Avoid_: Active session, shared control

**Platform Extension**:
An administrator-owned immutable extension that adds trusted tools, skills, helpers, schemas, or credential-use declarations to a TypeClaw Instance.
_Avoid_: System plugin, agent plugin

**Platform Bundle**:
A signed, digest-pinned OCI artifact that distributes one or more Platform Extensions and their executable or declarative assets.
_Avoid_: Plugin package, mutable extension

**Credential Consumer**:
An administrator-declared policy and audit classification for one bounded use of a credential. It does not identify or isolate an in-process plugin.
_Avoid_: Credential principal, secret owner

**Credential Runner**:
A one-invocation Restricted Workload that executes an approved Credential Consumer with only its granted credential, Authorized Workspace View, resources, and Network Authority.
_Avoid_: Secret Pod, credential proxy

**Opaque Credential Use**:
A credential use in which model-controlled code receives only a typed result or opaque handle and never receives the credential bytes.
_Avoid_: Hidden environment variable

**Raw Credential Disclosure**:
An explicit delegation of credential bytes to model-controlled code. It carries no claim that the recipient cannot read, transform, print, or exfiltrate those bytes.
_Avoid_: Safe secret injection

**Network Authority**:
The externally enforced destination policy granted to a runtime or Tool Execution Environment. It is unavailable when the selected enforcement adapter cannot represent the declared policy exactly.
_Avoid_: Best-effort egress filter, NetworkPolicy alone

**PublicWeb**:
The destination universe for ordinary web access: public DNS names and globally routable Internet addresses after excluding private, special-use, cluster, node, metadata, and control-plane destinations.
_Avoid_: Internet, unrestricted egress
