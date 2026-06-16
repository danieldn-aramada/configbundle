package bundler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	armadav1 "github.com/armada/configbundle/api/v1"
	"github.com/armada/configbundle/bundle"
)

type fakeQuerier struct {
	results []DataCenterResult
	err     error
}

func (f *fakeQuerier) QueryDataCenter(_ context.Context, _ string) ([]DataCenterResult, error) {
	return f.results, f.err
}

func newRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/bundle", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestHandleBundle_InvalidJSON(t *testing.T) {
	h := &Handler{Orbital: &fakeQuerier{}}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRequest("not json"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleBundle_EmptyDatacenter(t *testing.T) {
	h := &Handler{Orbital: &fakeQuerier{}}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRequest(`{}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleBundle_GraphQLError(t *testing.T) {
	h := &Handler{Orbital: &fakeQuerier{err: fmt.Errorf("connection refused")}}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRequest(`{"orbId":"colo:colo"}`))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestHandleBundle_DatacenterNotFound(t *testing.T) {
	h := &Handler{Orbital: &fakeQuerier{results: nil}}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRequest(`{"orbId":"colo:colo"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	resp := decodeResponse(t, w)
	if len(resp.Layers) != 0 {
		t.Errorf("expected empty layers, got %d", len(resp.Layers))
	}
}

func decodeResponse(t *testing.T, w *httptest.ResponseRecorder) bundleResponse {
	t.Helper()
	var resp bundleResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

func TestHandleBundle_Success(t *testing.T) {
	h := &Handler{Orbital: &fakeQuerier{results: []DataCenterResult{{
		Name:  "colo",
		OrbID: "colo:colo-galleon",
		Servers: []ServerResult{{
			Hostname:   "colo-r740-01",
			ServiceTag: "3RK3V64",
			OrbID:      "colo:srv-001",
			OobIP:      &IPAddressResult{Address: "10.10.1.45"},
			IdracSettings: &IdracSettingsResult{
				OrbID:           "colo:srv-001-idrac",
				FirmwareVersion: "7.20.10.05",
				SSHEnabled:      true,
				IPMIEnabled:     false,
				RacadmEnabled:   true,
			},
		}},
	}}}}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRequest(`{"orbId":"colo:colo"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	resp := decodeResponse(t, w)
	layers := resp.Layers
	if len(layers) != 2 {
		t.Fatalf("expected 2 layers, got %d", len(layers))
	}
	if layers[0].MediaType != bundle.MediaTypeManifest {
		t.Errorf("manifest mediaType: got %q", layers[0].MediaType)
	}
	if layers[1].MediaType != bundle.MediaTypeMapping {
		t.Errorf("mapping mediaType: got %q", layers[1].MediaType)
	}

	// Decode and unmarshal to verify round-trip fidelity.
	raw, err := base64.StdEncoding.DecodeString(layers[0].Data)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	var spec armadav1.ConfigBundleSpec
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	if spec.Datacenter != "colo" {
		t.Errorf("datacenter: got %q", spec.Datacenter)
	}
	if len(spec.Servers) != 1 {
		t.Fatalf("servers: got %d", len(spec.Servers))
	}
	srv := spec.Servers[0]
	if got := derefString(srv.Hostname); got != "colo-r740-01" {
		t.Errorf("hostname: got %q", got)
	}
	if srv.ServiceTag != "3RK3V64" {
		t.Errorf("serviceTag: got %q", srv.ServiceTag)
	}
	if got := derefString(srv.OobIP); got != "10.10.1.45" {
		t.Errorf("oobIP: got %q", got)
	}
	if got := derefString(srv.Idrac.FirmwareVersion); got != "7.20.10.05" {
		t.Errorf("firmwareVersion: got %q", got)
	}
	if !derefBool(srv.Idrac.SSHEnabled) {
		t.Error("sshEnabled: want true")
	}
	if !derefBool(srv.Idrac.RacadmEnabled) {
		t.Error("racadmEnabled: want true")
	}
}

// --- Unit tests for mapToSpec ---

func TestMapToSpec_SkipsServersWithoutHostname(t *testing.T) {
	dc := DataCenterResult{
		Name:  "colo",
		OrbID: "colo:colo-galleon",
		Servers: []ServerResult{
			{Hostname: "", ServiceTag: "NO-HOST", OrbID: "colo:srv-001"},
			{Hostname: "valid-host", ServiceTag: "HAS-HOST", OrbID: "colo:srv-002"},
		},
	}
	spec := mapToSpec(dc)
	if len(spec.Servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(spec.Servers))
	}
	if got := derefString(spec.Servers[0].Hostname); got != "valid-host" {
		t.Errorf("expected valid-host, got %q", got)
	}
}

func TestMapToSpec_SkipsServersWithoutOrbID(t *testing.T) {
	dc := DataCenterResult{
		Name:  "colo",
		OrbID: "colo:colo-galleon",
		Servers: []ServerResult{
			{Hostname: "no-orbid", ServiceTag: "TAG-A", OrbID: ""},
			{Hostname: "has-orbid", ServiceTag: "TAG-B", OrbID: "colo:srv-002"},
		},
	}
	spec := mapToSpec(dc)
	if len(spec.Servers) != 1 {
		t.Fatalf("expected 1 server (orbid-less skipped), got %d", len(spec.Servers))
	}
	if spec.Servers[0].OrbID != "colo:srv-002" {
		t.Errorf("expected colo:srv-002, got %q", spec.Servers[0].OrbID)
	}
}

func TestMapToSpec_NilOobIP(t *testing.T) {
	dc := DataCenterResult{
		Name:  "colo",
		OrbID: "colo:colo-galleon",
		Servers: []ServerResult{{
			Hostname: "host", ServiceTag: "TAG-A", OrbID: "colo:srv-001", OobIP: nil,
		}},
	}
	spec := mapToSpec(dc)
	if got := derefString(spec.Servers[0].OobIP); got != "" {
		t.Errorf("expected empty oobIP, got %q", got)
	}
}

func TestMapToSpec_NilIdracSettings(t *testing.T) {
	dc := DataCenterResult{
		Name:  "colo",
		OrbID: "colo:colo-galleon",
		Servers: []ServerResult{{
			Hostname: "host", ServiceTag: "TAG-A", OrbID: "colo:srv-001", IdracSettings: nil,
		}},
	}
	spec := mapToSpec(dc)
	idrac := spec.Servers[0].Idrac
	if derefBool(idrac.SSHEnabled) || derefBool(idrac.IPMIEnabled) || derefString(idrac.FirmwareVersion) != "" {
		t.Error("expected zero-value IdracSpec for nil idracSettings")
	}
}

func TestMapToSpec_PopulatesDatacenterOrbID(t *testing.T) {
	dc := DataCenterResult{
		Name:  "colo",
		OrbID: "colo:colo-galleon",
	}
	spec := mapToSpec(dc)
	if spec.OrbID != "colo:colo-galleon" {
		t.Errorf("expected colo:colo-galleon, got %q", spec.OrbID)
	}
	if spec.Datacenter != "colo" {
		t.Errorf("expected colo, got %q", spec.Datacenter)
	}
}

// derefString returns *p or "" if nil.
func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// derefBool returns *p or false if nil.
func derefBool(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}

// --- Unit tests for buildMapping ---

func TestBuildMapping_EmitsOneRulePerNestedType(t *testing.T) {
	dc := DataCenterResult{
		Name:  "colo",
		OrbID: "colo:colo-galleon",
		Servers: []ServerResult{
			{
				Hostname:   "host-01",
				ServiceTag: "3RK3V64",
				OrbID:      "colo:srv-001",
				IdracSettings: &IdracSettingsResult{
					OrbID:           "colo:srv-001-idrac",
					FirmwareVersion: "7.20.10.05",
				},
			},
			{
				Hostname:   "host-02",
				ServiceTag: "7BN2X91",
				OrbID:      "colo:srv-002",
				IdracSettings: &IdracSettingsResult{
					OrbID: "colo:srv-002-idrac",
				},
			},
		},
	}

	mapping := buildMapping(dc)
	// Post-orbId-migration AND post-structural-rule migration: mapping carries
	// ONE rule per nested type — covers ALL server instances structurally.
	// DC and Server orbIds live in spec directly (not in this payload).
	if len(mapping.Rules) != 1 {
		t.Fatalf("expected 1 rule (IdracSettings type), got %d: %+v", len(mapping.Rules), mapping.Rules)
	}
	r := mapping.Rules[0]
	if r.ListField != "spec.servers" || r.ItemKey != "orbId" || r.Field != "idrac" || r.Type != "IdracSettings" || r.OrbIDSuffix != "-idrac" {
		t.Errorf("unexpected rule shape: %+v", r)
	}
}

func TestBuildMapping_NoRuleWhenNoServerHasIdrac(t *testing.T) {
	dc := DataCenterResult{
		Name:  "colo",
		OrbID: "colo:colo-galleon",
		Servers: []ServerResult{
			{Hostname: "host-01", ServiceTag: "NO-IDRAC", OrbID: "colo:srv-001"},
		},
	}
	mapping := buildMapping(dc)
	if len(mapping.Rules) != 0 {
		t.Fatalf("no servers carry idrac → no rule expected; got %+v", mapping.Rules)
	}
}

// --- Unit tests for buildTakeover ---

func TestBuildTakeover_Empty(t *testing.T) {
	entries := buildTakeover(nil, bundle.MappingPayload{})
	if len(entries) != 0 {
		t.Errorf("expected no entries, got %d", len(entries))
	}
}

func TestBuildTakeover_DerivesServerOrbIDFromSuffix(t *testing.T) {
	mapping := bundle.MappingPayload{Rules: []bundle.MappingRule{
		{ListField: "spec.servers", ItemKey: "orbId", Field: "idrac", Type: "IdracSettings", OrbIDSuffix: "-idrac"},
	}}

	resolutions := []PendingForceResolution{
		{OrbID: "colo:srv-001-idrac", Field: "sshEnabled"},
		{OrbID: "colo:srv-002-idrac", Field: "racadmEnabled"},
	}

	entries := buildTakeover(resolutions, mapping)
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	if entries[0].ServerOrbID != "colo:srv-001" {
		t.Errorf("entry[0] serverOrbId: got %q, want colo:srv-001", entries[0].ServerOrbID)
	}
	if entries[0].Field != "sshEnabled" {
		t.Errorf("entry[0] field: got %q", entries[0].Field)
	}
	if entries[0].OrbID != "colo:srv-001-idrac" {
		t.Errorf("entry[0] orbId: got %q", entries[0].OrbID)
	}

	if entries[1].ServerOrbID != "colo:srv-002" {
		t.Errorf("entry[1] serverOrbId: got %q, want colo:srv-002", entries[1].ServerOrbID)
	}
	if entries[1].Field != "racadmEnabled" {
		t.Errorf("entry[1] field: got %q", entries[1].Field)
	}
}

func TestBuildTakeover_SkipsOrbIdWithUnknownSuffix(t *testing.T) {
	mapping := bundle.MappingPayload{Rules: []bundle.MappingRule{
		{ListField: "spec.servers", ItemKey: "orbId", Field: "idrac", Type: "IdracSettings", OrbIDSuffix: "-idrac"},
	}}

	resolutions := []PendingForceResolution{
		{OrbID: "colo:srv-001-idrac", Field: "sshEnabled"},          // matches -idrac
		{OrbID: "colo:srv-002-bios", Field: "biosMode"},             // no rule for -bios → skipped
		{OrbID: "this-is-not-a-suffix-match", Field: "x"},           // no rule matches → skipped
	}

	entries := buildTakeover(resolutions, mapping)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].OrbID != "colo:srv-001-idrac" {
		t.Errorf("expected -idrac entry to survive, got %q", entries[0].OrbID)
	}
}

// --- Handler test with resolutions ---

type fakeResolutionQuerier struct {
	resolutions []PendingForceResolution
	omissions   []Omission
	err         error
	omErr       error
}

func (f *fakeResolutionQuerier) QueryPendingForce(_ context.Context) ([]PendingForceResolution, error) {
	return f.resolutions, f.err
}

func (f *fakeResolutionQuerier) QueryOmissions(_ context.Context) ([]Omission, error) {
	return f.omissions, f.omErr
}

func TestHandleBundle_WithTakeover(t *testing.T) {
	h := &Handler{
		Orbital: &fakeQuerier{results: []DataCenterResult{{
			Name:  "colo",
			OrbID: "colo:colo-galleon",
			Servers: []ServerResult{{
				Hostname:   "host-01",
				ServiceTag: "3RK3V64",
				OrbID:      "colo:srv-001",
				OobIP:      &IPAddressResult{Address: "10.10.1.45"},
				IdracSettings: &IdracSettingsResult{
					OrbID:      "colo:srv-001-idrac",
					SSHEnabled: true,
				},
			}},
		}}},
		Resolutions: &fakeResolutionQuerier{resolutions: []PendingForceResolution{
			{OrbID: "colo:srv-001-idrac", Field: "sshEnabled"},
		}},
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRequest(`{"orbId":"colo:colo"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	resp := decodeResponse(t, w)

	// Verify manifest contains takeover entry
	raw, err := base64.StdEncoding.DecodeString(resp.Layers[0].Data)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	var spec armadav1.ConfigBundleSpec
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	if len(spec.Takeover) != 1 {
		t.Fatalf("expected 1 takeover entry, got %d", len(spec.Takeover))
	}
	if spec.Takeover[0].ServerOrbID != "colo:srv-001" {
		t.Errorf("takeover serverOrbId: got %q", spec.Takeover[0].ServerOrbID)
	}
	if spec.Takeover[0].Field != "sshEnabled" {
		t.Errorf("takeover field: got %q", spec.Takeover[0].Field)
	}
	if spec.Takeover[0].OrbID != "colo:srv-001-idrac" {
		t.Errorf("takeover orbId: got %q", spec.Takeover[0].OrbID)
	}
}

func TestHandleBundle_ResolutionQueryError(t *testing.T) {
	h := &Handler{
		Orbital: &fakeQuerier{results: []DataCenterResult{{
			Name:    "colo",
			Servers: []ServerResult{{Hostname: "host-01", ServiceTag: "3RK3V64"}},
		}}},
		Resolutions: &fakeResolutionQuerier{err: fmt.Errorf("connection refused")},
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRequest(`{"orbId":"colo:colo"}`))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestBuildMapping_ServerWithoutOrbIdSkipped(t *testing.T) {
	// A server without OrbID is skipped entirely. If it was the ONLY server
	// carrying an iDRAC, no rule is emitted (no nested type present in this bundle).
	dc := DataCenterResult{
		Name:  "colo",
		OrbID: "colo:colo-galleon",
		Servers: []ServerResult{
			{
				Hostname:   "host-01",
				ServiceTag: "3RK3V64",
				// No OrbID on server → skipped
				IdracSettings: &IdracSettingsResult{
					OrbID: "colo:srv-001-idrac",
				},
			},
		},
	}

	mapping := buildMapping(dc)
	if len(mapping.Rules) != 0 {
		t.Fatalf("expected 0 rules when no server-with-orbId carries idrac, got %d: %+v", len(mapping.Rules), mapping.Rules)
	}
}

// --- Unit tests for applyOmissions ---

func TestApplyOmissions_NoOp(t *testing.T) {
	spec := armadav1.ConfigBundleSpec{
		Datacenter: "colo",
		Servers: []armadav1.ServerSpec{{
			OrbID:    "colo:srv-001",
			Hostname: ptrString("host-01"),
			Idrac:    armadav1.IdracSpec{SSHEnabled: ptrBool(true)},
		}},
	}
	applyOmissions(&spec, nil, bundle.MappingPayload{})
	if spec.Servers[0].Idrac.SSHEnabled == nil || *spec.Servers[0].Idrac.SSHEnabled != true {
		t.Errorf("nil omissions must not touch the spec")
	}
}

func TestApplyOmissions_ZeroesIdracLeaf(t *testing.T) {
	spec := armadav1.ConfigBundleSpec{
		Datacenter: "colo",
		Servers: []armadav1.ServerSpec{{
			OrbID:    "colo:srv-001",
			Hostname: ptrString("host-01"),
			Idrac: armadav1.IdracSpec{
				SSHEnabled:    ptrBool(true),
				RacadmEnabled: ptrBool(true),
			},
		}},
	}
	mapping := bundle.MappingPayload{Rules: []bundle.MappingRule{
		{ListField: "spec.servers", ItemKey: "orbId", Field: "idrac", Type: "IdracSettings", OrbIDSuffix: "-idrac"},
	}}
	omissions := []Omission{
		{OrbID: "colo:srv-001-idrac", Field: "sshEnabled"},
	}

	applyOmissions(&spec, omissions, mapping)

	if spec.Servers[0].Idrac.SSHEnabled != nil {
		t.Errorf("sshEnabled must be nil after omission, got %v", *spec.Servers[0].Idrac.SSHEnabled)
	}
	if spec.Servers[0].Idrac.RacadmEnabled == nil || *spec.Servers[0].Idrac.RacadmEnabled != true {
		t.Errorf("racadmEnabled must be untouched (not in omission list)")
	}
}

func TestApplyOmissions_OmittedFieldIsAbsentFromYAML(t *testing.T) {
	// The whole point: after applyOmissions, the marshaled YAML must NOT contain
	// the omitted field. This is what releases cb-controller's claim under SSA.
	spec := armadav1.ConfigBundleSpec{
		OrbID:      "colo:colo-galleon",
		Datacenter: "colo",
		Servers: []armadav1.ServerSpec{{
			OrbID:    "colo:srv-001",
			Hostname: ptrString("host-01"),
			Idrac: armadav1.IdracSpec{
				SSHEnabled:    ptrBool(true),
				RacadmEnabled: ptrBool(true),
			},
		}},
	}
	mapping := bundle.MappingPayload{Rules: []bundle.MappingRule{
		{ListField: "spec.servers", ItemKey: "orbId", Field: "idrac", Type: "IdracSettings", OrbIDSuffix: "-idrac"},
	}}
	applyOmissions(&spec, []Omission{
		{OrbID: "colo:srv-001-idrac", Field: "sshEnabled"},
	}, mapping)

	out, err := yaml.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	yamlStr := string(out)
	if strings.Contains(yamlStr, "sshEnabled") {
		t.Errorf("omitted field still appears in YAML — omitempty not honored or filter broken.\nYAML:\n%s", yamlStr)
	}
	if !strings.Contains(yamlStr, "racadmEnabled") {
		t.Errorf("non-omitted field missing from YAML.\nYAML:\n%s", yamlStr)
	}
}

func TestApplyOmissions_UnknownOrbIdSkipped(t *testing.T) {
	spec := armadav1.ConfigBundleSpec{
		Servers: []armadav1.ServerSpec{{
			OrbID:    "colo:srv-001",
			Hostname: ptrString("host-01"),
			Idrac:    armadav1.IdracSpec{SSHEnabled: ptrBool(true)},
		}},
	}
	mapping := bundle.MappingPayload{} // empty — no resolution possible
	applyOmissions(&spec, []Omission{
		{OrbID: "unknown:srv-001-idrac", Field: "sshEnabled"},
	}, mapping)
	if spec.Servers[0].Idrac.SSHEnabled == nil {
		t.Errorf("unknown orbId omission must not zero the field")
	}
}

func ptrString(s string) *string { return &s }
func ptrBool(b bool) *bool       { return &b }
