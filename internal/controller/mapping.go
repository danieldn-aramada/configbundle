package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"

	armadav1 "github.com/armada/configbundle/api/v1"
	"github.com/armada/configbundle/bundle"
)

// LastAppliedSpecKey is the ConfigMap data key under which the most recent
// controller-applied manifest spec is stored. The reporter rehydrates its
// in-memory lastManifests from this on startup, eliminating the cold-start
// window where divergences can't be computed (because the in-memory map is
// empty until the next bundle dispatch).
const LastAppliedSpecKey = "last-applied-spec.yaml"

// ParseMapping is a thin alias for bundle.ParseMappingPayload, preserving the
// existing call-site name. The wire type and resolution semantics live in the
// shared bundle package so cb-bundler (producer) and cb-controller (consumer)
// import the same Go type — no per-side hand-rolled JSON parsing.
func ParseMapping(b []byte) (*bundle.MappingPayload, error) {
	return bundle.ParseMappingPayload(b)
}

// MappingConfigMapName returns the name of the mapping ConfigMap for a ConfigBundle.
func MappingConfigMapName(cbName string) string {
	return cbName + "-mapping"
}

// writeMappingConfigMap creates or updates the mapping ConfigMap for a ConfigBundle.
// The ConfigMap is owned by the CR so K8s GC deletes it when the CR is deleted.
// Wrapped in RetryOnConflict so a racing writer (or owner-reference reconciler)
// bumping the resourceVersion mid-update doesn't drop the mapping.
func writeMappingConfigMap(ctx context.Context, c client.Client, namespace, cbName, digest string, raw []byte, ownerRef metav1.OwnerReference) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      MappingConfigMapName(cbName),
				Namespace: namespace,
			},
		}
		_, err := controllerutil.CreateOrUpdate(ctx, c, cm, func() error {
			cm.Labels = map[string]string{
				"armada.ai/configbundle": cbName,
				"armada.ai/component":    "mapping",
			}
			cm.OwnerReferences = []metav1.OwnerReference{ownerRef}
			// Merge — the CM also carries LastAppliedSpecKey written by the
			// manifest dispatch path. Replacing the whole map would wipe it.
			if cm.Data == nil {
				cm.Data = map[string]string{}
			}
			cm.Data["digest"] = digest
			cm.Data["mapping.json"] = string(raw)
			return nil
		})
		return err
	})
}

// readMappingConfigMap reads the mapping ConfigMap for a ConfigBundle.
// Returns a not-found error if no ConfigMap exists yet.
func readMappingConfigMap(ctx context.Context, c client.Client, namespace, cbName string) (*bundle.MappingPayload, error) {
	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{
		Name:      MappingConfigMapName(cbName),
		Namespace: namespace,
	}, &cm); err != nil {
		return nil, err
	}
	raw := cm.Data["mapping.json"]
	if raw == "" {
		return nil, fmt.Errorf("mapping ConfigMap %s/%s has no mapping.json key", namespace, MappingConfigMapName(cbName))
	}
	return ParseMapping([]byte(raw))
}

// writeLastAppliedSpec persists the controller-applied manifest spec to the
// per-CR ConfigMap under LastAppliedSpecKey. The CM is shared with the mapping
// layer — adding/updating a key here doesn't disturb mapping.json or digest if
// they were written earlier. The OwnerReference ties lifecycle to the CR.
//
// The reporter rehydrates lastManifests from this on controller startup so a
// fresh process doesn't lose its intent baseline. Without persistence, every
// controller restart opens a recovery-required window until the next bundle
// dispatches — divergences would either not report or accidentally wipe orb's
// state (when extractOverrides returns nil from a missing baseline).
func writeLastAppliedSpec(ctx context.Context, c client.Client, namespace, cbName string, spec armadav1.ConfigBundleSpec) error {
	yamlBytes, err := yaml.Marshal(spec)
	if err != nil {
		return fmt.Errorf("marshal spec: %w", err)
	}
	// Fetch the CR for the OwnerReference UID. The apply that produced this
	// spec has already succeeded at this point, so the CR exists.
	var cb armadav1.ConfigBundle
	if err := c.Get(ctx, types.NamespacedName{Name: cbName, Namespace: namespace}, &cb); err != nil {
		return fmt.Errorf("get ConfigBundle for ownerRef: %w", err)
	}
	ownerRef := metav1.OwnerReference{
		APIVersion:         armadav1.GroupVersion.String(),
		Kind:               "ConfigBundle",
		Name:               cb.Name,
		UID:                cb.UID,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      MappingConfigMapName(cbName),
				Namespace: namespace,
			},
		}
		_, err := controllerutil.CreateOrUpdate(ctx, c, cm, func() error {
			if cm.Labels == nil {
				cm.Labels = map[string]string{}
			}
			cm.Labels["armada.ai/configbundle"] = cbName
			cm.Labels["armada.ai/component"] = "mapping"
			cm.OwnerReferences = []metav1.OwnerReference{ownerRef}
			if cm.Data == nil {
				cm.Data = map[string]string{}
			}
			cm.Data[LastAppliedSpecKey] = string(yamlBytes)
			return nil
		})
		return err
	})
}

// readLastAppliedSpec loads the spec persisted by writeLastAppliedSpec. Returns
// (nil, nil) when the ConfigMap doesn't exist or the key is absent — caller
// treats this as "no baseline known," same as the in-memory cold-start case.
func readLastAppliedSpec(ctx context.Context, c client.Client, namespace, cbName string) (*armadav1.ConfigBundleSpec, error) {
	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Name: MappingConfigMapName(cbName), Namespace: namespace}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get ConfigMap: %w", err)
	}
	raw, ok := cm.Data[LastAppliedSpecKey]
	if !ok || raw == "" {
		return nil, nil
	}
	var spec armadav1.ConfigBundleSpec
	if err := yaml.Unmarshal([]byte(raw), &spec); err != nil {
		return nil, fmt.Errorf("unmarshal spec: %w", err)
	}
	return &spec, nil
}
