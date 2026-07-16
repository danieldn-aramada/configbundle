package backupconfig

import (
	"context"
	"fmt"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
)

// azureEtcdStore is the production etcdBackupStore: it lists the etcd snapshot
// objects in Azure Blob to observe the actual backup artifacts. Read-only —
// bc lists, it never writes/deletes (retention is out of scope; see
// docs/reference/BACKUP.md).
//
// Auth uses a service-principal credential from the standard AZURE_TENANT_ID /
// AZURE_CLIENT_ID / AZURE_CLIENT_SECRET env vars (mounted from the storage
// credentials Secret). This is the "bc holds read-scoped store creds to
// observe its resource" cost the design accepts — the analog of sc-controller
// holding iDRAC creds.
type azureEtcdStore struct {
	cred azcore.TokenCredential
}

// NewAzureEtcdStore builds the store from the environment credential. Returns
// an error if the AZURE_* env vars are missing/invalid — callers should log and
// run with observation disabled rather than crashing.
func NewAzureEtcdStore() (EtcdBackupStore, error) {
	cred, err := azidentity.NewEnvironmentCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("azure credential (expects AZURE_TENANT_ID / AZURE_CLIENT_ID / AZURE_CLIENT_SECRET): %w", err)
	}
	return &azureEtcdStore{cred: cred}, nil
}

// List enumerates the blobs under the location's container+prefix and folds
// them into an inventory (count, newest modification time, newest size).
func (s *azureEtcdStore) List(ctx context.Context, location string) (etcdSnapshotInventory, error) {
	account, container, prefix, err := parseAzureBlobURL(location)
	if err != nil {
		return etcdSnapshotInventory{}, err
	}
	client, err := azblob.NewClient("https://"+account+".blob.core.windows.net", s.cred, nil)
	if err != nil {
		return etcdSnapshotInventory{}, fmt.Errorf("azblob client for %s: %w", account, err)
	}

	var items []blobMeta
	pager := client.NewListBlobsFlatPager(container, &azblob.ListBlobsFlatOptions{Prefix: &prefix})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return etcdSnapshotInventory{}, fmt.Errorf("list blobs %s/%s: %w", container, prefix, err)
		}
		for _, b := range page.Segment.BlobItems {
			if b == nil || b.Properties == nil || b.Properties.LastModified == nil {
				continue
			}
			var size int64
			if b.Properties.ContentLength != nil {
				size = *b.Properties.ContentLength
			}
			items = append(items, blobMeta{modified: *b.Properties.LastModified, bytes: size})
		}
	}
	return foldBlobInventory(items), nil
}

// blobMeta is the minimal per-object metadata the fold needs — decoupled from
// the SDK types so foldBlobInventory is unit-testable without Azure.
type blobMeta struct {
	modified time.Time
	bytes    int64
}

// foldBlobInventory reduces a set of blob metadata into the snapshot inventory:
// total count, and the modification time + size of the newest object.
func foldBlobInventory(items []blobMeta) etcdSnapshotInventory {
	inv := etcdSnapshotInventory{Count: len(items)}
	for _, it := range items {
		if it.modified.After(inv.LatestModified) {
			inv.LatestModified = it.modified
			inv.LatestBytes = it.bytes
		}
	}
	return inv
}
