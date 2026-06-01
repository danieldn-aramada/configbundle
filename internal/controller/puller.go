package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	armadav1 "github.com/armada/configbundle/api/v1"
)

// OCIArtifact holds the layers extracted from a verified OCI artifact.
type OCIArtifact struct {
	// Digest is the content-addressable digest of the artifact (sha256:...).
	Digest string
	// Manifest is the ConfigBundle manifest layer (MediaTypeManifest).
	Manifest []byte
	// Data is the DGraph subgraph data layer (MediaTypeData / data.json.gz).
	Data []byte
	// Schema is the DGraph schema layer (MediaTypeSchema / schema.gz).
	Schema []byte
}

// OCIClient pulls a cosign-verified OCI artifact and extracts its layers.
// The real implementation uses oras-go + cosign. Tests inject a fake.
type OCIClient interface {
	Pull(ctx context.Context, ref string) (*OCIArtifact, error)
}

// OrbClient calls orb's source-agnostic import API.
// The real implementation POSTs to POST /api/v1/import/subgraph. Tests inject a fake.
type OrbClient interface {
	ImportSubgraph(ctx context.Context, data, schema []byte) error
}

// PullerConfig holds environment-sourced configuration for the Puller.
type PullerConfig struct {
	// RegistryURL is the Zot OCI registry base URL (EDGE_REGISTRY_URL).
	RegistryURL string
	// PollInterval is how often the Puller checks for a new artifact (POLL_INTERVAL).
	PollInterval time.Duration
	// OrbEndpoint is orb's base URL (ORB_ENDPOINT).
	OrbEndpoint string
	// Datacenter identifies the OCI repository and the ConfigBundle CR name.
	Datacenter string
	// Namespace is the K8s namespace for the ConfigBundle CR.
	Namespace string
	// OrbImportEnabled controls whether the Puller calls orb's POST /import/subgraph.
	// Set to false for local development without orb running. In production this must
	// be true to maintain version coherence between Dgraph and the ConfigBundle CR.
	OrbImportEnabled bool
}

// Puller is a ctrl.Runnable that polls Zot on a fixed interval, cosign-verifies
// the artifact, calls orb's import API, then applies the ConfigBundle CR spec via SSA.
type Puller struct {
	Client client.Client
	Config PullerConfig
	OCI    OCIClient
	Orb    OrbClient
}

// NeedsLeaderElection ensures only the leader replica runs the Puller.
func (p *Puller) NeedsLeaderElection() bool { return true }

// Start implements ctrl.Runnable. Runs until ctx is cancelled.
func (p *Puller) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("puller")
	logger.Info("starting", "interval", p.Config.PollInterval, "datacenter", p.Config.Datacenter)

	if err := p.RunCycle(ctx); err != nil {
		logger.Error(err, "initial puller cycle failed")
	}
	ticker := time.NewTicker(p.Config.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := p.RunCycle(ctx); err != nil {
				logger.Error(err, "puller cycle failed")
			}
		}
	}
}

// RunCycle executes one pull-verify-import-apply cycle.
// Exported so tests can invoke a single cycle directly without the ticker.
func (p *Puller) RunCycle(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("puller")

	ref := p.Config.RegistryURL + "/" + p.Config.Datacenter + ":latest"
	artifact, err := p.OCI.Pull(ctx, ref)
	if err != nil {
		return fmt.Errorf("OCI pull %s: %w", ref, err)
	}

	// GET the current ConfigBundle CR to check whether the digest has changed.
	var cb armadav1.ConfigBundle
	err = p.Client.Get(ctx, types.NamespacedName{
		Name:      p.Config.Datacenter,
		Namespace: p.Config.Namespace,
	}, &cb)
	if client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("get ConfigBundle: %w", err)
	}
	if cb.Status.LastAppliedDigest == artifact.Digest {
		logger.V(1).Info("digest unchanged, skipping", "digest", artifact.Digest)
		return nil
	}

	spec, err := parseManifest(artifact.Manifest)
	if err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}

	// Import to orb BEFORE writing the ConfigBundle CR.
	// If orb fails the cycle aborts — config delivery state must not advance
	// while Dgraph is stale (version coherence guarantee).
	// OrbImportEnabled=false skips this step for local development without orb running.
	if p.Config.OrbImportEnabled {
		if err := p.Orb.ImportSubgraph(ctx, artifact.Data, artifact.Schema); err != nil {
			return fmt.Errorf("orb import: %w", err)
		}
	}

	// Omit server entries owned (even partially) by local:admin so SSA does not
	// conflict on those entries. With +listType=map, ownership is per-entry by serviceTag.
	patchSpec := omitAdminOwnedServers(spec, cb.ManagedFields)

	// Apply WITHOUT ForceOwnership — preserves local:admin fields on the ConfigBundle CR.
	apply := &armadav1.ConfigBundle{
		TypeMeta: metav1.TypeMeta{
			APIVersion: armadav1.GroupVersion.String(),
			Kind:       "ConfigBundle",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Config.Datacenter,
			Namespace: p.Config.Namespace,
		},
		Spec: patchSpec,
	}
	if err := p.Client.Patch(ctx, apply, client.Apply,
		client.FieldOwner("configbundle-controller"),
	); err != nil {
		return fmt.Errorf("apply ConfigBundle spec: %w", err)
	}

	// Re-fetch to get the latest resourceVersion before updating status.
	if err := p.Client.Get(ctx, types.NamespacedName{
		Name:      p.Config.Datacenter,
		Namespace: p.Config.Namespace,
	}, &cb); err != nil {
		return fmt.Errorf("re-get ConfigBundle for status update: %w", err)
	}

	now := metav1.Now()
	cb.Status.LastAppliedDigest = artifact.Digest
	cb.Status.LastAppliedAt = &now
	setCondition(&cb.Status.Conditions, armadav1.ConditionArtifactFetched,
		metav1.ConditionTrue, "ArtifactFetched", "OCI artifact fetched from Zot")
	setCondition(&cb.Status.Conditions, armadav1.ConditionSignatureVerified,
		metav1.ConditionTrue, "SignatureVerified", "cosign signature verified")
	if p.Config.OrbImportEnabled {
		setCondition(&cb.Status.Conditions, armadav1.ConditionGraphImported,
			metav1.ConditionTrue, "GraphImported", "orb import subgraph succeeded")
	} else {
		setCondition(&cb.Status.Conditions, armadav1.ConditionGraphImported,
			metav1.ConditionFalse, "Disabled", "orb import disabled via --enable-orb-import=false")
	}

	if err := p.Client.Status().Update(ctx, &cb); err != nil {
		return fmt.Errorf("update ConfigBundle status: %w", err)
	}

	logger.Info("cycle complete", "digest", artifact.Digest, "servers", len(patchSpec.Servers))
	return nil
}

// parseManifest deserialises the ConfigBundle manifest YAML layer into a ConfigBundleSpec.
func parseManifest(data []byte) (armadav1.ConfigBundleSpec, error) {
	var spec armadav1.ConfigBundleSpec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return spec, fmt.Errorf("unmarshal manifest YAML: %w", err)
	}
	return spec, nil
}

// omitAdminOwnedServers returns a copy of spec with server entries removed if
// local:admin owns any field within that entry. Omitting the entire entry is safe:
// it preserves the admin's full intent and avoids a 409 partial-apply conflict.
func omitAdminOwnedServers(spec armadav1.ConfigBundleSpec, managedFields []metav1.ManagedFieldsEntry) armadav1.ConfigBundleSpec {
	owned := adminOwnedServiceTags(managedFields)
	if len(owned) == 0 {
		return spec
	}
	filtered := make([]armadav1.ServerSpec, 0, len(spec.Servers))
	for _, s := range spec.Servers {
		if !owned[s.ServiceTag] {
			filtered = append(filtered, s)
		}
	}
	spec.Servers = filtered
	return spec
}

// adminOwnedServiceTags parses managedFields and returns the set of serviceTag values
// for server entries that local:admin owns (at any field depth).
//
// With +listType=map +listMapKey=serviceTag, the Kubernetes API encodes per-entry
// ownership in fieldsV1 as:
//
//	{"f:spec": {"f:servers": {"k:{\"serviceTag\":\"3RK3V64\"}": {...}}}}
func adminOwnedServiceTags(managedFields []metav1.ManagedFieldsEntry) map[string]bool {
	owned := map[string]bool{}
	for _, entry := range managedFields {
		if entry.Manager != "local:admin" || entry.FieldsV1 == nil {
			continue
		}
		var fields map[string]interface{}
		if err := json.Unmarshal(entry.FieldsV1.Raw, &fields); err != nil {
			continue
		}
		specFields, _ := fields["f:spec"].(map[string]interface{})
		serverFields, _ := specFields["f:servers"].(map[string]interface{})
		for key := range serverFields {
			// Map list entry keys look like: k:{"serviceTag":"3RK3V64"}
			if !strings.HasPrefix(key, "k:{") {
				continue
			}
			var keyMap struct {
				ServiceTag string `json:"serviceTag"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(key, "k:")), &keyMap); err != nil {
				continue
			}
			if keyMap.ServiceTag != "" {
				owned[keyMap.ServiceTag] = true
			}
		}
	}
	return owned
}

// setCondition upserts a metav1.Condition on the conditions slice.
func setCondition(conditions *[]metav1.Condition, condType string, status metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	for i, c := range *conditions {
		if c.Type == condType {
			(*conditions)[i].Status = status
			(*conditions)[i].Reason = reason
			(*conditions)[i].Message = message
			(*conditions)[i].LastTransitionTime = now
			return
		}
	}
	*conditions = append(*conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})
}
