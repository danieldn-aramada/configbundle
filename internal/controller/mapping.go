package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// MappingItem is one entry in the bundle's mapping layer. It pairs a K8s
// field-path prefix with the orbital orbId for the ConfigItem that owns
// fields under that prefix. The leaf segment of the matched path becomes
// the orbital field name at resolve time.
type MappingItem struct {
	Path  string `json:"path"`
	OrbID string `json:"orbId"`
	Type  string `json:"type,omitempty"`
}

// Mapping is the deserialized mapping.json layer for a single bundle.
type Mapping struct {
	BundleDigest string        `json:"bundleDigest"`
	Items        []MappingItem `json:"items"`

	// sortedItems is Items ordered by descending path length so prefix
	// matching can short-circuit on the first match. Built once on parse.
	sortedItems []MappingItem
}

// Resolve walks the longest-prefix MappingItem that prefixes the given path
// and returns the corresponding orbId, leaf field name, and orbital type.
// Returns an error when no prefix matches.
func (m *Mapping) Resolve(path string) (orbID, field, typeName string, err error) {
	if m == nil {
		return "", "", "", errors.New("nil mapping")
	}
	for _, item := range m.sortedItems {
		switch {
		case path == item.Path:
			return "", "", "", fmt.Errorf("path %q matches a ConfigItem boundary, not a field", path)
		case strings.HasPrefix(path, item.Path+"."):
			leaf := path[len(item.Path)+1:]
			if strings.ContainsAny(leaf, ".[") {
				return "", "", "", fmt.Errorf("matched prefix %q for path %q is too shallow; leaf %q is not a simple field name", item.Path, path, leaf)
			}
			return item.OrbID, leaf, item.Type, nil
		}
	}
	return "", "", "", fmt.Errorf("no mapping prefix matches path %q", path)
}

// ParseMapping deserializes a mapping JSON payload and validates it.
func ParseMapping(b []byte) (*Mapping, error) {
	var m Mapping
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("decode mapping: %w", err)
	}
	if len(m.Items) == 0 {
		return nil, errors.New("mapping has no items")
	}
	m.sortedItems = make([]MappingItem, len(m.Items))
	copy(m.sortedItems, m.Items)
	sort.SliceStable(m.sortedItems, func(i, j int) bool {
		return len(m.sortedItems[i].Path) > len(m.sortedItems[j].Path)
	})
	return &m, nil
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
			cm.Data = map[string]string{
				"digest":       digest,
				"mapping.json": string(raw),
			}
			return nil
		})
		return err
	})
}

// readMappingConfigMap reads the mapping ConfigMap for a ConfigBundle.
// Returns a not-found error if no ConfigMap exists yet.
func readMappingConfigMap(ctx context.Context, c client.Client, namespace, cbName string) (*Mapping, error) {
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
