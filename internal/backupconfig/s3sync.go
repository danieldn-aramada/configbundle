package backupconfig

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	armadav1 "github.com/armada/configbundle/api/v1"
)

const s3SyncContainerName = "s3-syncer"

// s3SyncScript runs rclone copy from a source S3-compatible endpoint to a
// destination S3-compatible endpoint. SOURCE_* and DEST_* env vars are injected
// from the parsed spec.s3Sync.sourceLocation / destLocation URLs; credentials
// come from the s3-creds K8s Secret. path_style=true is required for
// Rook-Ceph and most S3-compatible non-AWS endpoints.
// s3SyncScript writes a named-remote rclone config at runtime and copies.
// Inline connection strings are not used because ':' in http(s):// endpoints
// is parsed as a delimiter by rclone, truncating the endpoint to just the
// scheme word ("http") and producing "Custom endpoint 'http' was not a valid URI".
const s3SyncScript = `set -ex
cat > /tmp/rclone.conf << EOF
[source]
type = s3
provider = Ceph
endpoint = ${SOURCE_ENDPOINT}
access_key_id = ${SOURCE_ACCESS_KEY}
secret_access_key = ${SOURCE_SECRET_KEY}
no_check_bucket = true

[dest]
type = s3
provider = Other
endpoint = ${DEST_ENDPOINT}
access_key_id = ${DEST_ACCESS_KEY}
secret_access_key = ${DEST_SECRET_KEY}
no_check_bucket = true
EOF

if [ -n "${SOURCE_PREFIX}" ]; then
  SOURCE_PATH="${SOURCE_BUCKET}/${SOURCE_PREFIX}"
else
  SOURCE_PATH="${SOURCE_BUCKET}"
fi
if [ -n "${DEST_PREFIX}" ]; then
  DEST_PATH="${DEST_BUCKET}/${DEST_PREFIX}"
else
  DEST_PATH="${DEST_BUCKET}"
fi
rclone --config /tmp/rclone.conf -v copy "source:${SOURCE_PATH}" "dest:${DEST_PATH}"
`

// s3SyncCronJobName builds the deterministic CronJob name for a BackupConfig.
// Convention: "<bc-name>-s3sync" — matches the "<bc-name>-etcd" and
// "<bc-name>-velero" siblings.
func s3SyncCronJobName(bc *armadav1.BackupConfig) string {
	return bc.Name + "-s3sync"
}

// parseS3SyncURL parses an S3-compatible URL of the form
//
//	https://<endpoint>/<bucket>/<prefix>...
//
// into (endpoint, bucket, prefix). Endpoint includes scheme and host.
// Both http and https are accepted — Rook-Ceph RGW is commonly accessed
// over plain http inside the cluster.
func parseS3SyncURL(location string) (endpoint, bucket, prefix string, err error) {
	u, parseErr := url.Parse(location)
	if parseErr != nil {
		return "", "", "", fmt.Errorf("parse location %q: %w", location, parseErr)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", "", "", fmt.Errorf("unsupported scheme %q in location %q (expected http(s)://<endpoint>/<bucket>/<prefix>)", u.Scheme, location)
	}
	if u.Host == "" {
		return "", "", "", fmt.Errorf("no host in location %q", location)
	}
	endpoint = u.Scheme + "://" + u.Host
	path := strings.TrimPrefix(u.Path, "/")
	if path == "" {
		return "", "", "", fmt.Errorf("no bucket in path of location %q", location)
	}
	if i := strings.Index(path, "/"); i >= 0 {
		bucket = path[:i]
		prefix = path[i+1:]
	} else {
		bucket = path
		prefix = ""
	}
	return endpoint, bucket, prefix, nil
}

// reconcileS3Sync applies the desired S3 Sync CronJob from bc.Spec.S3Sync.
// Returns a human-readable summary of the PATCH (empty string = no PATCH
// needed) or an error if the apply failed.
//
// "Enabled = false" maps to spec.suspend = true on the CronJob. ConcurrencyPolicy
// is ForbidConcurrent — a sync run that overlaps itself could produce partial or
// duplicate data at the destination.
func (r *BackupConfigReconciler) reconcileS3Sync(ctx context.Context, bc *armadav1.BackupConfig) (string, error) {
	logger := log.FromContext(ctx).WithName("backupconfig.s3sync")
	block := bc.Spec.S3Sync
	name := s3SyncCronJobName(bc)

	if block.SourceLocation == nil {
		return "", fmt.Errorf("bc %s: spec.s3Sync.sourceLocation is required", bc.Name)
	}
	if block.DestLocation == nil {
		return "", fmt.Errorf("bc %s: spec.s3Sync.destLocation is required", bc.Name)
	}

	srcEndpoint, srcBucket, srcPrefix, err := parseS3SyncURL(*block.SourceLocation)
	if err != nil {
		return "", fmt.Errorf("bc %s: spec.s3Sync.sourceLocation: %w", bc.Name, err)
	}
	dstEndpoint, dstBucket, dstPrefix, err := parseS3SyncURL(*block.DestLocation)
	if err != nil {
		return "", fmt.Errorf("bc %s: spec.s3Sync.destLocation: %w", bc.Name, err)
	}

	params := s3SyncCronJobParams{
		Name:             name,
		Namespace:        r.S3SyncNamespace,
		SourceEndpoint:   srcEndpoint,
		SourceBucket:     srcBucket,
		SourcePrefix:     srcPrefix,
		DestEndpoint:     dstEndpoint,
		DestBucket:       dstBucket,
		DestPrefix:       dstPrefix,
		RcloneImage:      r.S3SyncRcloneImage,
		CredentialSecret: r.S3SyncCredSecret,
		Block:            block,
	}

	// Compute deltas for status.recentPatches only — NOT to gate the apply.
	// SSA is idempotent; always-apply is the convention (see reconcileVelero).
	deltas, err := s3syncDeltas(ctx, r.Client, r.S3SyncNamespace, name, block, params)
	if err != nil {
		return "", err
	}

	cj := buildS3SyncCronJob(params)
	// OwnerReference ties the CronJob lifecycle to the BackupConfig CR:
	// deleting the BC cascades to the CronJob via native K8s GC.
	if err := ctrl.SetControllerReference(bc, cj, r.Scheme); err != nil {
		return "", fmt.Errorf("set owner on s3sync cronjob: %w", err)
	}
	if err := r.Patch(ctx, cj, client.Apply,
		client.FieldOwner(fieldManager),
		client.ForceOwnership,
	); err != nil {
		return "", fmt.Errorf("ssa patch s3sync cronjob %s/%s: %w", r.S3SyncNamespace, name, err)
	}

	if len(deltas) == 0 {
		logger.V(1).Info("s3sync cronjob already matches intent (metadata reconciled)", "name", name)
		return "", nil
	}
	return formatBlockDeltas(fmt.Sprintf("s3sync/%s", name), deltas), nil
}

// s3SyncCronJobParams carries every piece of state buildS3SyncCronJob needs.
type s3SyncCronJobParams struct {
	Name             string
	Namespace        string
	SourceEndpoint   string
	SourceBucket     string
	SourcePrefix     string
	DestEndpoint     string
	DestBucket       string
	DestPrefix       string
	RcloneImage      string
	CredentialSecret string
	Block            *armadav1.S3SyncSpec
}

// buildS3SyncCronJob constructs the full desired CronJob. A single rclone
// container syncs from the source S3-compatible endpoint to the destination.
// Credentials are injected from a K8s Secret — never embedded in the spec.
// ForbidConcurrent prevents overlapping sync runs that could produce partial
// or duplicate data at the destination.
func buildS3SyncCronJob(p s3SyncCronJobParams) *batchv1.CronJob {
	schedule := ""
	if p.Block.Schedule != nil {
		schedule = *p.Block.Schedule
	}

	envRefs := []corev1.EnvVar{
		{Name: "SOURCE_ACCESS_KEY", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: p.CredentialSecret},
				Key:                  "source-access-key",
			},
		}},
		{Name: "SOURCE_SECRET_KEY", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: p.CredentialSecret},
				Key:                  "source-secret-key",
			},
		}},
		{Name: "DEST_ACCESS_KEY", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: p.CredentialSecret},
				Key:                  "dest-access-key",
			},
		}},
		{Name: "DEST_SECRET_KEY", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: p.CredentialSecret},
				Key:                  "dest-secret-key",
			},
		}},
		{Name: "SOURCE_ENDPOINT", Value: p.SourceEndpoint},
		{Name: "SOURCE_BUCKET", Value: p.SourceBucket},
		{Name: "SOURCE_PREFIX", Value: p.SourcePrefix},
		{Name: "DEST_ENDPOINT", Value: p.DestEndpoint},
		{Name: "DEST_BUCKET", Value: p.DestBucket},
		{Name: "DEST_PREFIX", Value: p.DestPrefix},
	}

	container := corev1.Container{
		Name:    s3SyncContainerName,
		Image:   p.RcloneImage,
		Command: []string{"/bin/sh", "-c", s3SyncScript},
		Env:     envRefs,
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
		},
	}

	cj := &batchv1.CronJob{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "batch/v1",
			Kind:       "CronJob",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name,
			Namespace: p.Namespace,
		},
		Spec: batchv1.CronJobSpec{
			Schedule:                   schedule,
			ConcurrencyPolicy:          batchv1.ForbidConcurrent,
			SuccessfulJobsHistoryLimit: ptr.To(int32(3)),
			FailedJobsHistoryLimit:     ptr.To(int32(3)),
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							RestartPolicy:                 corev1.RestartPolicyNever,
							Containers:                    []corev1.Container{container},
							TerminationGracePeriodSeconds: ptr.To(int64(30)),
						},
					},
				},
			},
		},
	}
	if p.Block.Enabled != nil {
		suspend := !*p.Block.Enabled
		cj.Spec.Suspend = &suspend
	}
	return cj
}

// observeS3Sync reads the live S3 Sync CronJob and projects the fields
// bc-controller manages into an ObservedS3SyncStatus. Returns nil when the
// CronJob does not exist — nil means "no live resource present," distinct
// from "present with empty fields."
func observeS3Sync(ctx context.Context, c client.Client, namespace, name string) (*armadav1.ObservedS3SyncStatus, error) {
	var live batchv1.CronJob
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &live); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get s3sync cronjob for observe: %w", err)
	}
	out := &armadav1.ObservedS3SyncStatus{}
	if live.Spec.Schedule != "" {
		s := live.Spec.Schedule
		out.Schedule = &s
	}
	enabled := true
	if live.Spec.Suspend != nil {
		enabled = !*live.Spec.Suspend
	}
	out.Enabled = &enabled

	if syncer := findContainer(live.Spec.JobTemplate.Spec.Template.Spec.Containers, s3SyncContainerName); syncer != nil {
		srcEndpoint := envValue(syncer.Env, "SOURCE_ENDPOINT")
		srcBucket := envValue(syncer.Env, "SOURCE_BUCKET")
		srcPrefix := envValue(syncer.Env, "SOURCE_PREFIX")
		if srcEndpoint != "" && srcBucket != "" {
			var loc string
			if srcPrefix != "" {
				loc = srcEndpoint + "/" + srcBucket + "/" + srcPrefix
			} else {
				loc = srcEndpoint + "/" + srcBucket
			}
			out.SourceLocation = &loc
		}

		dstEndpoint := envValue(syncer.Env, "DEST_ENDPOINT")
		dstBucket := envValue(syncer.Env, "DEST_BUCKET")
		dstPrefix := envValue(syncer.Env, "DEST_PREFIX")
		if dstEndpoint != "" && dstBucket != "" {
			var loc string
			if dstPrefix != "" {
				loc = dstEndpoint + "/" + dstBucket + "/" + dstPrefix
			} else {
				loc = dstEndpoint + "/" + dstBucket
			}
			out.DestLocation = &loc
		}
	}
	return out, nil
}

// s3syncDeltas returns the set of fields that differ between the live CronJob
// and the intent. NotFound means all intent fields are deltas (create on first
// apply). Used only to produce the recentPatches summary — never to gate the apply.
func s3syncDeltas(ctx context.Context, c client.Client, namespace, name string, block *armadav1.S3SyncSpec, params s3SyncCronJobParams) (map[string]string, error) {
	out := map[string]string{}

	var live batchv1.CronJob
	err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &live)
	switch {
	case apierrors.IsNotFound(err):
		if block.Schedule != nil {
			out["schedule"] = *block.Schedule
		}
		if block.Enabled != nil {
			out["suspend"] = fmt.Sprintf("%t", !*block.Enabled)
		}
		out["sourceEndpoint"] = params.SourceEndpoint
		out["sourceBucket"] = params.SourceBucket
		out["destEndpoint"] = params.DestEndpoint
		out["destBucket"] = params.DestBucket
		return out, nil
	case err != nil:
		return nil, fmt.Errorf("get s3sync cronjob: %w", err)
	}

	if block.Schedule != nil && live.Spec.Schedule != *block.Schedule {
		out["schedule"] = *block.Schedule
	}
	if block.Enabled != nil {
		desiredSuspend := !*block.Enabled
		liveSuspend := false
		if live.Spec.Suspend != nil {
			liveSuspend = *live.Spec.Suspend
		}
		if liveSuspend != desiredSuspend {
			out["suspend"] = fmt.Sprintf("%t", desiredSuspend)
		}
	}

	liveSyncer := findContainer(live.Spec.JobTemplate.Spec.Template.Spec.Containers, s3SyncContainerName)
	if liveSyncer == nil {
		out["sourceEndpoint"] = params.SourceEndpoint
		out["sourceBucket"] = params.SourceBucket
		out["destEndpoint"] = params.DestEndpoint
		out["destBucket"] = params.DestBucket
		return out, nil
	}
	if envValue(liveSyncer.Env, "SOURCE_ENDPOINT") != params.SourceEndpoint {
		out["sourceEndpoint"] = params.SourceEndpoint
	}
	if envValue(liveSyncer.Env, "SOURCE_BUCKET") != params.SourceBucket {
		out["sourceBucket"] = params.SourceBucket
	}
	if envValue(liveSyncer.Env, "SOURCE_PREFIX") != params.SourcePrefix {
		out["sourcePrefix"] = params.SourcePrefix
	}
	if envValue(liveSyncer.Env, "DEST_ENDPOINT") != params.DestEndpoint {
		out["destEndpoint"] = params.DestEndpoint
	}
	if envValue(liveSyncer.Env, "DEST_BUCKET") != params.DestBucket {
		out["destBucket"] = params.DestBucket
	}
	if envValue(liveSyncer.Env, "DEST_PREFIX") != params.DestPrefix {
		out["destPrefix"] = params.DestPrefix
	}
	return out, nil
}
