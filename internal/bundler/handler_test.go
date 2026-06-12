package bundler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	h.ServeHTTP(w, newRequest(`{"datacenter":"colo"}`))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestHandleBundle_DatacenterNotFound(t *testing.T) {
	h := &Handler{Orbital: &fakeQuerier{results: nil}}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRequest(`{"datacenter":"colo"}`))
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
		Name: "colo",
		Servers: []ServerResult{{
			Hostname:   "colo-r740-01",
			ServiceTag: "3RK3V64",
			OobIP:      &IPAddressResult{Address: "10.10.1.45"},
			IdracSettings: &IdracSettingsResult{
				FirmwareVersion: "7.20.10.05",
				SSHEnabled:      true,
				IPMIEnabled:     false,
				RacadmEnabled:   true,
			},
		}},
	}}}}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRequest(`{"datacenter":"colo"}`))
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
	if srv.Hostname != "colo-r740-01" {
		t.Errorf("hostname: got %q", srv.Hostname)
	}
	if srv.ServiceTag != "3RK3V64" {
		t.Errorf("serviceTag: got %q", srv.ServiceTag)
	}
	if srv.OobIP != "10.10.1.45" {
		t.Errorf("oobIP: got %q", srv.OobIP)
	}
	if srv.Idrac.FirmwareVersion != "7.20.10.05" {
		t.Errorf("firmwareVersion: got %q", srv.Idrac.FirmwareVersion)
	}
	if !srv.Idrac.SSHEnabled {
		t.Error("sshEnabled: want true")
	}
	if !srv.Idrac.RacadmEnabled {
		t.Error("racadmEnabled: want true")
	}
	if len(resp.ConsumedResolutionIDs) != 0 {
		t.Errorf("expected no consumed IDs without Resolutions querier, got %v", resp.ConsumedResolutionIDs)
	}
}

// --- Unit tests for mapToSpec ---

func TestMapToSpec_SkipsServersWithoutHostname(t *testing.T) {
	dc := DataCenterResult{
		Name: "colo",
		Servers: []ServerResult{
			{Hostname: "", ServiceTag: "NO-HOST"},
			{Hostname: "valid-host", ServiceTag: "HAS-HOST"},
		},
	}
	spec := mapToSpec(dc)
	if len(spec.Servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(spec.Servers))
	}
	if spec.Servers[0].Hostname != "valid-host" {
		t.Errorf("expected valid-host, got %q", spec.Servers[0].Hostname)
	}
}

func TestMapToSpec_NilOobIP(t *testing.T) {
	dc := DataCenterResult{
		Name:    "colo",
		Servers: []ServerResult{{Hostname: "host", OobIP: nil}},
	}
	spec := mapToSpec(dc)
	if spec.Servers[0].OobIP != "" {
		t.Errorf("expected empty oobIP, got %q", spec.Servers[0].OobIP)
	}
}

func TestMapToSpec_NilIdracSettings(t *testing.T) {
	dc := DataCenterResult{
		Name:    "colo",
		Servers: []ServerResult{{Hostname: "host", IdracSettings: nil}},
	}
	spec := mapToSpec(dc)
	// Zero-value IdracSpec — all bools false, firmware empty.
	idrac := spec.Servers[0].Idrac
	if idrac.SSHEnabled || idrac.IPMIEnabled || idrac.FirmwareVersion != "" {
		t.Error("expected zero-value IdracSpec for nil idracSettings")
	}
}

// --- Unit tests for buildMapping ---

func TestBuildMapping_FullDatacenter(t *testing.T) {
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
	if len(mapping.Items) != 5 {
		t.Fatalf("expected 5 mapping items, got %d: %+v", len(mapping.Items), mapping.Items)
	}

	type want struct {
		orbID string
		typ   string
	}
	expected := map[string]want{
		"spec":                                   {"colo:colo-galleon", "DataCenter"},
		"spec.servers[serviceTag=3RK3V64]":       {"colo:srv-001", "Server"},
		"spec.servers[serviceTag=3RK3V64].idrac": {"colo:srv-001-idrac", "IdracSettings"},
		"spec.servers[serviceTag=7BN2X91]":       {"colo:srv-002", "Server"},
		"spec.servers[serviceTag=7BN2X91].idrac": {"colo:srv-002-idrac", "IdracSettings"},
	}

	for _, item := range mapping.Items {
		w, ok := expected[item.Path]
		if !ok {
			t.Errorf("unexpected mapping path %q", item.Path)
			continue
		}
		if item.OrbID != w.orbID {
			t.Errorf("path %q: got orbId %q, want %q", item.Path, item.OrbID, w.orbID)
		}
		if item.Type != w.typ {
			t.Errorf("path %q: got type %q, want %q", item.Path, item.Type, w.typ)
		}
		delete(expected, item.Path)
	}
	for path := range expected {
		t.Errorf("missing expected mapping path %q", path)
	}
}

func TestBuildMapping_SkipsServersWithoutHostname(t *testing.T) {
	dc := DataCenterResult{
		Name:  "colo",
		OrbID: "colo:colo-galleon",
		Servers: []ServerResult{
			{Hostname: "", ServiceTag: "NO-HOST", OrbID: "colo:orphan"},
			{Hostname: "valid", ServiceTag: "HAS-HOST", OrbID: "colo:srv-001"},
		},
	}

	mapping := buildMapping(dc)
	for _, item := range mapping.Items {
		if item.OrbID == "colo:orphan" {
			t.Error("mapping should not include server without hostname")
		}
	}
}

// --- Unit tests for buildTakeover ---

func TestBuildTakeover_Empty(t *testing.T) {
	entries, ids := buildTakeover(nil, MappingLayer{})
	if len(entries) != 0 {
		t.Errorf("expected no entries, got %d", len(entries))
	}
	if len(ids) != 0 {
		t.Errorf("expected no ids, got %d", len(ids))
	}
}

func TestBuildTakeover_ResolvesOrbIdToServiceTag(t *testing.T) {
	mapping := MappingLayer{Items: []MappingEntry{
		{Path: "spec.servers[serviceTag=3RK3V64]", OrbID: "colo:srv-001", Type: "Server"},
		{Path: "spec.servers[serviceTag=3RK3V64].idrac", OrbID: "colo:srv-001-idrac", Type: "IdracSettings"},
		{Path: "spec.servers[serviceTag=7BN2X91]", OrbID: "colo:srv-002", Type: "Server"},
	}}

	resolutions := []PendingForceResolution{
		{ID: "res-1", OrbID: "colo:srv-001-idrac", Field: "sshEnabled"},
		{ID: "res-2", OrbID: "colo:srv-002", Field: "hostname"},
	}

	entries, ids := buildTakeover(resolutions, mapping)
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	// First entry: idrac field → serviceTag from parent server path
	if entries[0].ServiceTag != "3RK3V64" {
		t.Errorf("entry[0] serviceTag: got %q, want 3RK3V64", entries[0].ServiceTag)
	}
	if entries[0].Field != "sshEnabled" {
		t.Errorf("entry[0] field: got %q", entries[0].Field)
	}
	if entries[0].OrbID != "colo:srv-001-idrac" {
		t.Errorf("entry[0] orbId: got %q", entries[0].OrbID)
	}

	// Second entry: server-level field
	if entries[1].ServiceTag != "7BN2X91" {
		t.Errorf("entry[1] serviceTag: got %q, want 7BN2X91", entries[1].ServiceTag)
	}
	if entries[1].Field != "hostname" {
		t.Errorf("entry[1] field: got %q", entries[1].Field)
	}

	if len(ids) != 2 || ids[0] != "res-1" || ids[1] != "res-2" {
		t.Errorf("consumed IDs: got %v", ids)
	}
}

func TestBuildTakeover_SkipsUnknownOrbId(t *testing.T) {
	mapping := MappingLayer{Items: []MappingEntry{
		{Path: "spec.servers[serviceTag=3RK3V64].idrac", OrbID: "colo:srv-001-idrac", Type: "IdracSettings"},
	}}

	resolutions := []PendingForceResolution{
		{ID: "res-1", OrbID: "colo:srv-001-idrac", Field: "sshEnabled"},
		{ID: "res-2", OrbID: "unknown:orb-id", Field: "hostname"},
	}

	entries, ids := buildTakeover(resolutions, mapping)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry (unknown skipped), got %d", len(entries))
	}
	if len(ids) != 1 || ids[0] != "res-1" {
		t.Errorf("consumed IDs: got %v", ids)
	}
}

func TestBuildTakeover_SkipsDatacenterLevelOrbId(t *testing.T) {
	mapping := MappingLayer{Items: []MappingEntry{
		{Path: "spec", OrbID: "colo:colo-galleon", Type: "DataCenter"},
	}}

	resolutions := []PendingForceResolution{
		{ID: "res-1", OrbID: "colo:colo-galleon", Field: "datacenter"},
	}

	// spec path has no serviceTag — should be skipped
	entries, ids := buildTakeover(resolutions, mapping)
	if len(entries) != 0 {
		t.Errorf("expected 0 entries for DC-level resolution, got %d", len(entries))
	}
	if len(ids) != 0 {
		t.Errorf("expected 0 consumed IDs, got %v", ids)
	}
}

// --- Unit tests for extractServiceTag ---

func TestExtractServiceTag(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"spec.servers[serviceTag=3RK3V64]", "3RK3V64"},
		{"spec.servers[serviceTag=3RK3V64].idrac", "3RK3V64"},
		{"spec.servers[serviceTag=ABC123].idrac.sshEnabled", "ABC123"},
		{"spec", ""},
		{"spec.datacenter", ""},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := extractServiceTag(tt.path)
			if got != tt.want {
				t.Errorf("extractServiceTag(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

// --- Handler test with resolutions ---

type fakeResolutionQuerier struct {
	resolutions []PendingForceResolution
	err         error
}

func (f *fakeResolutionQuerier) QueryPendingForce(_ context.Context) ([]PendingForceResolution, error) {
	return f.resolutions, f.err
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
			{ID: "res-aaa", OrbID: "colo:srv-001-idrac", Field: "sshEnabled"},
		}},
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRequest(`{"datacenter":"colo"}`))
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
	if spec.Takeover[0].ServiceTag != "3RK3V64" {
		t.Errorf("takeover serviceTag: got %q", spec.Takeover[0].ServiceTag)
	}
	if spec.Takeover[0].Field != "sshEnabled" {
		t.Errorf("takeover field: got %q", spec.Takeover[0].Field)
	}
	if spec.Takeover[0].OrbID != "colo:srv-001-idrac" {
		t.Errorf("takeover orbId: got %q", spec.Takeover[0].OrbID)
	}

	// Verify consumed IDs
	if len(resp.ConsumedResolutionIDs) != 1 || resp.ConsumedResolutionIDs[0] != "res-aaa" {
		t.Errorf("consumed IDs: got %v", resp.ConsumedResolutionIDs)
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
	h.ServeHTTP(w, newRequest(`{"datacenter":"colo"}`))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestBuildMapping_MissingOrbIds(t *testing.T) {
	dc := DataCenterResult{
		Name: "colo",
		// No OrbID on datacenter
		Servers: []ServerResult{
			{
				Hostname:   "host-01",
				ServiceTag: "3RK3V64",
				// No OrbID on server
				IdracSettings: &IdracSettingsResult{
					OrbID: "colo:srv-001-idrac",
				},
			},
		},
	}

	mapping := buildMapping(dc)
	// Should only have the idrac entry (datacenter and server have no orbId)
	if len(mapping.Items) != 1 {
		t.Fatalf("expected 1 mapping item, got %d: %+v", len(mapping.Items), mapping.Items)
	}
	if mapping.Items[0].OrbID != "colo:srv-001-idrac" {
		t.Errorf("got orbId %q", mapping.Items[0].OrbID)
	}
}
