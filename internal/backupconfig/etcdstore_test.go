package backupconfig

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	armadav1 "github.com/armada/configbundle/api/v1"
)

// TestBackupReconcileSuccessGauge pins the reconcile_success contract: success →
// 1, failure → 0, skip/delete → ABSENT (never 0 — absence keeps a skipped
// cluster out of `== 0` alerts).
func TestBackupReconcileSuccessGauge(t *testing.T) {
	defer removeReconcileSuccess("bc-ok")
	defer removeReconcileSuccess("bc-fail")

	recordReconcileSuccess("bc-ok", "orb-bc-ok", 1234567890)
	recordReconcileError("bc-fail", "orb-bc-fail", "EtcdPatchFailed")

	if v := testutil.ToFloat64(backupConfigReconcileSuccess.WithLabelValues("bc-ok", "orb-bc-ok")); v != 1 {
		t.Errorf("bc-ok = %v, want 1 (converged)", v)
	}
	if v := testutil.ToFloat64(backupConfigReconcileSuccess.WithLabelValues("bc-fail", "orb-bc-fail")); v != 0 {
		t.Errorf("bc-fail = %v, want 0 (failed)", v)
	}

	// skip/delete ⇒ series absent, not 0.
	removeReconcileSuccess("bc-fail")
	reg := prometheus.NewRegistry()
	reg.MustRegister(backupConfigReconcileSuccess)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "cluster" && l.GetValue() == "bc-fail" {
					t.Errorf("bc-fail series should be absent after remove")
				}
			}
		}
	}
}

// fakeEtcdStore is a canned etcdBackupStore for tests.
type fakeEtcdStore struct {
	inv   etcdSnapshotInventory
	err   error
	calls int
}

func (f *fakeEtcdStore) List(_ context.Context, _ string) (etcdSnapshotInventory, error) {
	f.calls++
	return f.inv, f.err
}

func TestEtcdFreshness(t *testing.T) {
	now := time.Now()
	staleAfter := 26 * time.Hour

	cases := []struct {
		name       string
		inv        etcdSnapshotInventory
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{"empty", etcdSnapshotInventory{Count: 0}, metav1.ConditionUnknown, "NoSnapshotsYet"},
		{"fresh", etcdSnapshotInventory{Count: 3, LatestModified: now.Add(-4 * time.Hour)}, metav1.ConditionTrue, "RecentSnapshotPresent"},
		{"at max", etcdSnapshotInventory{Count: 1, LatestModified: now.Add(-26 * time.Hour)}, metav1.ConditionTrue, "RecentSnapshotPresent"},
		{"stale", etcdSnapshotInventory{Count: 3, LatestModified: now.Add(-48 * time.Hour)}, metav1.ConditionFalse, "SnapshotStale"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := etcdFreshness(tc.inv, now, staleAfter)
			if v.status != tc.wantStatus {
				t.Errorf("status = %q, want %q", v.status, tc.wantStatus)
			}
			if v.reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", v.reason, tc.wantReason)
			}
		})
	}
}

// gatherEtcdSeries renders an artifact collector into "name{k=v,...}" lines for
// substring assertions on which series exist, using a private registry.
func gatherEtcdSeries(t *testing.T, c prometheus.Collector) string {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var sb strings.Builder
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			sb.WriteString(mf.GetName() + "{")
			for _, l := range m.GetLabel() {
				sb.WriteString(l.GetName() + "=" + l.GetValue() + ",")
			}
			sb.WriteString("}\n")
		}
	}
	return sb.String()
}

func TestEtcdArtifactCollector(t *testing.T) {
	store := newEtcdArtifactStore()
	c := newEtcdArtifactCollector(store)

	// count>0: all three series present.
	store.set("colo-cluster-001", "orb-c1", etcdSnapshotInventory{
		Count: 7, LatestModified: time.Unix(1720000000, 0), LatestBytes: 1048576,
	})
	got := gatherEtcdSeries(t, c)
	for _, want := range []string{
		"configbundle_backupconfig_status_etcd_snapshot_count{cluster=colo-cluster-001,orb_id=orb-c1,}",
		"configbundle_backupconfig_status_etcd_last_snapshot_seconds{cluster=colo-cluster-001,orb_id=orb-c1,}",
		"configbundle_backupconfig_status_etcd_latest_bytes{cluster=colo-cluster-001,orb_id=orb-c1,}",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected series %q\ngot:\n%s", want, got)
		}
	}

	// empty: count series present (=0), but no last_snapshot / bytes series.
	store.set("empty-cluster", "orb-empty", etcdSnapshotInventory{Count: 0})
	got = gatherEtcdSeries(t, c)
	if !strings.Contains(got, "snapshot_count{cluster=empty-cluster,orb_id=orb-empty,}") {
		t.Errorf("empty cluster should still emit snapshot_count; got:\n%s", got)
	}
	if strings.Contains(got, "last_snapshot_seconds{cluster=empty-cluster,orb_id=orb-empty,}") {
		t.Errorf("empty cluster must NOT emit last_snapshot_seconds; got:\n%s", got)
	}

	// remove drops all series for a cluster.
	store.remove("colo-cluster-001")
	got = gatherEtcdSeries(t, c)
	if strings.Contains(got, "cluster=colo-cluster-001,") {
		t.Errorf("removed cluster still present:\n%s", got)
	}
}

func TestObserveEtcd_FreshSetsStatusAndCondition(t *testing.T) {
	bc := sampleBackupConfig()
	r, c := newReconciler(t, bc)
	r.EtcdStore = &fakeEtcdStore{inv: etcdSnapshotInventory{
		Count: 7, LatestModified: time.Now().Add(-4 * time.Hour), LatestBytes: 1048576,
	}}
	r.EtcdSnapshotStaleAfter = 26 * time.Hour

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: bc.Name},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got armadav1.BackupConfig
	if err := c.Get(context.Background(), types.NamespacedName{Name: bc.Name}, &got); err != nil {
		t.Fatalf("get bc: %v", err)
	}
	if got.Status.Etcd == nil || got.Status.Etcd.SnapshotCount == nil {
		t.Fatalf("expected observed.etcd artifact fields populated; got %+v", got.Status.Etcd)
	}
	if *got.Status.Etcd.SnapshotCount != 7 {
		t.Errorf("snapshotCount = %d, want 7", *got.Status.Etcd.SnapshotCount)
	}
	if got.Status.Etcd.LastSnapshotTime == nil {
		t.Errorf("expected lastSnapshotTime set")
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, ConditionBackupsFresh)
	if cond == nil {
		t.Fatalf("BackupsFresh condition missing")
	}
	if cond.Status != metav1.ConditionTrue || cond.Reason != "RecentSnapshotPresent" {
		t.Errorf("BackupsFresh = %s/%s, want True/RecentSnapshotPresent", cond.Status, cond.Reason)
	}
}

func TestObserveEtcd_StaleIsFalse(t *testing.T) {
	bc := sampleBackupConfig()
	r, c := newReconciler(t, bc)
	r.EtcdStore = &fakeEtcdStore{inv: etcdSnapshotInventory{
		Count: 3, LatestModified: time.Now().Add(-72 * time.Hour), LatestBytes: 999,
	}}
	r.EtcdSnapshotStaleAfter = 26 * time.Hour

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: bc.Name},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got armadav1.BackupConfig
	if err := c.Get(context.Background(), types.NamespacedName{Name: bc.Name}, &got); err != nil {
		t.Fatalf("get bc: %v", err)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, ConditionBackupsFresh)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "SnapshotStale" {
		t.Errorf("BackupsFresh = %v, want False/SnapshotStale", cond)
	}
}

func TestObserveEtcd_EmptyIsUnknown(t *testing.T) {
	bc := sampleBackupConfig()
	r, c := newReconciler(t, bc)
	r.EtcdStore = &fakeEtcdStore{inv: etcdSnapshotInventory{Count: 0}}
	r.EtcdSnapshotStaleAfter = 26 * time.Hour

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: bc.Name},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got armadav1.BackupConfig
	if err := c.Get(context.Background(), types.NamespacedName{Name: bc.Name}, &got); err != nil {
		t.Fatalf("get bc: %v", err)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, ConditionBackupsFresh)
	if cond == nil || cond.Status != metav1.ConditionUnknown || cond.Reason != "NoSnapshotsYet" {
		t.Errorf("BackupsFresh = %v, want Unknown/NoSnapshotsYet", cond)
	}
}

func TestObserveEtcd_StoreErrorUnknownPlusEvent(t *testing.T) {
	bc := sampleBackupConfig()
	r, c := newReconciler(t, bc)
	r.EtcdStore = &fakeEtcdStore{err: context.DeadlineExceeded}
	r.EtcdSnapshotStaleAfter = 26 * time.Hour

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: bc.Name},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got armadav1.BackupConfig
	if err := c.Get(context.Background(), types.NamespacedName{Name: bc.Name}, &got); err != nil {
		t.Fatalf("get bc: %v", err)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, ConditionBackupsFresh)
	if cond == nil || cond.Status != metav1.ConditionUnknown || cond.Reason != "StoreReadFailed" {
		t.Errorf("BackupsFresh = %v, want Unknown/StoreReadFailed", cond)
	}
	fr := r.Recorder.(*record.FakeRecorder)
	found := false
	for done := false; !done; {
		select {
		case ev := <-fr.Events:
			if strings.Contains(ev, "Warning") && strings.Contains(ev, "StoreReadFailed") {
				found = true
			}
		default:
			done = true
		}
	}
	if !found {
		t.Errorf("expected a Warning StoreReadFailed event")
	}
}

func TestObserveEtcd_DisabledNoConditionNoArtifacts(t *testing.T) {
	bc := sampleBackupConfig()
	r, c := newReconciler(t, bc) // no EtcdStore → observation off

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: bc.Name},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got armadav1.BackupConfig
	if err := c.Get(context.Background(), types.NamespacedName{Name: bc.Name}, &got); err != nil {
		t.Fatalf("get bc: %v", err)
	}
	if cond := meta.FindStatusCondition(got.Status.Conditions, ConditionBackupsFresh); cond != nil {
		t.Errorf("BackupsFresh must be absent when observation is disabled; got %v", cond)
	}
	if got.Status.Etcd != nil && got.Status.Etcd.SnapshotCount != nil {
		t.Errorf("artifact fields must be absent when observation is disabled")
	}
}
