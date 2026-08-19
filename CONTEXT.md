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
