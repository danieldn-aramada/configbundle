package backupconfig

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// etcdSnapshotInventory is the observed artifact state of the etcd snapshots in
// the backup store at a point in time — what bc reads, and reports.
type etcdSnapshotInventory struct {
	// Count is how many snapshot objects exist under the cluster's prefix.
	Count int
	// LatestModified is the modification time of the newest object. Zero when
	// Count == 0.
	LatestModified time.Time
	// LatestBytes is the size of the newest object. Zero when Count == 0.
	LatestBytes int64
}

// EtcdBackupStore reads the etcd snapshot inventory from the backup store.
//
// This is the external boundary bc observes for etcd (it fully owns the etcd
// backup stack; no independent subsystem watches the store — see
// docs/reference/BACKUP.md). It is an interface so the reconcile/observe logic
// is testable with a fake; the concrete Azure Blob implementation is a separate
// concern. Exported because cmd/ constructs the implementation and injects it.
// A nil EtcdBackupStore on the reconciler means observation is not configured —
// bc still manages the CronJob, it just doesn't read artifacts.
type EtcdBackupStore interface {
	// List returns the current snapshot inventory for the given
	// spec.etcd.location (an Azure Blob HTTPS URL). An empty store returns
	// Count == 0 and no error; only genuine read failures return an error.
	List(ctx context.Context, location string) (etcdSnapshotInventory, error)
}

// backupsFreshVerdict is the classification of the artifact state for the
// BackupsFresh condition.
type backupsFreshVerdict struct {
	status  metav1.ConditionStatus
	reason  string
	message string
}

// etcdFreshness classifies a snapshot inventory against a staleness threshold
// into a BackupsFresh verdict. Pure function — the controller supplies `now`
// and the configured staleAfter age. This is a health signal only; it never
// deletes a snapshot (retention is not implemented).
//
//   - no snapshots yet             → Unknown / NoSnapshotsYet (could be a
//     freshly-created config warming up, or genuinely broken; let Prometheus
//     alert on a sustained count==0 rather than false-alarm here)
//   - newest younger than staleAfter → True  / RecentSnapshotPresent
//   - newest older than staleAfter   → False / SnapshotStale (backups were
//     landing and stopped — a confident fault)
func etcdFreshness(inv etcdSnapshotInventory, now time.Time, staleAfter time.Duration) backupsFreshVerdict {
	if inv.Count == 0 {
		return backupsFreshVerdict{metav1.ConditionUnknown, "NoSnapshotsYet",
			"no etcd snapshot observed in the backup store yet"}
	}
	age := now.Sub(inv.LatestModified)
	if age <= staleAfter {
		return backupsFreshVerdict{metav1.ConditionTrue, "RecentSnapshotPresent",
			fmt.Sprintf("latest etcd snapshot %s ago (stale after %s)", age.Round(time.Second), staleAfter)}
	}
	return backupsFreshVerdict{metav1.ConditionFalse, "SnapshotStale",
		fmt.Sprintf("latest etcd snapshot %s ago, older than the %s staleness threshold", age.Round(time.Second), staleAfter)}
}
