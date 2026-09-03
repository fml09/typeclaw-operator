package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DefaultRuntimeVersion is the managed runtime release consumed when neither
// spec.runtime.version nor spec.runtime.image pins an artifact. It tracks the
// fml09/typeclaw releases that publish the managed runtime image (ADR 0003).
const DefaultRuntimeVersion = "0.48.9"

// PersonalDesktopMinimumRuntimeVersion is the first managed runtime release
// that honours TYPECLAW_PLATFORM_EXTENSIONS. An older runtime would mount the
// computer-use Platform Extension and silently never load it, so the operator
// refuses to provision instead of producing a desktop nothing can drive.
const PersonalDesktopMinimumRuntimeVersion = "0.52.0"

// PersonalDesktopDefaultVirtioContainerDisk carries the virtio-win drivers and
// guest tools the Windows guest needs before it can see its own disk or NIC.
const PersonalDesktopDefaultVirtioContainerDisk = "quay.io/kubevirt/virtio-container-disk:20260902_5d403aa64a"

// PersonalDesktopDefaultPythonURL and PersonalDesktopDefaultPythonSHA256 pin
// the interpreter the Windows first-logon script installs when the golden
// image has none. The digest is verified in the guest before execution.
const (
	PersonalDesktopDefaultPythonURL    = "https://www.python.org/ftp/python/3.13.15/python-3.13.15-amd64.exe"
	PersonalDesktopDefaultPythonSHA256 = "edec09c4853aeae9ac36efb8c9f95b6b8e2fee65eee56d9767a8b7c69c574403"
)

// ConditionPersonalDesktopReady reports that the Desktop Gateway is serving
// and the desktop's root volume finished cloning.
const ConditionPersonalDesktopReady = "PersonalDesktopReady"

// Reasons carried by ConditionPersonalDesktopReady.
const (
	// PersonalDesktopReasonDisabled means the feature is off; the root disk
	// and token Secret are deliberately retained.
	PersonalDesktopReasonDisabled = "Disabled"
	// PersonalDesktopReasonKubeVirtUnavailable means the cluster has no
	// KubeVirt or CDI CRDs, so nothing can be provisioned here.
	PersonalDesktopReasonKubeVirtUnavailable = "KubeVirtUnavailable"
	// PersonalDesktopReasonRuntimeTooOld means the effective runtime release
	// predates PersonalDesktopMinimumRuntimeVersion.
	PersonalDesktopReasonRuntimeTooOld = "RuntimeTooOld"
	// PersonalDesktopReasonProvisioning means the desktop objects are applied
	// but the Gateway or the root volume has not converged yet.
	PersonalDesktopReasonProvisioning = "Provisioning"
	// PersonalDesktopReasonReady means the desktop is usable.
	PersonalDesktopReasonReady = "Ready"
	// PersonalDesktopReasonError means the declared desktop cannot be applied.
	PersonalDesktopReasonError = "Error"
)

// Values of PersonalDesktopStatus.Phase.
const (
	PersonalDesktopPhaseDisabled     = "Disabled"
	PersonalDesktopPhasePending      = "Pending"
	PersonalDesktopPhaseProvisioning = "Provisioning"
	PersonalDesktopPhaseReady        = "Ready"
	PersonalDesktopPhaseDegraded     = "Degraded"
	PersonalDesktopPhaseDeleting     = "Deleting"
)

// VolumeClaimSpec declares one durable volume backed by a PVC template.
type VolumeClaimSpec struct {
	// Requested capacity of the persistent volume.
	// +kubebuilder:default="5Gi"
	Size resource.Quantity `json:"size"`
	// StorageClass to request. Unset means the cluster default class.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
}

// StorageSpec owns the durable volumes of a TypeClaw Instance. Credential
// bytes for Opaque Credential Use are never sourced from these volumes.
type StorageSpec struct {
	// AgentFolder provisions the PVC carrying authored configuration, Git
	// history, sessions, memory, and the public workspace. Kubernetes Secret
	// projections are never attached to this mount.
	// +kubebuilder:default={size: "5Gi"}
	AgentFolder VolumeClaimSpec `json:"agentFolder"`

	// RuntimeHome provisions optional runtime-owned state at /home/typeclaw.
	// The operator never populates it from a Kubernetes Secret; credential
	// operations use an out-of-process Credential Runner instead.
	// +optional
	RuntimeHome *VolumeClaimSpec `json:"runtimeHome,omitempty"`

	// OnInstanceDeletion decides what happens to the Instance's persistent
	// volumes when the TypeClawInstance is deleted. Retain (default) keeps
	// the Agent Folder recoverable; Delete removes it with the workload.
	// +kubebuilder:validation:Enum=Retain;Delete
	// +kubebuilder:default=Retain
	// +optional
	OnInstanceDeletion string `json:"onInstanceDeletion,omitempty"`
}

// BackupSpec configures scheduled snapshots of the public Agent Folder
// workspace. Credential files are outside this artifact by construction.
type BackupSpec struct {
	// Cron schedule (standard five-field crontab) driving snapshot Jobs.
	Schedule string `json:"schedule"`

	// SnapshotVolume provisions the destination volume receiving public
	// workspace snapshots.
	// +kubebuilder:default={size: "10Gi"}
	SnapshotVolume VolumeClaimSpec `json:"snapshotVolume"`

	// Retention is the maximum number of snapshots kept on the destination
	// volume; older snapshots are pruned after each successful run.
	// +kubebuilder:default=7
	Retention int32 `json:"retention,omitempty"`
}

// AutoUpdateSpec opts the Instance into managed runtime rollouts driven by
// registry tag polling. The controller never rewrites spec.runtime.version;
// promotion happens on the StatefulSet image with automatic rollback, so
// GitOps-authored specs stay authoritative about intent while status tracks
// what actually runs.
type AutoUpdateSpec struct {
	// Enabled turns on polling and rollout management.
	Enabled bool `json:"enabled"`

	// Track selects candidate versions: "latest", or a "<major>.<minor>"
	// prefix such as "0.48" restricting candidates to that minor stream.
	// +kubebuilder:validation:Pattern=`^(latest|[0-9]+\.[0-9]+)$`
	Track string `json:"track,omitempty"`

	// RequireFreshBackup blocks a rollout until a snapshot younger than
	// MaxBackupAgeHours exists. Ignored when backup is disabled or false.
	// +optional
	RequireFreshBackup bool `json:"requireFreshBackup,omitempty"`

	// MaxBackupAgeHours bounds freshness for RequireFreshBackup.
	// +kubebuilder:default=24
	MaxBackupAgeHours int32 `json:"maxBackupAgeHours,omitempty"`

	// ConfirmationTimeoutMinutes bounds how long a promoted version must
	// hold readiness before the controller rolls back to the previous image.
	// +kubebuilder:default=15
	ConfirmationTimeoutMinutes int32 `json:"confirmationTimeoutMinutes,omitempty"`
}

// NetworkSpec declares the externally enforced traffic policy rendered into
// NetworkPolicies for this Instance.
type NetworkSpec struct {
	// Egress selects the destination universe. PublicWeb permits public
	// DNS names and globally routable addresses after excluding private,
	// special-use, cluster, node, metadata, and control-plane destinations;
	// Unrestricted removes egress filtering entirely.
	// +kubebuilder:validation:Enum=PublicWeb;Unrestricted
	// +kubebuilder:default=PublicWeb
	Egress string `json:"egress,omitempty"`

	// IngressCIDRs lists additional CIDR ranges allowed to reach the server
	// port (TUI WebSocket and health endpoints). Same-namespace access is
	// always permitted.
	// +optional
	IngressCIDRs []string `json:"ingressCIDRs,omitempty"`
}

// RuntimeSpec selects the immutable Managed Runtime image.
type RuntimeSpec struct {
	// Release version of the managed runtime image
	// (ghcr.io/fml09/typeclaw-runtime:<version>). Ignored when Image is set.
	// +optional
	Version string `json:"version,omitempty"`

	// Full image reference overriding the registry+version pairing. Must be
	// version-pinned; floating tags are rejected by the workload contract.
	// +optional
	Image string `json:"image,omitempty"`

	// Timezone is an optional IANA timezone name injected into the Managed
	// Runtime as TZ. Empty preserves the image's default timezone (UTC in the
	// standard managed runtime image).
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._+-]+(/[A-Za-z0-9._+-]+)*$`
	// +optional
	Timezone string `json:"timezone,omitempty"`
}

// PersonalDesktopSpec provisions one persistent KubeVirt desktop for the
// Instance's single owner, a Desktop Gateway, and hydrates the computer-use
// Platform Extension into the Managed Runtime.
type PersonalDesktopSpec struct {
	// Enabled turns the feature on. false (or the whole block absent) removes
	// the VM, Gateway, console Ingress, and the extension mount, but keeps the
	// root DataVolume and the token Secret so re-enabling resumes the same disk.
	Enabled bool `json:"enabled"`

	// OS selects the guest template. Linux (default) uses the Ubuntu/XFCE
	// cloud-init template; Windows uses an administrator-built, sysprepped
	// golden image plus an unattend answer file.
	// +kubebuilder:validation:Enum=Linux;Windows
	// +kubebuilder:default=Linux
	OS string `json:"os,omitempty"`

	// Namespace hosting the VM, DataVolumes, Gateway, agent Service, and
	// console Ingress. Empty means the Instance namespace. KubeVirt relabels
	// the VM's namespace to pod-security enforce=privileged, so administrators
	// may prefer a dedicated namespace. Cross-namespace resources are cleaned
	// up by a finalizer, not owner references.
	Namespace string `json:"namespace,omitempty"`

	// Owner is the single human allowed to open the Desktop Console and the
	// identity the desktop is bound to.
	Owner PersonalDesktopOwnerSpec `json:"owner"`

	// Access declares how the Desktop Console is reached. Unset means the
	// console is not exposed (agent path still works).
	// +optional
	Access *PersonalDesktopAccessSpec `json:"access,omitempty"`

	// Image identifies the golden root image the desktop is cloned from.
	Image PersonalDesktopImageSpec `json:"image"`

	// RootVolume configures the per-desktop root disk.
	// +optional
	RootVolume PersonalDesktopRootVolumeSpec `json:"rootVolume,omitempty"`

	// Resources sizes the VM.
	// +optional
	Resources PersonalDesktopResourcesSpec `json:"resources,omitempty"`

	// NodeSelector pins the VM (KVM-capable nodes).
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Screen fixes the guest display geometry. Default 1280x800.
	// +optional
	Screen *PersonalDesktopScreenSpec `json:"screen,omitempty"`

	// MACAddress pins the desktop interface's hardware address. Leave it unset
	// for a desktop cloned from a golden image: KubeVirt then assigns one and
	// the guest's first boot writes network configuration to match.
	//
	// It matters when adopting a root disk that has already booted. A Linux
	// guest's persisted netplan matches its interface by MAC, so a disk that
	// comes back attached to a different address finds no interface it
	// recognizes and boots without networking. Set this to the address the
	// disk was provisioned with, and treat it as immutable for that disk's
	// lifetime.
	// +kubebuilder:validation:Pattern=`^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$`
	// +optional
	MACAddress string `json:"macAddress,omitempty"`

	// Linux holds Linux-only guest settings.
	// +optional
	Linux *PersonalDesktopLinuxSpec `json:"linux,omitempty"`

	// Windows holds Windows-only guest settings. Required when os=Windows.
	// +optional
	Windows *PersonalDesktopWindowsSpec `json:"windows,omitempty"`

	// Gateway overrides Desktop Gateway defaults.
	// +optional
	Gateway *PersonalDesktopGatewaySpec `json:"gateway,omitempty"`
}

// PersonalDesktopOwnerSpec names the one human bound to the desktop.
type PersonalDesktopOwnerSpec struct {
	// Issuer names the identity provider. Default https://login.tailscale.com
	// (the only provider the console supports in v1).
	// +kubebuilder:default="https://login.tailscale.com"
	Issuer string `json:"issuer,omitempty"`

	// Subject is the stable identifier the console compares against the
	// identity the access provider asserts. For Tailscale it is the login name
	// Tailscale sends as Tailscale-User-Login (for example alice@example.com).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	Subject string `json:"subject"`
}

// PersonalDesktopAccessSpec declares how the Desktop Console is published.
type PersonalDesktopAccessSpec struct {
	// Tailscale publishes the console through the Tailscale Kubernetes
	// operator (an Ingress with ingressClassName tailscale). Identity comes
	// from the Tailscale-User-Login header the operator proxy injects and
	// overwrites.
	// +optional
	Tailscale *PersonalDesktopTailscaleAccessSpec `json:"tailscale,omitempty"`
}

// PersonalDesktopTailscaleAccessSpec configures the tailnet-published console.
type PersonalDesktopTailscaleAccessSpec struct {
	// Hostname is the MagicDNS label (served as <hostname>.<tailnet>.ts.net).
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Hostname string `json:"hostname"`

	// Tags are applied to the proxy device via the tailscale.com/tags
	// annotation (for example tag:typeclaw-desktop). Access is then granted
	// with tailnet policy grants to that tag.
	// +optional
	Tags []string `json:"tags,omitempty"`

	// OperatorNamespace is where Tailscale proxy Pods run. It is meaningful
	// only in Ingress mode, where the console NetworkPolicy admits that
	// namespace. Default "tailscale".
	// +kubebuilder:default=tailscale
	OperatorNamespace string `json:"operatorNamespace,omitempty"`

	// Mode selects how the console reaches the tailnet, and with it what makes
	// the Tailscale-User-Login header trustworthy.
	//
	// Ingress publishes through the Tailscale Kubernetes operator. The console
	// listener binds the Pod network, and the only thing keeping other Pods off
	// it is a NetworkPolicy admitting OperatorNamespace. That is an honest
	// boundary only on a cluster whose CNI enforces NetworkPolicy. On a cluster
	// that does not — flannel, for one — the NetworkPolicy is stored and
	// enforced by nothing, and any Pod can forge the header and take the
	// console's exclusive input lease.
	//
	// Sidecar runs tailscaled in the Gateway Pod and binds the console to
	// loopback. Tailscale Serve attaches the identity headers itself and strips
	// client-supplied copies, and nothing outside that Pod's network namespace
	// can reach the listener at all. The guarantee is the kernel's, not a
	// policy engine's, so this mode is correct on every cluster. It costs one
	// tailnet credential, named by AuthSecret.
	// +kubebuilder:validation:Enum=Ingress;Sidecar
	// +kubebuilder:default=Ingress
	// +optional
	Mode string `json:"mode,omitempty"`

	// Image overrides the tailscaled image the Sidecar mode console runs.
	// Defaults to the image the Tailscale Kubernetes operator uses for its own
	// proxies, so one tailnet does not straddle two client versions.
	// +optional
	Image string `json:"image,omitempty"`

	// AuthSecret names a Secret in the desktop namespace holding the tailscaled
	// credential used in Sidecar mode: either a reusable auth key under
	// TS_AUTHKEY, or an OAuth client under TS_CLIENT_ID and TS_CLIENT_SECRET.
	// Required in Sidecar mode and ignored in Ingress mode.
	// +optional
	AuthSecret string `json:"authSecret,omitempty"`

	// AllowedLogins optionally widens console access beyond owner.subject.
	// Every entry is an exact Tailscale login name.
	// +optional
	AllowedLogins []string `json:"allowedLogins,omitempty"`
}

// PersonalDesktopImageSpec identifies the sealed golden root image.
type PersonalDesktopImageSpec struct {
	// GoldenDataVolume names the sealed golden root DataVolume (in the desktop
	// namespace) the root disk is cloned from. The operator never deletes it.
	// +kubebuilder:validation:MinLength=1
	GoldenDataVolume string `json:"goldenDataVolume"`

	// Import creates GoldenDataVolume from an HTTP source when it does not
	// exist yet (Linux cloud images). Windows golden images are built by an
	// administrator and are never imported here.
	// +optional
	Import *PersonalDesktopImageImportSpec `json:"import,omitempty"`
}

// PersonalDesktopImageImportSpec describes a CDI HTTP import of the golden
// image.
type PersonalDesktopImageImportSpec struct {
	// URL of the cloud image. HTTPS only.
	URL string `json:"url"`

	// Checksum in CDI form, for example sha256:<hex>. An unverified import
	// would make the desktop's whole root filesystem attacker-controlled.
	Checksum string `json:"checksum"`

	// Size of the imported golden volume.
	// +kubebuilder:default="32Gi"
	Size resource.Quantity `json:"size,omitempty"`

	// StorageClassName to request. Unset means the cluster default class.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
}

// PersonalDesktopRootVolumeSpec configures the per-desktop root disk.
type PersonalDesktopRootVolumeSpec struct {
	// Size of the cloned root disk.
	// +kubebuilder:default="32Gi"
	Size resource.Quantity `json:"size,omitempty"`

	// StorageClassName to request. Unset means the cluster default class.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`

	// ExistingDataVolume adopts an already provisioned root DataVolume (for
	// example a disk created by the earlier PoC). When set, the operator does
	// not create, resize, or delete the root disk.
	// +optional
	ExistingDataVolume string `json:"existingDataVolume,omitempty"`

	// OnInstanceDeletion: Retain (default) keeps the root disk when the
	// TypeClawInstance is deleted; Delete removes it. Never applied to an
	// adopted ExistingDataVolume.
	// +kubebuilder:validation:Enum=Retain;Delete
	// +kubebuilder:default=Retain
	OnInstanceDeletion string `json:"onInstanceDeletion,omitempty"`
}

// PersonalDesktopResourcesSpec sizes the desktop virtual machine.
type PersonalDesktopResourcesSpec struct {
	// CPUCores exposed to the guest.
	// +kubebuilder:default=2
	CPUCores int32 `json:"cpuCores,omitempty"`

	// Memory requested for the guest.
	// +kubebuilder:default="4Gi"
	Memory resource.Quantity `json:"memory,omitempty"`
}

// PersonalDesktopScreenSpec fixes the guest display geometry. The geometry is
// fixed rather than negotiated because the model reasons about pixel
// coordinates: a resolution that changes between screenshot and click would
// silently move the target.
type PersonalDesktopScreenSpec struct {
	// +kubebuilder:default=1280
	Width int32 `json:"width,omitempty"`
	// +kubebuilder:default=800
	Height int32 `json:"height,omitempty"`
}

// PersonalDesktopLinuxSpec holds Linux-only guest settings.
type PersonalDesktopLinuxSpec struct {
	// Username of the autologin desktop user.
	// +kubebuilder:default=desktop
	Username string `json:"username,omitempty"`

	// SSHAuthorizedKeys are optional test/maintenance keys for that user.
	// +optional
	SSHAuthorizedKeys []string `json:"sshAuthorizedKeys,omitempty"`
}

// PersonalDesktopWindowsSpec holds Windows-only guest settings.
type PersonalDesktopWindowsSpec struct {
	// Username of the interactive automation user created by the answer file.
	// +kubebuilder:default=desktop
	Username string `json:"username,omitempty"`

	// VirtioContainerDisk is the virtio-win driver/guest-tools containerDisk
	// attached as a CD-ROM.
	// +kubebuilder:default="quay.io/kubevirt/virtio-container-disk:20260902_5d403aa64a"
	VirtioContainerDisk string `json:"virtioContainerDisk,omitempty"`

	// PythonInstaller is downloaded by the first-logon setup script when the
	// golden image has no Python 3. Unset means the tracked python.org
	// release in PersonalDesktopDefaultPythonURL.
	// +optional
	PythonInstaller *PersonalDesktopWindowsInstallerSpec `json:"pythonInstaller,omitempty"`
}

// PersonalDesktopWindowsInstallerSpec pins one guest-side installer download.
type PersonalDesktopWindowsInstallerSpec struct {
	// URL of the installer. HTTPS only.
	URL string `json:"url"`

	// SHA256 the guest verifies before running the installer.
	SHA256 string `json:"sha256"`
}

// PersonalDesktopGatewaySpec overrides Desktop Gateway defaults.
type PersonalDesktopGatewaySpec struct {
	// Image overrides the Desktop Gateway image. Default is the operator image
	// (the gateway binary ships in it).
	// +optional
	Image string `json:"image,omitempty"`

	// AgentLeaseTTLSeconds is the idle TTL of the agent input lease.
	// +kubebuilder:default=120
	AgentLeaseTTLSeconds int32 `json:"agentLeaseTTLSeconds,omitempty"`
}

// PersonalDesktopStatus reports the observed state of one Personal Desktop.
type PersonalDesktopStatus struct {
	// Phase is Disabled, Pending, Provisioning, Ready, Degraded, or Deleting.
	Phase string `json:"phase,omitempty"`

	// DesktopName is the VirtualMachine name.
	DesktopName string `json:"desktopName,omitempty"`

	// Namespace hosting the desktop objects.
	Namespace string `json:"namespace,omitempty"`

	// GoldenImagePhase mirrors the golden DataVolume's CDI phase.
	GoldenImagePhase string `json:"goldenImagePhase,omitempty"`

	// RootVolumePhase mirrors the root DataVolume's CDI phase.
	RootVolumePhase string `json:"rootVolumePhase,omitempty"`

	// VMPrintableStatus mirrors the VirtualMachine's printable status.
	VMPrintableStatus string `json:"vmPrintableStatus,omitempty"`

	// GatewayReady reports whether the Desktop Gateway has a ready replica.
	GatewayReady bool `json:"gatewayReady,omitempty"`

	// ConsoleURL is the published Desktop Console address, taken from the
	// console Ingress load-balancer status.
	ConsoleURL string `json:"consoleURL,omitempty"`

	// Message carries human-readable provisioning context.
	Message string `json:"message,omitempty"`
}

// TypeClawInstanceSpec declares the operational policy the operator enforces
// for one TypeClaw Instance. Runtime-owned non-credential state (sessions,
// Git history, and memory) lives in the Agent Folder; Opaque Credential Use is
// declared separately through CredentialPolicy.
type TypeClawInstanceSpec struct {
	// Runtime selects the managed runtime image.
	// +optional
	Runtime RuntimeSpec `json:"runtime,omitempty"`

	// Storage owns the durable volumes.
	// +optional
	Storage StorageSpec `json:"storage,omitempty"`

	// Suspend scales the workload to zero without deleting the Agent Folder.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// ExposeTUI publishes a ClusterIP Service for the TUI WebSocket on the
	// fixed runtime port 8973. Unset means exposed; only an explicit false
	// removes the Service.
	// +optional
	ExposeTUI *bool `json:"exposeTUI,omitempty"`

	// RestartRelay runs a same-Pod platform sidecar consuming the upstream
	// file-spool restart contract and actuating Pod replacement through
	// narrowly-scoped RBAC. Unset means enabled.
	// +optional
	RestartRelay *bool `json:"restartRelay,omitempty"`

	// Backup configures scheduled Agent Folder snapshots. Unset disables
	// scheduled backups.
	// +optional
	Backup *BackupSpec `json:"backup,omitempty"`

	// AutoUpdate manages managed-runtime rollouts from registry tags.
	// +optional
	AutoUpdate *AutoUpdateSpec `json:"autoUpdate,omitempty"`

	// Network declares ingress/egress policy. Unset means PublicWeb egress
	// with same-namespace-only ingress to the server port.
	// +optional
	Network NetworkSpec `json:"network,omitempty"`

	// Security selects deviations from the Restricted Workload baseline
	// (ADR 0001). The default is the certified Localhost seccomp profile;
	// anything weaker is an explicit, recorded environment decision.
	// +optional
	Security *SecuritySpec `json:"security,omitempty"`

	// SelfConfig turns on observation of agent-authored typeclaw.json edits
	// (ADR 0005). Unset means the Agent Folder stays opaque.
	// +optional
	SelfConfig *SelfConfigSpec `json:"selfConfig,omitempty"`

	// CredentialPolicy declares the only credential that Credential Runners
	// may use for this Instance. The Managed Runtime never receives this
	// reference or a Secret projection.
	// +optional
	CredentialPolicy *CredentialPolicySpec `json:"credentialPolicy,omitempty"`

	// PersonalDesktop provisions the Instance owner's persistent KubeVirt
	// desktop and the computer-use Platform Extension. Unset means no desktop.
	// +optional
	PersonalDesktop *PersonalDesktopSpec `json:"personalDesktop,omitempty"`
}

// SecuritySpec declares workload security-envelope selections. Anything
// below the Localhost baseline weakens kernel-level tool isolation and MUST
// be a documented environment decision (operator ADR 0001/0006).
type SecuritySpec struct {
	// SeccompProfile selects the container seccomp posture. Localhost
	// (default) requires the administrator-installed bubblewrap-admitting
	// profile at SeccompLocalhostProfile; Unconfined admits every syscall
	// and exists for environments that cannot host profiles yet.
	// +kubebuilder:validation:Enum=Localhost;Unconfined
	// +kubebuilder:default=Localhost
	SeccompProfile string `json:"seccompProfile,omitempty"`
}

// TypeClawInstanceStatus reports observed reconciliation state. Conditions
// describe TypeClaw-owned milestones rather than generic replica mirrors.
type TypeClawInstanceStatus struct {
	// ObservedGeneration is the metadata.Generation the conditions describe.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions carry ResourcesReady (desired workload resources accepted),
	// RuntimeReady (workload Pod ready), BackupReady (scheduled snapshots
	// healthy), and AutoUpdateReady (rollout management healthy).
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Backup reports the last observed snapshot state.
	// +optional
	Backup *BackupStatus `json:"backup,omitempty"`

	// Update reports managed rollout progress.
	// +optional
	Update *UpdateStatus `json:"update,omitempty"`

	// RestoreLastProcessed records the last value of the
	// typeclaw.fml09.io/restore annotation this namespace's snapshot Job has
	// already acted on, making one-shot restores idempotent.
	// +optional
	RestoreLastProcessed string `json:"restoreLastProcessed,omitempty"`

	// SelfConfig carries the relay's latest observation of agent-authored
	// config changes. Written exclusively by the relay; the operator
	// projects it into the SelfConfigCompliant condition.
	// +optional
	SelfConfig *SelfConfigStatus `json:"selfConfig,omitempty"`

	// PersonalDesktop reports the observed Personal Desktop state.
	// +optional
	PersonalDesktop *PersonalDesktopStatus `json:"personalDesktop,omitempty"`
}

// SelfConfigSpec declares the platform policy for agent-authored config.
type SelfConfigSpec struct {
	// ProtectedPaths lists top-level typeclaw.json keys whose change marks
	// the Instance non-compliant. Empty means every key is observable-only.
	// +optional
	ProtectedPaths []string `json:"protectedPaths,omitempty"`
}

// SelfConfigStatus is the relay-written observation record. One entry per
// content change; the digest of the very first sighting seeds the baseline
// with no changed paths.
type SelfConfigStatus struct {
	// ObservedDigest is the SHA-256 of typeclaw.json at ObservedAt.
	ObservedDigest string `json:"observedDigest,omitempty"`

	// ObservedAt is when that content was first seen by the relay.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`

	// Revision increments on every observed content change, starting at 1.
	Revision int64 `json:"revision,omitempty"`

	// ChangedPaths lists the top-level keys that differ from the previously
	// observed content. Empty on the baseline observation.
	// +optional
	ChangedPaths []string `json:"changedPaths,omitempty"`

	// ProtectedViolation reports whether ChangedPaths intersected
	// spec.selfConfig.protectedPaths at evaluation time.
	ProtectedViolation bool `json:"protectedViolation,omitempty"`
}

// BackupStatus records snapshot health for gate decisions.
type BackupStatus struct {
	// LatestSnapshot names the most recent successful snapshot archive.
	LatestSnapshot string `json:"latestSnapshot,omitempty"`

	// LastSnapshotTime is when that snapshot completed.
	// +optional
	LastSnapshotTime *metav1.Time `json:"lastSnapshotTime,omitempty"`
}

// UpdatePhase enumerates managed rollout states.
// +kubebuilder:validation:Enum=Idle;AwaitingBackup;Updating;Confirming;Ready;RolledBack
type UpdatePhase string

// UpdatePhase values of the managed rollout state machine.
const (
	UpdatePhaseIdle           UpdatePhase = "Idle"
	UpdatePhaseAwaitingBackup UpdatePhase = "AwaitingBackup"
	UpdatePhaseUpdating       UpdatePhase = "Updating"
	UpdatePhaseConfirming     UpdatePhase = "Confirming"
	UpdatePhaseReady          UpdatePhase = "Ready"
	UpdatePhaseRolledBack     UpdatePhase = "RolledBack"
)

// UpdateStatus tracks registry-driven rollouts without touching spec.
type UpdateStatus struct {
	Phase UpdatePhase `json:"phase,omitempty"`

	// CurrentVersion is the runtime version currently confirmed ready.
	CurrentVersion string `json:"currentVersion,omitempty"`

	// TargetVersion is the version being promoted or confirmed.
	TargetVersion string `json:"targetVersion,omitempty"`

	// PromotedImage is the exact image reference the controller set on the
	// workload during an active rollout.
	PromotedImage string `json:"promotedImage,omitempty"`

	// ConfirmationDeadline bounds how long Confirming may last before the
	// controller rolls back to the previously confirmed version.
	// +optional
	ConfirmationDeadline *metav1.Time `json:"confirmationDeadline,omitempty"`

	// Message carries human-readable rollout context, including rollback
	// reasons.
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories=typeclaw
// +kubebuilder:printcolumn:name="Suspended",type="boolean",JSONPath=`.spec.suspend`
// +kubebuilder:printcolumn:name="Runtime",type="string",JSONPath=`.spec.runtime.version`
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=`.status.conditions[?(@.type=="RuntimeReady")].status`
// +kubebuilder:printcolumn:name="Desktop",type="string",JSONPath=`.status.personalDesktop.phase`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=`.metadata.creationTimestamp`

// TypeClawInstance manages one isolated TypeClaw agent: an independently
// managed runtime identity with its own Agent Folder. One namespaced resource
// owns exactly one workload; concurrent processes never share an Agent Folder.
//
// +kubebuilder:storageversion
type TypeClawInstance struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TypeClawInstanceSpec   `json:"spec,omitempty"`
	Status TypeClawInstanceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TypeClawInstanceList contains a list of TypeClawInstance resources.
type TypeClawInstanceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TypeClawInstance `json:"items"`
}

func init() {
	SchemeBuilder.Register(&TypeClawInstance{}, &TypeClawInstanceList{})
}
