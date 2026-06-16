package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	armadav1 "github.com/armada/configbundle/api/v1"
	"github.com/armada/configbundle/bundle"
)

// DivergenceReporter is a controller-runtime reconciler that watches ConfigBundle CRs
// for changes in "local:*" field managers and reports divergences to orb's intake.
type DivergenceReporter struct {
	Client            client.Client
	HTTPClient        *http.Client
	intakeURL         string
	namespace         string
	debounceWindow    time.Duration
	heartbeatInterval time.Duration
	enabled           bool

	mu             sync.Mutex
	lastEventAt    map[types.NamespacedName]time.Time
	lastPostedHash map[types.NamespacedName][32]byte

	lastManifestsMu sync.RWMutex
	lastManifests   map[string]armadav1.ConfigBundleSpec
}

// DivergenceReporterOption configures a DivergenceReporter.
type DivergenceReporterOption func(*DivergenceReporter)

func WithDivergenceIntakeURL(url string) DivergenceReporterOption {
	return func(r *DivergenceReporter) { r.intakeURL = url }
}

func WithDivergenceNamespace(ns string) DivergenceReporterOption {
	return func(r *DivergenceReporter) { r.namespace = ns }
}

func WithDivergenceDebounce(d time.Duration) DivergenceReporterOption {
	return func(r *DivergenceReporter) { r.debounceWindow = d }
}

// WithDivergenceHeartbeat sets the periodic re-send interval. On each tick the
// reporter lists ConfigBundles, clears the per-CR posted-hash cache, and triggers
// a reconcile. Bounds the staleness window when orb's persistent state is wiped
// (the reporter's in-memory hash cache would otherwise dedup the post forever
// because no managedFields event fires to invalidate it). 0 disables.
func WithDivergenceHeartbeat(d time.Duration) DivergenceReporterOption {
	return func(r *DivergenceReporter) { r.heartbeatInterval = d }
}

func WithDivergenceEnabled(enabled bool) DivergenceReporterOption {
	return func(r *DivergenceReporter) { r.enabled = enabled }
}

func NewDivergenceReporter(c client.Client, opts ...DivergenceReporterOption) *DivergenceReporter {
	r := &DivergenceReporter{
		Client:            c,
		HTTPClient:        &http.Client{Timeout: 10 * time.Second},
		intakeURL:         "http://orb:8010/api/v1/divergence",
		namespace:         "configbundle-system",
		debounceWindow:    5 * time.Second,
		heartbeatInterval: 5 * time.Minute,
		enabled:           false,
		lastEventAt:       make(map[types.NamespacedName]time.Time),
		lastPostedHash:    make(map[types.NamespacedName][32]byte),
		lastManifests:     make(map[string]armadav1.ConfigBundleSpec),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// OverrideEntry is one orbital-native divergence entry in the intake payload.
type OverrideEntry struct {
	OrbID         string      `json:"orbId"`
	Field         string      `json:"field"`
	Type          string      `json:"type"`
	IntendedValue interface{} `json:"intendedValue"`
	OverrideValue interface{} `json:"overrideValue"`
	Who           string      `json:"who"`
	When          string      `json:"when"`
}

// DivergencePayload is the full intake payload sent to orb.
type DivergencePayload struct {
	Overrides []OverrideEntry `json:"overrides"`
}

func (r *DivergenceReporter) postToOrb(ctx context.Context, payload DivergencePayload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.intakeURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST divergence: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("orb returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// extractOverrides walks managedFields on the ConfigBundle CR, finds fields
// owned by any "local:<id>" field manager, translates K8s paths to orbital-native
// entries via the mapping, and returns the divergence set. Each override carries
// the actual field-manager string (e.g. "local:daniel") in Who.
func (r *DivergenceReporter) extractOverrides(cb *armadav1.ConfigBundle, mapping *bundle.MappingPayload, lastManifest armadav1.ConfigBundleSpec) []OverrideEntry {
	adminPaths := extractAdminPaths(cb.ManagedFields)
	if len(adminPaths) == 0 {
		return nil
	}

	var overrides []OverrideEntry
	for _, ap := range adminPaths {
		intended := resolveValue(lastManifest, ap.path)
		override := resolveValue(cb.Spec, ap.path)

		// Skip if the field isn't part of the current manifest's intent.
		// This happens when orbital has resolved a prior divergence as `ignore`:
		// the bundler omits the field from the cb-manifest entirely, cb-controller
		// releases its claim, and only local:<id> remains as owner. Reporting it
		// again would re-trigger the same ignore decision every tick (loop).
		// A typed-nil pointer wrapped in interface{} is NOT == nil, so use the helper.
		if intendedAbsent(intended) {
			continue
		}

		if reflect.DeepEqual(intended, override) {
			continue
		}

		orbID, field, typeName, err := mapping.Resolve(ap.path)
		if err != nil {
			// Path doesn't resolve — skip (e.g. metadata fields, or missing mapping entry).
			continue
		}

		overrides = append(overrides, OverrideEntry{
			OrbID:         orbID,
			Field:         field,
			Type:          typeName,
			IntendedValue: intended,
			OverrideValue: override,
			Who:           ap.manager,
			When:          ap.when.Format(time.RFC3339),
		})
	}
	return overrides
}

// SetLastManifest records the last-applied manifest for a ConfigBundle so the
// reporter can compare current values against intended values.
func (r *DivergenceReporter) SetLastManifest(name string, spec armadav1.ConfigBundleSpec) {
	r.lastManifestsMu.Lock()
	defer r.lastManifestsMu.Unlock()
	r.lastManifests[name] = spec
}

// contentHash returns a stable SHA-256 hash of a DivergencePayload by sorting
// overrides before hashing so order-invariant payloads produce the same hash.
func contentHash(payload DivergencePayload) [32]byte {
	sorted := make([]OverrideEntry, len(payload.Overrides))
	copy(sorted, payload.Overrides)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].OrbID != sorted[j].OrbID {
			return sorted[i].OrbID < sorted[j].OrbID
		}
		return sorted[i].Field < sorted[j].Field
	})
	b, _ := json.Marshal(DivergencePayload{Overrides: sorted})
	return sha256.Sum256(b)
}

type adminPath struct {
	path    string
	when    time.Time
	manager string // field manager that owns this path (e.g. "local:daniel")
}

// intendedAbsent reports whether a value returned from resolveValue indicates
// "field not present in the manifest." Because IdracSpec/ServerSpec leaves are
// pointers with omitempty (ADR-007), an absent field decodes as a typed-nil
// pointer wrapped in an interface{}. The standard `== nil` check returns false
// for typed-nil interfaces, so we reflect to detect them.
func intendedAbsent(v interface{}) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Map, reflect.Slice:
		return rv.IsNil()
	}
	return false
}

// warnNonConformingManagers logs a warning for each field manager that owns
// spec.* on a ConfigBundle CR but doesn't follow the `local:<id>` convention.
// Such overrides are silently dropped by extractAdminPaths and omitAdminOwnedFields
// — this surface makes the silence visible at the runtime where the bug bites.
//
// Skipped silently:
//   - `configbundle-controller` (us)
//   - managers that don't own spec.* (e.g. status-only writers, GC)
//
// Warned (and still dropped):
//   - any other manager that owns a spec.* field (e.g. `daniel`, `kubectl-edit`,
//     `admin:bob`) — operator likely forgot the `local:` prefix
//
// See docs/runbooks/divergence-e2e-local.md (orbital repo).
func warnNonConformingManagers(logger logrLogger, cbName string, managedFields []metav1.ManagedFieldsEntry) {
	for _, entry := range managedFields {
		if entry.Manager == "configbundle-controller" || entry.FieldsV1 == nil {
			continue
		}
		if strings.HasPrefix(entry.Manager, "local:") {
			continue
		}
		var fields map[string]interface{}
		if err := json.Unmarshal(entry.FieldsV1.Raw, &fields); err != nil {
			continue
		}
		if _, ownsSpec := fields["f:spec"]; !ownsSpec {
			continue
		}
		logger.Info("non-conforming field manager owns spec — override will not be reported; use --field-manager=local:<id>",
			"configbundle", cbName,
			"manager", entry.Manager,
		)
	}
}

// logrLogger is the subset of logr.Logger we need. Defined locally to avoid an
// explicit logr import here; controller-runtime's log package returns this shape.
type logrLogger interface {
	Info(msg string, keysAndValues ...any)
}

// extractAdminPaths parses managedFields to find all leaf field paths owned by
// any field manager whose name starts with "local:". Returns one adminPath per
// owned leaf, carrying the manager identity so callers can attribute overrides.
// Paths are formatted as: spec.servers[orbId=Y].idrac.sshEnabled (orbId is the listMapKey).
func extractAdminPaths(managedFields []metav1.ManagedFieldsEntry) []adminPath {
	var paths []adminPath
	for _, entry := range managedFields {
		if !strings.HasPrefix(entry.Manager, "local:") || entry.FieldsV1 == nil {
			continue
		}
		when := time.Time{}
		if entry.Time != nil {
			when = entry.Time.Time
		}

		var fields map[string]interface{}
		if err := json.Unmarshal(entry.FieldsV1.Raw, &fields); err != nil {
			continue
		}

		walkFields(fields, "", when, entry.Manager, &paths)
	}
	return paths
}

// walkFields recursively walks the fieldsV1 structure and emits leaf paths.
func walkFields(node map[string]interface{}, prefix string, when time.Time, manager string, out *[]adminPath) {
	for key, val := range node {
		path := fieldKeyToPath(prefix, key)
		if path == "" {
			continue
		}

		child, ok := val.(map[string]interface{})
		if !ok || len(child) == 0 {
			*out = append(*out, adminPath{path: path, when: when, manager: manager})
			continue
		}

		// Check if all children are leaf markers (empty maps or non-maps).
		// If so, this is still a leaf set by the manager.
		hasSubfields := false
		for _, v := range child {
			if m, ok := v.(map[string]interface{}); ok && len(m) > 0 {
				hasSubfields = true
				break
			}
		}
		if hasSubfields {
			walkFields(child, path, when, manager, out)
		} else {
			// Leaf — all children are empty maps (field markers).
			for childKey := range child {
				leafPath := fieldKeyToPath(path, childKey)
				if leafPath != "" {
					*out = append(*out, adminPath{path: leafPath, when: when, manager: manager})
				}
			}
		}
	}
}

// fieldKeyToPath converts a fieldsV1 key (e.g. "f:hostname", "k:{\"serviceTag\":\"X\"}")
// into a dot-separated path segment appended to prefix.
func fieldKeyToPath(prefix, key string) string {
	switch {
	case strings.HasPrefix(key, "f:"):
		field := strings.TrimPrefix(key, "f:")
		if prefix == "" {
			return field
		}
		return prefix + "." + field
	case strings.HasPrefix(key, "k:"):
		// Map key — e.g. k:{"serviceTag":"3RK3V64"}
		raw := strings.TrimPrefix(key, "k:")
		var keyMap map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &keyMap); err != nil {
			return ""
		}
		// Build selector like [serviceTag=3RK3V64]
		for k, v := range keyMap {
			selector := fmt.Sprintf("[%s=%v]", k, v)
			if prefix == "" {
				return selector
			}
			return prefix + selector
		}
		return ""
	default:
		return ""
	}
}

// resolveValue reads a value from a ConfigBundleSpec given a K8s field path.
// Paths start with "spec." (from managedFields) — the prefix is stripped since
// the caller passes the spec object directly.
// Returns nil if the path cannot be resolved.
func resolveValue(spec armadav1.ConfigBundleSpec, path string) interface{} {
	path = strings.TrimPrefix(path, "spec.")
	parts := splitPath(path)
	if len(parts) == 0 {
		return nil
	}

	var current interface{} = spec
	for _, part := range parts {
		if part.selector != "" {
			// Array lookup by listMapKey (orbId).
			servers, ok := current.([]armadav1.ServerSpec)
			if !ok {
				return nil
			}
			found := false
			for _, s := range servers {
				if s.OrbID == part.selector {
					current = s
					found = true
					break
				}
			}
			if !found {
				return nil
			}
			continue
		}

		// Field access via JSON name matching
		current = getFieldByJSONName(current, part.field)
		if current == nil {
			return nil
		}
	}
	return current
}

type pathPart struct {
	field    string
	selector string // e.g. "colo:srv-001" for [orbId=colo:srv-001]
}

// splitPath splits a K8s field path into parts.
// "spec.servers[orbId=Y].idrac.sshEnabled" →
// [{field:"spec"}, {field:"servers", selector:"Y"}, {field:"idrac"}, {field:"sshEnabled"}]
func splitPath(path string) []pathPart {
	var parts []pathPart
	remaining := path
	for remaining != "" {
		// Find next dot or bracket
		dotIdx := strings.Index(remaining, ".")
		bracketIdx := strings.Index(remaining, "[")

		if bracketIdx >= 0 && (dotIdx < 0 || bracketIdx < dotIdx) {
			// There's a bracket before the next dot
			field := remaining[:bracketIdx]
			if field != "" {
				parts = append(parts, pathPart{field: field})
			}
			// Parse the selector
			endBracket := strings.Index(remaining, "]")
			if endBracket < 0 {
				break
			}
			selectorStr := remaining[bracketIdx+1 : endBracket]
			// Parse "serviceTag=X"
			eqIdx := strings.Index(selectorStr, "=")
			if eqIdx >= 0 {
				parts = append(parts, pathPart{selector: selectorStr[eqIdx+1:]})
			}
			remaining = remaining[endBracket+1:]
			if strings.HasPrefix(remaining, ".") {
				remaining = remaining[1:]
			}
		} else if dotIdx >= 0 {
			field := remaining[:dotIdx]
			if field != "" {
				parts = append(parts, pathPart{field: field})
			}
			remaining = remaining[dotIdx+1:]
		} else {
			if remaining != "" {
				parts = append(parts, pathPart{field: remaining})
			}
			break
		}
	}
	return parts
}

// getFieldByJSONName returns the value of a struct field matched by its json tag name.
func getFieldByJSONName(obj interface{}, name string) interface{} {
	v := reflect.ValueOf(obj)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		jsonName := strings.Split(tag, ",")[0]
		if jsonName == name {
			return v.Field(i).Interface()
		}
	}
	return nil
}
