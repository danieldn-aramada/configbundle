package backupconfig

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
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

// ConditionBackupsFresh reports the etcd ARTIFACT state — whether a recent
// snapshot actually exists in the store — as distinct from ConditionReconciled
// (which is producer-config fidelity). True = newest snapshot within max-age;
// False = snapshots exist but stale (broken schedule); Unknown = no snapshot
// yet observed, or the store read failed. Only set when the etcd backup store
// is configured for observation. See docs/reference/BACKUP.md.
const ConditionBackupsFresh = "BackupsFresh"

// fieldManager is the SSA field-owner string this controller uses when writing
// Velero Schedule CRDs and the etcd CronJob. Matches the convention used by
// the sibling serverconfig-controller — single fixed owner per controller.
const fieldManager = "controller"

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

	// EtcdRetainPerDay is how many snapshots to keep per UTC day per cluster.
	// The prune step in snapshotWriterScript deletes older snapshots beyond
	// this count. Zero means no pruning.
	EtcdRetainPerDay int

	// EtcdBackupTimeZone is the IANA timezone for the etcd CronJob schedule
	// (e.g. "America/Los_Angeles"). Empty string uses the cluster default (UTC).
	EtcdBackupTimeZone string

	// EtcdGCSchedule is the cron schedule for the day-level GC CronJob that
	// drops blobs older than spec.etcd.retentionDays. Only consulted when the
	// BackupConfig CR has spec.etcd.retentionDays set. From controller config
	// (ETCD_GC_SCHEDULE env), not from the CR.
	EtcdGCSchedule string

	// ObserveInterval is the cadence at which the reconciler re-polls Velero +
	// CronJob state even when nothing on the CR has changed. Drives drift
	// detection. Zero = no periodic poll (event-driven only).
	// ObserveInterval is the periodic re-observe cadence (RequeueAfter). Config
	// drift on the owned CronJob/Schedule is caught instantly by watches, so
	// this interval mainly governs polling non-watchable state — chiefly the
	// etcd backup store (blob) — plus a cheap K8s re-read backstop. Zero = no
	// periodic poll (watch/event-driven only).
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
	// Set via overlay for now; long-term this moves to the Orbital schema.
	VeleroAzureResourceGroup string

	// VeleroAzureSubscriptionID is the Azure subscription ID for the
	// storage account(s) referenced by Azure-backed locations.
	// Set via overlay for now; long-term this moves to the Orbital schema.
	VeleroAzureSubscriptionID string

	// VeleroAzureCredentialSecret is the K8s Secret name (in VeleroNamespace)
	// holding the Velero Azure plugin's credentials file (single "cloud" key,
	// AZURE_* env-style content). Distinct from CredentialSecret (etcd's).
	VeleroAzureCredentialSecret string
	// Recorder emits per-action Kubernetes Events (Velero Schedule PATCHed,
	// etcd CronJob PATCHed, PATCH failed, etc.) so operators can see action
	// history via `kubectl describe backupconfig <name>` alongside the
	// K8s-native Events section. Replaces the bounded status.recentPatches[]
	// we used to maintain — see the sc-controller Recorder doc-comment for
	// the full rationale.
	Recorder record.EventRecorder

	// EtcdStore observes the actual etcd snapshots in the backup store (the
	// artifacts bc truly manages for etcd — see docs/reference/BACKUP.md).
	// Nil = observation not configured: bc still manages the CronJob, it just
	// does not read artifacts (no BackupsFresh condition, no etcd artifact
	// metrics). Feature-gated exactly like sc-controller's IDRAC_OBSERVE_INTERVAL.
	EtcdStore EtcdBackupStore

	// EtcdSnapshotStaleAfter is the staleness threshold for the BackupsFresh
	// condition: when the newest snapshot is older than this, BackupsFresh flips
	// to False (SnapshotStale). Health alarm only — never deletes a snapshot
	// (NOT retention). Ignored when EtcdStore is nil.
	EtcdSnapshotStaleAfter time.Duration
}

func (r *BackupConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// GenerationChangedPredicate fires reconcile on Create + spec changes only.
	// Status updates and managedFields-only changes don't bump generation, so
	// they don't re-fire — keeps the log clean.
	b := ctrl.NewControllerManagedBy(mgr).
		For(&armadav1.BackupConfig{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("backupconfig").
		// Watch the etcd CronJob this controller owns: an edit or deletion fires
		// a reconcile instantly, so config drift on the producer is corrected
		// without waiting for the periodic poll. Spec-only predicate — the
		// CronJob's status churns (status.lastScheduleTime bumps every run) and
		// that's Layer-3 runtime, not our concern here. (Snapshot artifacts in
		// the store aren't watchable — those still need the periodic blob poll.)
		Owns(&batchv1.CronJob{}, builder.WithPredicates(predicate.GenerationChangedPredicate{}))

	// Watch the Velero Schedule this controller owns — but only when the Velero
	// CRD is installed. It's an unstructured type, so the RESTMapper must know
	// the GVK or the watch setup fails at startup. bc can manage etcd-only
	// clusters where Velero is absent, so this is best-effort.
	if r.veleroScheduleWatchable(mgr) {
		sched := &unstructured.Unstructured{}
		sched.SetGroupVersionKind(veleroScheduleGVK)
		b = b.Owns(sched, builder.WithPredicates(predicate.GenerationChangedPredicate{}))
	} else {
		mgr.GetLogger().WithName("backupconfig.setup").Info(
			"velero Schedule CRD not installed; skipping Schedule watch (etcd-only observation unaffected)")
	}

	return b.Complete(r)
}

// veleroScheduleWatchable reports whether the Velero Schedule GVK is known to
// the cluster's RESTMapper (i.e. the Velero CRD is installed). Used to make the
// Schedule watch best-effort so bc still starts on etcd-only clusters.
func (r *BackupConfigReconciler) veleroScheduleWatchable(mgr ctrl.Manager) bool {
	_, err := mgr.GetRESTMapper().RESTMapping(
		schema.GroupKind{Group: veleroScheduleGVK.Group, Kind: veleroScheduleGVK.Kind},
		veleroScheduleGVK.Version,
	)
	return err == nil
}

// +kubebuilder:rbac:groups=armada.ai,resources=backupconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=armada.ai,resources=backupconfigs/status,verbs=get;patch;update
// +kubebuilder:rbac:groups=armada.ai,resources=configbundles,verbs=get;list;watch
// +kubebuilder:rbac:groups=velero.io,resources=schedules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *BackupConfigReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx).WithName("backupconfig")

	var bc armadav1.BackupConfig
	if err := r.Get(ctx, req.NamespacedName, &bc); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("backupconfig deleted")
			// Drop this cluster from the etcd artifact metrics so a deleted
			// BackupConfig stops appearing in the inventory.
			removeEtcdArtifacts(req.Name)
			removeReconcileSuccess(req.Name)
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
			"s3SyncOrbId", bc.Spec.S3Sync.OrbID)
	}

	// No reconcilable blocks — deliberately skip. Surface it on status
	// (Phase=Skipped, Reconciled=Unknown) so `kubectl describe` explains the
	// no-op, consistent with sc-controller's skip handling.
	if bc.Spec.Velero == nil && bc.Spec.Etcd == nil {
		logger.V(1).Info("no velero or etcd block on backupconfig; skipping",
			"orbId", bc.Spec.OrbID)
		r.setStatusSkipped(ctx, &bc, "NoBackupBlocks",
			"neither spec.velero nor spec.etcd is set — nothing to reconcile")
		return reconcile.Result{}, nil
	}

	patchMessages := []string{}

	if bc.Spec.Velero != nil {
		msg, err := r.reconcileVelero(ctx, &bc)
		if err != nil {
			logger.Error(err, "reconcile Velero Schedule")
			r.setStatusFailed(ctx, &bc, "VeleroPatchFailed", err.Error())
			recordReconcileError(bc.Name, bc.Spec.OrbID, "VeleroPatchFailed")
			return reconcile.Result{}, fmt.Errorf("reconcile velero: %w", err)
		}
		if msg != "" {
			patchMessages = append(patchMessages, msg)
		}
	}

	if bc.Spec.Etcd != nil {
		msg, err := r.reconcileEtcd(ctx, &bc)
		if err != nil {
			logger.Error(err, "reconcile etcd CronJob")
			r.setStatusFailed(ctx, &bc, "EtcdPatchFailed", err.Error())
			recordReconcileError(bc.Name, bc.Spec.OrbID, "EtcdPatchFailed")
			return reconcile.Result{}, fmt.Errorf("reconcile etcd: %w", err)
		}
		if msg != "" {
			patchMessages = append(patchMessages, msg)
		}
	}

	// One live read, several independent consumers: Prom gauges
	// (resource-presence + etcd artifacts) and CR status. Metrics never depend on
	// the status subresource write succeeding — one read fans out to status AND
	// metrics independently (DOMAIN-CONTROLLER.md §6).
	live := r.readLiveObserved(ctx, &bc, logger)
	// Observe the etcd ARTIFACTS (the actual snapshots in the store) and fold
	// them into `live` so they persist in one status write. Feature-gated on
	// EtcdStore; augments live.Etcd, refreshes artifact metrics, and returns the
	// BackupsFresh verdict. See docs/reference/BACKUP.md.
	freshVerdict, freshObserved := r.observeEtcdArtifacts(ctx, &bc, &live, logger)
	recordPresence(bc.Name, &bc, live)
	r.updateObservedStatus(ctx, &bc, live)
	if freshObserved {
		r.setBackupsFresh(ctx, &bc, freshVerdict)
	}

	recordReconcileSuccess(bc.Name, bc.Spec.OrbID, time.Now().Unix())

	if len(patchMessages) == 0 {
		logger.V(1).Info("reconciled (no PATCH needed)")
		// Steady-state: keep the Reconciled=True condition's LastTransitionTime
		// stable (K8s norm — LTT only moves on Status flip). markReconcileSuccess
		// bumps ObservedGeneration + LastAppliedAt.
		if !meta.IsStatusConditionTrue(bc.Status.Conditions, ConditionReconciled) {
			r.setStatusApplied(ctx, &bc)
		} else {
			r.markReconcileSuccess(ctx, &bc)
		}
		return reconcile.Result{RequeueAfter: r.ObserveInterval}, nil
	}

	patchMsg := strings.Join(patchMessages, "; ")
	logger.Info("reconciled (PATCH applied)", "actions", patchMsg)
	// K8s Event carries per-PATCH detail; Condition stays generic (see
	// setStatusApplied). `kubectl describe backupconfig <name>` shows both.
	r.Recorder.Eventf(&bc, corev1.EventTypeNormal, "Applied", patchMsg)
	r.setStatusApplied(ctx, &bc)
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
	if observedEqual(observedFromStatus(&bc.Status), live) {
		return
	}
	logger := log.FromContext(ctx).WithName("backupconfig.status")
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh armadav1.BackupConfig
		if err := r.Get(ctx, client.ObjectKeyFromObject(bc), &fresh); err != nil {
			return err
		}
		if observedEqual(observedFromStatus(&fresh.Status), live) {
			return nil
		}
		fresh.Status.Velero = live.Velero
		fresh.Status.Etcd = live.Etcd
		fresh.Status.S3Sync = live.S3Sync
		return r.Status().Update(ctx, &fresh)
	})
	if err != nil {
		logger.Info("observed status update failed (will retry next reconcile)", "err", err.Error())
	}
}

// observedFromStatus projects the flattened status blocks back into the
// ObservedBackup DTO so it can be compared against a fresh live read.
func observedFromStatus(s *armadav1.BackupConfigStatus) armadav1.ObservedBackup {
	return armadav1.ObservedBackup{Velero: s.Velero, Etcd: s.Etcd, S3Sync: s.S3Sync}
}

// observeEtcdArtifacts reads the actual etcd snapshots from the backup store —
// the resource bc truly manages for etcd (see docs/reference/BACKUP.md) — and:
//   - augments live.Etcd with the artifact fields, so the single
//     updateObservedStatus write persists them alongside the config fields;
//   - refreshes the artifact metrics (bc is the sole source for etcd);
//   - returns the BackupsFresh verdict, or ok=false when observation is off.
//
// Feature-gated: no-op when EtcdStore is nil or spec.etcd has no location. On a
// store read failure the verdict is Unknown/StoreReadFailed (+ a Warning Event)
// and the artifact metrics keep their last values — the reconcile-timestamp gap
// is the staleness signal, same discipline as sc-controller.
func (r *BackupConfigReconciler) observeEtcdArtifacts(ctx context.Context, bc *armadav1.BackupConfig, live *armadav1.ObservedBackup, logger logr.Logger) (backupsFreshVerdict, bool) {
	if r.EtcdStore == nil || bc.Spec.Etcd == nil || bc.Spec.Etcd.Location == nil {
		removeEtcdArtifacts(bc.Name)
		return backupsFreshVerdict{}, false
	}

	inv, err := r.EtcdStore.List(ctx, *bc.Spec.Etcd.Location)
	if err != nil {
		logger.Info("etcd backup store read failed; artifact freshness unknown this cycle",
			"err", err.Error())
		r.Recorder.Event(bc, corev1.EventTypeWarning, "StoreReadFailed", err.Error())
		return backupsFreshVerdict{metav1.ConditionUnknown, "StoreReadFailed",
			"could not read the etcd backup store: " + err.Error()}, true
	}

	recordEtcdArtifacts(bc.Name, bc.Spec.OrbID, inv)

	// Fold artifact fields into the observed snapshot. Allocate live.Etcd if the
	// CronJob is absent (live.Etcd==nil) but snapshots still exist — the
	// artifacts are real independent of the producer's presence.
	if live.Etcd == nil {
		live.Etcd = &armadav1.ObservedEtcdStatus{}
	}
	count := int32(inv.Count)
	live.Etcd.SnapshotCount = &count
	if inv.Count > 0 {
		t := metav1.NewTime(inv.LatestModified)
		live.Etcd.LastSnapshotTime = &t
		bytes := inv.LatestBytes
		live.Etcd.LatestSnapshotBytes = &bytes
	}

	return etcdFreshness(inv, time.Now(), r.EtcdSnapshotStaleAfter), true
}

// setBackupsFresh upserts the BackupsFresh condition (artifact freshness),
// independent of the Reconciled condition (config fidelity). Best-effort,
// RetryOnConflict, only-write-on-change via meta.SetStatusCondition.
func (r *BackupConfigReconciler) setBackupsFresh(ctx context.Context, bc *armadav1.BackupConfig, v backupsFreshVerdict) {
	logger := log.FromContext(ctx).WithName("backupconfig.status")
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh armadav1.BackupConfig
		if err := r.Get(ctx, client.ObjectKeyFromObject(bc), &fresh); err != nil {
			return err
		}
		meta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{
			Type:               ConditionBackupsFresh,
			Status:             v.status,
			Reason:             v.reason,
			Message:            v.message,
			ObservedGeneration: fresh.Generation,
		})
		return r.Status().Update(ctx, &fresh)
	})
	if err != nil {
		logger.Info("BackupsFresh condition update failed (will retry next reconcile)", "err", err.Error())
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
				"err", err.Error())
		}
		out.Velero = v
	}
	if bc.Spec.Etcd != nil {
		e, err := observeEtcd(ctx, r.Client, r.EtcdBackupNamespace, etcdCronJobName(bc))
		if err != nil {
			logger.Info("observe etcd live state failed; reporting as absent this reconcile",
				"err", err.Error())
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
		stringPtrEqual(a.Location, b.Location) &&
		// Artifact fields must be compared too, or an artifact-only change
		// (new snapshot, count bump) wouldn't trigger a status write.
		timePtrEqual(a.LastSnapshotTime, b.LastSnapshotTime) &&
		int32PtrEqual(a.SnapshotCount, b.SnapshotCount) &&
		int64PtrEqual(a.LatestSnapshotBytes, b.LatestSnapshotBytes)
}

func timePtrEqual(a, b *metav1.Time) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.Equal(b)
}

func int32PtrEqual(a, b *int32) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func int64PtrEqual(a, b *int64) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
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

// setStatusApplied writes Phase=Applied + Reconciled=True with a stable,
// generic Reason and Message — the K8s norm is that Condition Message
// describes STATE, not last-ACTION. Per-PATCH action detail goes to
// Kubernetes Events (r.Recorder), not the Condition. Bumps LastAppliedAt.
func (r *BackupConfigReconciler) setStatusApplied(ctx context.Context, bc *armadav1.BackupConfig) {
	r.writeStatus(ctx, bc, armadav1.BackupConfigPhaseApplied, metav1.ConditionTrue,
		"SettingsApplied", "all backup settings match intent", true /* bumpLastApplied */)
}

// setStatusFailed writes Phase=Diverged + Reconciled=False with a Reason that
// names which step failed (VeleroPatchFailed, EtcdPatchFailed). Does NOT
// bump LastAppliedAt — the reconcile did not succeed.
//
// Emits a Warning Event so failures surface via `kubectl describe backupconfig`
// / `kubectl get events`, symmetric with the Normal "Applied" events on
// success and with sc-controller. K8s Event aggregation dedups repeats.
func (r *BackupConfigReconciler) setStatusFailed(ctx context.Context, bc *armadav1.BackupConfig, reason, msg string) {
	r.Recorder.Event(bc, corev1.EventTypeWarning, reason, msg)
	r.writeStatus(ctx, bc, armadav1.BackupConfigPhaseDiverged, metav1.ConditionFalse, reason, msg, false)
}

// setStatusSkipped writes Phase=Skipped + Reconciled=Unknown with a Reason
// describing why the controller deliberately did not reconcile this CR (e.g.
// NoBackupBlocks — neither spec.velero nor spec.etcd is set). Unknown, not
// False: a skip is an expected, benign state, not a fault, so it stays out of
// the False bucket that operators alert on. Mirrors sc-controller's
// setStatusSkipped. Does NOT bump LastAppliedAt — no apply happened, and does
// NOT emit a Warning Event — a skip is not an error.
func (r *BackupConfigReconciler) setStatusSkipped(ctx context.Context, bc *armadav1.BackupConfig, reason, msg string) {
	// Skipped ⇒ not managed here ⇒ the reconcile_success series must be ABSENT
	// (absent = skip), not 0/stale.
	removeReconcileSuccess(bc.Name)
	r.writeStatus(ctx, bc, armadav1.BackupConfigPhaseSkipped, metav1.ConditionUnknown, reason, msg, false)
}

func (r *BackupConfigReconciler) writeStatus(ctx context.Context, bc *armadav1.BackupConfig, phase armadav1.BackupConfigPhase, condStatus metav1.ConditionStatus, reason, msg string, bumpLastApplied bool) {
	logger := log.FromContext(ctx).WithName("backupconfig.status")
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh armadav1.BackupConfig
		if err := r.Get(ctx, client.ObjectKeyFromObject(bc), &fresh); err != nil {
			return err
		}
		fresh.Status.Phase = phase
		fresh.Status.ObservedGeneration = fresh.Generation
		if bumpLastApplied {
			now := metav1.Now()
			fresh.Status.LastAppliedAt = &now
		}
		// meta.SetStatusCondition: apimachinery-canonical upsert — LTT moves
		// only on Status flip, Reason/Message/ObservedGeneration always refresh.
		meta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{
			Type:               ConditionReconciled,
			Status:             condStatus,
			Reason:             reason,
			Message:            msg,
			ObservedGeneration: fresh.Generation,
		})
		syncS3SyncCondition(&fresh)
		return r.Status().Update(ctx, &fresh)
	})
	if err != nil {
		logger.Info("status update failed (will retry next reconcile)", "err", err.Error())
	}
}

// syncS3SyncCondition mirrors spec.s3Sync presence into the S3SyncSupported
// condition. Present → False/NotImplemented. Absent → condition removed so
// stale "unsupported" state does not linger on a CR that dropped its s3Sync
// block. Called from writeStatus so every status write keeps the condition
// in lockstep with the current spec.
func syncS3SyncCondition(bc *armadav1.BackupConfig) {
	if bc.Spec.S3Sync != nil {
		meta.SetStatusCondition(&bc.Status.Conditions, metav1.Condition{
			Type:               ConditionS3SyncSupported,
			Status:             metav1.ConditionFalse,
			Reason:             "NotImplemented",
			Message:            "spec.s3Sync present; S3Sync actuation not yet implemented",
			ObservedGeneration: bc.Generation,
		})
		return
	}
	meta.RemoveStatusCondition(&bc.Status.Conditions, ConditionS3SyncSupported)
}

// markReconcileSuccess bumps status.observedGeneration and status.lastAppliedAt
// on steady-state successful reconciles (Reconciled=True stays True). See
// serverconfig-controller markReconcileSuccess doc for the full rationale —
// LTT stays put per K8s norm; LastAppliedAt is the truthful "still doing work"
// signal; both are skipped when neither would change to keep steady-state
// polls apiserver-write-free.
func (r *BackupConfigReconciler) markReconcileSuccess(ctx context.Context, bc *armadav1.BackupConfig) {
	needBump := bc.Status.ObservedGeneration != bc.Generation || bc.Status.LastAppliedAt == nil
	if !needBump && r.ObserveInterval > 0 && time.Since(bc.Status.LastAppliedAt.Time) > r.ObserveInterval/2 {
		needBump = true
	}
	if !needBump {
		return
	}
	logger := log.FromContext(ctx).WithName("backupconfig.status")
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh armadav1.BackupConfig
		if err := r.Get(ctx, client.ObjectKeyFromObject(bc), &fresh); err != nil {
			return err
		}
		changed := false
		if fresh.Status.ObservedGeneration != fresh.Generation {
			fresh.Status.ObservedGeneration = fresh.Generation
			changed = true
		}
		if fresh.Status.LastAppliedAt == nil || (r.ObserveInterval > 0 && time.Since(fresh.Status.LastAppliedAt.Time) > r.ObserveInterval/2) {
			now := metav1.Now()
			fresh.Status.LastAppliedAt = &now
			changed = true
		}
		if !changed {
			return nil
		}
		return r.Status().Update(ctx, &fresh)
	})
	if err != nil {
		logger.Info("reconcile-marker update failed", "err", err.Error())
	}
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
