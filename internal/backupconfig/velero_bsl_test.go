package backupconfig

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	armadav1 "github.com/armada/configbundle/api/v1"
)

func TestBslNameFromLocation(t *testing.T) {
	cases := []struct {
		location string
		want     string
	}{
		{"s3://bucket/velero", "velero"},
		{"s3://bucket/path/to/prefix", "prefix"},
		{"https://acct.blob.core.windows.net/container/etcd", "etcd"},
		{"https://acct.blob.core.windows.net/container/path/sub", "sub"},
		{"s3://bucket/velero/", "velero"},
		{"s3://bucket/", "bucket"},
		{"justname", "justname"},
	}
	for _, tc := range cases {
		t.Run(tc.location, func(t *testing.T) {
			got := bslNameFromLocation(tc.location)
			if got != tc.want {
				t.Errorf("bslNameFromLocation(%q) = %q, want %q", tc.location, got, tc.want)
			}
		})
	}
}

func TestDeriveStorage(t *testing.T) {
	cases := []struct {
		name         string
		location     string
		wantErr      bool
		wantProvider string
		wantBucket   string
		wantPrefix   string
		wantAccount  string
	}{
		{
			name:         "s3 with prefix",
			location:     "s3://my-bucket/velero",
			wantProvider: "aws",
			wantBucket:   "my-bucket",
			wantPrefix:   "velero",
		},
		{
			name:         "s3 no prefix",
			location:     "s3://my-bucket",
			wantProvider: "aws",
			wantBucket:   "my-bucket",
			wantPrefix:   "",
		},
		{
			name:         "azure blob",
			location:     "https://storageacct.blob.core.windows.net/mycontainer/etcd",
			wantProvider: "azure",
			wantBucket:   "mycontainer",
			wantPrefix:   "etcd",
			wantAccount:  "storageacct",
		},
		{
			name:         "azure blob nested prefix",
			location:     "https://storageacct.blob.core.windows.net/mycontainer/colo/cluster",
			wantProvider: "azure",
			wantBucket:   "mycontainer",
			wantPrefix:   "colo/cluster",
			wantAccount:  "storageacct",
		},
		{
			name:     "unsupported scheme",
			location: "gs://bucket/prefix",
			wantErr:  true,
		},
		{
			name:     "plain https not azure",
			location: "https://example.com/bucket/prefix",
			wantErr:  true,
		},
		{
			name:     "empty s3 bucket",
			location: "s3://",
			wantErr:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := deriveStorage(tc.location)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil (result %+v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Provider != tc.wantProvider {
				t.Errorf("provider = %q, want %q", got.Provider, tc.wantProvider)
			}
			if got.Bucket != tc.wantBucket {
				t.Errorf("bucket = %q, want %q", got.Bucket, tc.wantBucket)
			}
			if got.Prefix != tc.wantPrefix {
				t.Errorf("prefix = %q, want %q", got.Prefix, tc.wantPrefix)
			}
			if got.AzureAccount != tc.wantAccount {
				t.Errorf("azureAccount = %q, want %q", got.AzureAccount, tc.wantAccount)
			}
		})
	}
}

func TestBuildBSLSpec_AWS(t *testing.T) {
	r := &BackupConfigReconciler{
		VeleroS3URL:            "http://rgw.local:8080",
		VeleroS3Region:         "default",
		VeleroS3ForcePathStyle: true,
	}
	desc := storageDescriptor{Provider: "aws", Bucket: "my-bucket", Prefix: "velero"}
	spec, err := r.buildBSLSpec(desc)
	if err != nil {
		t.Fatalf("buildBSLSpec: %v", err)
	}
	if spec["provider"] != "aws" {
		t.Errorf("provider = %v, want aws", spec["provider"])
	}
	objStorage, _ := spec["objectStorage"].(map[string]any)
	if objStorage["bucket"] != "my-bucket" {
		t.Errorf("bucket = %v, want my-bucket", objStorage["bucket"])
	}
	if objStorage["prefix"] != "velero" {
		t.Errorf("prefix = %v, want velero", objStorage["prefix"])
	}
	config, _ := spec["config"].(map[string]any)
	if config["s3Url"] != "http://rgw.local:8080" {
		t.Errorf("s3Url = %v, want http://rgw.local:8080", config["s3Url"])
	}
	if config["s3ForcePathStyle"] != "true" {
		t.Errorf("s3ForcePathStyle = %v, want true", config["s3ForcePathStyle"])
	}
	// AWS does not set credential block.
	if _, ok := spec["credential"]; ok {
		t.Errorf("AWS BSL must not set spec.credential (falls back to Velero default)")
	}
}

func TestBuildBSLSpec_Azure(t *testing.T) {
	r := &BackupConfigReconciler{
		VeleroAzureResourceGroup:    "my-rg",
		VeleroAzureSubscriptionID:   "sub-123",
		VeleroAzureCredentialSecret: "azure-creds",
	}
	desc := storageDescriptor{
		Provider:     "azure",
		Bucket:       "mycontainer",
		Prefix:       "etcd",
		AzureAccount: "storageacct",
	}
	spec, err := r.buildBSLSpec(desc)
	if err != nil {
		t.Fatalf("buildBSLSpec: %v", err)
	}
	if spec["provider"] != "azure" {
		t.Errorf("provider = %v, want azure", spec["provider"])
	}
	objStorage, _ := spec["objectStorage"].(map[string]any)
	if objStorage["bucket"] != "mycontainer" {
		t.Errorf("bucket = %v, want mycontainer", objStorage["bucket"])
	}
	config, _ := spec["config"].(map[string]any)
	if config["resourceGroup"] != "my-rg" {
		t.Errorf("resourceGroup = %v, want my-rg", config["resourceGroup"])
	}
	if config["storageAccount"] != "storageacct" {
		t.Errorf("storageAccount = %v, want storageacct", config["storageAccount"])
	}
	cred, _ := spec["credential"].(map[string]any)
	if cred["name"] != "azure-creds" {
		t.Errorf("credential.name = %v, want azure-creds", cred["name"])
	}
	if cred["key"] != "cloud" {
		t.Errorf("credential.key = %v, want cloud", cred["key"])
	}
}

func TestBuildBSLSpec_AzureMissingConfig(t *testing.T) {
	cases := []struct {
		name string
		r    BackupConfigReconciler
	}{
		{
			name: "missing resource group and subscription",
			r:    BackupConfigReconciler{VeleroAzureCredentialSecret: "creds"},
		},
		{
			name: "missing credential secret",
			r: BackupConfigReconciler{
				VeleroAzureResourceGroup:  "rg",
				VeleroAzureSubscriptionID: "sub",
			},
		},
	}
	desc := storageDescriptor{Provider: "azure", Bucket: "c", AzureAccount: "a"}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.r.buildBSLSpec(desc)
			if err == nil {
				t.Errorf("expected error for incomplete azure config, got nil")
			}
		})
	}
}

func TestBslChanged_NotFound(t *testing.T) {
	r, c := newReconciler(t)
	desc := storageDescriptor{Provider: "aws", Bucket: "b", Prefix: "p"}
	changed, err := bslChanged(context.Background(), c, r.VeleroNamespace, "no-such-bsl", desc)
	if err != nil {
		t.Fatalf("bslChanged: %v", err)
	}
	if !changed {
		t.Errorf("expected changed=true for NotFound BSL")
	}
}

func TestBslChanged_MatchingSpec(t *testing.T) {
	bsl := newBSLUnstructured(testVeleroNs, "test-bsl", "aws", "my-bucket", "velero")
	r, c := newReconciler(t, bsl)
	desc := storageDescriptor{Provider: "aws", Bucket: "my-bucket", Prefix: "velero"}

	changed, err := bslChanged(context.Background(), c, r.VeleroNamespace, "test-bsl", desc)
	if err != nil {
		t.Fatalf("bslChanged: %v", err)
	}
	if changed {
		t.Errorf("expected changed=false when live BSL matches intent")
	}
}

func TestBslChanged_DriftedSpec(t *testing.T) {
	cases := []struct {
		name       string
		liveBucket string
		livePrefix string
		wantChange bool
	}{
		{"bucket changed", "other-bucket", "velero", true},
		{"prefix changed", "my-bucket", "different", true},
		{"matches", "my-bucket", "velero", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bsl := newBSLUnstructured(testVeleroNs, "test-bsl", "aws", tc.liveBucket, tc.livePrefix)
			r, c := newReconciler(t, bsl)
			desc := storageDescriptor{Provider: "aws", Bucket: "my-bucket", Prefix: "velero"}
			changed, err := bslChanged(context.Background(), c, r.VeleroNamespace, "test-bsl", desc)
			if err != nil {
				t.Fatalf("bslChanged: %v", err)
			}
			if changed != tc.wantChange {
				t.Errorf("changed = %v, want %v", changed, tc.wantChange)
			}
		})
	}
}

func TestSyncS3SyncCondition(t *testing.T) {
	cases := []struct {
		name       string
		s3Sync     *armadav1.S3SyncSpec
		wantStatus metav1.ConditionStatus
		wantAbsent bool
	}{
		{
			name:       "nil s3Sync removes condition",
			s3Sync:     nil,
			wantAbsent: true,
		},
		{
			name:       "present s3Sync sets False/NotImplemented",
			s3Sync:     &armadav1.S3SyncSpec{OrbID: "colo:cluster-001-s3sync", Enabled: ptr.To(true)},
			wantStatus: metav1.ConditionFalse,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bc := &armadav1.BackupConfig{
				Spec: armadav1.BackupConfigSpec{S3Sync: tc.s3Sync},
			}
			syncS3SyncCondition(bc)
			var found *metav1.Condition
			for i := range bc.Status.Conditions {
				if bc.Status.Conditions[i].Type == ConditionS3SyncSupported {
					found = &bc.Status.Conditions[i]
					break
				}
			}
			if tc.wantAbsent {
				if found != nil {
					t.Errorf("expected S3SyncSupported condition absent, got %+v", found)
				}
				return
			}
			if found == nil {
				t.Fatalf("expected S3SyncSupported condition present")
			}
			if found.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", found.Status, tc.wantStatus)
			}
			if found.Reason != "NotImplemented" {
				t.Errorf("reason = %q, want NotImplemented", found.Reason)
			}
		})
	}
}

// newBSLUnstructured builds a fake BackupStorageLocation for fake client seeding.
func newBSLUnstructured(ns, name, provider, bucket, prefix string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(backupStorageLocationGVK)
	u.SetNamespace(ns)
	u.SetName(name)
	_ = unstructured.SetNestedField(u.Object, provider, "spec", "provider")
	_ = unstructured.SetNestedField(u.Object, bucket, "spec", "objectStorage", "bucket")
	_ = unstructured.SetNestedField(u.Object, prefix, "spec", "objectStorage", "prefix")
	return u
}

// TestReconcileBSL_CreatesAndPatches verifies reconcileBSL SSA-creates a new
// BSL and returns a non-empty patch summary, then is idempotent on re-apply.
func TestReconcileBSL_CreatesAndPatches(t *testing.T) {
	bc := sampleBackupConfig()
	r, c := newReconciler(t, bc)
	r.VeleroS3URL = "http://rgw.local:8080"
	r.VeleroS3Region = "default"
	r.VeleroS3ForcePathStyle = true

	desc := storageDescriptor{Provider: "aws", Bucket: "test-cluster-bucket", Prefix: "velero"}

	msg, err := r.reconcileBSL(context.Background(), bc, "velero", desc)
	if err != nil {
		t.Fatalf("reconcileBSL: %v", err)
	}
	if msg == "" {
		t.Errorf("expected non-empty patch summary on first create")
	}

	// BSL must now exist.
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(backupStorageLocationGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testVeleroNs, Name: "velero"}, live); err != nil {
		t.Fatalf("get BSL: %v", err)
	}
	gotProvider, _, _ := unstructured.NestedString(live.Object, "spec", "provider")
	if gotProvider != "aws" {
		t.Errorf("provider = %q, want aws", gotProvider)
	}
	gotBucket, _, _ := unstructured.NestedString(live.Object, "spec", "objectStorage", "bucket")
	if gotBucket != "test-cluster-bucket" {
		t.Errorf("bucket = %q, want test-cluster-bucket", gotBucket)
	}

	// Second apply: no change → empty summary.
	msg2, err := r.reconcileBSL(context.Background(), bc, "velero", desc)
	if err != nil {
		t.Fatalf("reconcileBSL (second): %v", err)
	}
	if msg2 != "" {
		t.Errorf("expected empty summary on idempotent re-apply, got %q", msg2)
	}
}

// TestReconcileBSL_CarriesOwnerReference verifies the BSL carries a controller
// OwnerReference to the parent BackupConfig so K8s GC cascades correctly.
func TestReconcileBSL_CarriesOwnerReference(t *testing.T) {
	bc := sampleBackupConfig()
	r, c := newReconciler(t, bc)
	r.VeleroS3URL = "http://rgw.local:8080"
	r.VeleroS3Region = "default"

	if _, err := r.reconcileBSL(context.Background(), bc, "velero",
		storageDescriptor{Provider: "aws", Bucket: "b", Prefix: "p"}); err != nil {
		t.Fatalf("reconcileBSL: %v", err)
	}

	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(backupStorageLocationGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testVeleroNs, Name: "velero"}, live); err != nil {
		t.Fatalf("get BSL: %v", err)
	}
	if !hasControllerOwnerRef(live.GetOwnerReferences(), "BackupConfig", bc.Name) {
		t.Errorf("BSL missing BackupConfig OwnerReference; refs = %+v", live.GetOwnerReferences())
	}
}

func TestReconcileVelero_SetsRetentionDaysTTL(t *testing.T) {
	bc := sampleBackupConfig()
	days := 7
	bc.Spec.Velero.RetentionDays = &days
	r, c := newReconciler(t, bc)

	if _, err := r.reconcileVelero(context.Background(), bc); err != nil {
		t.Fatalf("reconcileVelero: %v", err)
	}

	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(veleroScheduleGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testVeleroNs, Name: veleroScheduleName(bc)}, live); err != nil {
		t.Fatalf("get schedule: %v", err)
	}
	ttl, _, _ := unstructured.NestedString(live.Object, "spec", "template", "ttl")
	if ttl != "168h0m0s" {
		t.Errorf("ttl = %q, want 168h0m0s (7 days)", ttl)
	}
}

func TestReconcileVelero_NilRetentionDays_NoTTL(t *testing.T) {
	bc := sampleBackupConfig()
	// RetentionDays deliberately not set.
	r, c := newReconciler(t, bc)

	if _, err := r.reconcileVelero(context.Background(), bc); err != nil {
		t.Fatalf("reconcileVelero: %v", err)
	}

	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(veleroScheduleGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testVeleroNs, Name: veleroScheduleName(bc)}, live); err != nil {
		t.Fatalf("get schedule: %v", err)
	}
	if _, ok, _ := unstructured.NestedString(live.Object, "spec", "template", "ttl"); ok {
		t.Error("expected ttl absent when retentionDays is nil")
	}
}

func TestReconcileVelero_StampsClusterOrbIDLabel(t *testing.T) {
	bc := sampleBackupConfig()
	r, c := newReconciler(t, bc)

	if _, err := r.reconcileVelero(context.Background(), bc); err != nil {
		t.Fatalf("reconcileVelero: %v", err)
	}

	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(veleroScheduleGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testVeleroNs, Name: veleroScheduleName(bc)}, live); err != nil {
		t.Fatalf("get schedule: %v", err)
	}
	if got := live.GetAnnotations()["orbital.armada.ai/cluster-orb-id"]; got != bc.Spec.ClusterOrbID {
		t.Errorf("annotation orbital.armada.ai/cluster-orb-id = %q, want %q", got, bc.Spec.ClusterOrbID)
	}
}

func TestVeleroDeltas_AnnotationDrift(t *testing.T) {
	// Seed a Schedule that is missing the annotation.
	sched := &unstructured.Unstructured{}
	sched.SetGroupVersionKind(veleroScheduleGVK)
	sched.SetNamespace(testVeleroNs)
	sched.SetName("colo-cluster-001-velero")
	_ = unstructured.SetNestedField(sched.Object, "0 2 * * *", "spec", "schedule")

	_, c := newReconciler(t, sched)
	block := &armadav1.VeleroBackupSpec{Schedule: ptr.To("0 2 * * *")}

	d, err := veleroDeltas(context.Background(), c, testVeleroNs, "colo-cluster-001-velero", block, "colo:cluster-001")
	if err != nil {
		t.Fatalf("veleroDeltas: %v", err)
	}
	if d["annotation:cluster-orb-id"] != "colo:cluster-001" {
		t.Errorf("expected annotation delta when annotation absent, got %+v", d)
	}
}

