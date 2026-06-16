package bundler

import (
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"reflect"
	"strings"

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
	mapping := buildMapping(results[0])

	// Query both takeover (accept+reject) and omission (ignore) resolutions.
	// Order matters for the apply config: omissions zero out fields first, then
	// the takeover list is computed (omissions never conflict with takeover —
	// orbital writes one resolution row per field, mutually exclusive actions).
	if h.Resolutions != nil {
		omissions, err := h.Resolutions.QueryOmissions(r.Context())
		if err != nil {
			http.Error(w, "query omissions: "+err.Error(), http.StatusInternalServerError)
			return
		}
		applyOmissions(&spec, omissions, mapping)

		resolutions, err := h.Resolutions.QueryPendingForce(r.Context())
		if err != nil {
			http.Error(w, "query pending-force: "+err.Error(), http.StatusInternalServerError)
			return
		}
		spec.Takeover = buildTakeover(resolutions, mapping)
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
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// buildTakeover translates pending accept/reject-resolutions into TakeoverEntry values
// using the mapping rules to recover the owning server's orbId from each
// resolution's nested-entity orbId (e.g. "colo:GQK3V64-idrac" → "colo:GQK3V64").
// Resolutions whose orbId doesn't match any mapping rule are silently skipped —
// the resolution may belong to a different bundle or a stale entry.
func buildTakeover(resolutions []PendingForceResolution, mapping bundle.MappingPayload) []armadav1.TakeoverEntry {
	if len(resolutions) == 0 {
		return nil
	}
	var entries []armadav1.TakeoverEntry
	for _, res := range resolutions {
		_, serverOrbID, ok := mapping.ResolveByOrbID(res.OrbID)
		if !ok {
			continue
		}
		entries = append(entries, armadav1.TakeoverEntry{
			OrbID:       res.OrbID,
			ServerOrbID: serverOrbID,
			Field:       res.Field,
		})
	}
	return entries
}

// applyOmissions removes (orbId, field) pairs from spec for each ignore-resolution.
// The orbId in the Omission identifies an Orbital ConfigItem (e.g. IdracSettings
// owns idrac fields). We resolve via the mapping rules to find which server the
// field lives on, then nil out the matching leaf — because IdracSpec/ServerSpec
// leaves are pointers with omitempty (ADR-007), nil → absent from the serialized
// apply config → cb-controller releases its claim → local:<id> remains sole manager.
//
// Unknown orbIds, fields, or missing server entries are silently skipped — the
// resolution may belong to a different bundle or a stale entry.
func applyOmissions(spec *armadav1.ConfigBundleSpec, omissions []Omission, mapping bundle.MappingPayload) {
	if len(omissions) == 0 {
		return
	}
	serverIndex := make(map[string]*armadav1.ServerSpec, len(spec.Servers))
	for i := range spec.Servers {
		serverIndex[spec.Servers[i].OrbID] = &spec.Servers[i]
	}
	for _, om := range omissions {
		_, serverOrbID, ok := mapping.ResolveByOrbID(om.OrbID)
		if !ok {
			continue
		}
		server, ok := serverIndex[serverOrbID]
		if !ok {
			continue
		}
		nilFieldOnServer(server, om.Field)
	}
}

// nilFieldOnServer sets the named field on a ServerSpec to its zero value,
// matching by JSON tag. Tries top-level ServerSpec fields first, then IdracSpec.
// Returns true if a match was found and zeroed. For pointer leaves (ADR-007),
// the zero value is nil — and omitempty will exclude it from the serialized YAML.
func nilFieldOnServer(server *armadav1.ServerSpec, jsonName string) bool {
	if zeroStructFieldByJSONTag(reflect.ValueOf(server).Elem(), jsonName) {
		return true
	}
	return zeroStructFieldByJSONTag(reflect.ValueOf(&server.Idrac).Elem(), jsonName)
}

// zeroStructFieldByJSONTag finds a field on v whose first json tag part matches
// jsonName, sets it to its zero value, and returns true.
func zeroStructFieldByJSONTag(v reflect.Value, jsonName string) bool {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		tag := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
		if tag == jsonName {
			v.Field(i).SetZero()
			return true
		}
	}
	return false
}

// buildMapping produces the structural identity-translation rules for a
// DataCenterResult. After the orbId-as-identity migration
// (docs/plans/server-identity-orbid.md), the mapping carries ONLY rules for
// nested Orbital nodes that don't have their own first-class identity in the
// K8s spec — IdracSettings today, future nested config types (NetworkConfig,
// BIOSConfig, etc.). Datacenter and server orbIds live in spec.orbId and
// spec.servers[].orbId respectively, not here.
//
// One rule per nested type, emitted iff at least one server in the result has
// the nested entity populated. Naming convention: nested orbital entities are
// "<parent-orbId><suffix>" — e.g. "colo:GQK3V64-idrac" lives under server
// "colo:GQK3V64". The rule carries the suffix so consumers can derive parent
// from child without re-querying orbital.
func buildMapping(dc DataCenterResult) bundle.MappingPayload {
	var rules []bundle.MappingRule
	for _, s := range dc.Servers {
		if s.Hostname == "" || s.OrbID == "" {
			continue
		}
		if s.IdracSettings != nil && s.IdracSettings.OrbID != "" {
			rules = append(rules, bundle.MappingRule{
				ListField:   "spec.servers",
				ItemKey:     "orbId",
				Field:       "idrac",
				Type:        "IdracSettings",
				OrbIDSuffix: "-idrac",
			})
			break
		}
	}
	return bundle.MappingPayload{Rules: rules}
}

// mapToSpec maps a GraphQL DataCenterResult to a ConfigBundleSpec.
// Servers without a hostname or orbId are skipped — hostname is required by the
// CRD and orbId is the SSA listMapKey.
// IdracSettings fields are transferred via JSON round-trip: both IdracSettingsResult
// and IdracSpec use identical json tags, so adding a field to both structs is
// sufficient — no field-by-field copy code to update.
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
