package backupconfig

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	armadav1 "github.com/armada/configbundle/api/v1"
)

// veleroScheduleGVK is the Velero Schedule type. We use unstructured rather
// than importing velero.io/v1 to keep go.mod thin — the only Velero fields we
// touch are spec.schedule, spec.paused, and spec.template.storageLocation.
var veleroScheduleGVK = schema.GroupVersionKind{
	Group:   "velero.io",
	Version: "v1",
	Kind:    "Schedule",
}

// veleroScheduleName builds the deterministic Schedule name for a BackupConfig.
// Convention: "<bc-name>-velero" — the BackupConfig.Name is already an
// RFC 1123–safe form of the cluster orbId.
func veleroScheduleName(bc *armadav1.BackupConfig) string {
	return bc.Name + "-velero"
}

// bslNameFromLocation derives the Velero BackupStorageLocation name from an
// Orbital location URL. Convention: the last path segment is the BSL name
// (e.g. "s3://test-cluster-bucket/velero" -> "velero"; works the same for
// https://<account>.blob.core.windows.net/... URLs — scheme-agnostic).
func bslNameFromLocation(location string) string {
	trimmed := strings.TrimRight(location, "/")
	idx := strings.LastIndex(trimmed, "/")
	if idx == -1 {
		return trimmed
	}
	return trimmed[idx+1:]
}

// reconcileVelero applies the desired Velero Schedule spec from bc.Spec.Velero,
// and, when a location is set, ensures the referenced BackupStorageLocation
// exists first (create-and-own — see reconcileBSL). Returns a human-readable
// summary of all PATCHes (empty string = no PATCH needed) or an error.
//
// "Enabled = false" maps to spec.paused = true on the Schedule (Velero's native
// pause/resume toggle). The schedule resource keeps existing — we don't delete
// when disabled, so re-enabling is a one-field flip rather than a recreate.
func (r *BackupConfigReconciler) reconcileVelero(ctx context.Context, bc *armadav1.BackupConfig) (string, error) {
	logger := log.FromContext(ctx).WithName("backupconfig.velero")
	block := bc.Spec.Velero
	name := veleroScheduleName(bc)

	patchMessages := []string{}

	if block.Location != nil {
		bslName := bslNameFromLocation(*block.Location)
		desc, err := deriveStorage(*block.Location)
		if err != nil {
			return "", fmt.Errorf("derive storage descriptor: %w", err)
		}
		bslMsg, err := r.reconcileBSL(ctx, bc, bslName, desc)
		if err != nil {
			return "", fmt.Errorf("reconcile BSL: %w", err)
		}
		if bslMsg != "" {
			patchMessages = append(patchMessages, bslMsg)
		}
	}

	desired := &unstructured.Unstructured{}
	desired.SetGroupVersionKind(veleroScheduleGVK)
	desired.SetNamespace(r.VeleroNamespace)
	desired.SetName(name)
	desired.SetLabels(map[string]string{
		"armada.ai/cluster-orb-id": labelSafeOrbID(bc.Spec.ClusterOrbID),
	})

	specMap := map[string]any{}
	if block.Schedule != nil {
		specMap["schedule"] = *block.Schedule
	}
	// Paused mirrors the inverse of Enabled. Absent intent (Enabled == nil)
	// leaves the field unowned — neither we nor an operator forced a value.
	if block.Enabled != nil {
		specMap["paused"] = !*block.Enabled
	}
	templateMap := map[string]any{}
	if block.Location != nil {
		templateMap["storageLocation"] = bslNameFromLocation(*block.Location)
	}
	if block.RetentionDays != nil {
		templateMap["ttl"] = (time.Duration(*block.RetentionDays) * 24 * time.Hour).String()
	}
	if len(templateMap) > 0 {
		specMap["template"] = templateMap
	}
	if err := unstructured.SetNestedMap(desired.Object, specMap, "spec"); err != nil {
		return "", fmt.Errorf("build velero spec: %w", err)
	}

	// OwnerReference ties the Schedule's lifecycle to the BackupConfig CR:
	// deleting the BC cascades to the Schedule via native K8s GC. Cluster-
	// scoped parent → namespaced child is a documented K8s pattern (see
	// cert-manager's ClusterIssuer → Certificate).
	if err := ctrl.SetControllerReference(bc, desired, r.Scheme); err != nil {
		return "", fmt.Errorf("set owner on velero schedule: %w", err)
	}

	// Compute what will change on this reconcile. Used ONLY to produce the
	// summary written to status.recentPatches — NOT to gate the apply.
	// Gating on spec-deltas is an anti-pattern: it silently skips metadata
	// reconciliation (OwnerReferences, labels, finalizers), so any pre-
	// existing Schedule whose spec matches intent keeps missing whatever
	// metadata bc-controller wants to backfill. Convention (cert-manager,
	// cluster-api, kubebuilder samples): SSA is idempotent — always apply,
	// let the API server do the work.
	deltas, err := veleroDeltas(ctx, r.Client, r.VeleroNamespace, name, block, bc.Spec.ClusterOrbID)
	if err != nil {
		return "", err
	}

	if err := r.Patch(ctx, desired, client.Apply,
		client.FieldOwner(fieldManager),
		client.ForceOwnership,
	); err != nil {
		return "", fmt.Errorf("ssa patch velero schedule %s/%s: %w", r.VeleroNamespace, name, err)
	}

	if len(deltas) > 0 {
		patchMessages = append(patchMessages, formatBlockDeltas(fmt.Sprintf("velero/%s", name), deltas))
	}

	if len(patchMessages) == 0 {
		logger.V(1).Info("velero schedule already matches intent (metadata reconciled)", "name", name)
		return "", nil
	}
	return strings.Join(patchMessages, "; "), nil
}

// observeVelero reads the live Velero Schedule and projects the fields
// bc-controller manages into an ObservedVeleroStatus. Returns nil when the
// Schedule does not exist (unmanaged / deleted / not-yet-created) so that
// status.observed.velero == nil means "no live resource present" — distinct
// from "present but all fields empty." Get errors other than NotFound are
// returned to the caller which logs and treats as unobserved for this pass.
//
// Field mapping mirrors the intent-writer in reconcileVelero:
//   - live.spec.paused (bool)                  → observed.Enabled  (inverted)
//   - live.spec.schedule (string)              → observed.Schedule
//   - live.spec.template.storageLocation (str) → observed.Location
func observeVelero(ctx context.Context, c client.Client, namespace, name string) (*armadav1.ObservedVeleroStatus, error) {
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(veleroScheduleGVK)
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, live); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get velero schedule for observe: %w", err)
	}
	out := &armadav1.ObservedVeleroStatus{}
	if sched, ok, _ := unstructured.NestedString(live.Object, "spec", "schedule"); ok {
		s := sched
		out.Schedule = &s
	}
	if loc, ok, _ := unstructured.NestedString(live.Object, "spec", "template", "storageLocation"); ok {
		l := loc
		out.Location = &l
	}
	if paused, ok, _ := unstructured.NestedBool(live.Object, "spec", "paused"); ok {
		enabled := !paused
		out.Enabled = &enabled
	}
	return out, nil
}

// veleroDeltas returns the set of fields that differ between the live Velero
// Schedule and the intent. A NotFound Schedule means all intent fields are
// deltas (we need to create the Schedule).
//
// Returns an empty map when intent and live agree — caller skips the PATCH.
func veleroDeltas(ctx context.Context, c client.Client, namespace, name string, block *armadav1.VeleroBackupSpec, clusterOrbID string) (map[string]string, error) {
	out := map[string]string{}

	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(veleroScheduleGVK)
	err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, live)
	switch {
	case apierrors.IsNotFound(err):
		if block.Schedule != nil {
			out["schedule"] = *block.Schedule
		}
		if block.Location != nil {
			out["storageLocation"] = bslNameFromLocation(*block.Location)
		}
		if block.Enabled != nil {
			out["paused"] = fmt.Sprintf("%t", !*block.Enabled)
		}
		if block.RetentionDays != nil {
			out["ttl"] = (time.Duration(*block.RetentionDays) * 24 * time.Hour).String()
		}
		out["label:cluster-orb-id"] = labelSafeOrbID(clusterOrbID)
		return out, nil
	case err != nil:
		return nil, fmt.Errorf("get velero schedule: %w", err)
	}

	if block.Schedule != nil {
		liveSchedule, _, _ := unstructured.NestedString(live.Object, "spec", "schedule")
		if liveSchedule != *block.Schedule {
			out["schedule"] = *block.Schedule
		}
	}
	if block.Location != nil {
		liveLocation, _, _ := unstructured.NestedString(live.Object, "spec", "template", "storageLocation")
		desired := bslNameFromLocation(*block.Location)
		if liveLocation != desired {
			out["storageLocation"] = desired
		}
	}
	if block.Enabled != nil {
		livePaused, _, _ := unstructured.NestedBool(live.Object, "spec", "paused")
		desiredPaused := !*block.Enabled
		if livePaused != desiredPaused {
			out["paused"] = fmt.Sprintf("%t", desiredPaused)
		}
	}
	if block.RetentionDays != nil {
		desiredTTL := (time.Duration(*block.RetentionDays) * 24 * time.Hour).String()
		liveTTL, _, _ := unstructured.NestedString(live.Object, "spec", "template", "ttl")
		if liveTTL != desiredTTL {
			out["ttl"] = desiredTTL
		}
	}
	if live.GetLabels()["armada.ai/cluster-orb-id"] != labelSafeOrbID(clusterOrbID) {
		out["label:cluster-orb-id"] = labelSafeOrbID(clusterOrbID)
	}
	return out, nil
}

// labelSafeOrbID converts an orbId to a valid Kubernetes label value by
// replacing colons with hyphens. OrbIds use ":" as a namespace separator
// (e.g. "colo:dev-main") which is not valid in label values. Consumers
// building label selectors must apply the same transformation.
func labelSafeOrbID(orbID string) string {
	return strings.ReplaceAll(orbID, ":", "-")
}
