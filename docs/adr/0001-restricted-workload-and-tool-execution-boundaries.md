---
status: accepted
---

# Use Restricted workloads with independent runtime and tool-execution boundaries

Every TypeClaw-owned Pod will conform to the Kubernetes Restricted Pod Security Standard, while privileged node-security infrastructure such as the Security Profiles Operator remains an administrator-installed dependency outside that boundary. Restricted admission is only the workload floor: Runtime Isolation and the Tool Execution Environment remain independent because isolating a Pod from its node does not separate model-controlled tools from the trusted TypeClaw runtime.

Runtime Isolation exposes `Native`, `GVisor`, and `Auto`; the Tool Execution Environment independently exposes `LocalBwrap` and `RemoteSandbox`. The v1 acceptance baseline is `Native + LocalBwrap`, using upstream TypeClaw `bwrap` and an immutable Localhost seccomp profile. The Tool Execution Environment is declared separately; `Auto` resolves only Runtime Isolation before first activation, preferring `GVisor` over `Native` when the pairing with that declared environment is administrator-allowed and fully certified. It does not reconsider that choice after activation.

Remote execution is a separate Tool Execution Environment, not a Kubernetes-specific fork of TypeClaw. TypeClaw owns a platform-neutral execution and filesystem capability seam, while the operator owns the out-of-process provider and the capability boundary between a Sandbox Broker data plane and a Kubernetes-facing Sandbox Reconciler control plane, including an Agent Sandbox adapter. A production remote environment requires a certified sandbox RuntimeClass such as gVisor or Kata, receives neither a live Agent Folder mount nor Kubernetes credentials, and exchanges only an Authorized Workspace View and a validated output delta.

A Security Binding durably records the selected Runtime Isolation and Tool Execution Environment identities when an Instance is first activated. Artifact, environment, or binding-preserving policy changes create a Security Epoch that revalidates those same identities. Changing either identity is instead an explicit administrative Security Binding transition that requires quiescence and successful preflight; no failure, restart, reschedule, upgrade, or restore may select another identity automatically.

## Considered Options

- A privileged TypeClaw workload, root ownership init container, or operator-installed privileged seccomp DaemonSet was rejected because it would make Restricted operation an exception rather than the product floor. Existing cluster infrastructure should own node-level profile installation.
- Whole-Pod gVisor alone was rejected as the tool-security boundary because model-controlled code would still share the trusted runtime's files, credentials, and process authority.
- An operator-specific fork or replacement of TypeClaw's `bwrap` path was rejected because it would couple workload semantics to Kubernetes and create a second upstream execution model.
- Automatic identity reselection after activation and security-control fallback were rejected because a capacity or compatibility failure must not change a previously established security claim.
- Mounting the live Agent Folder into a remote sandbox was rejected because it breaks single-writer, revision-conflict, restore, and least-authority guarantees.

## Consequences

- Every Pod hosting an operator, broker, reconciler, canary, runtime, or sandbox component must satisfy the Restricted Workload floor; only separately administered node-security infrastructure may require privilege.
- No Restricted Workload uses a root ownership repair path. Storage is supported only when non-root ownership and access are proven through `fsGroup` or CSI `VOLUME_MOUNT_GROUP`; incompatible or root-owned restored volumes fail closed.
- Localhost seccomp profiles and image/provider inputs are immutable, signed, digest-pinned, release-coupled compatibility artifacts. A profile is valid only for its certified managed-runtime and dependency tuple; production recording, automatic profile learning, and automatic syscall widening are forbidden. Kernel, CRI, RuntimeClass handler, node, and storage behavior are observed environment evidence rather than artifact provenance.
- Canary evidence and node eligibility are role-specific. Managed Runtime nodes prove the architecture, boot/runtime fingerprint, managed-runtime and seccomp identities, and required Agent Folder storage and runtime-network behavior; remote sandbox nodes separately prove the sandbox image/provider protocol, certified RuntimeClass handler, and sandbox network/resource contract. Unknown or stale evidence is a failure, evidence is not reused across roles, and only nodes passing the relevant role's canary are eligible; reboot or relevant runtime/profile change invalidates prior evidence.
- Failure may not degrade Localhost seccomp to `RuntimeDefault` or `Unconfined`, GVisor to Native isolation, real `/proc` isolation to an alternate unsafe strategy, enforced network policy to unrestricted egress, or non-root storage to privileged repair. It blocks execution and readiness without fallback or a restart loop.
- Model-controlled process and file operations must pass through the Tool Execution Environment contract. Approved plugins are trusted supply-chain components and may use only the provided execution and filesystem capabilities for model-controlled work.
- Runtime, broker, sandbox, and tool-execution surfaces receive no Kubernetes credential or service-account token. A reconciler or operator that needs Kubernetes access uses narrowly scoped RBAC and never passes that authority into the data plane.
- Remote execution uses session-scoped Sandbox Leases, per-invocation authorization, default-deny network authority, explicitly granted non-Kubernetes credentials, bounded resources and output, durable outcome records, and fail-closed cancellation and unknown-outcome handling.
- The v1 release is blocked on executable `Native + LocalBwrap` evidence under the exact rootless managed-runtime artifacts on amd64 and arm64. `RemoteSandbox` and `GVisor + LocalBwrap` remain experimental until their complete contracts are independently certified; production RemoteSandbox additionally requires a certified gVisor- or Kata-class RuntimeClass and never falls back to native runc.
