package backupconfig

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	armadav1 "github.com/armada/configbundle/api/v1"
)

// backupStorageLocationGVK is the Velero BackupStorageLocation type.
var backupStorageLocationGVK = schema.GroupVersionKind{
	Group:   "velero.io",
	Version: "v1",
	Kind:    "BackupStorageLocation",
}

// storageDescriptor is the provider-neutral shape bc-controller needs to
// build a BackupStorageLocation, derived from an Orbital location URL.
// AzureAccount is only populated when Provider == "azure".
type storageDescriptor struct {
	Provider     string // "aws" or "azure"
	Bucket       string // S3 bucket, or Azure container name
	Prefix       string
	AzureAccount string
}

// deriveStorage classifies an Orbital location URL by scheme/host and
// extracts everything needed to build a BackupStorageLocation. Supports:
//   - s3://<bucket>/<prefix>
//   - https://<account>.blob.core.windows.net/<container>/<prefix>
//
// Any other scheme is an error — bc-controller does not guess.
func deriveStorage(location string) (storageDescriptor, error) {
	switch {
	case strings.HasPrefix(location, "s3://"):
		bucket, prefix, err := parseS3Location(location)
		if err != nil {
			return storageDescriptor{}, err
		}
		return storageDescriptor{Provider: "aws", Bucket: bucket, Prefix: prefix}, nil

	case strings.HasPrefix(location, "https://") && strings.Contains(location, ".blob.core.windows.net/"):
		account, container, prefix, err := parseAzureBlobLocation(location)
		if err != nil {
			return storageDescriptor{}, err
		}
		return storageDescriptor{Provider: "azure", Bucket: container, Prefix: prefix, AzureAccount: account}, nil

	default:
		return storageDescriptor{}, fmt.Errorf(
			"unsupported location scheme %q (bc-controller supports s3://<bucket>/<prefix> or https://<account>.blob.core.windows.net/<container>/<prefix>)",
			location)
	}
}

// parseS3Location splits "s3://<bucket>/<prefix...>" into bucket and prefix.
func parseS3Location(location string) (bucket, prefix string, err error) {
	rest := strings.TrimPrefix(location, "s3://")
	rest = strings.Trim(rest, "/")
	if rest == "" {
		return "", "", fmt.Errorf("empty bucket in location %q", location)
	}
	parts := strings.SplitN(rest, "/", 2)
	bucket = parts[0]
	if len(parts) == 2 {
		prefix = parts[1]
	}
	return bucket, prefix, nil
}

// parseAzureBlobLocation splits
// "https://<account>.blob.core.windows.net/<container>/<prefix...>" into
// storage account, container, and prefix.
func parseAzureBlobLocation(location string) (account, container, prefix string, err error) {
	const suffix = ".blob.core.windows.net/"
	idx := strings.Index(location, suffix)
	if idx == -1 {
		return "", "", "", fmt.Errorf("not an Azure Blob URL: %q", location)
	}
	account = strings.TrimPrefix(location[:idx], "https://")
	if account == "" {
		return "", "", "", fmt.Errorf("empty storage account in location %q", location)
	}
	rest := strings.Trim(location[idx+len(suffix):], "/")
	if rest == "" {
		return "", "", "", fmt.Errorf("empty container in location %q", location)
	}
	parts := strings.SplitN(rest, "/", 2)
	container = parts[0]
	if len(parts) == 2 {
		prefix = parts[1]
	}
	return account, container, prefix, nil
}

// buildBSLSpec renders the provider-specific spec map for a
// BackupStorageLocation. AWS and Azure need entirely different config keys
// and credential handling — this is the one place that difference exists.
//
// AWS: no spec.credential set — Velero falls back to its own default
// credential Secret ("cloud-credentials" convention).
//
// Azure: spec.credential is required and explicit — Azure's plugin secret
// format is completely different from AWS's, so no shared fallback exists.
// References VeleroAzureCredentialSecret (key "cloud").
func (r *BackupConfigReconciler) buildBSLSpec(desc storageDescriptor) (map[string]any, error) {
	switch desc.Provider {
	case "aws":
		return map[string]any{
			"provider": "aws",
			"objectStorage": map[string]any{
				"bucket": desc.Bucket,
				"prefix": desc.Prefix,
			},
			"config": map[string]any{
				"region":           r.VeleroS3Region,
				"s3Url":            r.VeleroS3URL,
				"s3ForcePathStyle": fmt.Sprintf("%t", r.VeleroS3ForcePathStyle),
			},
		}, nil

	case "azure":
		if r.VeleroAzureResourceGroup == "" || r.VeleroAzureSubscriptionID == "" {
			return nil, fmt.Errorf(
				"azure location requires VeleroAzureResourceGroup and VeleroAzureSubscriptionID to be configured")
		}
		if r.VeleroAzureCredentialSecret == "" {
			return nil, fmt.Errorf("azure location requires VeleroAzureCredentialSecret to be configured")
		}
		return map[string]any{
			"provider": "azure",
			"objectStorage": map[string]any{
				"bucket": desc.Bucket,
				"prefix": desc.Prefix,
			},
			"config": map[string]any{
				"resourceGroup":  r.VeleroAzureResourceGroup,
				"storageAccount": desc.AzureAccount,
				"subscriptionId": r.VeleroAzureSubscriptionID,
				"useAAD":         "true",
			},
			"credential": map[string]any{
				"name": r.VeleroAzureCredentialSecret,
				"key":  "cloud",
			},
		}, nil

	default:
		return nil, fmt.Errorf("unknown storage provider %q", desc.Provider)
	}
}

// reconcileBSL ensures a BackupStorageLocation named bslName exists and
// matches the derived storageDescriptor. Create-and-own: bc-controller is
// authoritative for this BSL's storage config once derived from intent.
// SSA-idempotent, same convention as reconcileVelero: always apply, let the
// API server do the work.
func (r *BackupConfigReconciler) reconcileBSL(ctx context.Context, bc *armadav1.BackupConfig, bslName string, desc storageDescriptor) (string, error) {
	logger := log.FromContext(ctx).WithName("backupconfig.velero.bsl")

	specMap, err := r.buildBSLSpec(desc)
	if err != nil {
		return "", err
	}

	desired := &unstructured.Unstructured{}
	desired.SetGroupVersionKind(backupStorageLocationGVK)
	desired.SetNamespace(r.VeleroNamespace)
	desired.SetName(bslName)
	if err := unstructured.SetNestedMap(desired.Object, specMap, "spec"); err != nil {
		return "", fmt.Errorf("build BSL spec: %w", err)
	}
	if err := ctrl.SetControllerReference(bc, desired, r.Scheme); err != nil {
		return "", fmt.Errorf("set owner on BSL: %w", err)
	}

	changed, err := bslChanged(ctx, r.Client, r.VeleroNamespace, bslName, desc)
	if err != nil {
		return "", err
	}

	if err := r.Patch(ctx, desired, client.Apply,
		client.FieldOwner(fieldManager),
		client.ForceOwnership,
	); err != nil {
		return "", fmt.Errorf("ssa patch BSL %s/%s: %w", r.VeleroNamespace, bslName, err)
	}

	if !changed {
		logger.V(1).Info("BSL already matches intent", "name", bslName)
		return "", nil
	}
	return fmt.Sprintf("bsl/%s: provider=%s, bucket=%s, prefix=%s", bslName, desc.Provider, desc.Bucket, desc.Prefix), nil
}

// bslChanged reports whether the live BSL's provider/bucket/prefix differ
// from intent. A NotFound BSL counts as changed (we're about to create it).
func bslChanged(ctx context.Context, c client.Client, namespace, name string, desc storageDescriptor) (bool, error) {
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(backupStorageLocationGVK)
	err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, live)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("get BSL: %w", err)
	}
	liveProvider, _, _ := unstructured.NestedString(live.Object, "spec", "provider")
	liveBucket, _, _ := unstructured.NestedString(live.Object, "spec", "objectStorage", "bucket")
	livePrefix, _, _ := unstructured.NestedString(live.Object, "spec", "objectStorage", "prefix")
	return liveProvider != desc.Provider || liveBucket != desc.Bucket || livePrefix != desc.Prefix, nil
}
