# typeclaw-operator

A Kubernetes operator for declaratively operating isolated TypeClaw Instances.
Each TypeClaw Instance gets its own Managed Runtime, durable Agent Folder,
restricted pod security posture, and enforced Network Authority — see
[CONTEXT.md](CONTEXT.md) for the domain vocabulary.

## Install

Apply the CRDs, then install the operator with Helm:

```sh
kubectl apply -f config/crd/bases
helm upgrade --install typeclaw-operator charts/typeclaw-operator \
  --namespace typeclaw-system --create-namespace
```

Container images are published to `ghcr.io/fml09/typeclaw-operator` on every
push to `main` (`latest`, `<commit-sha7>` multi-arch amd64/arm64).

## Try it

Deploy a sample TypeClaw Instance:

```sh
kubectl apply -f config/samples/typeclaw_v1alpha1_typeclawinstance.yaml
```

## Personal Desktop

A TypeClaw Instance can own a **Personal Desktop**: a persistent KubeVirt
virtual machine that its single human owner opens in a browser over Tailscale
and that the agent drives through typed actions on the same screen. The root
disk is retained across sessions and power cycles, Linux and Windows guests are
both supported, and only one party — the human or the agent — holds input at a
time. KubeVirt, CDI, and the Tailscale Kubernetes operator are administrator-owned
prerequisites that this operator uses but never installs. See the
[Personal Desktop guide](docs/personal-desktop.md).

## Design decisions

Architecture and security boundaries are recorded as ADRs:

- [ADR-0001: Restricted Workload and Tool Execution boundaries](docs/adr/0001-restricted-workload-and-tool-execution-boundaries.md)
- [ADR-0002: SPIFFE workload identity and credential execution](docs/adr/0002-spiffe-workload-identity-and-credential-execution.md)
- [ADR-0003: Consume forked Managed Runtime images](docs/adr/0003-consume-fork-managed-runtime-images.md)
- [ADR-0004: Implement parity capabilities](docs/adr/0004-implement-parity-capabilities.md)
- [ADR-0005: SelfConfig as observed declarations](docs/adr/0005-selfconfig-observed-declarations.md)
- [ADR-0006: Per-environment seccomp deviation](docs/adr/0006-per-environment-seccomp-deviation.md)
- [ADR-0007: Personal Desktop as a first-class capability](docs/adr/0007-personal-desktop.md)

## License

Apache License 2.0 — see [LICENSE](LICENSE).
