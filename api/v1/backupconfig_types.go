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

// ClusterBackupSpec projects the orbital ClusterBackup ConfigItem — the
// grouping node between a KubernetesCluster and its per-mechanism backup
// children (etcd, velero, s3Sync). Its OrbID is distinct from the parent
// cluster's, matching the graph's identity for this node.
type ClusterBackupSpec struct {
	// OrbID is the immutable Orbital identifier for this ClusterBackup node
	// (e.g. "colo:cluster-001-backup"). Set by the bundler; identity-only.
	// +kubebuilder:validation:Required
	OrbID string `json:"orbId"`

	// Velero holds desired Velero backup configuration.
	// +optional
	Velero *VeleroBackupSpec `json:"velero,omitempty"`

	// Etcd holds desired etcd backup configuration.
	// +optional
	Etcd *EtcdBackupSpec `json:"etcd,omitempty"`

	// S3Sync holds desired S3-sync backup configuration.
	// +optional
	S3Sync *S3SyncSpec `json:"s3Sync,omitempty"`
}

// VeleroBackupSpec mirrors orbital VeleroBackup — the Velero-Schedule-backed
// mechanism. Shape currently equals EtcdBackupSpec but they are declared
// separately so grep, divergence audit, and future divergence remain honest.
type VeleroBackupSpec struct {
	// OrbID is the immutable Orbital identifier for this VeleroBackup node
	// (e.g. "colo:cluster-001-velero"). Set by the bundler; identity-only.
	// +kubebuilder:validation:Required
	OrbID string `json:"orbId"`

	// Enabled toggles the backup mechanism on or off.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Schedule is a cron expression for when the backup runs (e.g. "0 2 * * *").
	// +optional
	Schedule *string `json:"schedule,omitempty"`

	// Location is the storage location the backup writes to (e.g. an S3 URL or
	// a Velero BackupStorageLocation name).
	// +optional
	Location *string `json:"location,omitempty"`
}

// EtcdBackupSpec mirrors orbital EtcdBackup — the etcd-snapshot mechanism.
// See VeleroBackupSpec for why this is a distinct type despite shape overlap.
type EtcdBackupSpec struct {
	// OrbID is the immutable Orbital identifier for this EtcdBackup node
	// (e.g. "colo:cluster-001-etcd"). Set by the bundler; identity-only.
	// +kubebuilder:validation:Required
	OrbID string `json:"orbId"`

	// Enabled toggles the backup mechanism on or off.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Schedule is a cron expression for when the backup runs (e.g. "0 3 * * *").
	// +optional
	Schedule *string `json:"schedule,omitempty"`

	// Location is the storage location the backup writes to.
	// +optional
	Location *string `json:"location,omitempty"`
}

// S3SyncSpec mirrors orbital S3Sync. Orbital exposes only `enabled` on this
// node today; schedule/location live on the sibling velero/etcd types.
type S3SyncSpec struct {
	// OrbID is the immutable Orbital identifier for this S3Sync node
	// (e.g. "colo:cluster-001-s3sync"). Set by the bundler; identity-only.
	// +kubebuilder:validation:Required
	OrbID string `json:"orbId"`

	// Enabled toggles the S3-sync mechanism on or off.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
}

// BackupConfigSpec is the projected desired state for one ClusterBackup node.
// The ConfigBundle controller creates and updates this CR via SSA from
// ConfigBundleSpec.kubernetesClusters[].backup entries. One BackupConfig CR
// per ClusterBackup graph node.
type BackupConfigSpec struct {
	// OrbID is the immutable Orbital identifier for the ClusterBackup node
	// this CR projects (matches ClusterBackupSpec.OrbID on the parent).
	// Named to match the "resource identity = orbId" convention.
	// +kubebuilder:validation:Required
	OrbID string `json:"orbId"`

	// ClusterOrbID is the OrbID of the parent KubernetesCluster this backup
	// belongs to. Carried on the child so cross-system grep, audit logs, and
	// downstream telemetry can correlate back to the cluster without a parent
	// round-trip.
	// +kubebuilder:validation:Required
	ClusterOrbID string `json:"clusterOrbId"`

	// Velero holds desired Velero backup configuration. Absent = the
	// controller does not manage Velero for this cluster.
	// +optional
	Velero *VeleroBackupSpec `json:"velero,omitempty"`

	// Etcd holds desired etcd backup configuration. Absent = the controller
	// does not manage etcd backups for this cluster.
	// +optional
	Etcd *EtcdBackupSpec `json:"etcd,omitempty"`

	// S3Sync holds desired S3-sync backup configuration. Absent = the
	// controller does not manage S3-sync for this cluster. Actuation not
	// implemented yet — bc-controller logs the spec and moves on.
	// +optional
	S3Sync *S3SyncSpec `json:"s3Sync,omitempty"`
}

// BackupConfigPhase represents the current lifecycle phase.
// +kubebuilder:validation:Enum=Pending;Applied;Diverged;Skipped
type BackupConfigPhase string

const (
	BackupConfigPhasePending  BackupConfigPhase = "Pending"
	BackupConfigPhaseApplied  BackupConfigPhase = "Applied"
	BackupConfigPhaseDiverged BackupConfigPhase = "Diverged"
	// BackupConfigPhaseSkipped means the controller deliberately did not
	// reconcile this CR (e.g. no velero/etcd block to actuate). The Reconciled
	// condition is Unknown, not False — this is an expected, benign state, not
	// a fault. Mirrors ServerConfigPhaseSkipped.
	BackupConfigPhaseSkipped BackupConfigPhase = "Skipped"
)

// BackupConfigStatus records the controller's observed state. Mirrors the
// ServerConfigStatus shape so operators can reason about both child kinds the
// same way (Phase, ObservedGeneration, Conditions, per-mechanism Observed
// ledger, LastAppliedAt). Per-action history goes to Kubernetes Events.
type BackupConfigStatus struct {
	// Phase is the current lifecycle phase.
	// +optional
	Phase BackupConfigPhase `json:"phase,omitempty"`

	// ObservedGeneration is the spec.generation the controller last successfully
	// reconciled. K8s-standard "are we converged?" signal.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions records detailed status conditions.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Velero holds the controller's observed Velero producer config, read live
	// from the cluster. Nil = not managed. Mirrors spec.velero — desired ↔
	// observed at matching paths; the spec/status prefix is the label, no
	// `observed:` wrapper (see docs/reference/DOMAIN-CONTROLLER.md §1).
	// +optional
	Velero *ObservedVeleroStatus `json:"velero,omitempty"`

	// Etcd holds the controller's observed etcd state — the producer config read
	// live from the CronJob PLUS the artifact fields (snapshot freshness/count/
	// size) read from the backup store. Nil = not managed. Mirrors spec.etcd and
	// is a superset: the artifact fields have no desired counterpart.
	// +optional
	Etcd *ObservedEtcdStatus `json:"etcd,omitempty"`

	// S3Sync holds observed S3-sync state. Nil = not managed (always today —
	// S3Sync actuation is not implemented; ConditionS3SyncSupported surfaces that).
	// +optional
	S3Sync *ObservedS3SyncStatus `json:"s3Sync,omitempty"`

	// LastAppliedAt is the wall-clock time of the most recent successful
	// reconcile action. See ServerConfigStatus.LastAppliedAt for the
	// rationale — Conditions[].LastTransitionTime tells operators when
	// Status last flipped (K8s convention), LastAppliedAt tells them when
	// the controller last did anything.
	// +optional
	LastAppliedAt *metav1.Time `json:"lastAppliedAt,omitempty"`
}

// ObservedBackup is the controller's transient live-read aggregate — the shape
// readLiveObserved builds each reconcile and hands to updateObservedStatus,
// which fans it out onto the flattened status blocks (status.velero/etcd/s3Sync).
// It is NOT a persisted status field itself. Each block is a pointer so a nil
// block means "controller does not manage this mechanism" — distinct from
// "manages it but all fields are unset".
type ObservedBackup struct {
	// Velero holds controller-confirmed Velero field values. Nil = not managed.
	// +optional
	Velero *ObservedVeleroStatus `json:"velero,omitempty"`

	// Etcd holds controller-confirmed etcd field values. Nil = not managed.
	// +optional
	Etcd *ObservedEtcdStatus `json:"etcd,omitempty"`

	// S3Sync holds controller-confirmed S3-sync field values. Nil = not
	// managed (which is always the case today — S3Sync actuation is not yet
	// implemented; the S3SyncSupported condition surfaces that fact).
	// +optional
	S3Sync *ObservedS3SyncStatus `json:"s3Sync,omitempty"`
}

// ObservedVeleroStatus mirrors the controller-managed subset of
// VeleroBackupSpec. Pointer types so absence means "never confirmed"
// (vs. "confirmed and false").
type ObservedVeleroStatus struct {
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// +optional
	Schedule *string `json:"schedule,omitempty"`
	// +optional
	Location *string `json:"location,omitempty"`
}

// ObservedEtcdStatus mirrors the controller-managed subset of EtcdBackupSpec.
type ObservedEtcdStatus struct {
	// Enabled / Schedule / Location describe the producer (CronJob) config,
	// observed live from the cluster.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// +optional
	Schedule *string `json:"schedule,omitempty"`
	// +optional
	Location *string `json:"location,omitempty"`

	// The fields below describe the ARTIFACTS — the actual snapshots in the
	// backup store — read live by bc-controller. This is the resource bc truly
	// manages for etcd (it owns the full stack; no independent subsystem
	// observes the store). See docs/reference/BACKUP.md.

	// LastSnapshotTime is the modification time of the newest snapshot object
	// in the store. Nil = no snapshot observed (or store not yet read).
	// +optional
	LastSnapshotTime *metav1.Time `json:"lastSnapshotTime,omitempty"`

	// SnapshotCount is how many snapshot objects exist under the cluster's
	// prefix — a retention-health signal. Nil = store not yet read.
	// +optional
	SnapshotCount *int32 `json:"snapshotCount,omitempty"`

	// LatestSnapshotBytes is the size of the newest snapshot object. Nil =
	// none observed.
	// +optional
	LatestSnapshotBytes *int64 `json:"latestSnapshotBytes,omitempty"`
}

// ObservedS3SyncStatus mirrors the controller-managed subset of S3SyncSpec.
type ObservedS3SyncStatus struct {
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=bc
// +kubebuilder:printcolumn:name="OrbID",type=string,JSONPath=`.spec.orbId`
// +kubebuilder:printcolumn:name="ClusterOrbID",type=string,JSONPath=`.spec.clusterOrbId`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// BackupConfig is a domain child CR owned by a ConfigBundle.
// Created and updated by the ConfigBundle Controller via SSA (field manager:
// "configbundle-controller"). The BackupConfig Controller actuates the spec
// by SSA-patching Velero Schedule CRDs and an etcd backup CronJob in the
// same cluster.
type BackupConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BackupConfigSpec   `json:"spec,omitempty"`
	Status BackupConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// BackupConfigList contains a list of BackupConfig.
type BackupConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BackupConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BackupConfig{}, &BackupConfigList{})
}
