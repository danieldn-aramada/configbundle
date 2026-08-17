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

// Naming convention across api/v1/: Go types and JSON edges mirror the orbital
// GraphQL schema (~/armada/orbital/schema/schema.graphql). Type names carry a
// "Spec" suffix per K8s convention; strip it to get orbital's type identity
// (IdracSettingsSpec ↔ IdracSettings). JSON edge names match orbital exactly
// (idracSettings, kubernetesClusters, backup, velero, etcd, s3Sync).

// IdracSettingsSpec holds desired iDRAC configuration, mirroring orbital
// IdracSettings. All fields are desired state — not observed state. The
// ServerConfig controller actuates these via Redfish PATCH calls to the OOB IP.
//
// All admin-overridable leaf fields are pointers with omitempty so SSA partial
// patches can omit admin-owned fields. See ADR-007. OrbID is identity metadata,
// not a configurable field — a required string set by the bundler from
// orbital's GraphQL and never overridden by local:admin.
type IdracSettingsSpec struct {
	// OrbID is the immutable Orbital identifier for this IdracSettings node
	// (e.g. "colo:srv-001-idrac"). Set by the bundler; identity-only, never
	// admin-overridable. Required so a malformed bundle (bundler bug — forgot
	// to set orbId on an idracSettings block) fails at apply time, not later.
	// +kubebuilder:validation:Required
	OrbID string `json:"orbId"`

	// FirmwareVersion is the desired iDRAC firmware version (e.g. "7.20.10.05").
	// Controller reads current version via Redfish GET and upgrades/downgrades to match.
	// +optional
	FirmwareVersion *string `json:"firmwareVersion,omitempty"`

	// +optional
	SSHEnabled *bool `json:"sshEnabled,omitempty"`

	// +optional
	IPMIEnabled *bool `json:"ipmiEnabled,omitempty"`

	// +optional
	LockdownModeEnabled *bool `json:"lockdownModeEnabled,omitempty"`

	// +optional
	OsToIdracPassThroughEnabled *bool `json:"osToIdracPassThroughEnabled,omitempty"`

	// +optional
	UsbManagementPortEnabled *bool `json:"usbManagementPortEnabled,omitempty"`

	// +optional
	DHCPEnabled *bool `json:"dhcpEnabled,omitempty"`

	// +optional
	RacadmEnabled *bool `json:"racadmEnabled,omitempty"`
}

// KubernetesClusterSpec describes one Kubernetes cluster within a ConfigBundle,
// mirroring orbital KubernetesCluster (interface). The bundler populates this
// from the orbital KubernetesCluster subgraph; the decomposer projects the
// Backup field into a BackupConfig child CR (one per ClusterBackup node).
type KubernetesClusterSpec struct {
	// OrbID is the immutable Orbital identifier for this cluster (e.g.
	// "colo:cluster-001"). Used as the SSA listMapKey for
	// spec.kubernetesClusters[].
	// +kubebuilder:validation:Required
	OrbID string `json:"orbId"`

	// Name is the cluster's display name. Informational — not used for routing
	// or identity.
	// +optional
	Name *string `json:"name,omitempty"`

	// Backup holds this cluster's backup configuration, mirroring the orbital
	// KubernetesCluster.backup edge. Absent = no backup config for this cluster.
	// +optional
	Backup *ClusterBackupSpec `json:"backup,omitempty"`
}

// ServerSpec describes one server's desired configuration within a ConfigBundle,
// mirroring orbital Server.
//
// Hostname and OobIP are pointers so SSA partial patches can omit admin-owned
// fields (see ADR-007). OrbID is the listMapKey — Orbital's immutable identifier
// for this server, stable across hardware swaps and renames.
// See docs/plans/server-identity-orbid.md.
type ServerSpec struct {
	// OrbID is the immutable Orbital identifier for this server (e.g. "colo:srv-001").
	// Used as the SSA listMapKey for spec.servers[]. Never changes for the same
	// physical server, even when serviceTag is re-stamped or hostname renamed.
	// +kubebuilder:validation:Required
	OrbID string `json:"orbId"`

	// ServiceTag is the Dell hardware service tag (e.g. "3RK3V64"). Mutable across
	// board swaps. Propagated to the child ServerConfig spec for operator visibility.
	// +kubebuilder:validation:Required
	ServiceTag string `json:"serviceTag"`

	// Hostname is the server's hostname. Mandatory — the bundler skips servers without one.
	// +kubebuilder:validation:Required
	Hostname *string `json:"hostname,omitempty"`

	// OobIP is the out-of-band management (iDRAC) IP address.
	// The ServerConfig controller sends Redfish calls here. Mandatory for actuation.
	// +kubebuilder:validation:Required
	OobIP *string `json:"oobIP,omitempty"`

	// IdracSettings holds desired iDRAC configuration, mirroring the orbital
	// Server.idracSettings edge. Value type; its leaf fields are pointers
	// (see IdracSettingsSpec).
	// +optional
	IdracSettings IdracSettingsSpec `json:"idracSettings,omitempty"`

	// KubernetesNode describes the K8s node this server maps to in the cluster
	// this controller is deployed on. Sourced from Orbital's kubernetesNode edge.
	// Nil means the server is not a node in this cluster.
	// +optional
	KubernetesNode *KubernetesNodeSpec `json:"kubernetesNode,omitempty"`

	// Maintenance controls when the sc-controller may enter the maintenance sequence.
	// Written by configbundle-controller from Orbital. local:admin may SSA-override.
	// Nil and enabled:false are both treated as "no maintenance."
	// +optional
	Maintenance *MaintenanceSpec `json:"maintenance,omitempty"`
}

// TakeoverEntry represents a cloud admin's "force" resolution: reclaim ownership
// of a specific field from local:admin. The consume handler processes these by
// running a per-field SSA apply with ForceOwnership after the normal apply pass.
type TakeoverEntry struct {
	// ServerOrbID identifies which server entry the field belongs to (matches
	// ServerSpec.OrbID, the listMapKey for spec.servers[]).
	// +kubebuilder:validation:Required
	ServerOrbID string `json:"serverOrbId"`

	// OrbID is the Orbital ConfigItem identifier of the node that owns the field
	// (e.g. "colo:srv-001-idrac" for an idracSettings field). Informational —
	// used for audit and divergence-resolution correlation.
	OrbID string `json:"orbId"`

	// Field is the leaf field name to reclaim (e.g. "sshEnabled").
	// Must match the JSON tag name on IdracSettingsSpec (or ServerSpec for
	// top-level fields).
	// +kubebuilder:validation:Required
	Field string `json:"field"`
}

// IgnoredEntry represents a cloud admin's "ignore" resolution: the controller
// MUST NOT claim this field even when the spec value matches the live value.
// The field's intent value stays in the spec so the divergence-reporter can
// surface it as an ongoing divergence; cb-controller bows out unconditionally.
//
// Ignore stays surfaced as divergence until the local admin releases ownership
// or the cloud admin re-decides as Accept/Reject. Mirror of TakeoverEntry.
type IgnoredEntry struct {
	// ServerOrbID identifies which server entry the field belongs to (matches
	// ServerSpec.OrbID, the listMapKey for spec.servers[]).
	// +kubebuilder:validation:Required
	ServerOrbID string `json:"serverOrbId"`

	// OrbID is the Orbital ConfigItem identifier of the node that owns the field
	// (e.g. "colo:srv-001-idrac" for an idracSettings field). Informational —
	// used for audit and divergence-resolution correlation.
	OrbID string `json:"orbId"`

	// Field is the leaf field name to leave to the local manager (e.g. "racadmEnabled").
	// Must match the JSON tag name on IdracSettingsSpec (or ServerSpec for
	// top-level fields).
	// +kubebuilder:validation:Required
	Field string `json:"field"`
}

// ConfigBundleSpec holds the full intended configuration for a datacenter.
// The ConfigBundle controller decomposes this into domain child CRs via SSA.
type ConfigBundleSpec struct {
	// OrbID is the immutable Orbital identifier for this datacenter (e.g. "colo:colo-galleon").
	// First-class cross-system correlation key; see docs/plans/server-identity-orbid.md.
	// +kubebuilder:validation:Required
	OrbID string `json:"orbId"`

	// Datacenter is the identifier of the target datacenter (matches Orbital namespace name).
	// +kubebuilder:validation:Required
	Datacenter string `json:"datacenter"`

	// Servers is the list of server configurations for this datacenter.
	// +optional
	// +listType=map
	// +listMapKey=orbId
	Servers []ServerSpec `json:"servers,omitempty"`

	// KubernetesClusters is the list of Kubernetes cluster configurations for
	// this datacenter, mirroring the orbital DataCenter.kubernetesClusters
	// collection. The decomposer projects each entry's Backup into a
	// BackupConfig child CR.
	// +optional
	// +listType=map
	// +listMapKey=orbId
	KubernetesClusters []KubernetesClusterSpec `json:"kubernetesClusters,omitempty"`

	// Takeover contains force-resolution directives from the cloud admin.
	// Each entry triggers a ForceOwnership SSA apply to reclaim the field from local:admin.
	// Entries persist until the next bundle replaces the spec (cb-bundler omits consumed entries).
	// +optional
	// +listType=atomic
	Takeover []TakeoverEntry `json:"takeover,omitempty"`

	// Ignored contains "do not claim" directives from the cloud admin. For each
	// entry, cb-controller leaves the field to its local manager unconditionally
	// — even when the spec's intent value matches the live value (which would
	// otherwise trigger auto-claim under the simplified controller). The
	// divergence-reporter still surfaces these fields as divergent so the
	// operator continues to see the override; the resolution row in orbital
	// records the deliberate "leave to edge" decision.
	// +optional
	// +listType=atomic
	Ignored []IgnoredEntry `json:"ignored,omitempty"`
}

// ConfigBundlePhase represents the current lifecycle phase.
// +kubebuilder:validation:Enum=Pending;Applying;Applied;Failed
type ConfigBundlePhase string

const (
	ConfigBundlePhasePending  ConfigBundlePhase = "Pending"
	ConfigBundlePhaseApplying ConfigBundlePhase = "Applying"
	ConfigBundlePhaseApplied  ConfigBundlePhase = "Applied"
	ConfigBundlePhaseFailed   ConfigBundlePhase = "Failed"
)

// Condition type constants for ConfigBundleStatus.Conditions.
const (
	// ConditionReconciled is set by the Decomposition Reconciler when all child CRs are in sync.
	ConditionReconciled = "Reconciled"
)

// ConfigBundleStatus records the controller's observed state.
type ConfigBundleStatus struct {
	// ObservedGeneration is the .metadata.generation the controller has most
	// recently reconciled. When equal to .metadata.generation, the controller
	// has observed the latest spec. Used to distinguish spec-change reconciles
	// from drift / status-only reconciles in logging.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase is the current lifecycle phase.
	// +optional
	Phase ConfigBundlePhase `json:"phase,omitempty"`

	// Conditions records detailed status conditions using the standard K8s convention.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LastAppliedVersion is the OCI tag (X-Orb-Tag) from the most recent
	// successful consume dispatch, e.g. "v38".
	// +optional
	LastAppliedVersion string `json:"lastAppliedVersion,omitempty"`

	// LastAppliedDigest is the artifact manifest digest (X-Orb-Digest) from the most
	// recent successful consume dispatch.
	// +optional
	LastAppliedDigest string `json:"lastAppliedDigest,omitempty"`

	// LastOrbImportID is the orb import UUID (X-Orb-Import-ID) from the most recent
	// successful consume dispatch. Used for correlation with orb's import history.
	// +optional
	LastOrbImportID string `json:"lastOrbImportID,omitempty"`

	// LastAppliedAt is the time the last successful apply completed.
	// +optional
	LastAppliedAt *metav1.Time `json:"lastAppliedAt,omitempty"`

	// DivergenceReporting captures the divergence-reporter's dedup state so a
	// restarted controller resumes without either wiping orb's known state or
	// missing a state-clearing POST. Written only by the divergence-reporter.
	//
	// A nil value means "no POST has ever landed for this CB" — the reporter
	// treats this as unknown and posts once on the next reconcile (biased
	// toward orb-sync-correctness over one avoidable POST). See
	// docs/reference/EDGE.md for the cold-start semantics.
	// +optional
	DivergenceReporting *DivergenceReportingStatus `json:"divergenceReporting,omitempty"`
}

// DivergenceReportingStatus records what the divergence-reporter last sent to
// orb. Persisting this on the CR (instead of in-process memory) makes the
// reporter fully restart-resilient: a cold-started reporter can distinguish
// "never posted" (nil) from "posted empty last time" (LastPostedOverrideCount
// pointer to 0) from "posted N overrides" — the exact distinction the
// steady-state-quiet optimization needs to make correct decisions.
type DivergenceReportingStatus struct {
	// LastCheckedForDivergence is the timestamp of the most recent reconcile
	// that evaluated the override set (whether or not a POST to orb followed).
	// Distinct from LastPostedAt: the reporter skips the POST when the payload
	// is unchanged (dedup) or the last POST was already empty (quiet-state).
	// A recent LastCheckedForDivergence with a stale LastPostedAt means the
	// reporter is healthy and the override set is steady — not that it is stuck.
	// +optional
	LastCheckedForDivergence *metav1.Time `json:"lastCheckedForDivergence,omitempty"`

	// LastPostedAt is the timestamp of the last successful POST to orb.
	// +optional
	LastPostedAt *metav1.Time `json:"lastPostedAt,omitempty"`

	// LastPostedHash is the SHA-256 hex of the last POST payload. Reconcile
	// skips the POST when the freshly-computed payload hash matches this value.
	// Cleared by the heartbeat to force periodic re-syncs against orb-wipe.
	// +optional
	LastPostedHash string `json:"lastPostedHash,omitempty"`

	// LastPostedOverrideCount is the number of override entries in the last
	// POST. Pointer type distinguishes "never posted" (nil) from "posted an
	// empty set" (*0). The steady-state-quiet skip only fires when the current
	// count is 0 AND this is non-nil and *this == 0. That combination means
	// "empty now, and last time we told orb we were empty" — the only case
	// where skipping the POST is safe.
	// +optional
	LastPostedOverrideCount *int `json:"lastPostedOverrideCount,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=cb
// +kubebuilder:printcolumn:name="Datacenter",type=string,JSONPath=`.spec.datacenter`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ConfigBundle is the top-level CR for a datacenter's intended configuration.
// The ConfigBundle controller decomposes its spec into domain child CRs (ServerConfig, etc.)
// using Server-Side Apply with field manager "configbundle-controller".
type ConfigBundle struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ConfigBundleSpec   `json:"spec,omitempty"`
	Status ConfigBundleStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ConfigBundleList contains a list of ConfigBundle.
type ConfigBundleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ConfigBundle `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ConfigBundle{}, &ConfigBundleList{})
}
