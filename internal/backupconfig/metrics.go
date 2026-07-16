package backupconfig

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	armadav1 "github.com/armada/configbundle/api/v1"
)

// bc-controller metrics, per docs/reference/DOMAIN-CONTROLLER.md §6. Intent
// (.spec) is NOT a metric — the cloud/orbital already authored it, so there is
// no `_spec_`/intent gauge and no PromQL `spec != status` drift query (the
// controller already computes drift; that is reconciliation). We emit only what
// the cloud cannot otherwise know:
//   - reconcile OUTCOME: reconcile_success (per-object 0/1, absent = skip),
//     reconcile timestamp, error counter;
//   - OBSERVED state: resource-presence gauges + the etcd artifact metrics
//     (snapshot freshness/count/size — the reads-only outcome layer).
//
// One live read fans out to these gauges AND to CR status independently; metrics
// never block on a status write. Labels are identity only, both 1:1 with the
// object (zero added cardinality): {cluster} (the CR name, for kubectl) and
// {orb_id} (spec.orbId, the immutable orbital node identity — the uniform
// cross-controller join key to the CMDB and divergence reports). Never a value,
// timestamp, or unbounded id.
var (
	// backupConfigReconcileTimestamp is set to time.Now().Unix() after every
	// successful reconcile. Alerts on "bc-controller has stopped reconciling
	// this CR" fire when (time() - <this>) exceeds an expected reconcile
	// cadence. Populated only on success paths — failures leave the last
	// success timestamp alone so operators see how stale the current state is.
	backupConfigReconcileTimestamp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "configbundle_backupconfig_reconcile_timestamp_seconds",
		Help: "Unix timestamp of the last successful reconcile of this BackupConfig CR. Absent series = never reconciled.",
	}, []string{"cluster", "orb_id"})

	// backupConfigReconcileErrors counts reconcile failures by cause. Labels
	// stay bounded (fixed set of reason strings: VeleroPatchFailed,
	// EtcdPatchFailed). Rate() over this counter tells cloud dashboards
	// which failure mode is dominant.
	backupConfigReconcileErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "configbundle_backupconfig_reconciliation_errors_total",
		Help: "Cumulative reconcile failures per BackupConfig CR, labelled by failure reason (VeleroPatchFailed | EtcdPatchFailed).",
	}, []string{"cluster", "orb_id", "reason"})

	// backupConfigReconcileSuccess is the per-object "did the last reconcile
	// converge?" level: 1 = converged, 0 = failed. ABSENT for a deliberately
	// skipped BackupConfig (no velero/etcd block) or one never reconciled — so
	// `== 0` alerts fire only for genuinely-broken clusters. The load-bearing
	// "which cluster is failing?" signal. Set from recordReconcileSuccess/Error;
	// deleted by removeReconcileSuccess on skip and on CR delete.
	backupConfigReconcileSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "configbundle_backupconfig_reconcile_success",
		Help: "1 if this BackupConfig's last reconcile converged, 0 if it failed. Absent = deliberately skipped or never reconciled.",
	}, []string{"cluster", "orb_id"})

	// veleroSchedulePresent = 1 when the underlying Velero Schedule CR that
	// bc-controller manages for this cluster exists on-cluster, 0 when spec
	// asked for it but live is missing (deleted OOB, CRD absent, or apply
	// pending). Series is deleted entirely when spec.velero is nil, so
	// "no series" means "not managed" — distinct from "should exist but is
	// gone (0)". Same semantic for etcd below.
	veleroSchedulePresent = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "configbundle_backup_velero_schedule_present",
		Help: "1 when the live Velero Schedule for this BackupConfig exists on-cluster; 0 when spec.velero is set but the live resource is missing.",
	}, []string{"cluster", "orb_id"})

	etcdCronJobPresent = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "configbundle_backup_etcd_cronjob_present",
		Help: "1 when the live etcd CronJob for this BackupConfig exists on-cluster; 0 when spec.etcd is set but the live resource is missing.",
	}, []string{"cluster", "orb_id"})
)

// etcdArtifacts is the snapshot store the etcd artifact Collector renders each
// scrape. Fed by the reconciler from its live read of the backup store.
var etcdArtifacts = newEtcdArtifactStore()

func init() {
	metrics.Registry.MustRegister(
		backupConfigReconcileTimestamp, backupConfigReconcileErrors, backupConfigReconcileSuccess,
		veleroSchedulePresent, etcdCronJobPresent,
		newEtcdArtifactCollector(etcdArtifacts),
	)
}

// recordPresence updates the resource-present gauges from a live snapshot.
// Nil block for a mechanism spec-asks-for = live-missing (0). Nil block for a
// mechanism spec-does-not-ask-for = delete the series entirely (mechanism
// not managed, absence is not "missing"). Called from the same live-read
// snapshot as the other observed metrics so all surfaces stay coherent.
func recordPresence(cluster string, bc *armadav1.BackupConfig, live armadav1.ObservedBackup) {
	labels := prometheus.Labels{"cluster": cluster, "orb_id": bc.Spec.OrbID}
	if bc.Spec.Velero != nil {
		if live.Velero != nil {
			veleroSchedulePresent.With(labels).Set(1)
		} else {
			veleroSchedulePresent.With(labels).Set(0)
		}
	} else {
		veleroSchedulePresent.Delete(labels)
	}
	if bc.Spec.Etcd != nil {
		if live.Etcd != nil {
			etcdCronJobPresent.With(labels).Set(1)
		} else {
			etcdCronJobPresent.With(labels).Set(0)
		}
	} else {
		etcdCronJobPresent.Delete(labels)
	}
}

// recordReconcileSuccess marks a successful reconcile: bumps the last-success
// timestamp and sets the reconcile_success level to 1.
func recordReconcileSuccess(cluster, orbID string, now int64) {
	labels := prometheus.Labels{"cluster": cluster, "orb_id": orbID}
	backupConfigReconcileTimestamp.With(labels).Set(float64(now))
	backupConfigReconcileSuccess.With(labels).Set(1)
}

// recordReconcileError increments the failure counter for the given reason and
// drops the reconcile_success level to 0. Reason strings should stay bounded —
// pick from a fixed enum, do not interpolate error messages.
func recordReconcileError(cluster, orbID, reason string) {
	backupConfigReconcileErrors.With(prometheus.Labels{"cluster": cluster, "orb_id": orbID, "reason": reason}).Inc()
	backupConfigReconcileSuccess.With(prometheus.Labels{"cluster": cluster, "orb_id": orbID}).Set(0)
}

// removeReconcileSuccess deletes a cluster's reconcile_success series so it
// becomes absent — on skip (absent = skip, never 0) or CR delete. Partial match
// on the stable {cluster} key, so removal needs no orbID.
func removeReconcileSuccess(cluster string) {
	backupConfigReconcileSuccess.DeletePartialMatch(prometheus.Labels{"cluster": cluster})
}

// -----------------------------------------------------------------------------
// etcd ARTIFACT metrics: the observed state of the actual snapshots in the
// backup store (not the CronJob config). bc is the SOLE source for etcd — no
// downstream owner emits these. Collector-over-snapshot, rebuilt each scrape,
// mirroring serverconfig's idracObservedCollector: a new snapshot replaces the
// series rather than leaving a stale one.
//
//	configbundle_backupconfig_status_etcd_last_snapshot_seconds{cluster}
//	configbundle_backupconfig_status_etcd_snapshot_count{cluster}
//	configbundle_backupconfig_status_etcd_latest_bytes{cluster}
//
// Do NOT add velero equivalents — Velero owns its backups; defer to velero_*.
// See docs/reference/BACKUP.md.
// -----------------------------------------------------------------------------

// etcdArtifactStore is a concurrency-safe latest-inventory map keyed by cluster
// (BackupConfig name). The reconciler writes from its live store read; the
// scrape goroutine reads. Holds only the current view. A read failure leaves
// the last entry in place (stale), so metrics show last-known while the
// reconcile-timestamp gap signals the staleness — same discipline as
// serverconfig.
// etcdArtifactEntry pairs a cluster's observed snapshot inventory with its orbId
// (spec.orbId) so the artifact metrics can carry the orb_id identity label.
type etcdArtifactEntry struct {
	orbID string
	inv   etcdSnapshotInventory
}

type etcdArtifactStore struct {
	mu        sync.RWMutex
	byCluster map[string]etcdArtifactEntry
}

func newEtcdArtifactStore() *etcdArtifactStore {
	return &etcdArtifactStore{byCluster: map[string]etcdArtifactEntry{}}
}

func (s *etcdArtifactStore) set(cluster, orbID string, inv etcdSnapshotInventory) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byCluster[cluster] = etcdArtifactEntry{orbID: orbID, inv: inv}
}

func (s *etcdArtifactStore) remove(cluster string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byCluster, cluster)
}

func (s *etcdArtifactStore) snapshot() map[string]etcdArtifactEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]etcdArtifactEntry, len(s.byCluster))
	for k, v := range s.byCluster {
		out[k] = v
	}
	return out
}

type etcdArtifactCollector struct {
	store        *etcdArtifactStore
	lastSnapDesc *prometheus.Desc
	countDesc    *prometheus.Desc
	bytesDesc    *prometheus.Desc
}

func newEtcdArtifactCollector(store *etcdArtifactStore) *etcdArtifactCollector {
	return &etcdArtifactCollector{
		store: store,
		lastSnapDesc: prometheus.NewDesc(
			"configbundle_backupconfig_status_etcd_last_snapshot_seconds",
			"Unix time of the newest etcd snapshot in the backup store (bc is the sole source). Absent when no snapshot has been observed.",
			[]string{"cluster", "orb_id"}, nil),
		countDesc: prometheus.NewDesc(
			"configbundle_backupconfig_status_etcd_snapshot_count",
			"Number of etcd snapshot objects observed under the cluster's prefix (0 when the store is empty).",
			[]string{"cluster", "orb_id"}, nil),
		bytesDesc: prometheus.NewDesc(
			"configbundle_backupconfig_status_etcd_latest_bytes",
			"Size in bytes of the newest etcd snapshot object. Absent when no snapshot has been observed.",
			[]string{"cluster", "orb_id"}, nil),
	}
}

func (c *etcdArtifactCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.lastSnapDesc
	ch <- c.countDesc
	ch <- c.bytesDesc
}

func (c *etcdArtifactCollector) Collect(ch chan<- prometheus.Metric) {
	for cluster, e := range c.store.snapshot() {
		inv := e.inv
		ch <- prometheus.MustNewConstMetric(c.countDesc, prometheus.GaugeValue, float64(inv.Count), cluster, e.orbID)
		if inv.Count > 0 && !inv.LatestModified.IsZero() {
			ch <- prometheus.MustNewConstMetric(c.lastSnapDesc, prometheus.GaugeValue, float64(inv.LatestModified.Unix()), cluster, e.orbID)
			ch <- prometheus.MustNewConstMetric(c.bytesDesc, prometheus.GaugeValue, float64(inv.LatestBytes), cluster, e.orbID)
		}
	}
}

// recordEtcdArtifacts publishes a cluster's observed snapshot inventory to the
// artifact metrics. removeEtcdArtifacts drops it (BackupConfig deleted, or etcd
// no longer in spec).
func recordEtcdArtifacts(cluster, orbID string, inv etcdSnapshotInventory) {
	etcdArtifacts.set(cluster, orbID, inv)
}
func removeEtcdArtifacts(cluster string) { etcdArtifacts.remove(cluster) }
