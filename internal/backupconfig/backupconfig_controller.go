package backupconfig

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	armadav1 "github.com/armada/configbundle/api/v1"
)

// ConditionReconciled is the canonical condition type written by this
// controller. Type=True ⇒ live state matches intent (after PATCH, or already
// matching on read). Type=False ⇒ a step failed; Reason names which.
const ConditionReconciled = "Reconciled"

// ConditionS3SyncSupported surfaces the "spec.s3Sync is present but bc-
// controller does not yet actuate it" gap in status. Set to False + Reason
// NotImplemented whenever spec.s3Sync is non-nil; removed when spec.s3Sync
// is absent so operators do not see stale "unsupported" state on a CR that
// no longer requests S3Sync. When S3Sync actuation ships, flip to True.
const ConditionS3SyncSupported = "S3SyncSupported"

// fieldManager is the SSA field-owner string this controller uses when writing
// Velero Schedule CRDs and the etcd CronJob. Matches the convention used by
// the sibling serverconfig-controller — single fixed owner per controller.
const fieldManager = "controller"

// recentPatchesLimit caps how many PATCH-history entries we keep in
// status.recentPatches. Mirrors the serverconfig-controller constant.
const recentPatchesLimit = 5

// BackupConfigReconciler watches BackupConfig CRs and reconciles spec.velero
// against a Velero Schedule CRD and spec.etcd against an etcd CronJob, both
// in the same cluster as the controller.
type BackupConfigReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// VeleroNamespace is where Velero Schedule CRDs are written
	// (conventionally "velero").
	VeleroNamespace string

	// EtcdBackupNamespace is where the etcd CronJob is written
	// (conventionally "kube-system").
	EtcdBackupNamespace string

	// EtcdctlImage is the container image that runs `etcdctl snapshot save`
	// in the CronJob's initContainer. Convention: pin a specific etcdctl
	// version so bc-controller-managed backups don't drift when the etcdctl
	// upstream tags move.
	EtcdctlImage string

	// UploadImage is the container image that uploads the snapshot to blob
	// storage. Convention: azure-cli today (matches existing etcd-backup on
	// colo-dev-main). Future clouds will need different tooling — swap the
	// image via env when we get there.
	UploadImage string

	// CredentialSecret is the K8s Secret name (in EtcdBackupNamespace)
	// holding Azure service-principal credentials. Data keys required:
	// client-id, client-secret, tenant-id. Provisioned out-of-band; bc-
	// controller only references it.
	CredentialSecret string

	// ObserveInterval is the cadence at which the reconciler re-polls Velero +
	// CronJob state even when nothing on the CR has changed. Drives drift
	// detection. Zero = no periodic poll (event-driven only).
	ObserveInterval time.Duration

	// VeleroS3URL is the S3-compatible endpoint (RGW/MinIO) Velero's
	// BackupStorageLocations point at. Infra-level, not part of BackupConfig
	// intent.
	VeleroS3URL string

	// VeleroS3Region is the region value Velero's S3 client requires on the
	// BSL config block. RGW/MinIO ignore the value but Velero requires it
	// non-empty.
	VeleroS3Region string

	// VeleroS3ForcePathStyle controls whether the BSL uses path-style S3
	// addressing (required for RGW/MinIO, not for real AWS S3).
	VeleroS3ForcePathStyle bool

	// VeleroAzureResourceGroup is the Azure resource group containing the
	// storage account(s) referenced by Azure-backed BackupConfig locations.
	// Only required when a BackupConfig's velero.location is an Azure Blob
	// URL; empty is fine for S3-only fleets.
	VeleroAzureResourceGroup string

	// VeleroAzureSubscriptionID is the Azure subscription ID for the
	// storage account(s) referenced by Azure-backed locations.
	VeleroAzureSubscriptionID string

	// VeleroAzureCredentialSecret is the K8s Secret name (in VeleroNamespace)
	// holding the Velero Azure plugin's credentials file (single "cloud" key,
	// AZURE_* env-style content). Distinct from CredentialSecret (etcd's).
	VeleroAzureCredentialSecret string
}

func (r *BackupConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// GenerationChangedPredicate fires reconcile on Create + spec changes only.
	// Status updates and managedFields-only changes don't bump generation, so
	// they don't re-fire — keeps the log clean.
	return ctrl.NewControllerManagedBy(mgr).
		For(&armadav1.BackupConfig{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("backupconfig").
		Complete(r)
}

// +kubebuilder:rbac:groups=armada.ai,resources=backupconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=armada.ai,resources=backupconfigs/status,verbs=get;patch;update
// +kubebuilder:rbac:groups=armada.ai,resources=configbundles,verbs=get;list;watch
// +kubebuilder:rbac:groups=velero.io,resources=schedules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete

func (r *BackupConfigReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx).WithName("backupconfig")

	var bc armadav1.BackupConfig
	if err := r.Get(ctx, req.NamespacedName, &bc); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("backupconfig deleted", "name", req.Name)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	// S3Sync actuation is not implemented yet — a spec.s3Sync block travels
	// end-to-end (bundler → cb-controller decomposition → BackupConfig CR)
	// but no reconciler writes it to a cluster resource. Log at V(1) so the
	// spec presence is visible without cluttering the info-level log stream.
	// TODO: implement S3Sync reconciler (needs its own actuator design).
	if bc.Spec.S3Sync != nil {
		logger.V(1).Info("spec.s3Sync present but S3Sync actuation not implemented; ignoring",
			"name", bc.Name, "s3SyncOrbId", bc.Spec.S3Sync.OrbID)
	}

	// No reconcilable blocks — log and skip without status write.
	if bc.Spec.Velero == nil && bc.Spec.Etcd == nil {
		logger.Info("no velero or etcd block on backupconfig; skipping",
			"name", bc.Name, "orbId", bc.Spec.OrbID)
		recordIntent(&bc)
		return reconcile.Result{}, nil
	}

	// Refresh metrics before the reconcile so they stay in sync with the CR
	// even if a downstream PATCH fails. Ignored stays a no-op until cluster-scoped
	// IgnoredEntry support lands.
	recordIntent(&bc)
	recordIgnored(bc.Name, ignoredFieldsForCluster(ctx, r.Client, &bc, logger))

	patchMessages := []string{}

	if bc.Spec.Velero != nil {
		msg, err := r.reconcileVelero(ctx, &bc)
		if err != nil {
			logger.Error(err, "reconcile Velero Schedule", "name", bc.Name)
			r.setStatusFailed(ctx, &bc, "VeleroPatchFailed", err.Error())
			recordReconcileError(bc.Name, "VeleroPatchFailed")
			return reconcile.Result{}, fmt.Errorf("reconcile velero: %w", err)
		}
		if msg != "" {
			patchMessages = append(patchMessages, msg)
		}
	}

	if bc.Spec.Etcd != nil {
		msg, err := r.reconcileEtcd(ctx, &bc)
		if err != nil {
			logger.Error(err, "reconcile etcd CronJob", "name", bc.Name)
			r.setStatusFailed(ctx, &bc, "EtcdPatchFailed", err.Error())
			recordReconcileError(bc.Name, "EtcdPatchFailed")
			return reconcile.Result{}, fmt.Errorf("reconcile etcd: %w", err)
		}
		if msg != "" {
			patchMessages = append(patchMessages, msg)
		}
	}

	// One live read, several independent consumers: Prom gauges (field-level,
	// resource-presence) and CR status. Mirrors sc-controller's shape:
	// metrics never depend on the status subresource write succeeding.
	live := r.readLiveObserved(ctx, &bc, logger)
	recordObservedMetric(bc.Name, live)
	recordPresence(bc.Name, &bc, live)
	r.updateObservedStatus(ctx, &bc, live)

	recordReconcileSuccess(bc.Name, time.Now().Unix())

	if len(patchMessages) == 0 {
		logger.Info("reconciled (no PATCH needed)", "name", bc.Name)
		// Don't overwrite a prior "PATCHed ..." status message on every periodic
		// poll — that erases useful action history. Only refresh status here if
		// we're recovering from a non-Reconciled state.
		if !isCurrentlyReconciled(&bc) {
			r.setStatusApplied(ctx, &bc, "all backup settings already match intent")
		} else {
			r.bumpObservedGeneration(ctx, &bc)
		}
		return reconcile.Result{RequeueAfter: r.ObserveInterval}, nil
	}

	patchMsg := strings.Join(patchMessages, "; ")
	logger.Info("reconciled (PATCH applied)", "name", bc.Name, "actions", patchMsg)
	r.setStatusApplied(ctx, &bc, patchMsg)
	r.appendRecentPatch(ctx, &bc, patchMsg)
	return reconcile.Result{RequeueAfter: r.ObserveInterval}, nil
}

// updateObservedStatus writes the pre-computed live-observed snapshot to
// status.observed. Takes `live` as an argument (rather than re-reading here)
// so metrics and status share the exact same snapshot — no possibility of
// scrape-timing races where the two surfaces disagree on what the live
// resource looked like this reconcile.
//
// Read-modify-write with RetryOnConflict; skipped when the snapshot equals
// what's already in status so periodic polls in steady state are zero-cost.
// S3Sync is never written because actuation is not implemented (see
// ConditionS3SyncSupported).
func (r *BackupConfigReconciler) updateObservedStatus(ctx context.Context, bc *armadav1.BackupConfig, live armadav1.ObservedBackup) {
	if observedEqual(bc.Status.Observed, live) {
		return
	}
	logger := log.FromContext(ctx).WithName("backupconfig.status")
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh armadav1.BackupConfig
		if err := r.Get(ctx, client.ObjectKeyFromObject(bc), &fresh); err != nil {
			return err
		}
		if observedEqual(fresh.Status.Observed, live) {
			return nil
		}
		fresh.Status.Observed = live
		return r.Status().Update(ctx, &fresh)
	})
	if err != nil {
		logger.Info("observed status update failed (will retry next reconcile)", "name", bc.Name, "err", err.Error())
	}
}

// readLiveObserved builds an ObservedBackup by reading the actual Velero
// Schedule and etcd CronJob from the cluster. Only reads mechanisms the CR's
// spec asks for — a stray Schedule for a cluster we don't manage is not our
// business to observe. A Get error other than NotFound leaves that block nil
// and logs; nil semantics ("no live resource present") is the same shape the
// caller sees on a real NotFound, and next reconcile retries.
func (r *BackupConfigReconciler) readLiveObserved(ctx context.Context, bc *armadav1.BackupConfig, logger logr.Logger) armadav1.ObservedBackup {
	var out armadav1.ObservedBackup
	if bc.Spec.Velero != nil {
		v, err := observeVelero(ctx, r.Client, r.VeleroNamespace, veleroScheduleName(bc))
		if err != nil {
			logger.Info("observe velero live state failed; reporting as absent this reconcile",
				"name", bc.Name, "err", err.Error())
		}
		out.Velero = v
	}
	if bc.Spec.Etcd != nil {
		e, err := observeEtcd(ctx, r.Client, r.EtcdBackupNamespace, etcdCronJobName(bc))
		if err != nil {
			logger.Info("observe etcd live state failed; reporting as absent this reconcile",
				"name", bc.Name, "err", err.Error())
		}
		out.Etcd = e
	}
	return out
}

// observedEqual returns true when both ledgers point at the same
// (present/absent, value) state per mechanism. Comparing pointer-triples
// handles the nil-vs-empty distinction inline.
func observedEqual(a, b armadav1.ObservedBackup) bool {
	return backupBlockEqual(a.Velero, b.Velero) &&
		etcdBlockEqual(a.Etcd, b.Etcd)
}

func backupBlockEqual(a, b *armadav1.ObservedVeleroStatus) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return boolPtrEqual(a.Enabled, b.Enabled) &&
		stringPtrEqual(a.Schedule, b.Schedule) &&
		stringPtrEqual(a.Location, b.Location)
}

func etcdBlockEqual(a, b *armadav1.ObservedEtcdStatus) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return boolPtrEqual(a.Enabled, b.Enabled) &&
		stringPtrEqual(a.Schedule, b.Schedule) &&
		stringPtrEqual(a.Location, b.Location)
}

func boolPtrEqual(a, b *bool) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func stringPtrEqual(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// setStatusApplied writes Phase=Applied + Reconciled=True. Best-effort.
func (r *BackupConfigReconciler) setStatusApplied(ctx context.Context, bc *armadav1.BackupConfig, msg string) {
	r.writeStatus(ctx, bc, armadav1.BackupConfigPhaseApplied, metav1.ConditionTrue, "BackupApplied", msg)
}

// setStatusFailed writes Phase=Diverged + Reconciled=False with a Reason that
// names which step failed (VeleroPatchFailed, EtcdPatchFailed).
func (r *BackupConfigReconciler) setStatusFailed(ctx context.Context, bc *armadav1.BackupConfig, reason, msg string) {
	r.writeStatus(ctx, bc, armadav1.BackupConfigPhaseDiverged, metav1.ConditionFalse, reason, msg)
}

func (r *BackupConfigReconciler) writeStatus(ctx context.Context, bc *armadav1.BackupConfig, phase armadav1.BackupConfigPhase, condStatus metav1.ConditionStatus, reason, msg string) {
	logger := log.FromContext(ctx).WithName("backupconfig.status")
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh armadav1.BackupConfig
		if err := r.Get(ctx, client.ObjectKeyFromObject(bc), &fresh); err != nil {
			return err
		}
		fresh.Status.Phase = phase
		fresh.Status.ObservedGeneration = fresh.Generation
		setCondition(&fresh.Status.Conditions, ConditionReconciled, condStatus, reason, msg)
		syncS3SyncCondition(&fresh)
		return r.Status().Update(ctx, &fresh)
	})
	if err != nil {
		logger.Info("status update failed (will retry next reconcile)", "name", bc.Name, "err", err.Error())
	}
}

// syncS3SyncCondition mirrors spec.s3Sync presence into the S3SyncSupported
// condition. Present → False/NotImplemented. Absent → condition removed so
// stale "unsupported" state does not linger on a CR that dropped its s3Sync
// block. Called from writeStatus so every status write keeps the condition
// in lockstep with the current spec.
func syncS3SyncCondition(bc *armadav1.BackupConfig) {
	if bc.Spec.S3Sync != nil {
		setCondition(&bc.Status.Conditions, ConditionS3SyncSupported,
			metav1.ConditionFalse, "NotImplemented",
			"spec.s3Sync present; S3Sync actuation not yet implemented")
		return
	}
	removeCondition(&bc.Status.Conditions, ConditionS3SyncSupported)
}

// appendRecentPatch prepends a new PATCH-action entry to status.recentPatches
// and truncates the list to recentPatchesLimit. Called only on successful PATCH.
func (r *BackupConfigReconciler) appendRecentPatch(ctx context.Context, bc *armadav1.BackupConfig, message string) {
	entry := armadav1.RecentPatch{Time: metav1.Now(), Message: message}
	logger := log.FromContext(ctx).WithName("backupconfig.status")
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh armadav1.BackupConfig
		if err := r.Get(ctx, client.ObjectKeyFromObject(bc), &fresh); err != nil {
			return err
		}
		recent := make([]armadav1.RecentPatch, 0, recentPatchesLimit+1)
		recent = append(recent, entry)
		recent = append(recent, fresh.Status.RecentPatches...)
		if len(recent) > recentPatchesLimit {
			recent = recent[:recentPatchesLimit]
		}
		fresh.Status.RecentPatches = recent
		return r.Status().Update(ctx, &fresh)
	})
	if err != nil {
		logger.Info("recentPatches update failed (next PATCH will retry)", "name", bc.Name, "err", err.Error())
	}
}

func (r *BackupConfigReconciler) bumpObservedGeneration(ctx context.Context, bc *armadav1.BackupConfig) {
	if bc.Status.ObservedGeneration == bc.Generation {
		return
	}
	logger := log.FromContext(ctx).WithName("backupconfig.status")
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh armadav1.BackupConfig
		if err := r.Get(ctx, client.ObjectKeyFromObject(bc), &fresh); err != nil {
			return err
		}
		if fresh.Status.ObservedGeneration == fresh.Generation {
			return nil
		}
		fresh.Status.ObservedGeneration = fresh.Generation
		return r.Status().Update(ctx, &fresh)
	})
	if err != nil {
		logger.Info("observedGeneration update failed", "name", bc.Name, "err", err.Error())
	}
}

func isCurrentlyReconciled(bc *armadav1.BackupConfig) bool {
	for _, c := range bc.Status.Conditions {
		if c.Type == ConditionReconciled {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}

// removeCondition drops any entry with the given Type from the slice. No-op
// when the condition is absent. Preserves order of the remaining entries.
func removeCondition(conds *[]metav1.Condition, condType string) {
	filtered := (*conds)[:0]
	for _, c := range *conds {
		if c.Type != condType {
			filtered = append(filtered, c)
		}
	}
	*conds = filtered
}

func setCondition(conds *[]metav1.Condition, condType string, status metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	for i := range *conds {
		c := &(*conds)[i]
		if c.Type != condType {
			continue
		}
		if c.Status != status {
			c.LastTransitionTime = now
		}
		c.Status = status
		c.Reason = reason
		c.Message = message
		return
	}
	*conds = append(*conds, metav1.Condition{
		Type:               condType,
		Status:             status,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
	})
}

// formatBlockDeltas renders changed-field map as a stable, human-readable string
// for status messages: "schedule=0 2 * * *, location=s3://backups".
func formatBlockDeltas(prefix string, d map[string]string) string {
	if len(d) == 0 {
		return ""
	}
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, d[k]))
	}
	return fmt.Sprintf("%s: %s", prefix, strings.Join(parts, ", "))
}
