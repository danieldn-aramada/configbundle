package backupconfig

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	armadav1 "github.com/armada/configbundle/api/v1"
)

// TestMetricsEndpoint_BackupConfig is the integration check for §6: after a real
// reconcile, every expected metric family actually renders on the /metrics HTTP
// endpoint (promhttp over the controller-runtime registry) — proving they are
// registered and scrapeable, which the unit collector tests don't cover.
func TestMetricsEndpoint_BackupConfig(t *testing.T) {
	bc := &armadav1.BackupConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "metrics-int"},
		Spec: armadav1.BackupConfigSpec{
			OrbID:        "validate:etcd",
			ClusterOrbID: "validate:cluster",
			Etcd: &armadav1.EtcdBackupSpec{
				OrbID:    "validate:etcd-block",
				Enabled:  ptr.To(true),
				Schedule: ptr.To("0 3 * * *"),
				Location: ptr.To("https://acct.blob.core.windows.net/etcd/c1"),
			},
		},
	}
	defer func() {
		removeReconcileSuccess("metrics-int")
		removeEtcdArtifacts("metrics-int")
	}()

	r, _ := newReconciler(t, bc)
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "metrics-int"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	srv := httptest.NewServer(promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	page := string(body)

	for _, want := range []string{
		`configbundle_backupconfig_reconcile_success{cluster="metrics-int",orb_id="validate:etcd"} 1`,
		`configbundle_backupconfig_reconcile_timestamp_seconds{cluster="metrics-int",orb_id="validate:etcd"}`,
		`configbundle_backup_etcd_cronjob_present{cluster="metrics-int",orb_id="validate:etcd"}`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/metrics endpoint missing %q", want)
		}
	}
}

// TestBackupConfigStatus_JSONRoundTrip pins the reshaped status serialization:
// observed state lives at status.{velero,etcd,s3Sync} directly (not the old
// status.observed.* wrapper).
func TestBackupConfigStatus_JSONRoundTrip(t *testing.T) {
	in := armadav1.BackupConfigStatus{
		Etcd: &armadav1.ObservedEtcdStatus{
			Schedule:      ptr.To("0 3 * * *"),
			SnapshotCount: ptr.To(int32(7)),
		},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(b)
	for _, want := range []string{`"etcd":`, `"schedule":"0 3 * * *"`, `"snapshotCount":7`} {
		if !strings.Contains(js, want) {
			t.Errorf("marshalled status missing %q\ngot: %s", want, js)
		}
	}
	if strings.Contains(js, `"observed":`) {
		t.Errorf("marshalled status must not contain the removed 'observed' wrapper\ngot: %s", js)
	}
	var out armadav1.BackupConfigStatus
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Etcd == nil || out.Etcd.SnapshotCount == nil || *out.Etcd.SnapshotCount != 7 {
		t.Errorf("round-trip etcd.snapshotCount = %+v, want 7", out.Etcd)
	}
}

// testLogger returns a no-op logr.Logger for tests that call methods
// requiring a logger but don't care about the output.
func testLogger() logr.Logger { return logr.Discard() }

// makeEtcdPodSpec builds a minimal PodTemplateSpec matching the shape
// buildEtcdCronJob writes: one container named etcdBackupContainerName with
// the location wired through BACKUP_LOCATION env. Used by observed-status
// tests that need a live CronJob to read. The container name and env-var
// name match what buildEtcdCronJob produces so observed / delta reads on the
// fake client find the fields they expect.
func makeEtcdPodSpec(container string) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  etcdSnapshotWriterContainerName,
				Image: testUploadImage,
				Env: []corev1.EnvVar{
					{Name: "STORAGE_ACCOUNT", Value: testStorageAccount},
					{Name: "STORAGE_CONTAINER", Value: container},
				},
			}},
		},
	}
}

const (
	testVeleroNs       = "velero"
	testEtcdNs         = "kube-system"
	testEtcdctlImage   = "test/etcdctl:test"
	testUploadImage    = "test/azure-cli:test"
	testCredSecret     = "test-az-creds"
	testStorageAccount = "teststorage"
)

func newReconciler(t *testing.T, objs ...client.Object) (*BackupConfigReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := armadav1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&armadav1.BackupConfig{}).
		Build()
	r := &BackupConfigReconciler{
		Client:              c,
		Scheme:              scheme,
		VeleroNamespace:     testVeleroNs,
		EtcdBackupNamespace: testEtcdNs,
		EtcdctlImage:        testEtcdctlImage,
		UploadImage:         testUploadImage,
		CredentialSecret:    testCredSecret,
		// FakeRecorder with a generous buffer — tests that assert on Events
		// read from r.Recorder.(*record.FakeRecorder).Events; tests that don't
		// care just let events fall into the channel and get GC'd.
		Recorder: record.NewFakeRecorder(32),
	}
	return r, c
}

func sampleBackupConfig() *armadav1.BackupConfig {
	return &armadav1.BackupConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "colo-cluster-001"},
		Spec: armadav1.BackupConfigSpec{
			OrbID:        "colo:cluster-001-backup",
			ClusterOrbID: "colo:cluster-001",
			Velero: &armadav1.VeleroBackupSpec{
				OrbID:    "colo:cluster-001-velero",
				Enabled:  ptr.To(true),
				Schedule: ptr.To("0 2 * * *"),
				Location: ptr.To("default"),
			},
			Etcd: &armadav1.EtcdBackupSpec{
				OrbID:    "colo:cluster-001-etcd",
				Enabled:  ptr.To(true),
				Schedule: ptr.To("0 3 * * *"),
				Location: ptr.To("https://teststorage.blob.core.windows.net/etcd-backups/colo/cluster-001"),
			},
		},
	}
}

func TestReconcile_CreatesVeleroScheduleAndEtcdCronJob(t *testing.T) {
	bc := sampleBackupConfig()
	r, c := newReconciler(t, bc)

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: bc.Name},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Velero Schedule created.
	sched := &unstructured.Unstructured{}
	sched.SetGroupVersionKind(veleroScheduleGVK)
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: testVeleroNs, Name: veleroScheduleName(bc),
	}, sched); err != nil {
		t.Fatalf("get velero schedule: %v", err)
	}
	if got, _, _ := unstructured.NestedString(sched.Object, "spec", "schedule"); got != "0 2 * * *" {
		t.Errorf("velero schedule: got %q", got)
	}
	if got, _, _ := unstructured.NestedString(sched.Object, "spec", "template", "storageLocation"); got != "default" {
		t.Errorf("velero storageLocation: got %q", got)
	}
	if paused, _, _ := unstructured.NestedBool(sched.Object, "spec", "paused"); paused {
		t.Errorf("velero paused: expected false (Enabled=true)")
	}

	// Etcd CronJob created.
	var cj batchv1.CronJob
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: testEtcdNs, Name: etcdCronJobName(bc),
	}, &cj); err != nil {
		t.Fatalf("get etcd cronjob: %v", err)
	}
	if cj.Spec.Schedule != "0 3 * * *" {
		t.Errorf("etcd schedule: got %q", cj.Spec.Schedule)
	}
	if cj.Spec.Suspend != nil && *cj.Spec.Suspend {
		t.Errorf("etcd suspend: expected false (Enabled=true)")
	}
	podSpec := cj.Spec.JobTemplate.Spec.Template.Spec
	if len(podSpec.InitContainers) != 1 || podSpec.InitContainers[0].Image != testEtcdctlImage {
		t.Errorf("etcd initContainer (snapshot-taker): expected image %q, got %+v", testEtcdctlImage, podSpec.InitContainers)
	}
	if len(podSpec.Containers) != 1 || podSpec.Containers[0].Image != testUploadImage {
		t.Errorf("etcd main container (snapshot-writer): expected image %q, got %+v", testUploadImage, podSpec.Containers)
	}
	if envValue(podSpec.Containers[0].Env, "STORAGE_ACCOUNT") != "teststorage" {
		t.Errorf("etcd STORAGE_ACCOUNT: got %q", envValue(podSpec.Containers[0].Env, "STORAGE_ACCOUNT"))
	}
	if envValue(podSpec.Containers[0].Env, "STORAGE_CONTAINER") != "etcd-backups" {
		t.Errorf("etcd STORAGE_CONTAINER: got %q", envValue(podSpec.Containers[0].Env, "STORAGE_CONTAINER"))
	}
	if envValue(podSpec.Containers[0].Env, "BLOB_PREFIX") != "colo/cluster-001" {
		t.Errorf("etcd BLOB_PREFIX: got %q", envValue(podSpec.Containers[0].Env, "BLOB_PREFIX"))
	}
	if !podSpec.HostNetwork {
		t.Errorf("etcd CronJob should use hostNetwork (to reach local etcd)")
	}

	// Status reflects success.
	var got armadav1.BackupConfig
	if err := c.Get(context.Background(), types.NamespacedName{Name: bc.Name}, &got); err != nil {
		t.Fatalf("get bc: %v", err)
	}
	if got.Status.Phase != armadav1.BackupConfigPhaseApplied {
		t.Errorf("phase: got %q", got.Status.Phase)
	}
	if got.Status.LastAppliedAt == nil {
		t.Errorf("expected status.lastAppliedAt to be set after a successful reconcile")
	}
	// The Recorder should have emitted an "Applied" Event with per-PATCH detail.
	fr := r.Recorder.(*record.FakeRecorder)
	select {
	case ev := <-fr.Events:
		if !strings.Contains(ev, "Applied") || !strings.Contains(ev, "velero/") {
			t.Errorf("expected an Applied event mentioning velero PATCH; got %q", ev)
		}
	default:
		t.Errorf("expected at least one Event on r.Recorder.Events; got none")
	}
}

// TestReconcile_SubResourcesCarryOwnerReference verifies that the Velero
// Schedule and the etcd CronJob both carry a controller OwnerReference to
// the parent BackupConfig — so deleting the BackupConfig triggers native
// K8s GC of both sub-resources instead of leaving them running.
func TestReconcile_SubResourcesCarryOwnerReference(t *testing.T) {
	bc := sampleBackupConfig()
	r, c := newReconciler(t, bc)

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: bc.Name},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Re-fetch the BackupConfig so UID matches whatever the fake client
	// assigned on the initial create. Owner references must reference this
	// exact UID.
	var parent armadav1.BackupConfig
	if err := c.Get(context.Background(), types.NamespacedName{Name: bc.Name}, &parent); err != nil {
		t.Fatalf("get bc: %v", err)
	}

	assertControllerOwnerRef := func(t *testing.T, kind string, refs []metav1.OwnerReference) {
		t.Helper()
		for _, ref := range refs {
			if ref.Kind != "BackupConfig" {
				continue
			}
			if ref.Name != parent.Name {
				t.Errorf("%s ownerRef.Name = %q, want %q", kind, ref.Name, parent.Name)
			}
			if ref.APIVersion != armadav1.GroupVersion.String() {
				t.Errorf("%s ownerRef.APIVersion = %q, want %q", kind, ref.APIVersion, armadav1.GroupVersion.String())
			}
			if ref.Controller == nil || !*ref.Controller {
				t.Errorf("%s ownerRef.Controller must be true (marks BC as the controlling owner)", kind)
			}
			if ref.BlockOwnerDeletion == nil || !*ref.BlockOwnerDeletion {
				t.Errorf("%s ownerRef.BlockOwnerDeletion must be true (finalizer safety)", kind)
			}
			return
		}
		t.Fatalf("%s: no BackupConfig ownerReference found; refs = %+v", kind, refs)
	}

	sched := &unstructured.Unstructured{}
	sched.SetGroupVersionKind(veleroScheduleGVK)
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: testVeleroNs, Name: veleroScheduleName(bc),
	}, sched); err != nil {
		t.Fatalf("get velero schedule: %v", err)
	}
	assertControllerOwnerRef(t, "velero Schedule", sched.GetOwnerReferences())

	var cj batchv1.CronJob
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: testEtcdNs, Name: etcdCronJobName(bc),
	}, &cj); err != nil {
		t.Fatalf("get etcd cronjob: %v", err)
	}
	assertControllerOwnerRef(t, "etcd CronJob", cj.OwnerReferences)
}

// TestReconcile_BackfillsOwnerReferenceOnPreExistingSubResources catches the
// class of bug the pre-diff-then-maybe-apply pattern used to have: pre-existing
// sub-resources whose spec already matched intent would keep missing metadata
// (OwnerReferences, labels) forever because the delta short-circuit skipped
// the SSA apply. The convention (cert-manager, cluster-api, kubebuilder
// samples) is always-apply — SSA is idempotent. This test simulates an upgrade
// from an older bc-controller by pre-populating a Velero Schedule + etcd
// CronJob with matching specs but NO OwnerReferences, then asserting the
// OwnerReferences are backfilled on the next reconcile.
func TestReconcile_BackfillsOwnerReferenceOnPreExistingSubResources(t *testing.T) {
	bc := sampleBackupConfig()

	// Velero Schedule: spec matches sampleBackupConfig's intent exactly.
	sched := &unstructured.Unstructured{}
	sched.SetGroupVersionKind(veleroScheduleGVK)
	sched.SetNamespace(testVeleroNs)
	sched.SetName(veleroScheduleName(bc))
	_ = unstructured.SetNestedField(sched.Object, "0 2 * * *", "spec", "schedule")
	_ = unstructured.SetNestedField(sched.Object, "default", "spec", "template", "storageLocation")
	_ = unstructured.SetNestedField(sched.Object, false, "spec", "paused")

	// etcd CronJob: spec matches sampleBackupConfig's intent exactly.
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: etcdCronJobName(bc), Namespace: testEtcdNs},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 3 * * *",
			Suspend:  ptr.To(false),
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{Template: makeEtcdPodSpec("etcd-backups")},
			},
		},
	}

	r, c := newReconciler(t, bc, sched, cj)

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: bc.Name},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Fetch fresh — must see the OwnerReference the reconcile wrote.
	liveSched := &unstructured.Unstructured{}
	liveSched.SetGroupVersionKind(veleroScheduleGVK)
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: testVeleroNs, Name: veleroScheduleName(bc),
	}, liveSched); err != nil {
		t.Fatalf("get velero schedule: %v", err)
	}
	if !hasControllerOwnerRef(liveSched.GetOwnerReferences(), "BackupConfig", bc.Name) {
		t.Errorf("velero Schedule missing BackupConfig OwnerReference after reconcile-with-matching-spec; refs = %+v", liveSched.GetOwnerReferences())
	}

	var liveCJ batchv1.CronJob
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: testEtcdNs, Name: etcdCronJobName(bc),
	}, &liveCJ); err != nil {
		t.Fatalf("get etcd cronjob: %v", err)
	}
	if !hasControllerOwnerRef(liveCJ.OwnerReferences, "BackupConfig", bc.Name) {
		t.Errorf("etcd CronJob missing BackupConfig OwnerReference after reconcile-with-matching-spec; refs = %+v", liveCJ.OwnerReferences)
	}
}

// hasControllerOwnerRef reports whether refs contains an OwnerReference of
// the given Kind + Name with Controller=true. Used by ownership-backfill
// tests; separate from the stricter assertControllerOwnerRef closure in
// TestReconcile_SubResourcesCarryOwnerReference (which also verifies
// APIVersion and BlockOwnerDeletion).
func hasControllerOwnerRef(refs []metav1.OwnerReference, kind, name string) bool {
	for _, ref := range refs {
		if ref.Kind == kind && ref.Name == name && ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}

func TestReconcile_NoBlocksSkipped(t *testing.T) {
	bc := &armadav1.BackupConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "empty"},
		Spec:       armadav1.BackupConfigSpec{OrbID: "colo:cluster-empty"},
	}
	r, c := newReconciler(t, bc)

	res, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: bc.Name},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("no-op reconcile should not requeue, got %v", res.RequeueAfter)
	}

	// No Schedule or CronJob created.
	sched := &unstructured.Unstructured{}
	sched.SetGroupVersionKind(veleroScheduleGVK)
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: testVeleroNs, Name: veleroScheduleName(bc),
	}, sched); err == nil {
		t.Errorf("velero schedule should not exist for empty backupconfig")
	}

	// Status surfaces the skip: Phase=Skipped, Reconciled=Unknown (not False —
	// an empty backup config is benign, not a fault), and no Warning Event.
	var got armadav1.BackupConfig
	if err := c.Get(context.Background(), types.NamespacedName{Name: bc.Name}, &got); err != nil {
		t.Fatalf("get bc: %v", err)
	}
	if got.Status.Phase != armadav1.BackupConfigPhaseSkipped {
		t.Errorf("phase = %q, want Skipped", got.Status.Phase)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, ConditionReconciled)
	if cond == nil {
		t.Fatalf("Reconciled condition missing")
	}
	if cond.Status != metav1.ConditionUnknown {
		t.Errorf("Reconciled.status = %q, want Unknown", cond.Status)
	}
	if cond.Reason != "NoBackupBlocks" {
		t.Errorf("Reconciled.reason = %q, want NoBackupBlocks", cond.Reason)
	}
	fr := r.Recorder.(*record.FakeRecorder)
	select {
	case ev := <-fr.Events:
		t.Errorf("skip must NOT emit an Event (it is not a fault); got %q", ev)
	default:
	}
}

func TestReconcile_PausedWhenEnabledFalse(t *testing.T) {
	bc := &armadav1.BackupConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "paused"},
		Spec: armadav1.BackupConfigSpec{
			OrbID: "colo:cluster-paused",
			Velero: &armadav1.VeleroBackupSpec{
				OrbID:    "colo:cluster-paused-velero",
				Enabled:  ptr.To(false),
				Schedule: ptr.To("0 4 * * *"),
			},
		},
	}
	r, c := newReconciler(t, bc)
	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: bc.Name},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	sched := &unstructured.Unstructured{}
	sched.SetGroupVersionKind(veleroScheduleGVK)
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: testVeleroNs, Name: veleroScheduleName(bc),
	}, sched); err != nil {
		t.Fatalf("get velero schedule: %v", err)
	}
	paused, _, _ := unstructured.NestedBool(sched.Object, "spec", "paused")
	if !paused {
		t.Errorf("expected paused=true when Enabled=false")
	}
}

func TestReconcile_Idempotent(t *testing.T) {
	bc := sampleBackupConfig()
	r, c := newReconciler(t, bc)

	for i := 0; i < 3; i++ {
		if _, err := r.Reconcile(context.Background(), reconcile.Request{
			NamespacedName: types.NamespacedName{Name: bc.Name},
		}); err != nil {
			t.Fatalf("Reconcile iter %d: %v", i, err)
		}
	}

	var got armadav1.BackupConfig
	if err := c.Get(context.Background(), types.NamespacedName{Name: bc.Name}, &got); err != nil {
		t.Fatalf("get bc: %v", err)
	}
	// Only the first reconcile has real deltas to PATCH → only one "Applied"
	// Event carrying PATCH detail. Subsequent reconciles are always-apply
	// no-ops (SSA idempotent, no delta) → no additional Applied Events.
	fr := r.Recorder.(*record.FakeRecorder)
	applied := 0
	for done := false; !done; {
		select {
		case ev := <-fr.Events:
			if strings.Contains(ev, "Applied") {
				applied++
			}
		default:
			done = true
		}
	}
	if applied != 1 {
		t.Errorf("expected exactly one Applied Event (first reconcile PATCHed; rest were no-ops), got %d", applied)
	}
	// LastAppliedAt should be set — bumped on the first reconcile at minimum.
	if got.Status.LastAppliedAt == nil {
		t.Errorf("expected status.lastAppliedAt to be set")
	}
}

func TestVeleroDeltas_NotFound(t *testing.T) {
	r, c := newReconciler(t)
	block := &armadav1.VeleroBackupSpec{
		Schedule: ptr.To("0 2 * * *"),
		Location: ptr.To("default"),
		Enabled:  ptr.To(true),
	}
	d, err := veleroDeltas(context.Background(), c, r.VeleroNamespace, "missing-velero", block)
	if err != nil {
		t.Fatalf("veleroDeltas: %v", err)
	}
	if d["schedule"] != "0 2 * * *" || d["storageLocation"] != "default" || d["paused"] != "false" {
		t.Errorf("expected all-fields delta when missing, got %+v", d)
	}
}

func TestEtcdDeltas_NotFound(t *testing.T) {
	r, c := newReconciler(t)
	block := &armadav1.EtcdBackupSpec{
		Schedule: ptr.To("0 3 * * *"),
		Location: ptr.To("https://teststorage.blob.core.windows.net/etcd-backups/colo/cluster-001"),
		Enabled:  ptr.To(true),
	}
	params := etcdCronJobParams{
		StorageAccount:   "teststorage",
		StorageContainer: "etcd-backups",
		BlobPrefix:       "colo/cluster-001",
	}
	d, err := etcdDeltas(context.Background(), c, r.EtcdBackupNamespace, "missing-etcd", block, params)
	if err != nil {
		t.Fatalf("etcdDeltas: %v", err)
	}
	if d["schedule"] != "0 3 * * *" || d["storageContainer"] != "etcd-backups" || d["storageAccount"] != "teststorage" || d["blobPrefix"] != "colo/cluster-001" || d["suspend"] != "false" {
		t.Errorf("expected all-fields delta when missing, got %+v", d)
	}
}

// TestReadLiveObserved covers the honest-observer contract: observed reflects
// what actually exists on the cluster, not what was intended. Every case
// pins one of the shapes readLiveObserved must produce.
func TestReadLiveObserved_LivesResourcesPresent(t *testing.T) {
	// Both a Velero Schedule and etcd CronJob exist. Observed should read
	// them and populate all four fields per mechanism from live state.
	sched := &unstructured.Unstructured{}
	sched.SetGroupVersionKind(veleroScheduleGVK)
	sched.SetNamespace(testVeleroNs)
	sched.SetName("colo-cluster-001-velero")
	_ = unstructured.SetNestedField(sched.Object, "0 2 * * *", "spec", "schedule")
	_ = unstructured.SetNestedField(sched.Object, "default", "spec", "template", "storageLocation")
	_ = unstructured.SetNestedField(sched.Object, false, "spec", "paused")

	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "colo-cluster-001-etcd", Namespace: testEtcdNs},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 3 * * *",
			Suspend:  ptr.To(false),
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{Template: makeEtcdPodSpec("s3://backups/etcd")},
			},
		},
	}
	r, _ := newReconciler(t, sched, cj)
	bc := sampleBackupConfig()

	got := r.readLiveObserved(context.Background(), bc, testLogger())

	if got.Velero == nil {
		t.Fatal("velero: expected non-nil for present live Schedule")
	}
	if !boolPtrEqual(got.Velero.Enabled, ptr.To(true)) {
		t.Errorf("velero.enabled: got %v want true (live paused=false)", got.Velero.Enabled)
	}
	if !stringPtrEqual(got.Velero.Schedule, ptr.To("0 2 * * *")) {
		t.Errorf("velero.schedule: got %v", got.Velero.Schedule)
	}
	if !stringPtrEqual(got.Velero.Location, ptr.To("default")) {
		t.Errorf("velero.location: got %v", got.Velero.Location)
	}
	if got.Etcd == nil {
		t.Fatal("etcd: expected non-nil for present live CronJob")
	}
	if !boolPtrEqual(got.Etcd.Enabled, ptr.To(true)) {
		t.Errorf("etcd.enabled: got %v want true (live suspend=false)", got.Etcd.Enabled)
	}
	if !stringPtrEqual(got.Etcd.Schedule, ptr.To("0 3 * * *")) {
		t.Errorf("etcd.schedule: got %v", got.Etcd.Schedule)
	}
	if !stringPtrEqual(got.Etcd.Location, ptr.To("s3://backups/etcd")) {
		t.Errorf("etcd.location: got %v", got.Etcd.Location)
	}
}

func TestReadLiveObserved_ResourcesAbsent(t *testing.T) {
	// Spec asks for velero + etcd but neither live resource exists. Observed
	// reports both as nil — the "honest" answer instead of copying spec.
	r, _ := newReconciler(t)
	bc := sampleBackupConfig()

	got := r.readLiveObserved(context.Background(), bc, testLogger())

	if got.Velero != nil {
		t.Errorf("velero: expected nil when live Schedule absent, got %+v", got.Velero)
	}
	if got.Etcd != nil {
		t.Errorf("etcd: expected nil when live CronJob absent, got %+v", got.Etcd)
	}
}

func TestReadLiveObserved_LiveDriftedFromSpec(t *testing.T) {
	// Live Schedule has paused=true even though spec says enabled=true.
	// Observed must report the LIVE value (enabled=false), surfacing the
	// drift in status. This is the core payoff of live-read observed.
	sched := &unstructured.Unstructured{}
	sched.SetGroupVersionKind(veleroScheduleGVK)
	sched.SetNamespace(testVeleroNs)
	sched.SetName("colo-cluster-001-velero")
	_ = unstructured.SetNestedField(sched.Object, "0 2 * * *", "spec", "schedule")
	_ = unstructured.SetNestedField(sched.Object, true, "spec", "paused") // ← drifted
	r, _ := newReconciler(t, sched)
	bc := sampleBackupConfig()

	got := r.readLiveObserved(context.Background(), bc, testLogger())

	if got.Velero == nil {
		t.Fatal("velero: expected non-nil (live Schedule present)")
	}
	if !boolPtrEqual(got.Velero.Enabled, ptr.To(false)) {
		t.Errorf("velero.enabled: expected false (live paused=true drifted from spec enabled=true), got %v", got.Velero.Enabled)
	}
}

func TestReadLiveObserved_UnmanagedMechanismStayNil(t *testing.T) {
	// Spec only manages Velero. Live CronJob happens to exist under our
	// deterministic name (stray from a prior deploy). readLiveObserved must
	// still report etcd as nil — we don't observe mechanisms the CR does
	// not ask us to manage.
	strayCj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "colo-cluster-001-etcd", Namespace: testEtcdNs},
		Spec:       batchv1.CronJobSpec{Schedule: "0 3 * * *"},
	}
	r, _ := newReconciler(t, strayCj)
	bc := &armadav1.BackupConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "colo-cluster-001"},
		Spec: armadav1.BackupConfigSpec{
			OrbID:  "colo:cluster-001",
			Velero: &armadav1.VeleroBackupSpec{OrbID: "colo:cluster-001-velero"},
			// Etcd intentionally nil — spec does not manage etcd.
		},
	}

	got := r.readLiveObserved(context.Background(), bc, testLogger())

	if got.Etcd != nil {
		t.Errorf("etcd: expected nil for unmanaged mechanism, got %+v", got.Etcd)
	}
}

func TestObservedEqual_DetectsAnyFieldChange(t *testing.T) {
	a := armadav1.ObservedBackup{
		Velero: &armadav1.ObservedVeleroStatus{Enabled: ptr.To(true), Schedule: ptr.To("a")},
	}
	b := armadav1.ObservedBackup{
		Velero: &armadav1.ObservedVeleroStatus{Enabled: ptr.To(true), Schedule: ptr.To("b")},
	}
	if observedEqual(a, b) {
		t.Errorf("expected schedule diff to surface as not-equal")
	}
	if !observedEqual(a, a) {
		t.Errorf("expected identical observed to be equal")
	}
}

func TestBuildEtcdCronJob_PruneAndConcurrency(t *testing.T) {
	schedule := "*/15 * * * *"
	enabled := true
	p := etcdCronJobParams{
		Name:             "colo-dev-main-backup-etcd",
		Namespace:        "kube-system",
		StorageAccount:   "stgalbackupsdevccwus01",
		StorageContainer: "etcd",
		BlobPrefix:       "colo/dev-main",
		EtcdctlImage:     "etcdctl:3.5.15",
		UploadImage:      "azure-cli:2.67.0",
		CredentialSecret: "az-storage-creds",
		RetainPerDay:     5,
		TimeZone:         "America/Los_Angeles",
		Block:            &armadav1.EtcdBackupSpec{Schedule: &schedule, Enabled: &enabled},
	}
	cj := buildEtcdCronJob(p)

	if cj.Spec.ConcurrencyPolicy != batchv1.ForbidConcurrent {
		t.Errorf("ConcurrencyPolicy = %q, want Forbid", cj.Spec.ConcurrencyPolicy)
	}
	if cj.Spec.TimeZone == nil || *cj.Spec.TimeZone != "America/Los_Angeles" {
		t.Errorf("TimeZone = %v, want America/Los_Angeles", cj.Spec.TimeZone)
	}
	writer := cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0]
	script := writer.Command[2]
	if !strings.Contains(script, "storage blob delete") {
		t.Error("writer script missing prune (blob delete) logic")
	}
	if !strings.Contains(script, "$BLOB_PREFIX") {
		t.Error("prune must be scoped to $BLOB_PREFIX (this cluster's folder only)")
	}
	if strings.Contains(script, "houston-stage") || strings.Contains(script, "g2-w2") {
		t.Error("script must not hardcode galleon/cluster names")
	}
	if got := envValue(writer.Env, "RETAIN_PER_DAY"); got != "5" {
		t.Errorf("RETAIN_PER_DAY env = %q, want 5", got)
	}
}
