package bundler

import (
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"

	"sigs.k8s.io/yaml"

	armadav1 "github.com/armada/configbundle/api/v1"
	"github.com/armada/configbundle/bundle"
)

// bundleRequest is the JSON body sent to POST /bundle.
// OrbID is the canonical DataCenter identifier (hash-indexed in DGraph,
// supports `eq` filters). Orbital always provides it for completed exports.
type bundleRequest struct {
	OrbID string `json:"orbId"`
}

// bundleResponse is the JSON object returned by POST /bundle.
type bundleResponse struct {
	Layers []bundleLayer `json:"layers"`
}

// bundleLayer is one element in the layers array.
type bundleLayer struct {
	MediaType string `json:"mediaType"`
	Data      string `json:"data"` // standard base64
}

// Handler handles POST /bundle for Orbital's enricher pipeline.
// It is stateless — all data is fetched from Orbital per request.
type Handler struct {
	Orbital     OrbitalQuerier
	Resolutions ResolutionQuerier // nil = skip takeover (e.g. in tests)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req bundleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("bundler: %s %s 400 invalid request body: %v", r.Method, r.URL.Path, err)
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.OrbID == "" {
		log.Printf("bundler: %s %s 400 missing orbId field", r.Method, r.URL.Path)
		http.Error(w, "orbId field is required", http.StatusBadRequest)
		return
	}

	log.Printf("bundler: POST /bundle orbId=%q — querying orbital", req.OrbID)
	results, err := h.Orbital.QueryDataCenter(r.Context(), req.OrbID)
	if err != nil {
		log.Printf("bundler: POST /bundle orbId=%q FAILED orbital query: %v", req.OrbID, err)
		http.Error(w, "orbital query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("bundler: POST /bundle orbId=%q got %d result(s) from orbital", req.OrbID, len(results))

	// No datacenter found — return empty response. Orbital treats this as success
	// with no configbundle layer in the artifact.
	if len(results) == 0 {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(bundleResponse{})
		return
	}

	spec := mapToSpec(results[0])

	// Query both takeover (accept+reject) and ignored resolutions. Both surface
	// as parallel directive lists in the manifest (spec.takeover / spec.ignored).
	// Field values stay in the spec for either list — cb-controller decides
	// per-field at reconcile time. Ignored fields keep their intent value so
	// the divergence-reporter can compare it against the local override and
	// continue surfacing the divergence.
	//
	// After ADR-011: orbId lookups walk spec.servers[].idracSettings.orbId
	// directly instead of consulting a mapping payload. The spec IS the
	// identity manifest.
	if h.Resolutions != nil {
		omissions, err := h.Resolutions.QueryOmissions(r.Context())
		if err != nil {
			http.Error(w, "query omissions: "+err.Error(), http.StatusInternalServerError)
			return
		}
		spec.Ignored = buildIgnored(omissions, spec.Servers)

		resolutions, err := h.Resolutions.QueryPendingForce(r.Context())
		if err != nil {
			http.Error(w, "query pending-force: "+err.Error(), http.StatusInternalServerError)
			return
		}
		spec.Takeover = buildTakeover(resolutions, spec.Servers)
	}

	yamlBytes, err := yaml.Marshal(spec)
	if err != nil {
		http.Error(w, "marshal manifest: "+err.Error(), http.StatusInternalServerError)
		return
	}

	resp := bundleResponse{
		Layers: []bundleLayer{
			{
				MediaType: bundle.MediaTypeManifest,
				Data:      base64.StdEncoding.EncodeToString(yamlBytes),
			},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// findServerByNestedOrbID returns the ServerSpec whose nested-entity orbId
// matches the given resolution orbId (e.g. resolution orbId
// "colo:GQK3V64-idrac" matches the server whose IdracSettings.OrbID ==
// "colo:GQK3V64-idrac"). Returns nil if no match.
//
// Replaces bundle.MappingPayload.ResolveByOrbID (deleted in ADR-011). The
// suffix convention ("<server>-idrac") is no longer encoded anywhere — instead
// every nested entity stores its own orbId directly on the CR, and the lookup
// is a direct walk.
func findServerByNestedOrbID(orbID string, servers []armadav1.ServerSpec) *armadav1.ServerSpec {
	for i := range servers {
		if servers[i].IdracSettings.OrbID == orbID {
			return &servers[i]
		}
		// Future: extend with other nested types here as they're added.
	}
	return nil
}

// buildTakeover translates pending accept/reject-resolutions into TakeoverEntry values.
// For each resolution, looks up the owning server by matching the resolution's orbId
// against each server's nested-entity orbIds (e.g. IdracSettings.OrbID).
// Resolutions whose orbId doesn't match any server are silently skipped —
// the resolution may belong to a different bundle or a stale entry.
func buildTakeover(resolutions []PendingForceResolution, servers []armadav1.ServerSpec) []armadav1.TakeoverEntry {
	if len(resolutions) == 0 {
		return nil
	}
	var entries []armadav1.TakeoverEntry
	for _, res := range resolutions {
		server := findServerByNestedOrbID(res.OrbID, servers)
		if server == nil {
			continue
		}
		entries = append(entries, armadav1.TakeoverEntry{
			OrbID:       res.OrbID,
			ServerOrbID: server.OrbID,
			Field:       res.Field,
		})
	}
	return entries
}

// buildIgnored translates ignore-resolutions into IgnoredEntry values. Same lookup
// model as buildTakeover. The intent VALUE for the field stays in spec.servers[N]
// so the divergence-reporter can continue surfacing the divergence; only the
// cb-controller's claim behavior changes (it bows out unconditionally for fields
// in this list).
//
// Resolutions whose orbId doesn't match any server are silently skipped —
// the resolution may belong to a different bundle or a stale entry.
func buildIgnored(omissions []Omission, servers []armadav1.ServerSpec) []armadav1.IgnoredEntry {
	if len(omissions) == 0 {
		return nil
	}
	var entries []armadav1.IgnoredEntry
	for _, om := range omissions {
		server := findServerByNestedOrbID(om.OrbID, servers)
		if server == nil {
			continue
		}
		entries = append(entries, armadav1.IgnoredEntry{
			OrbID:       om.OrbID,
			ServerOrbID: server.OrbID,
			Field:       om.Field,
		})
	}
	return entries
}

// mapToSpec maps a GraphQL DataCenterResult to a ConfigBundleSpec.
// Servers without a hostname or orbId are skipped — hostname is required by the
// CRD and orbId is the SSA listMapKey.
// IdracSettings fields are transferred via JSON round-trip: both
// IdracSettingsResult and armadav1.IdracSettingsSpec use identical json tags
// (including the OrbID added in ADR-011), so adding a field to both structs
// is sufficient — no field-by-field copy code to update.
func mapToSpec(dc DataCenterResult) armadav1.ConfigBundleSpec {
	spec := armadav1.ConfigBundleSpec{
		OrbID:      dc.OrbID,
		Datacenter: dc.Name,
	}
	for _, s := range dc.Servers {
		if s.Hostname == "" || s.OrbID == "" {
			continue
		}
		hostname := s.Hostname
		oobIP := ""
		if s.OobIP != nil {
			oobIP = s.OobIP.Address
		}
		srv := armadav1.ServerSpec{
			OrbID:      s.OrbID,
			ServiceTag: s.ServiceTag,
			Hostname:   &hostname,
			OobIP:      &oobIP,
		}
		if s.IdracSettings != nil {
			srv.IdracSettings = mapIdrac(s.IdracSettings)
		}
		spec.Servers = append(spec.Servers, srv)
	}
	for _, k := range dc.KubernetesClusters {
		if k.OrbID == "" {
			continue
		}
		cluster := armadav1.KubernetesClusterSpec{
			OrbID: k.OrbID,
		}
		if k.Name != "" {
			name := k.Name
			cluster.Name = &name
		}
		if k.Backup != nil && k.Backup.OrbID != "" {
			cluster.Backup = mapClusterBackup(k.Backup)
		}
		spec.KubernetesClusters = append(spec.KubernetesClusters, cluster)
	}
	return spec
}

// mapIdrac transfers IdracSettings fields via JSON round-trip. Works because
// IdracSettingsResult and armadav1.IdracSettingsSpec share identical json tag
// names, including the OrbID added in ADR-011.
func mapIdrac(src *IdracSettingsResult) armadav1.IdracSettingsSpec {
	var dst armadav1.IdracSettingsSpec
	b, err := json.Marshal(src)
	if err != nil {
		return dst
	}
	json.Unmarshal(b, &dst)
	return dst
}

// mapClusterBackup translates the GraphQL ClusterBackupResult into a
// ClusterBackupSpec. Null edges on the graph become nil pointer fields on
// the spec — a cluster with only etcd configured produces
// ClusterBackupSpec{OrbID: ..., Etcd: {...}} with Velero and S3Sync nil.
func mapClusterBackup(src *ClusterBackupResult) *armadav1.ClusterBackupSpec {
	dst := &armadav1.ClusterBackupSpec{OrbID: src.OrbID}
	if src.Etcd != nil && src.Etcd.OrbID != "" {
		dst.Etcd = &armadav1.EtcdBackupSpec{
			OrbID:    src.Etcd.OrbID,
			Enabled:  boolPtr(src.Etcd.Enabled),
			Schedule: stringPtrNonEmpty(src.Etcd.Schedule),
			Location: stringPtrNonEmpty(src.Etcd.Location),
		}
	}
	if src.Velero != nil && src.Velero.OrbID != "" {
		dst.Velero = &armadav1.VeleroBackupSpec{
			OrbID:    src.Velero.OrbID,
			Enabled:  boolPtr(src.Velero.Enabled),
			Schedule: stringPtrNonEmpty(src.Velero.Schedule),
			Location: stringPtrNonEmpty(src.Velero.Location),
		}
	}
	if src.S3Sync != nil && src.S3Sync.OrbID != "" {
		dst.S3Sync = &armadav1.S3SyncSpec{
			OrbID:          src.S3Sync.OrbID,
			Enabled:        boolPtr(src.S3Sync.Enabled),
			Schedule:       stringPtrNonEmpty(src.S3Sync.Schedule),
			SourceLocation: stringPtrNonEmpty(src.S3Sync.SourceLocation),
			DestLocation:   stringPtrNonEmpty(src.S3Sync.DestLocation),
		}
	}
	if dst.Etcd == nil && dst.Velero == nil && dst.S3Sync == nil {
		return nil
	}
	return dst
}

// boolPtr returns a pointer to the given bool. Used to promote graph
// primitive booleans (which are always present in the response) into the
// pointer types the CRD uses to distinguish nil ("intent absent") from
// false ("intent explicitly disabled"). Orbital doesn't distinguish today —
// a boolean edge is always emitted — so this promotion is one-way.
func boolPtr(b bool) *bool { return &b }

// stringPtrNonEmpty returns nil for empty strings so absent-in-graph fields
// don't populate spec with empty strings (which the CRD would treat as
// "value is empty string", not "unset").
func stringPtrNonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
