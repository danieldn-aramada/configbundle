package bundler

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"sigs.k8s.io/yaml"

	armadav1 "github.com/armada/configbundle/api/v1"
	"github.com/armada/configbundle/bundle"
)

// bundleRequest is the JSON body sent to POST /bundle.
type bundleRequest struct {
	Datacenter string `json:"datacenter"`
}

// bundleResponse is the JSON object returned by POST /bundle.
type bundleResponse struct {
	Layers                []bundleLayer `json:"layers"`
	ConsumedResolutionIDs []string      `json:"consumedResolutionIds,omitempty"`
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
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Datacenter == "" {
		http.Error(w, "datacenter field is required", http.StatusBadRequest)
		return
	}

	results, err := h.Orbital.QueryDataCenter(r.Context(), req.Datacenter)
	if err != nil {
		http.Error(w, "orbital query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// No datacenter found — return empty response. Orbital treats this as success
	// with no configbundle layer in the artifact.
	if len(results) == 0 {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(bundleResponse{})
		return
	}

	spec := mapToSpec(results[0])
	mapping := buildMapping(results[0])

	// Query pending force-resolutions and produce spec.takeover[] entries.
	var consumedIDs []string
	if h.Resolutions != nil {
		resolutions, err := h.Resolutions.QueryPendingForce(r.Context())
		if err != nil {
			http.Error(w, "query pending-force: "+err.Error(), http.StatusInternalServerError)
			return
		}
		takeover, ids := buildTakeover(resolutions, mapping)
		spec.Takeover = takeover
		consumedIDs = ids
	}

	yamlBytes, err := yaml.Marshal(spec)
	if err != nil {
		http.Error(w, "marshal manifest: "+err.Error(), http.StatusInternalServerError)
		return
	}

	mappingBytes, err := json.Marshal(mapping)
	if err != nil {
		http.Error(w, "marshal mapping: "+err.Error(), http.StatusInternalServerError)
		return
	}

	resp := bundleResponse{
		Layers: []bundleLayer{
			{
				MediaType: bundle.MediaTypeManifest,
				Data:      base64.StdEncoding.EncodeToString(yamlBytes),
			},
			{
				MediaType: bundle.MediaTypeMapping,
				Data:      base64.StdEncoding.EncodeToString(mappingBytes),
			},
		},
		ConsumedResolutionIDs: consumedIDs,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// MappingEntry is one path→orbId entry in the mapping layer.
// Type carries the Orbital GraphQL type name so Orbital can dispatch
// update{Type}(...) mutations on Accept.
type MappingEntry struct {
	Path  string `json:"path"`
	OrbID string `json:"orbId"`
	Type  string `json:"type"`
}

// MappingLayer is the mapping layer content written to the OCI artifact.
type MappingLayer struct {
	Items []MappingEntry `json:"items"`
}

// buildTakeover translates pending force-resolutions into TakeoverEntry values
// using the mapping layer to resolve orbId → serviceTag. Returns the entries and
// the resolution IDs that were consumed (for Orbital to mark as consumed after push).
// Resolutions whose orbId doesn't appear in the mapping are silently skipped —
// the resolution may belong to a different bundle or a stale entry.
func buildTakeover(resolutions []PendingForceResolution, mapping MappingLayer) ([]armadav1.TakeoverEntry, []string) {
	if len(resolutions) == 0 {
		return nil, nil
	}

	// Index mapping by orbId for fast lookup.
	orbIndex := make(map[string]MappingEntry, len(mapping.Items))
	for _, item := range mapping.Items {
		orbIndex[item.OrbID] = item
	}

	var entries []armadav1.TakeoverEntry
	var consumedIDs []string

	for _, res := range resolutions {
		item, ok := orbIndex[res.OrbID]
		if !ok {
			continue
		}
		serviceTag := extractServiceTag(item.Path)
		if serviceTag == "" {
			continue
		}
		entries = append(entries, armadav1.TakeoverEntry{
			OrbID:      res.OrbID,
			ServiceTag: serviceTag,
			Field:      res.Field,
		})
		consumedIDs = append(consumedIDs, res.ID)
	}
	return entries, consumedIDs
}

// extractServiceTag pulls the serviceTag value from a mapping path.
// e.g. "spec.servers[serviceTag=3RK3V64]" → "3RK3V64"
//
//	"spec.servers[serviceTag=3RK3V64].idrac" → "3RK3V64"
func extractServiceTag(path string) string {
	const prefix = "serviceTag="
	idx := strings.Index(path, prefix)
	if idx < 0 {
		return ""
	}
	rest := path[idx+len(prefix):]
	end := strings.IndexByte(rest, ']')
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// buildMapping produces the flat path→orbId mapping from a DataCenterResult.
// Entries follow the K8s field path convention used by the divergence reporter.
func buildMapping(dc DataCenterResult) MappingLayer {
	var items []MappingEntry

	if dc.OrbID != "" {
		items = append(items, MappingEntry{Path: "spec", OrbID: dc.OrbID, Type: "DataCenter"})
	}

	for _, s := range dc.Servers {
		if s.Hostname == "" {
			continue
		}
		if s.OrbID != "" {
			items = append(items, MappingEntry{
				Path:  fmt.Sprintf("spec.servers[serviceTag=%s]", s.ServiceTag),
				OrbID: s.OrbID,
				Type:  "Server",
			})
		}
		if s.IdracSettings != nil && s.IdracSettings.OrbID != "" {
			items = append(items, MappingEntry{
				Path:  fmt.Sprintf("spec.servers[serviceTag=%s].idrac", s.ServiceTag),
				OrbID: s.IdracSettings.OrbID,
				Type:  "IdracSettings",
			})
		}
	}

	return MappingLayer{Items: items}
}

// mapToSpec maps a GraphQL DataCenterResult to a ConfigBundleSpec.
// Servers without a hostname are skipped — hostname is required by the CRD.
// IdracSettings fields are transferred via JSON round-trip: both IdracSettingsResult
// and IdracSpec use identical json tags, so adding a field to both structs is
// sufficient — no field-by-field copy code to update.
func mapToSpec(dc DataCenterResult) armadav1.ConfigBundleSpec {
	spec := armadav1.ConfigBundleSpec{Datacenter: dc.Name}
	for _, s := range dc.Servers {
		if s.Hostname == "" {
			continue
		}
		oobIP := ""
		if s.OobIP != nil {
			oobIP = s.OobIP.Address
		}
		srv := armadav1.ServerSpec{
			ServiceTag: s.ServiceTag,
			Hostname:   s.Hostname,
			OobIP:      oobIP,
		}
		if s.IdracSettings != nil {
			srv.Idrac = mapIdrac(s.IdracSettings)
		}
		spec.Servers = append(spec.Servers, srv)
	}
	return spec
}

// mapIdrac transfers IdracSettings fields via JSON round-trip.
// Works because IdracSettingsResult and IdracSpec share identical json tag names.
func mapIdrac(src *IdracSettingsResult) armadav1.IdracSpec {
	var dst armadav1.IdracSpec
	b, err := json.Marshal(src)
	if err != nil {
		return dst
	}
	json.Unmarshal(b, &dst)
	return dst
}
