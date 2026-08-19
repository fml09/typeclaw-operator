---
status: accepted
---

# Authenticate workload capabilities with SPIFFE and isolate credential execution

TypeClaw Operator will use SPIFFE X.509-SVID mTLS for workload authentication at managed capability boundaries while keeping human authorization and Kubernetes reconciliation in the control plane. SPIRE remains administrator-owned infrastructure: the baseline `Native + LocalBwrap` TypeClaw Instance does not require it, while production `RemoteSandbox` and Credential Runner capabilities require a certified SPIRE deployment and fail closed without one.

Runtime identities are stable for one TypeClaw Instance and Security Epoch. Each Credential Runner receives an invocation-specific identity rendered from administrator-owned `ClusterSPIFFEID` templates and Kubernetes-attested Pod metadata. The Runtime, capability data plane, sandbox, and Credential Runner receive no Kubernetes credential or service-account token; only the reconciler uses narrowly scoped Kubernetes RBAC. Kubernetes tokens remain appropriate for a human administrator using `kubectl`, but they are not a workload-authentication fallback.

Credential execution is an out-of-process Tool Execution Environment capability. A TypeClaw-facing Platform Extension supplies tools and skills, a capability gateway authenticates intent, a Kubernetes reconciler creates the workload, and a fresh Restricted Credential Runner executes a signed, digest-pinned image. The Runner receives neither the live Agent Folder nor runtime state. It may receive an Authorized Workspace View and a bounded output delta, together with only the declared same-namespace immutable Secret or version-pinned CSI projection. The operator never reads or transports credential plaintext.

Every executable plugin in a credential-enabled TypeClaw Instance is administrator-approved and distributed through a signed, digest-pinned OCI Platform Bundle; the complete Platform Bundle set belongs to the Security Epoch. A Credential Consumer is a policy and audit classification rather than a cryptographic plugin identity because in-process TypeClaw plugins share one process and workload identity. Protocol-specific HTTP, database, or SSH credential proxies remain user-owned Platform Extensions or external SPIFFE-authenticated services rather than operator features.

Credential access supports `Deny`, `Confirm`, `PreAuthorized`, and `Bypass`. `Bypass` omits the human decision only: cluster policy and the Credential Consumer must both enable it, and it never bypasses the Security Epoch, bundle and Secret bindings, destination policy, TTL, resource limits, cleanup, audit, or immutable system destinations. Opaque Credential Use may claim that model-controlled code never received the credential bytes; Raw Credential Disclosure, including arbitrary shell execution, is an explicit delegation with no non-disclosure claim.

`Confirm` uses an immutable controller-created `CredentialRequest` and a separate append-only `CredentialApproval` created by a Kubernetes-authenticated administrator. The Issue-like metadata record contains only a digest and redacted summary; the exact intent is encrypted in a KMS-backed store and is inspected through the approval control plane. The v1 TypeClaw integration returns a pending request and consumes a one-shot grant on an exact retry. A future TypeClaw `ApprovalProvider` may add durable session suspension and resumption, but it will adapt this authorization state machine rather than create another authority.

Ordinary public-web authority may use an administrator-selected allow list or a default deny list, always intersected with an immutable denial set covering Kubernetes, cloud metadata, node, link-local, loopback, and control-plane destinations. Credential-bearing execution always uses a destination allow list. Kubernetes NetworkPolicy is only the L3/L4 floor; domain, DNS-rebinding, redirect, and CONNECT/SNI enforcement belongs to an administrator-owned Network Authority behind an operator adapter. An unavailable or inexact adapter blocks the capability rather than granting unrestricted egress.

Credential execution is at most once after external side effects may have begun. Lost results become `UnknownOutcome`, cancellation revokes the execution ticket before workload termination, and no automatic retry creates another Runner. Durable records contain identities, bindings, digests, state transitions, exit classification, and bounded output metadata, but never credential values, raw output, or sensitive command bytes.

## Considered Options

- Kubernetes projected workload tokens were rejected as a fallback because enabling a weaker authentication path would make the capability's identity claim depend on runtime availability rather than declared policy.
- An in-process helper, plugin hook, or shared sidecar was rejected as the security authority because TypeClaw plugins share the Runtime's process, filesystem, network, and SPIFFE identity, and plugin hook failures are not a fail-closed isolation boundary.
- An operator-owned HTTP, database, or SSH credential proxy was rejected because protocol semantics belong in user-owned extensions; the operator owns generic authorization, workload creation, and policy enforcement only.
- Plaintext intent in a Kubernetes custom resource and approver writes to controller-owned status were rejected because they expose sensitive commands and conflate human decisions with reconciliation outcome.
- Kubernetes NetworkPolicy alone was rejected for destination policy because its additive L3/L4 allow model cannot represent domain deny lists, redirects, or DNS-rebinding checks.

## Consequences

- Production RemoteSandbox and Credential Runner activation is conditional on certified SPIRE, Network Authority, KMS, workload image, Secret projection, and result-channel evidence for the active Security Epoch.
- SPIRE Controller Manager, Secret-store providers, and egress enforcement remain administrator-installed infrastructure. The operator detects and consumes them but does not install privileged or cluster-wide security dependencies.
- Credential-enabled Instances cannot load mutable or agent-authored executable plugins. Agent-authored skills remain untrusted prompt content and cannot alter Platform Bundles, Credential Consumers, approval policy, or Network Authority.
- Secret rotation creates a new immutable binding and invalidates pending or reusable grants without changing the Instance's Runtime Isolation or Tool Execution Environment identity.
- No restart, outage, missing dependency, or failed conformance check may fall back to Kubernetes workload tokens, mutable plugins, native execution, plaintext secret transport, unrestricted egress, or automatic replay.
