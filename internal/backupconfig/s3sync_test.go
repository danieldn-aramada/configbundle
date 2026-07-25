package backupconfig

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	armadav1 "github.com/armada/configbundle/api/v1"
)

func TestParseS3SyncURL(t *testing.T) {
	tests := []struct {
		name           string
		location       string
		wantEndpoint   string
		wantBucket     string
		wantPrefix     string
		wantErrContain string
	}{
		{
			name:         "http endpoint with bucket only",
			location:     "http://10.20.22.211/source",
			wantEndpoint: "http://10.20.22.211",
			wantBucket:   "source",
			wantPrefix:   "",
		},
		{
			name:         "http endpoint with port and bucket",
			location:     "http://10.20.22.244:30080/canary-final",
			wantEndpoint: "http://10.20.22.244:30080",
			wantBucket:   "canary-final",
			wantPrefix:   "",
		},
		{
			name:         "https endpoint with bucket and prefix",
			location:     "https://dest-endpoint/mybucket/my/prefix",
			wantEndpoint: "https://dest-endpoint",
			wantBucket:   "mybucket",
			wantPrefix:   "my/prefix",
		},
		{
			name:         "http with hostname and deep prefix",
			location:     "http://rook-ceph-rgw.rook-ceph.svc:80/mirror-bucket/colo/cluster-001",
			wantEndpoint: "http://rook-ceph-rgw.rook-ceph.svc:80",
			wantBucket:   "mirror-bucket",
			wantPrefix:   "colo/cluster-001",
		},
		{
			name:           "unsupported scheme",
			location:       "s3://my-bucket/prefix",
			wantErrContain: "unsupported scheme",
		},
		{
			name:           "no host",
			location:       "http:///bucket",
			wantErrContain: "no host",
		},
		{
			name:           "no bucket",
			location:       "http://10.20.22.211",
			wantErrContain: "no bucket",
		},
		{
			name:           "no bucket trailing slash only",
			location:       "http://10.20.22.211/",
			wantErrContain: "no bucket",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			endpoint, bucket, prefix, err := parseS3SyncURL(tc.location)
			if tc.wantErrContain != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErrContain)
				}
				if !contains(err.Error(), tc.wantErrContain) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrContain)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if endpoint != tc.wantEndpoint {
				t.Errorf("endpoint: got %q want %q", endpoint, tc.wantEndpoint)
			}
			if bucket != tc.wantBucket {
				t.Errorf("bucket: got %q want %q", bucket, tc.wantBucket)
			}
			if prefix != tc.wantPrefix {
				t.Errorf("prefix: got %q want %q", prefix, tc.wantPrefix)
			}
		})
	}
}

func TestBuildS3SyncCronJob(t *testing.T) {
	enabled := true
	schedule := "0 4 * * *"
	p := s3SyncCronJobParams{
		Name:             "colo-cluster-001-backup-s3sync",
		Namespace:        "default",
		SourceEndpoint:   "http://10.20.22.211",
		SourceBucket:     "source",
		SourcePrefix:     "",
		DestEndpoint:     "http://10.20.22.244:30080",
		DestBucket:       "canary-final",
		DestPrefix:       "",
		RcloneImage:      "rclone/rclone:1.68",
		CredentialSecret: "s3-creds",
		Block: &armadav1.S3SyncSpec{
			OrbID:    "colo:cluster-001-s3sync",
			Enabled:  &enabled,
			Schedule: &schedule,
		},
	}

	cj := buildS3SyncCronJob(p)

	if cj.Name != p.Name {
		t.Errorf("name: got %q want %q", cj.Name, p.Name)
	}
	if cj.Namespace != p.Namespace {
		t.Errorf("namespace: got %q want %q", cj.Namespace, p.Namespace)
	}
	if cj.Spec.Schedule != "0 4 * * *" {
		t.Errorf("schedule: got %q want %q", cj.Spec.Schedule, "0 4 * * *")
	}
	if cj.Spec.ConcurrencyPolicy != batchv1.ForbidConcurrent {
		t.Errorf("concurrencyPolicy: got %v want ForbidConcurrent", cj.Spec.ConcurrencyPolicy)
	}
	if cj.Spec.Suspend == nil || *cj.Spec.Suspend {
		t.Errorf("suspend: want false for enabled=true, got %v", cj.Spec.Suspend)
	}

	containers := cj.Spec.JobTemplate.Spec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Fatalf("containers: want 1, got %d", len(containers))
	}
	c := containers[0]
	if c.Name != s3SyncContainerName {
		t.Errorf("container name: got %q want %q", c.Name, s3SyncContainerName)
	}
	if c.Image != p.RcloneImage {
		t.Errorf("image: got %q want %q", c.Image, p.RcloneImage)
	}

	// Verify plain env vars carry the parsed endpoint/bucket values.
	wantEnv := map[string]string{
		"SOURCE_ENDPOINT": "http://10.20.22.211",
		"SOURCE_BUCKET":   "source",
		"SOURCE_PREFIX":   "",
		"DEST_ENDPOINT":   "http://10.20.22.244:30080",
		"DEST_BUCKET":     "canary-final",
		"DEST_PREFIX":     "",
	}
	for k, want := range wantEnv {
		got := envValue(c.Env, k)
		if got != want {
			t.Errorf("env %s: got %q want %q", k, got, want)
		}
	}

	// Verify credentials are secretKeyRef, not plaintext.
	for _, credKey := range []string{"SOURCE_ACCESS_KEY", "SOURCE_SECRET_KEY", "DEST_ACCESS_KEY", "DEST_SECRET_KEY"} {
		ev := findEnvVar(c.Env, credKey)
		if ev == nil {
			t.Errorf("env %s: missing", credKey)
			continue
		}
		if ev.ValueFrom == nil || ev.ValueFrom.SecretKeyRef == nil {
			t.Errorf("env %s: want secretKeyRef, got plain value", credKey)
		}
		if ev.ValueFrom.SecretKeyRef.Name != p.CredentialSecret {
			t.Errorf("env %s: secret name got %q want %q", credKey, ev.ValueFrom.SecretKeyRef.Name, p.CredentialSecret)
		}
	}
}

func TestBuildS3SyncCronJob_DisabledSuspends(t *testing.T) {
	disabled := false
	p := s3SyncCronJobParams{
		Name: "test", Namespace: "default",
		RcloneImage: "rclone/rclone:1.68", CredentialSecret: "s3-creds",
		Block: &armadav1.S3SyncSpec{OrbID: "x", Enabled: &disabled},
	}
	cj := buildS3SyncCronJob(p)
	if cj.Spec.Suspend == nil || !*cj.Spec.Suspend {
		t.Error("suspend: want true for enabled=false")
	}
}

func TestBuildS3SyncCronJob_NilEnabledNoSuspend(t *testing.T) {
	p := s3SyncCronJobParams{
		Name: "test", Namespace: "default",
		RcloneImage: "rclone/rclone:1.68", CredentialSecret: "s3-creds",
		Block: &armadav1.S3SyncSpec{OrbID: "x"},
	}
	cj := buildS3SyncCronJob(p)
	if cj.Spec.Suspend != nil {
		t.Errorf("suspend: want nil for nil enabled, got %v", *cj.Spec.Suspend)
	}
}

func TestS3SyncDeltas_NotFound(t *testing.T) {
	_, c := newReconciler(t)
	block := &armadav1.S3SyncSpec{
		OrbID:    "colo:s3sync",
		Enabled:  ptr.To(true),
		Schedule: ptr.To("0 4 * * *"),
	}
	params := s3SyncCronJobParams{
		SourceEndpoint: "http://10.20.22.211",
		SourceBucket:   "source",
		DestEndpoint:   "http://10.20.22.244:30080",
		DestBucket:     "canary-final",
	}
	d, err := s3syncDeltas(context.Background(), c, "default", "missing-s3sync", block, params)
	if err != nil {
		t.Fatalf("s3syncDeltas: %v", err)
	}
	if d["schedule"] != "0 4 * * *" {
		t.Errorf("schedule: got %q", d["schedule"])
	}
	if d["sourceEndpoint"] != "http://10.20.22.211" {
		t.Errorf("sourceEndpoint: got %q", d["sourceEndpoint"])
	}
	if d["destBucket"] != "canary-final" {
		t.Errorf("destBucket: got %q", d["destBucket"])
	}
}

func TestObserveS3Sync_Present(t *testing.T) {
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "colo-cluster-001-backup-s3sync", Namespace: "default"},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 4 * * *",
			Suspend:  ptr.To(false),
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{
								Name: s3SyncContainerName,
								Env: []corev1.EnvVar{
									{Name: "SOURCE_ENDPOINT", Value: "http://10.20.22.211"},
									{Name: "SOURCE_BUCKET", Value: "source"},
									{Name: "SOURCE_PREFIX", Value: ""},
									{Name: "DEST_ENDPOINT", Value: "http://10.20.22.244:30080"},
									{Name: "DEST_BUCKET", Value: "canary-final"},
									{Name: "DEST_PREFIX", Value: ""},
								},
							}},
						},
					},
				},
			},
		},
	}
	_, c := newReconciler(t, cj)
	got, err := observeS3Sync(context.Background(), c, "default", "colo-cluster-001-backup-s3sync")
	if err != nil {
		t.Fatalf("observeS3Sync: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil for present CronJob")
	}
	if got.Schedule == nil || *got.Schedule != "0 4 * * *" {
		t.Errorf("schedule: got %v", got.Schedule)
	}
	if got.Enabled == nil || !*got.Enabled {
		t.Errorf("enabled: want true (suspend=false), got %v", got.Enabled)
	}
	if got.SourceLocation == nil || *got.SourceLocation != "http://10.20.22.211/source" {
		t.Errorf("sourceLocation: got %v", got.SourceLocation)
	}
	if got.DestLocation == nil || *got.DestLocation != "http://10.20.22.244:30080/canary-final" {
		t.Errorf("destLocation: got %v", got.DestLocation)
	}
}

func TestObserveS3Sync_NotFound(t *testing.T) {
	_, c := newReconciler(t)
	got, err := observeS3Sync(context.Background(), c, "default", "missing")
	if err != nil {
		t.Fatalf("observeS3Sync: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing CronJob, got %+v", got)
	}
}

// findEnvVar returns the EnvVar with the given name or nil.
func findEnvVar(env []corev1.EnvVar, name string) *corev1.EnvVar {
	for i := range env {
		if env[i].Name == name {
			return &env[i]
		}
	}
	return nil
}

// contains reports whether s contains substr.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(substr); i++ {
				if s[i:i+len(substr)] == substr {
					return true
				}
			}
			return false
		}())
}
