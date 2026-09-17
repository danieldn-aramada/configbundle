/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// LabelCluster is the label key stamped on ServerConfig CRs to identify the
	// Kubernetes cluster the server belongs to. Value is KubernetesCluster.name
	// from Orbital. Used by sc-controller to filter its watch to local servers only.
	LabelCluster = "serverconfig.armada.ai/cluster-name"

	// AnnotationClusterOrbID is the annotation key carrying the full Orbital orbId
	// of the owning KubernetesCluster (e.g. "alaska-dot:eksa-cluster-colo-galleon").
	// Stable identity; label carries the human-readable name for kubectl filtering.
	AnnotationClusterOrbID = "serverconfig.armada.ai/cluster-orb-id"
)

// NodeRole mirrors KubernetesNode.role values in Orbital's DGraph schema.
// +kubebuilder:validation:Enum=control_plane;worker
type NodeRole string

const (
	NodeRoleControlPlane NodeRole = "control_plane"
	NodeRoleWorker       NodeRole = "worker"
)

// KubernetesNodeSpec describes the K8s node this server maps to within the
// cluster this controller is deployed on. Sourced from Orbital's kubernetesNode
// edge on Server. Nil = server is not a node in this cluster; sc-controller
// skips maintenance for this CR.
type KubernetesNodeSpec struct {
	// Name is the K8s node name (e.g. "dev-main-cp9-7"). Distinct from
	// spec.hostname which carries Orbital's server hostname (e.g. "r09-u02.colo-galleon").
	// +optional
	Name *string `json:"name,omitempty"`

	// Role is this node's role in the cluster.
	// +kubebuilder:validation:Enum=control_plane;worker
	// +optional
	Role NodeRole `json:"role,omitempty"`

	// ClusterName is the human-readable name of the owning KubernetesCluster from
	// Orbital (KubernetesCluster.name via ConfigItem). Mirrored on the ServerConfig
	// label serverconfig.armada.ai/cluster for kubectl filtering and sc-controller
	// watch scoping.
	// +optional
	ClusterName string `json:"clusterName,omitempty"`

	// ClusterOrbID is the full Orbital orbId of the owning KubernetesCluster
	// (e.g. "alaska-dot:eksa-cluster-colo-galleon"). Immutable stable identity;
	// mirrored on the annotation serverconfig.armada.ai/cluster-orb-id.
	// +optional
	ClusterOrbID string `json:"clusterOrbId,omitempty"`
}

// MaintenancePhase is the current phase of the maintenance sequence.
// +kubebuilder:validation:Enum=Checking;Draining;Active;Restoring;Done;Failed
type MaintenancePhase string

const (
	// MaintenancePhaseChecking: maintenance requested; running eligibility checks.
	// blockedReason is set if any check fails. Stays here until all checks pass.
	// Distinct from no-phase (maintenance not requested) so operators can see
	// "requested but blocked" vs "not requested."
	MaintenancePhaseChecking MaintenancePhase = "Checking"
	// MaintenancePhaseDraining: all checks passed; node is being cordoned and pods evicted.
	MaintenancePhaseDraining MaintenancePhase = "Draining"
	// MaintenancePhaseActive: node is safe for maintenance operations (cordon + drain complete).
	MaintenancePhaseActive MaintenancePhase = "Active"
	// MaintenancePhaseRestoring: node returning to service (waiting for kubelet Ready, uncordoning).
	MaintenancePhaseRestoring MaintenancePhase = "Restoring"
	// MaintenancePhaseDone: node is back in service; lastMaintenanceAt is stamped.
	MaintenancePhaseDone MaintenancePhase = "Done"
	// MaintenancePhaseFailed: maintenance failed (drain timeout or Redfish error); requires human intervention.
	MaintenancePhaseFailed MaintenancePhase = "Failed"
)

// MaintenanceWindowSpec defines the optional time window within which maintenance may begin.
// Window gates ENTRY only — a running sequence is never aborted because the window closed.
type MaintenanceWindowSpec struct {
	// Start is the earliest time the controller may begin maintenance.
	// Nil = begin as soon as enabled.
	// +optional
	Start *metav1.Time `json:"start,omitempty"`

	// End is the deadline for entry. After this time the controller will not
	// start a new maintenance sequence, but in-flight sequences run to completion.
	// +optional
	End *metav1.Time `json:"end,omitempty"`
}

// DrainSpec tunes eviction behavior. NOT sourced from Orbital.
// Absent = controller defaults. Override via local:admin SSA only.
type DrainSpec struct {
	// Force, if true, deletes pods that cannot be evicted due to PDB violations.
	// Use with extreme caution — may disrupt protected workloads.
	// +optional
	Force bool `json:"force,omitempty"`

	// Timeout is the maximum time to wait for pod eviction to complete.
	// Defaults to 10m. On expiry, phase transitions to Failed and an alert fires.
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// MaintenanceSpec controls when and how the sc-controller enters the maintenance
// sequence for this server. Sourced from Orbital (configbundle-controller writes
// it via SSA). local:admin may force-override via SSA for emergencies.
//
// Enabled is the authoritative gate — never gate on struct presence.
// Nil spec and enabled:false are both "no maintenance."
type MaintenanceSpec struct {
	// Enabled is the authoritative on/off switch sourced from Orbital.
	// true = maintenance requested; false (or absent struct) = no maintenance.
	Enabled bool `json:"enabled"`

	// Window optionally restricts when the controller may begin. Nil = begin immediately when enabled.
	// +optional
	Window *MaintenanceWindowSpec `json:"window,omitempty"`

	// Reason is a human-readable justification sourced from Orbital. Informational only.
	// +optional
	Reason *string `json:"reason,omitempty"`

	// Drain tunes eviction behavior. NOT sourced from Orbital.
	// Absent = controller defaults. Override via local:admin SSA only.
	// +optional
	Drain *DrainSpec `json:"drain,omitempty"`
}

// MaintenanceStatus reflects the current maintenance sequence state.
// History lives in Kubernetes Events; this struct holds only current state.
type MaintenanceStatus struct {
	// Phase is the current phase of the maintenance sequence.
	// +optional
	Phase MaintenancePhase `json:"phase,omitempty"`

	// DrainProgressPercentage is the percent of pods evicted (0–100).
	// +optional
	DrainProgressPercentage *int32 `json:"drainProgressPercentage,omitempty"`

	// PendingPods lists pod names still awaiting eviction.
	// +optional
	PendingPods []string `json:"pendingPods,omitempty"`

	// BlockedReason is set when a pre-drain eligibility check fails (e.g. EtcdQuorumRisk,
	// InsufficientCapacity). Cleared when all checks pass. Does not set phase=Failed --
	// that is reserved for runtime failures like drain timeout.
	// +optional
	BlockedReason *string `json:"blockedReason,omitempty"`

	// LastError is the most recent reconcile error, if any.
	// +optional
	LastError *string `json:"lastError,omitempty"`

	// CordonedAt is the time the node was cordoned. Cleared when maintenance exits.
	// +optional
	CordonedAt *metav1.Time `json:"cordonedAt,omitempty"`

	// DrainedAt is the time drain completed (entry to Active). Cleared when maintenance exits.
	// +optional
	DrainedAt *metav1.Time `json:"drainedAt,omitempty"`

	// LastMaintenanceAt is the time the most recent maintenance sequence completed (Done).
	// Persists after Done for audit/alerting purposes.
	// +optional
	LastMaintenanceAt *metav1.Time `json:"lastMaintenanceAt,omitempty"`
}

// ServerConfigSpec mirrors the ServerSpec from ConfigBundle.
// The ConfigBundle controller creates and updates this CR via SSA. ServerConfig
// is derived state — admin overrides happen on the parent ConfigBundle CR only.
//
// Field types match ServerSpec (pointer leaves, see ADR-007) so the parent→child
// copy is a direct assignment.
type ServerConfigSpec struct {
	// OrbID is the immutable Orbital identifier for this server
	// (mirrors ConfigBundle.spec.servers[].orbId). Carried on the child so
	// cross-system grep, audit logs, and downstream telemetry can correlate
	// without a parent round-trip. See docs/plans/server-identity-orbid.md.
	// +kubebuilder:validation:Required
	OrbID string `json:"orbId"`

	// ServiceTag is the original-case Dell service tag (e.g. "3RK3V64").
	// Repeated here (vs. deriving from CR name) so the controller has it without string manipulation.
	// +kubebuilder:validation:Required
	ServiceTag string `json:"serviceTag"`

	// Hostname is the server's hostname for display and logging.
	// +kubebuilder:validation:Required
	Hostname *string `json:"hostname,omitempty"`

	// OobIP is the iDRAC management IP. The ServerConfig controller targets Redfish here.
	// +kubebuilder:validation:Required
	OobIP *string `json:"oobIP,omitempty"`

	// IdracSettings holds desired iDRAC configuration, mirroring the orbital
	// Server.idracSettings edge.
	// +optional
	IdracSettings IdracSettingsSpec `json:"idracSettings,omitempty"`

	// KubernetesNode describes the K8s node this server maps to in the cluster
	// this controller is deployed on. Sourced from Orbital's kubernetesNode edge.
	// Nil means the server is not a node in this cluster — sc-controller skips maintenance.
	// +optional
	KubernetesNode *KubernetesNodeSpec `json:"kubernetesNode,omitempty"`

	// Maintenance controls when the controller may enter the maintenance sequence.
	// Written by configbundle-controller from Orbital. local:admin may SSA-override.
	// Nil and enabled:false are both treated as "no maintenance."
	// +optional
	Maintenance *MaintenanceSpec `json:"maintenance,omitempty"`
}

// BundleArtifact records the ConfigBundle OCI artifact that produced the
// current spec of this ServerConfig. Written by cb-controller on each dispatch;
// sc-controller never touches this field.
type BundleArtifact struct {
	// Version is the OCI tag of the ConfigBundle artifact (e.g. "v94").
	// +optional
	Version string `json:"version,omitempty"`

	// Digest is the immutable OCI manifest digest of the ConfigBundle artifact.
	// +optional
	Digest string `json:"digest,omitempty"`

	// AppliedAt is when cb-controller received and applied this bundle version.
	// +optional
	AppliedAt *metav1.Time `json:"appliedAt,omitempty"`
}

// ServerConfigObserved holds the live device state read during the last
// reconcile. Written by sc-controller from live Redfish reads.
type ServerConfigObserved struct {
	// IdracSettings is the iDRAC attribute state read from the device.
	// +optional
	IdracSettings ObservedIdracSettingsStatus `json:"idracSettings,omitempty"`
}

// ServerConfigStatus records the controller's observed state.
type ServerConfigStatus struct {
	// ObservedGeneration is the spec.generation the controller last successfully
	// reconciled. Tooling compares this to metadata.generation to know "has the
	// controller caught up to my spec change yet?" — the K8s-standard
	// "are we converged?" signal.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// LastReconciledAt is when sc-controller last successfully compared spec
	// against live iDRAC state (and applied any deltas). Bumps on every
	// successful reconcile whether triggered by a spec change or IDRAC_POLL_INTERVAL.
	// Distinct from Conditions[Reconciled].LastTransitionTime, which only moves
	// on status flip — this is the truthful "controller is still doing work" signal.
	// Nil = no successful reconcile yet.
	// +optional
	LastReconciledAt *metav1.Time `json:"lastReconciledAt,omitempty"`

	// Conditions records detailed status conditions.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LastAppliedBundle records the ConfigBundle OCI artifact whose spec produced
	// the current state of this ServerConfig. Written by cb-controller on each
	// dispatch; nil until the first bundle is received.
	// +optional
	LastAppliedBundle *BundleArtifact `json:"lastAppliedBundle,omitempty"`

	// LastObserved holds the live device state captured during the last
	// successful reconcile.
	// +optional
	LastObserved *ServerConfigObserved `json:"lastObserved,omitempty"`

	// Maintenance reflects the current maintenance sequence state.
	// +optional
	Maintenance *MaintenanceStatus `json:"maintenance,omitempty"`
}

// ObservedIdracSettingsStatus mirrors the controller-managed subset of
// IdracSettingsSpec. Pointer types so absence means "never confirmed" (vs.
// "confirmed and false").
type ObservedIdracSettingsStatus struct {
	// +optional
	SSHEnabled *bool `json:"sshEnabled,omitempty"`
	// +optional
	IPMIEnabled *bool `json:"ipmiEnabled,omitempty"`
	// +optional
	RacadmEnabled *bool `json:"racadmEnabled,omitempty"`
	// FirmwareVersion is the observed iDRAC firmware read from Redfish, mirroring
	// spec.idracSettings.firmwareVersion. Nil until a firmware read lands (not yet
	// implemented) — observation-only for now.
	// +optional
	FirmwareVersion *string `json:"firmwareVersion,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=sc
// +kubebuilder:printcolumn:name="Reconciled",type=string,JSONPath=`.status.conditions[?(@.type=="Reconciled")].status`
// +kubebuilder:printcolumn:name="ServiceTag",type=string,JSONPath=`.spec.serviceTag`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="OrbID",type=string,priority=1,JSONPath=`.spec.orbId`
// +kubebuilder:printcolumn:name="Hostname",type=string,priority=1,JSONPath=`.spec.hostname`
// +kubebuilder:printcolumn:name="OOB IP",type=string,priority=1,JSONPath=`.spec.oobIP`

// ServerConfig is a domain child CR owned by a ConfigBundle.
// Created and updated by the ConfigBundle Controller via SSA (field manager: "configbundle-controller").
// The ServerConfig Controller (separate, out of scope for v1) actuates the spec via Redfish.
type ServerConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ServerConfigSpec   `json:"spec,omitempty"`
	Status ServerConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ServerConfigList contains a list of ServerConfig.
type ServerConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ServerConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ServerConfig{}, &ServerConfigList{})
}
