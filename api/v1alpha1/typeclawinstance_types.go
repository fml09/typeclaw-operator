package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DefaultRuntimeVersion is the managed runtime release consumed when neither
// spec.runtime.version nor spec.runtime.image pins an artifact. It tracks the
// fml09/typeclaw releases that publish the managed runtime image (ADR 0003).
const DefaultRuntimeVersion = "0.48.7"

// VolumeClaimSpec declares one durable volume backed by a PVC template.
type VolumeClaimSpec struct {
	// Requested capacity of the persistent volume.
	// +kubebuilder:default="5Gi"
	Size resource.Quantity `json:"size"`
	// StorageClass to request. Unset means the cluster default class.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
}

// StorageSpec owns the durable volumes of a TypeClaw Instance. The Agent Folder
// and the runtime home are separate single-writer volumes so credential state
// never collapses into the Agent Folder boundary.
type StorageSpec struct {
	// AgentFolder provisions the PVC carrying the complete Agent Folder:
	// authored config, Git history, sessions, memory, workspace, and the
	// writable secrets envelope.
	// +kubebuilder:default={size: "5Gi"}
	AgentFolder VolumeClaimSpec `json:"agentFolder"`

	// RuntimeHome provisions the durable /home/typeclaw volume holding local
	// CLI credentials and channel encryption keys. Leaving it unset attaches
	// an emptyDir instead, which loses those secrets on Pod replacement; only
	// accept that tradeoff for throwaway Instances.
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

// BackupSpec configures scheduled filesystem snapshots of the Agent Folder.
// Snapshots are quiesced by scaling the workload to zero for the copy and
// restoring it afterwards.
type BackupSpec struct {
	// Cron schedule (standard five-field crontab) driving snapshot Jobs.
	Schedule string `json:"schedule"`

	// SnapshotVolume provisions the destination volume receiving tar
	// snapshots of the Agent Folder.
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
}

// TypeClawInstanceSpec declares the operational policy the operator enforces
// for one TypeClaw Instance. Runtime-owned state (sessions, Git history,
// refreshed credentials) never lives here; it lives in the Agent Folder.
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
