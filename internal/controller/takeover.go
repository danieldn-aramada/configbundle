package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	armadav1 "github.com/armada/configbundle/api/v1"
)

// controllerFieldManager is the field manager string used by every cb-controller
// Apply call (normal pass and takeover pass). Centralized so the release pass
// can recognize "self" entries and skip them.
const controllerFieldManager = "configbundle-controller"

// processTakeover runs the second pass of the consume pipeline. For each takeover
// entry, it injects the controller-intended value for the targeted field into
// patchSpec (which is the admin-omitted spec already prepared by applyManifest),
// then applies the resulting spec with ForceOwnership. Force only effectively
// wrests the takeover-target fields — every other field in patchSpec is already
// owned by configbundle-controller (or absent because admin owns it).
//
// This runs after the normal SSA apply and regardless of whether that apply
// succeeded (per ADR-006). It uses the SAME field manager as the normal apply
// so controller ownership is preserved consistently across both passes.
func (s *ConsumeServer) processTakeover(ctx context.Context, fullSpec armadav1.ConfigBundleSpec, patchSpec *armadav1.ConfigBundleSpec) error {
	if len(fullSpec.Takeover) == 0 {
		return nil
	}

	logger := log.FromContext(ctx).WithName("takeover")
	var errs []error

	// Inject each takeover target into patchSpec. Failures here are per-entry —
	// continue processing the rest. Apply happens once at the end.
	for _, entry := range fullSpec.Takeover {
		if err := injectTakeoverField(patchSpec, fullSpec, entry); err != nil {
			logger.Error(err, "takeover entry failed",
				"serverOrbId", entry.ServerOrbID, "field", entry.Field, "orbId", entry.OrbID)
			errs = append(errs, err)
			continue
		}
		logger.Info("takeover queued",
			"serverOrbId", entry.ServerOrbID, "field", entry.Field, "orbId", entry.OrbID)
	}

	if len(errs) == len(fullSpec.Takeover) {
		// All entries failed before reaching the apply — nothing left to do.
		return fmt.Errorf("%d of %d takeover entries failed", len(errs), len(fullSpec.Takeover))
	}

	apply := &armadav1.ConfigBundle{
		TypeMeta: metav1.TypeMeta{
			APIVersion: armadav1.GroupVersion.String(),
			Kind:       "ConfigBundle",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      fullSpec.Datacenter,
			Namespace: s.namespace,
		},
		Spec: *patchSpec,
	}
	if err := s.Client.Patch(ctx, apply, client.Apply,
		client.FieldOwner(controllerFieldManager),
		client.ForceOwnership,
	); err != nil {
		return fmt.Errorf("takeover apply: %w", err)
	}

	// Release stale managedFields claims on takeover-target fields held by
	// other managers (typically local:<id>). K8s SSA's ForceOwnership only
	// transfers ownership when there's a VALUE conflict; on accept-divergence
	// the controller-intent now matches the local override, so no conflict
	// fires and the local manager's claim persists. Orbital's divergence model
	// requires the local claim to be released on both accept and reject — only
	// `ignore` retains local ownership. See docs/decisions/008-managedfields-release.md.
	if err := s.releaseOtherClaims(ctx, fullSpec); err != nil {
		logger.Error(err, "release of stale managedFields claims failed; takeover apply itself succeeded")
		// Non-fatal: the takeover apply succeeded, values are correct, only the
		// managedFields cleanup is incomplete. Reporting this as a hard error
		// would mask the successful value transfer.
	}

	if len(errs) > 0 {
		return fmt.Errorf("%d of %d takeover entries failed", len(errs), len(fullSpec.Takeover))
	}
	return nil
}

// releaseOtherClaims completes ownership transfer on takeover-target fields
// using K8s SSA's documented "transferring ownership between managers"
// protocol: the controller's force-conflicts Apply (just above) already wrote
// the same value as the local override, so both managers co-own. This step
// re-issues an SSA as each other manager with the takeover-target leaves
// OMITTED — release-on-omit then strips just those claims, leaving the
// manager's other claims intact (so Ignore-resolved and pending-divergence
// fields they still legitimately own are preserved).
//
// Mechanism (per https://kubernetes.io/docs/reference/using-api/server-side-apply/#transferring-ownership-between-managers):
//
//  1. Manager A (local:admin) owns field F.
//  2. Manager B (configbundle-controller) Applies F with the same value →
//     shared ownership.
//  3. Manager A re-Applies a body that omits F → SSA releases A's claim on F.
//     A's other claims (fields still in the new body with current values)
//     persist.
//  4. B is now sole owner of F.
//
// Steps 1-2 happened earlier in processTakeover. This function runs step 3
// for every non-self manager that claims any takeover target. The Apply body
// is reconstructed from live spec values at the paths the manager currently
// claims, minus the takeover targets.
//
// We Apply as field-owner=<that manager's name>. K8s does not authenticate
// the FieldManager string — the controller's ServiceAccount can submit Apply
// requests on behalf of any manager name. This is by design and is the
// recommended pattern when a controller mediates ownership transfer.
func (s *ConsumeServer) releaseOtherClaims(ctx context.Context, fullSpec armadav1.ConfigBundleSpec) error {
	if len(fullSpec.Takeover) == 0 {
		return nil
	}

	var cb armadav1.ConfigBundle
	if err := s.Client.Get(ctx, types.NamespacedName{
		Name:      fullSpec.Datacenter,
		Namespace: s.namespace,
	}, &cb); err != nil {
		return fmt.Errorf("re-fetch CR for managedFields read: %w", err)
	}

	// Marshal live spec for value lookup during reconstruction.
	rawSpec, err := json.Marshal(cb.Spec)
	if err != nil {
		return fmt.Errorf("marshal live spec: %w", err)
	}
	var specMap map[string]any
	if err := json.Unmarshal(rawSpec, &specMap); err != nil {
		return fmt.Errorf("unmarshal live spec: %w", err)
	}

	// Index takeover targets: serverOrbId -> set of field names.
	exclude := make(map[string]map[string]bool)
	for _, te := range fullSpec.Takeover {
		if exclude[te.ServerOrbID] == nil {
			exclude[te.ServerOrbID] = map[string]bool{}
		}
		exclude[te.ServerOrbID][te.Field] = true
	}

	logger := log.FromContext(ctx).WithName("takeover")
	for _, mf := range cb.ManagedFields {
		if mf.Manager == controllerFieldManager {
			continue // self — controller's own claims stay
		}
		if mf.Subresource == "status" {
			continue // status writers (K8s "main") don't touch spec
		}
		if mf.FieldsV1 == nil {
			continue
		}
		var owned map[string]any
		if err := json.Unmarshal(mf.FieldsV1.Raw, &owned); err != nil {
			continue
		}
		specOwned, _ := owned["f:spec"].(map[string]any)
		if specOwned == nil {
			continue
		}

		newSpec, touched := reconstructApplyExcluding(specMap, specOwned, exclude)
		if !touched {
			continue // this manager doesn't claim any takeover targets
		}

		// Use unstructured so the Apply body contains EXACTLY the keys we put
		// into newSpec — nothing else. A typed armadav1.ConfigBundle would
		// serialize zero-value `spec.datacenter` (json tag has no omitempty),
		// which would make local:admin claim a field cb-controller already
		// owns and the Apply would fail with a 409 conflict. With unstructured,
		// only the manager's actually-claimed fields appear in the request body,
		// and SSA's release-on-omit handles the rest.
		//
		// Omit "spec" entirely when newSpec is empty. Including spec:{} would
		// make SSA record this manager as claiming the spec object itself
		// (f:spec: {} in managedFields), so kubectl --show-managed-fields keeps
		// reporting the manager even though every leaf was released. Omitting
		// spec lets release-on-omit drop the f:spec claim too — zero residual
		// ownership, manager entry disappears from managedFields.
		applyObj := map[string]any{
			"apiVersion": armadav1.GroupVersion.String(),
			"kind":       "ConfigBundle",
			"metadata": map[string]any{
				"name":      fullSpec.Datacenter,
				"namespace": s.namespace,
			},
		}
		if len(newSpec) > 0 {
			applyObj["spec"] = newSpec
		}
		apply := &unstructured.Unstructured{Object: applyObj}
		if err := s.Client.Patch(ctx, apply, client.Apply,
			client.FieldOwner(mf.Manager),
		); err != nil {
			logger.Error(err, "release-as-manager apply failed", "manager", mf.Manager)
			continue
		}
		logger.Info("released claims via SSA-as-manager", "manager", mf.Manager)
	}
	return nil
}

// reconstructApplyExcluding builds a spec subtree containing only the values
// the manager currently claims (sourced from specMap), EXCLUDING any leaf
// whose path matches a takeover target. Returns (output, touchedExcluded);
// the second return is true iff at least one excluded path was present in the
// manager's claims (caller skips the Apply otherwise — nothing to release).
//
// specOwned is the manager's fieldsV1 sub-tree under "f:spec". The returned
// map is the corresponding spec content with takeover-target leaves removed.
func reconstructApplyExcluding(
	specMap map[string]any,
	specOwned map[string]any,
	excludeByServer map[string]map[string]bool,
) (map[string]any, bool) {
	out := map[string]any{}
	touched := false

	for ownedKey, ownedVal := range specOwned {
		if ownedKey == "." {
			continue // existence marker, no value to copy
		}
		if !strings.HasPrefix(ownedKey, "f:") {
			continue
		}
		field := strings.TrimPrefix(ownedKey, "f:")
		ownedSub, _ := ownedVal.(map[string]any)
		srcVal, present := specMap[field]
		if !present {
			continue
		}

		if field == "servers" {
			// Special-case the list-map. ownedSub is keyed by k:{"orbId":"..."}.
			srcList, ok := srcVal.([]any)
			if !ok {
				continue
			}
			rebuilt, anyTouched := reconstructServerList(srcList, ownedSub, excludeByServer)
			if anyTouched {
				touched = true
			}
			if len(rebuilt) > 0 {
				out[field] = rebuilt
			}
			continue
		}

		// Non-server top-level field. If the manager claims a leaf, include the
		// value as-is. (No takeover targets reach this branch — takeover is
		// scoped to per-server fields by design.)
		if len(ownedSub) == 0 {
			out[field] = srcVal
		} else {
			// Partial sub-claim — descend.
			if srcMap, ok := srcVal.(map[string]any); ok {
				sub := reconstructMapExcluding(srcMap, ownedSub)
				if len(sub) > 0 {
					out[field] = sub
				}
			}
		}
	}
	return out, touched
}

// reconstructServerList walks a list of server entries and produces a
// reconstructed list containing only the entries the manager claims, each
// with that manager's claimed leaf values, EXCLUDING takeover-target fields
// on a per-server basis.
//
// The reconstructed entry always carries the listMapKey (orbId) so SSA can
// match the existing element on Apply. ServiceTag is included when the
// manager claimed it (or the entry, identified by listMapKey) so CRD
// Required-field validation succeeds; ServerSpec has both orbId and
// serviceTag marked +kubebuilder:validation:Required.
func reconstructServerList(
	srcList []any,
	ownedServers map[string]any,
	excludeByServer map[string]map[string]bool,
) ([]any, bool) {
	out := []any{}
	touched := false
	for keyStr, keyOwned := range ownedServers {
		if !strings.HasPrefix(keyStr, "k:") {
			continue
		}
		var keyMap map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(keyStr, "k:")), &keyMap); err != nil {
			continue
		}
		orbID, _ := keyMap["orbId"].(string)
		if orbID == "" {
			continue
		}
		// Find the matching server in the live spec.
		var srcEntry map[string]any
		for _, item := range srcList {
			e, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if id, _ := e["orbId"].(string); id == orbID {
				srcEntry = e
				break
			}
		}
		if srcEntry == nil {
			continue
		}
		keyOwnedMap, _ := keyOwned.(map[string]any)

		// Build the reconstructed server entry. orbId is the listMapKey — must
		// be included when we want SSA to match an existing list element to
		// preserve its other claims. We do NOT inject serviceTag here: CRD
		// Required-field validation runs against the merged final state of the
		// object, not the individual Apply body, and cb-controller's own field
		// manager has already established serviceTag in the object.
		newEntry := map[string]any{"orbId": orbID}

		excludedFields := excludeByServer[orbID]
		entryTouched := reconstructServerEntry(newEntry, srcEntry, keyOwnedMap, excludedFields)
		if entryTouched {
			touched = true
		}
		// Full-release semantics: if Accept/Reject consumed ALL of this
		// manager's leaves on the server, newEntry has only orbId. Omitting
		// the entry from the release body lets SSA's release-on-omit strip
		// the manager's claims on the listMapKey + entry-presence marker
		// too — leaving zero residual ownership for this server. Without
		// this, kubectl --show-managed-fields keeps reporting the manager
		// even though every meaningful claim was released. orbital's
		// semantic is "Accept/Reject release; Ignore preserves" — that has
		// to mean nothing remains for fully-resolved servers.
		if len(newEntry) == 1 {
			continue
		}
		out = append(out, newEntry)
	}
	return out, touched
}

// reconstructServerEntry walks a single server entry's claimed fields and
// copies their values into newEntry, skipping any field in excludedFields.
// Returns true iff at least one excluded field was found among the manager's
// claims.
func reconstructServerEntry(
	newEntry map[string]any,
	srcEntry map[string]any,
	ownedEntry map[string]any,
	excludedFields map[string]bool,
) bool {
	touched := false
	for ownedKey, ownedVal := range ownedEntry {
		if ownedKey == "." || ownedKey == "f:orbId" || ownedKey == "f:serviceTag" {
			continue // already in newEntry (orbId, serviceTag)
		}
		if !strings.HasPrefix(ownedKey, "f:") {
			continue
		}
		field := strings.TrimPrefix(ownedKey, "f:")
		ownedSub, _ := ownedVal.(map[string]any)
		srcVal, present := srcEntry[field]
		if !present {
			continue
		}

		if field == "idrac" {
			// Nested struct — recurse, scoping the exclusion to idrac fields.
			srcIdrac, ok := srcVal.(map[string]any)
			if !ok {
				continue
			}
			newIdrac, anyTouched := reconstructIdracExcluding(srcIdrac, ownedSub, excludedFields)
			if anyTouched {
				touched = true
			}
			if len(newIdrac) > 0 {
				newEntry["idrac"] = newIdrac
			}
			continue
		}

		// Top-level server leaf. Check exclusion.
		if excludedFields[field] {
			touched = true
			continue // skip — release this claim
		}
		newEntry[field] = srcVal
	}
	return touched
}

// reconstructIdracExcluding builds a new idrac map containing only the leaves
// the manager claims, EXCLUDING any field in excludedFields. Returns
// (output, touchedExcluded).
func reconstructIdracExcluding(
	srcIdrac map[string]any,
	ownedIdrac map[string]any,
	excludedFields map[string]bool,
) (map[string]any, bool) {
	out := map[string]any{}
	touched := false
	for ownedKey := range ownedIdrac {
		if !strings.HasPrefix(ownedKey, "f:") {
			continue
		}
		field := strings.TrimPrefix(ownedKey, "f:")
		if excludedFields[field] {
			touched = true
			continue // release this claim
		}
		if val, ok := srcIdrac[field]; ok {
			out[field] = val
		}
	}
	return out, touched
}

// reconstructMapExcluding is the generic helper for nested structs that have
// no special list-map handling. Currently unused by the takeover-target paths
// (which are all under spec.servers[*].idrac), but kept so reconstructApplyExcluding
// can recurse into future struct fields without special-casing each one.
func reconstructMapExcluding(srcMap map[string]any, ownedMap map[string]any) map[string]any {
	out := map[string]any{}
	for ownedKey, ownedVal := range ownedMap {
		if !strings.HasPrefix(ownedKey, "f:") {
			continue
		}
		field := strings.TrimPrefix(ownedKey, "f:")
		ownedSub, _ := ownedVal.(map[string]any)
		srcVal, ok := srcMap[field]
		if !ok {
			continue
		}
		if len(ownedSub) == 0 {
			out[field] = srcVal
		} else if subMap, ok := srcVal.(map[string]any); ok {
			out[field] = reconstructMapExcluding(subMap, ownedSub)
		}
	}
	return out
}

// injectTakeoverField sets the takeover-target field on patchSpec, copying the
// value from fullSpec (the orbital intent). If the target server is missing
// from patchSpec, it's added from fullSpec.
func injectTakeoverField(patchSpec *armadav1.ConfigBundleSpec, fullSpec armadav1.ConfigBundleSpec, entry armadav1.TakeoverEntry) error {
	var srcServer *armadav1.ServerSpec
	for i := range fullSpec.Servers {
		if fullSpec.Servers[i].OrbID == entry.ServerOrbID {
			srcServer = &fullSpec.Servers[i]
			break
		}
	}
	if srcServer == nil {
		return fmt.Errorf("server with orbId %q not found in spec", entry.ServerOrbID)
	}

	var dstServer *armadav1.ServerSpec
	for i := range patchSpec.Servers {
		if patchSpec.Servers[i].OrbID == entry.ServerOrbID {
			dstServer = &patchSpec.Servers[i]
			break
		}
	}
	if dstServer == nil {
		// Server entry was fully admin-owned and got dropped from patchSpec.
		// Append a minimal stub carrying just identity — only the takeover field
		// will be populated below. ServiceTag is included for CRD Required-field
		// validation; SSA listMapKey matches on orbId.
		patchSpec.Servers = append(patchSpec.Servers, armadav1.ServerSpec{
			OrbID:      srcServer.OrbID,
			ServiceTag: srcServer.ServiceTag,
		})
		dstServer = &patchSpec.Servers[len(patchSpec.Servers)-1]
	}

	if err := setFieldOnServer(dstServer, srcServer, entry.Field); err != nil {
		return fmt.Errorf("set takeover field: %w", err)
	}
	return nil
}

// setFieldOnServer copies a single field (by JSON tag name) from src to dst.
// It checks ServerSpec top-level fields first, then IdracSpec fields.
// Uses reflection so adding new fields to the CRD types is sufficient —
// no switch cases to maintain.
func setFieldOnServer(dst, src *armadav1.ServerSpec, field string) error {
	if copyStructFieldByJSONTag(reflect.ValueOf(dst).Elem(), reflect.ValueOf(src).Elem(), field) {
		return nil
	}
	if copyStructFieldByJSONTag(reflect.ValueOf(&dst.Idrac).Elem(), reflect.ValueOf(&src.Idrac).Elem(), field) {
		return nil
	}
	return fmt.Errorf("unknown takeover field %q", field)
}

// copyStructFieldByJSONTag finds a field on dst whose json tag matches jsonName,
// copies the value from the corresponding field on src, and returns true.
// Returns false if no matching field is found.
func copyStructFieldByJSONTag(dst, src reflect.Value, jsonName string) bool {
	t := dst.Type()
	for i := 0; i < t.NumField(); i++ {
		tag := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
		if tag == jsonName {
			dst.Field(i).Set(src.Field(i))
			return true
		}
	}
	return false
}
