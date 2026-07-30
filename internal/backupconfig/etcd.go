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

// Well-known names used inside the built CronJob. These are conventions the
// existing `kube-system/etcd-backup` on colo-dev-main also uses — bc-controller
// mirrors them so a hand-crafted and controller-managed job have identical
// shape (aside from ObjectMeta).
const (
	etcdSnapshotTakerContainerName  = "snapshot-taker"
	etcdSnapshotWriterContainerName = "snapshot-writer"
	etcdGCContainerName             = "snapshot-gc"
	etcdSnapshotVolumeName          = "backup-dir"
	etcdSnapshotVolumeSize          = "2Gi"
	etcdPKIVolumeName               = "pki"
	etcdPKIHostPath                 = "/etc/kubernetes/pki"
	etcdSnapshotMountPath           = "/tmp/etcd-backups"
	etcdPKIMountPath                = "/etc/kubernetes/pki"
)

// snapshotTakerScript runs etcdctl snapshot-save against the local etcd
// (mounted PKI certs → auth via peer cert). Writes a timestamped .tar.gz to
// the shared emptyDir volume. Same shape as the hand-crafted job on colo-dev-main.
const snapshotTakerScript = `SNAPSHOT_TIMESTAMP=$(date -u +%Y-%m-%dT%H_%M_%S)
SNAPSHOT_NAME="snapshot-${SNAPSHOT_TIMESTAMP}"
SNAPSHOT_PATH="/tmp/etcd-backups/${SNAPSHOT_NAME}.db"
etcdctl snapshot save "$SNAPSHOT_PATH" \
  --cacert=/etc/kubernetes/pki/etcd/ca.crt \
  --cert=/etc/kubernetes/pki/etcd/server.crt \
  --key=/etc/kubernetes/pki/etcd/server.key
tar -czf "/tmp/etcd-backups/${SNAPSHOT_NAME}.tar.gz" "$SNAPSHOT_PATH"
rm "$SNAPSHOT_PATH"
`

// snapshotWriterScript uploads the .tar.gz produced by snapshotTakerScript to
// Azure Blob, then prunes old snapshots keeping the newest RETAIN_PER_DAY per
// UTC day. STORAGE_ACCOUNT, STORAGE_CONTAINER, BLOB_PREFIX come from
// bc-controller parsing spec.etcd.location; RETAIN_PER_DAY comes from
// controller config. The prune is scoped to "$BLOB_PREFIX/" so it can only
// ever touch THIS cluster's folder.
//
// Day-level cutoff (spec.etcd.retentionDays) is handled by a separate GC
// CronJob — single responsibility.
const snapshotWriterScript = `set -eu
az login --service-principal --username "$AZURE_CLIENT_ID" --password "$AZURE_CLIENT_SECRET" --tenant "$AZURE_TENANT_ID"

PREFIX="$BLOB_PREFIX/"

# --- 1. Upload the snapshot the init container produced ---
SNAPSHOT_NAME=$(ls /tmp/etcd-backups | grep 'snapshot-.*\.tar\.gz' | tail -n 1)
echo "Uploading ${PREFIX}${SNAPSHOT_NAME}"
az storage blob upload \
  --account-name "$STORAGE_ACCOUNT" \
  --container "$STORAGE_CONTAINER" \
  --name "${PREFIX}${SNAPSHOT_NAME}" \
  --file "/tmp/etcd-backups/${SNAPSHOT_NAME}" \
  --auth-mode login

# --- 2. Prune: keep newest RETAIN_PER_DAY snapshots per UTC day ---
(
  set +e
  ALL=$(az storage blob list \
    --account-name "$STORAGE_ACCOUNT" \
    --container-name "$STORAGE_CONTAINER" \
    --prefix "$PREFIX" \
    --num-results "*" \
    --auth-mode login \
    --query "[].name" -o tsv)

  KEEP_FROM=$((RETAIN_PER_DAY + 1))
  echo "=== Count-based prune for $PREFIX (keeping newest $RETAIN_PER_DAY per day) ==="
  DATES=$(echo "$ALL" | sed -n 's#.*snapshot-\([0-9-]*\)T.*#\1#p' | sort -u)
  for DAY in $DATES; do
    echo "--- Pruning day $DAY (keeping newest $RETAIN_PER_DAY) ---"
    echo "$ALL" | grep "snapshot-${DAY}T" | sort -r | tail -n +$KEEP_FROM | while read -r BLOB; do
      echo "Deleting $BLOB"
      az storage blob delete \
        --account-name "$STORAGE_ACCOUNT" \
        --container-name "$STORAGE_CONTAINER" \
        --name "$BLOB" \
        --auth-mode login \
        || echo "WARN: failed to delete $BLOB (continuing)"
    done
  done
)
echo "=== Prune finished ==="
`

// snapshotGCScript drops all blobs under BLOB_PREFIX whose date part is older
// than RETAIN_DAYS UTC days ago. Run in a dedicated GC CronJob — single
// responsibility. RETAIN_DAYS comes from spec.etcd.retentionDays on the
// BackupConfig CR; the azure-cli image includes python3 for date arithmetic
// (Alpine busybox date does not support -d "N days ago").
const snapshotGCScript = `set -eu
az login --service-principal --username "$AZURE_CLIENT_ID" --password "$AZURE_CLIENT_SECRET" --tenant "$AZURE_TENANT_ID"

PREFIX="$BLOB_PREFIX/"

ALL=$(az storage blob list \
  --account-name "$STORAGE_ACCOUNT" \
  --container-name "$STORAGE_CONTAINER" \
  --prefix "$PREFIX" \
  --num-results "*" \
  --auth-mode login \
  --query "[].name" -o tsv)

CUTOFF=$(python3 -c "from datetime import date, timedelta; print((date.today() - timedelta(days=$RETAIN_DAYS)).strftime('%Y-%m-%d'))")
echo "=== Day-level GC for $PREFIX (retentionDays=${RETAIN_DAYS}, cutoff=$CUTOFF) ==="
(
  set +e
  echo "$ALL" | grep "snapshot-" | while read -r BLOB; do
    BLOB_DATE=$(echo "$BLOB" | sed -n 's#.*snapshot-\([0-9-]*\)T.*#\1#p')
    if [ -n "$BLOB_DATE" ] && [ "$BLOB_DATE" \< "$CUTOFF" ]; then
      echo "Deleting $BLOB (date $BLOB_DATE before cutoff $CUTOFF)"
      az storage blob delete \
        --account-name "$STORAGE_ACCOUNT" \
        --container-name "$STORAGE_CONTAINER" \
        --name "$BLOB" \
        --auth-mode login \
        || echo "WARN: failed to delete $BLOB (continuing)"
    fi
  done
)
echo "=== Day-level GC finished ==="
`

// etcdCronJobName builds the deterministic CronJob name for a BackupConfig.
// Convention: "<bc-name>-etcd" — suffix names the spec.etcd domain, matching
// veleroScheduleName's "<bc-name>-velero" shape. BackupConfig.Name is already
// an RFC 1123–safe form of the ClusterBackup orbId (which ends in "-backup"),
// so the suffix must NOT repeat "backup".
func etcdCronJobName(bc *armadav1.BackupConfig) string {
	return bc.Name + "-etcd"
}

// etcdGCCronJobName builds the deterministic GC CronJob name for a BackupConfig.
func etcdGCCronJobName(bc *armadav1.BackupConfig) string {
	return bc.Name + "-etcd-gc"
}

// parseAzureBlobURL parses an Azure Blob HTTPS URL of the form
//
//	https://<account>.blob.core.windows.net/<container>/<prefix>...
//
// into (account, container, prefix). Prefix may be empty, may contain
// slashes, and does NOT include the snapshot filename — the runtime shell
// script appends "/${SNAPSHOT_NAME}" at upload time.
//
// Only `https` scheme is accepted. Azure Blob requires HTTPS in practice;
// rejecting http:// early gives operators a clear message instead of a
// mystery az-cli failure at snapshot time.
//
// The host's first label is treated as the storage account — matches the
// documented Azure Blob URL structure. Non-`.blob.core.windows.net` hosts
// are accepted (private-endpoint / custom-DNS scenarios) as long as they
// parse; az-cli will fail cleanly at runtime if the host is unreachable.
func parseAzureBlobURL(location string) (account, container, prefix string, err error) {
	u, parseErr := url.Parse(location)
	if parseErr != nil {
		return "", "", "", fmt.Errorf("parse location %q: %w", location, parseErr)
	}
	if u.Scheme != "https" {
		return "", "", "", fmt.Errorf("unsupported scheme %q in location %q (bc-controller etcd upload requires https://<account>.blob.core.windows.net/<container>/<prefix>)", u.Scheme, location)
	}
	if u.Host == "" {
		return "", "", "", fmt.Errorf("no host in location %q", location)
	}
	// First hostname label is the storage account.
	// "stgalbackupsdevccwus01.blob.core.windows.net" → "stgalbackupsdevccwus01"
	account = u.Host
	if i := strings.Index(account, "."); i >= 0 {
		account = account[:i]
	}
	if account == "" {
		return "", "", "", fmt.Errorf("no storage account in host of location %q", location)
	}
	// Path is "/<container>/<prefix>...". Split on the first "/" after the
	// leading one.
	path := strings.TrimPrefix(u.Path, "/")
	if path == "" {
		return "", "", "", fmt.Errorf("no container in path of location %q", location)
	}
	if i := strings.Index(path, "/"); i >= 0 {
		container = path[:i]
		prefix = path[i+1:]
	} else {
		container = path
		prefix = ""
	}
	return account, container, prefix, nil
}

// reconcileEtcd applies the desired etcd CronJob from bc.Spec.Etcd, then
// reconciles the GC CronJob if spec.etcd.retentionDays is set.
// Returns a human-readable summary of any PATCHes made, or an error.
//
// "Enabled = false" maps to spec.suspend = true on the CronJob — K8s' native
// pause toggle. The CronJob stays in place when disabled, so re-enabling is
// a one-field flip. Location is a canonical Azure Blob HTTPS URL of the form
// https://<account>.blob.core.windows.net/<container>/<prefix>; invalid
// values fail fast at reconcile time so operators see the mistake.
func (r *BackupConfigReconciler) reconcileEtcd(ctx context.Context, bc *armadav1.BackupConfig) (string, error) {
	logger := log.FromContext(ctx).WithName("backupconfig.etcd")
	block := bc.Spec.Etcd
	name := etcdCronJobName(bc)

	if block.Location == nil {
		return "", fmt.Errorf("bc %s: spec.etcd.location is required (expected https://<account>.blob.core.windows.net/<container>/<prefix>)", bc.Name)
	}
	account, container, prefix, err := parseAzureBlobURL(*block.Location)
	if err != nil {
		return "", err
	}

	params := etcdCronJobParams{
		Name:             name,
		Namespace:        r.EtcdBackupNamespace,
		StorageAccount:   account,
		StorageContainer: container,
		BlobPrefix:       prefix,
		EtcdctlImage:     r.EtcdctlImage,
		UploadImage:      r.UploadImage,
		CredentialSecret: r.CredentialSecret,
		RetainPerDay:     r.EtcdRetainPerDay,
		TimeZone:         r.EtcdBackupTimeZone,
		Block:            block,
	}

	// Compute what will change on this reconcile. Used ONLY to produce the
	// summary written to status.recentPatches — NOT to gate the apply. See
	// reconcileVelero for the rationale (always-apply is the SSA convention;
	// gating on spec-delta silently skips metadata reconciliation).
	deltas, err := etcdDeltas(ctx, r.Client, r.EtcdBackupNamespace, name, block, params)
	if err != nil {
		return "", err
	}

	cj := buildEtcdCronJob(params)
	// OwnerReference ties the CronJob's lifecycle to the BackupConfig CR:
	// deleting the BC cascades to the CronJob via native K8s GC. Cluster-
	// scoped parent → namespaced child is a documented K8s pattern (see
	// cert-manager's ClusterIssuer → Certificate).
	if err := ctrl.SetControllerReference(bc, cj, r.Scheme); err != nil {
		return "", fmt.Errorf("set owner on etcd cronjob: %w", err)
	}
	if err := r.Patch(ctx, cj, client.Apply,
		client.FieldOwner(fieldManager),
		client.ForceOwnership,
	); err != nil {
		return "", fmt.Errorf("ssa patch etcd cronjob %s/%s: %w", r.EtcdBackupNamespace, name, err)
	}

	if len(deltas) == 0 {
		logger.V(1).Info("etcd cronjob already matches intent (metadata reconciled)", "name", name)
	}

	gcSummary, err := r.reconcileEtcdGC(ctx, bc, account, container, prefix)
	if err != nil {
		return "", err
	}

	var parts []string
	if len(deltas) > 0 {
		parts = append(parts, formatBlockDeltas(fmt.Sprintf("etcd/%s", name), deltas))
	}
	if gcSummary != "" {
		parts = append(parts, gcSummary)
	}
	return strings.Join(parts, "; "), nil
}

// reconcileEtcdGC creates or updates the GC CronJob when spec.etcd.retentionDays
// is set, and deletes it when retentionDays is cleared.
func (r *BackupConfigReconciler) reconcileEtcdGC(ctx context.Context, bc *armadav1.BackupConfig, account, container, prefix string) (string, error) {
	logger := log.FromContext(ctx).WithName("backupconfig.etcd-gc")
	block := bc.Spec.Etcd
	gcName := etcdGCCronJobName(bc)

	if block.RetentionDays == nil {
		// No retention configured — delete the GC CronJob if it exists.
		var existing batchv1.CronJob
		err := r.Get(ctx, types.NamespacedName{Namespace: r.EtcdBackupNamespace, Name: gcName}, &existing)
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("get etcd gc cronjob: %w", err)
		}
		if err := r.Delete(ctx, &existing); err != nil && !apierrors.IsNotFound(err) {
			return "", fmt.Errorf("delete etcd gc cronjob %s/%s: %w", r.EtcdBackupNamespace, gcName, err)
		}
		logger.Info("deleted etcd gc cronjob (retentionDays cleared)", "name", gcName)
		return "", nil
	}

	gcParams := etcdGCCronJobParams{
		Name:             gcName,
		Namespace:        r.EtcdBackupNamespace,
		StorageAccount:   account,
		StorageContainer: container,
		BlobPrefix:       prefix,
		UploadImage:      r.UploadImage,
		CredentialSecret: r.CredentialSecret,
		RetainDays:       *block.RetentionDays,
		Schedule:         r.EtcdGCSchedule,
		TimeZone:         r.EtcdBackupTimeZone,
		Block:            block,
	}

	gcDeltas, err := etcdGCDeltas(ctx, r.Client, r.EtcdBackupNamespace, gcName, gcParams)
	if err != nil {
		return "", err
	}

	gcCJ := buildEtcdGCCronJob(gcParams)
	if err := ctrl.SetControllerReference(bc, gcCJ, r.Scheme); err != nil {
		return "", fmt.Errorf("set owner on etcd gc cronjob: %w", err)
	}
	if err := r.Patch(ctx, gcCJ, client.Apply,
		client.FieldOwner(fieldManager),
		client.ForceOwnership,
	); err != nil {
		return "", fmt.Errorf("ssa patch etcd gc cronjob %s/%s: %w", r.EtcdBackupNamespace, gcName, err)
	}

	if len(gcDeltas) == 0 {
		logger.V(1).Info("etcd gc cronjob already matches intent (metadata reconciled)", "name", gcName)
		return "", nil
	}
	return formatBlockDeltas(fmt.Sprintf("etcd-gc/%s", gcName), gcDeltas), nil
}

// etcdCronJobParams carries every piece of state buildEtcdCronJob needs to
// construct the CronJob. Struct-arg keeps buildEtcdCronJob's signature stable
// as fields are added (image versions, credential-secret name, etc.).
type etcdCronJobParams struct {
	Name             string
	Namespace        string
	StorageAccount   string
	StorageContainer string
	BlobPrefix       string // path prefix within the container (galleon/cluster hierarchy from orbital URL)
	EtcdctlImage     string
	UploadImage      string
	CredentialSecret string
	RetainPerDay     int    // how many snapshots to keep per UTC day
	TimeZone         string // IANA tz for the schedule ("" = cluster default/UTC)
	Block            *armadav1.EtcdBackupSpec
}

// buildEtcdCronJob constructs the full desired CronJob matching the existing
// `kube-system/etcd-backup` shape on colo-dev-main:
//   - hostNetwork on the control-plane node (to reach the local etcd)
//   - initContainer takes the snapshot with etcdctl using host-mounted PKI
//   - main container uploads the tarball to Azure Blob via az-cli and prunes
//     old snapshots (count-based: keep newest RETAIN_PER_DAY per UTC day)
//   - credentials come from a K8s Secret referenced by name
//   - blob path: <container>/<galleon>/<cluster>/<snapshot>
//
// Container images, secret name, and namespace are injected from bc-controller
// env; schedule, enabled, storage-account, storage-container come from
// orbital's EtcdBackup node → BackupConfig CR.
func buildEtcdCronJob(p etcdCronJobParams) *batchv1.CronJob {
	schedule := ""
	if p.Block.Schedule != nil {
		schedule = *p.Block.Schedule
	}

	envRefs := []corev1.EnvVar{
		{Name: "AZURE_CLIENT_ID", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: p.CredentialSecret},
				Key:                  "client-id",
			},
		}},
		{Name: "AZURE_CLIENT_SECRET", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: p.CredentialSecret},
				Key:                  "client-secret",
			},
		}},
		{Name: "AZURE_TENANT_ID", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: p.CredentialSecret},
				Key:                  "tenant-id",
			},
		}},
		{Name: "STORAGE_ACCOUNT", Value: p.StorageAccount},
		{Name: "STORAGE_CONTAINER", Value: p.StorageContainer},
		{Name: "BLOB_PREFIX", Value: p.BlobPrefix},
		{Name: "RETAIN_PER_DAY", Value: fmt.Sprintf("%d", p.RetainPerDay)},
	}

	volumes := []corev1.Volume{
		{
			Name: etcdSnapshotVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					SizeLimit: ptr.To(resource.MustParse(etcdSnapshotVolumeSize)),
				},
			},
		},
		{
			Name: etcdPKIVolumeName,
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: etcdPKIHostPath,
					Type: ptr.To(corev1.HostPathDirectory),
				},
			},
		},
	}

	initContainer := corev1.Container{
		Name:    etcdSnapshotTakerContainerName,
		Image:   p.EtcdctlImage,
		Command: []string{"/bin/sh", "-c", snapshotTakerScript},
		VolumeMounts: []corev1.VolumeMount{
			{Name: etcdSnapshotVolumeName, MountPath: etcdSnapshotMountPath},
			{Name: etcdPKIVolumeName, MountPath: etcdPKIMountPath},
		},
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
		},
	}

	writer := corev1.Container{
		Name:    etcdSnapshotWriterContainerName,
		Image:   p.UploadImage,
		Command: []string{"/bin/sh", "-c", snapshotWriterScript},
		Env:     envRefs,
		VolumeMounts: []corev1.VolumeMount{
			{Name: etcdSnapshotVolumeName, MountPath: etcdSnapshotMountPath},
		},
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
		},
	}

	podSpec := corev1.PodSpec{
		RestartPolicy:  corev1.RestartPolicyNever,
		HostNetwork:    true,
		DNSPolicy:      corev1.DNSClusterFirstWithHostNet,
		InitContainers: []corev1.Container{initContainer},
		Containers:     []corev1.Container{writer},
		Volumes:        volumes,
		NodeSelector: map[string]string{
			"node-role.kubernetes.io/control-plane": "",
		},
		Tolerations: []corev1.Toleration{{
			Key:      "node-role.kubernetes.io/control-plane",
			Operator: corev1.TolerationOpExists,
			Effect:   corev1.TaintEffectNoSchedule,
		}},
		TerminationGracePeriodSeconds: ptr.To(int64(30)),
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
					Template: corev1.PodTemplateSpec{Spec: podSpec},
				},
			},
		},
	}
	if p.TimeZone != "" {
		cj.Spec.TimeZone = ptr.To(p.TimeZone)
	}
	if p.Block.Enabled != nil {
		suspend := !*p.Block.Enabled
		cj.Spec.Suspend = &suspend
	}
	return cj
}

// etcdGCCronJobParams carries every piece of state buildEtcdGCCronJob needs.
type etcdGCCronJobParams struct {
	Name             string
	Namespace        string
	StorageAccount   string
	StorageContainer string
	BlobPrefix       string
	UploadImage      string
	CredentialSecret string
	RetainDays       int    // from spec.etcd.retentionDays on the BackupConfig CR
	Schedule         string // from controller config (EtcdGCSchedule)
	TimeZone         string
	Block            *armadav1.EtcdBackupSpec
}

// buildEtcdGCCronJob constructs the GC CronJob that drops blobs older than
// RetainDays UTC days. Unlike the snapshot CronJob it does not need hostNetwork
// or a control-plane node selector — it only calls Azure Blob APIs.
func buildEtcdGCCronJob(p etcdGCCronJobParams) *batchv1.CronJob {
	envRefs := []corev1.EnvVar{
		{Name: "AZURE_CLIENT_ID", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: p.CredentialSecret},
				Key:                  "client-id",
			},
		}},
		{Name: "AZURE_CLIENT_SECRET", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: p.CredentialSecret},
				Key:                  "client-secret",
			},
		}},
		{Name: "AZURE_TENANT_ID", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: p.CredentialSecret},
				Key:                  "tenant-id",
			},
		}},
		{Name: "STORAGE_ACCOUNT", Value: p.StorageAccount},
		{Name: "STORAGE_CONTAINER", Value: p.StorageContainer},
		{Name: "BLOB_PREFIX", Value: p.BlobPrefix},
		{Name: "RETAIN_DAYS", Value: fmt.Sprintf("%d", p.RetainDays)},
	}

	gcContainer := corev1.Container{
		Name:    etcdGCContainerName,
		Image:   p.UploadImage,
		Command: []string{"/bin/sh", "-c", snapshotGCScript},
		Env:     envRefs,
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("200m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
	}

	podSpec := corev1.PodSpec{
		RestartPolicy:                 corev1.RestartPolicyNever,
		Containers:                    []corev1.Container{gcContainer},
		TerminationGracePeriodSeconds: ptr.To(int64(30)),
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
			Schedule:                   p.Schedule,
			ConcurrencyPolicy:          batchv1.ForbidConcurrent,
			SuccessfulJobsHistoryLimit: ptr.To(int32(3)),
			FailedJobsHistoryLimit:     ptr.To(int32(3)),
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{Spec: podSpec},
				},
			},
		},
	}
	if p.TimeZone != "" {
		cj.Spec.TimeZone = ptr.To(p.TimeZone)
	}
	if p.Block.Enabled != nil {
		suspend := !*p.Block.Enabled
		cj.Spec.Suspend = &suspend
	}
	return cj
}

// observeEtcd reads the live etcd CronJob and projects the fields
// bc-controller manages into an ObservedEtcdStatus. Returns nil when the
// CronJob does not exist (same semantics as observeVelero — nil means "no
// live resource present," distinct from "present with empty fields").
//
// Field mapping mirrors the intent-writer in reconcileEtcd:
//   - live.spec.suspend (bool, nil-safe)                     → observed.Enabled (inverted)
//   - live.spec.schedule (string)                            → observed.Schedule
//   - container STORAGE_CONTAINER env value on the writer    → observed.Location (container name only)
func observeEtcd(ctx context.Context, c client.Client, namespace, name string) (*armadav1.ObservedEtcdStatus, error) {
	var live batchv1.CronJob
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &live); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get etcd cronjob for observe: %w", err)
	}
	out := &armadav1.ObservedEtcdStatus{}
	if live.Spec.Schedule != "" {
		s := live.Spec.Schedule
		out.Schedule = &s
	}
	enabled := true
	if live.Spec.Suspend != nil {
		enabled = !*live.Spec.Suspend
	}
	out.Enabled = &enabled
	if c := findContainer(live.Spec.JobTemplate.Spec.Template.Spec.Containers, etcdSnapshotWriterContainerName); c != nil {
		if v := envValue(c.Env, "STORAGE_CONTAINER"); v != "" {
			l := v
			out.Location = &l
		}
	}
	return out, nil
}

// etcdDeltas returns the set of fields that differ between the live CronJob
// and the intent. NotFound means all intent fields are deltas
// (create-on-first-apply). Delta detection covers only the orbital-driven
// knobs (schedule, suspend, storage target — account/container/prefix);
// everything else in the CronJob is controller-owned convention, always
// reapplied via SSA when a delta is present on any tracked field.
func etcdDeltas(ctx context.Context, c client.Client, namespace, name string, block *armadav1.EtcdBackupSpec, params etcdCronJobParams) (map[string]string, error) {
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
		out["storageAccount"] = params.StorageAccount
		out["storageContainer"] = params.StorageContainer
		out["blobPrefix"] = params.BlobPrefix
		return out, nil
	case err != nil:
		return nil, fmt.Errorf("get etcd cronjob: %w", err)
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

	liveWriter := findContainer(live.Spec.JobTemplate.Spec.Template.Spec.Containers, etcdSnapshotWriterContainerName)
	if liveWriter == nil {
		// Missing the writer container entirely — anything about storage is a
		// delta. Full re-apply will restore the shape.
		out["storageAccount"] = params.StorageAccount
		out["storageContainer"] = params.StorageContainer
		out["blobPrefix"] = params.BlobPrefix
		return out, nil
	}
	if envValue(liveWriter.Env, "STORAGE_ACCOUNT") != params.StorageAccount {
		out["storageAccount"] = params.StorageAccount
	}
	if envValue(liveWriter.Env, "STORAGE_CONTAINER") != params.StorageContainer {
		out["storageContainer"] = params.StorageContainer
	}
	if envValue(liveWriter.Env, "BLOB_PREFIX") != params.BlobPrefix {
		out["blobPrefix"] = params.BlobPrefix
	}
	return out, nil
}

// etcdGCDeltas returns the set of fields that differ between the live GC
// CronJob and the intent. NotFound means all fields are deltas (first create).
func etcdGCDeltas(ctx context.Context, c client.Client, namespace, name string, params etcdGCCronJobParams) (map[string]string, error) {
	out := map[string]string{}

	var live batchv1.CronJob
	err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &live)
	switch {
	case apierrors.IsNotFound(err):
		out["schedule"] = params.Schedule
		out["storageAccount"] = params.StorageAccount
		out["storageContainer"] = params.StorageContainer
		out["blobPrefix"] = params.BlobPrefix
		out["retainDays"] = fmt.Sprintf("%d", params.RetainDays)
		return out, nil
	case err != nil:
		return nil, fmt.Errorf("get etcd gc cronjob: %w", err)
	}

	if live.Spec.Schedule != params.Schedule {
		out["schedule"] = params.Schedule
	}
	gcContainer := findContainer(live.Spec.JobTemplate.Spec.Template.Spec.Containers, etcdGCContainerName)
	if gcContainer == nil {
		out["storageAccount"] = params.StorageAccount
		out["storageContainer"] = params.StorageContainer
		out["blobPrefix"] = params.BlobPrefix
		out["retainDays"] = fmt.Sprintf("%d", params.RetainDays)
		return out, nil
	}
	if envValue(gcContainer.Env, "STORAGE_ACCOUNT") != params.StorageAccount {
		out["storageAccount"] = params.StorageAccount
	}
	if envValue(gcContainer.Env, "STORAGE_CONTAINER") != params.StorageContainer {
		out["storageContainer"] = params.StorageContainer
	}
	if envValue(gcContainer.Env, "BLOB_PREFIX") != params.BlobPrefix {
		out["blobPrefix"] = params.BlobPrefix
	}
	if envValue(gcContainer.Env, "RETAIN_DAYS") != fmt.Sprintf("%d", params.RetainDays) {
		out["retainDays"] = fmt.Sprintf("%d", params.RetainDays)
	}
	return out, nil
}

func findContainer(cs []corev1.Container, name string) *corev1.Container {
	for i := range cs {
		if cs[i].Name == name {
			return &cs[i]
		}
	}
	return nil
}

func envValue(env []corev1.EnvVar, name string) string {
	for _, e := range env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}
