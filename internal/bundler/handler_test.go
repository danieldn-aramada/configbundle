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
	if len(layers) != 1 {
		t.Fatalf("expected 1 layer, got %d", len(layers))
	}
	if layers[0].MediaType != bundle.MediaTypeManifest {
		t.Errorf("manifest mediaType: got %q", layers[0].MediaType)
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
	if got := derefString(srv.IdracSettings.FirmwareVersion); got != "7.20.10.05" {
		t.Errorf("firmwareVersion: got %q", got)
	}
	if !derefBool(srv.IdracSettings.SSHEnabled) {
		t.Error("sshEnabled: want true")
	}
	if !derefBool(srv.IdracSettings.RacadmEnabled) {
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
	idrac := spec.Servers[0].IdracSettings
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

func TestMapClusterBackup_S3SyncFieldsPopulated(t *testing.T) {
	src := &ClusterBackupResult{
		OrbID: "colo:cluster-001-backup",
		S3Sync: &S3SyncResult{
			OrbID:          "colo:cluster-001-s3sync",
			Enabled:        true,
			Schedule:       "0 4 * * *",
			SourceLocation: "http://rook-rgw.rook-ceph.svc:80/mirror-bucket/prefix",
			DestLocation:   "https://dest-endpoint/dest-bucket/prefix",
		},
	}
	dst := mapClusterBackup(src)
	if dst.S3Sync == nil {
		t.Fatal("expected S3Sync to be non-nil")
	}
	if dst.S3Sync.OrbID != "colo:cluster-001-s3sync" {
		t.Errorf("OrbID: got %q", dst.S3Sync.OrbID)
	}
	if !derefBool(dst.S3Sync.Enabled) {
		t.Error("Enabled: want true")
	}
	if derefString(dst.S3Sync.Schedule) != "0 4 * * *" {
		t.Errorf("Schedule: got %q", derefString(dst.S3Sync.Schedule))
	}
	if derefString(dst.S3Sync.SourceLocation) != "http://rook-rgw.rook-ceph.svc:80/mirror-bucket/prefix" {
		t.Errorf("SourceLocation: got %q", derefString(dst.S3Sync.SourceLocation))
	}
	if derefString(dst.S3Sync.DestLocation) != "https://dest-endpoint/dest-bucket/prefix" {
		t.Errorf("DestLocation: got %q", derefString(dst.S3Sync.DestLocation))
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

// --- Unit tests for buildTakeover ---
//
// After ADR-011: resolutions are looked up against spec.servers[] directly —
// each server's nested-entity orbId (Idrac.OrbID) is the join key. No mapping
// payload anymore; the spec carries identity for both top-level and nested nodes.

func TestBuildTakeover_Empty(t *testing.T) {
	entries := buildTakeover(nil, nil)
	if len(entries) != 0 {
		t.Errorf("expected no entries, got %d", len(entries))
	}
}

func TestBuildTakeover_ResolvesServerOrbIDViaNestedOrbID(t *testing.T) {
	servers := []armadav1.ServerSpec{
		{OrbID: "colo:srv-001", IdracSettings: armadav1.IdracSettingsSpec{OrbID: "colo:srv-001-idrac"}},
		{OrbID: "colo:srv-002", IdracSettings: armadav1.IdracSettingsSpec{OrbID: "colo:srv-002-idrac"}},
	}

	resolutions := []PendingForceResolution{
		{OrbID: "colo:srv-001-idrac", Field: "sshEnabled"},
		{OrbID: "colo:srv-002-idrac", Field: "racadmEnabled"},
	}

	entries := buildTakeover(resolutions, servers)
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

func TestBuildTakeover_SkipsOrbIdNotMatchingAnyServer(t *testing.T) {
	servers := []armadav1.ServerSpec{
		{OrbID: "colo:srv-001", IdracSettings: armadav1.IdracSettingsSpec{OrbID: "colo:srv-001-idrac"}},
	}

	resolutions := []PendingForceResolution{
		{OrbID: "colo:srv-001-idrac", Field: "sshEnabled"},    // matches server-001
		{OrbID: "colo:srv-002-idrac", Field: "racadmEnabled"}, // no server-002 in bundle → skipped
		{OrbID: "this-is-not-a-known-id", Field: "x"},         // not matching any nested orbId → skipped
	}

	entries := buildTakeover(resolutions, servers)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].OrbID != "colo:srv-001-idrac" {
		t.Errorf("expected server-001 entry to survive, got %q", entries[0].OrbID)
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

// --- Unit tests for buildIgnored (replaces applyOmissions, 2026-06-16) ---
//
// Ignore now emits spec.ignored[] entries instead of niling field values.
// The intent value stays in spec.servers[N].<field> so the divergence-reporter
// can keep comparing intent vs local override and surfacing the divergence.

func TestBuildIgnored_NilInput(t *testing.T) {
	got := buildIgnored(nil, nil)
	if got != nil {
		t.Errorf("nil omissions must return nil entries, got %v", got)
	}
}

func TestBuildIgnored_ResolvesServerOrbIDViaNestedOrbID(t *testing.T) {
	servers := []armadav1.ServerSpec{
		{OrbID: "colo:srv-001", IdracSettings: armadav1.IdracSettingsSpec{OrbID: "colo:srv-001-idrac"}},
	}
	omissions := []Omission{
		{OrbID: "colo:srv-001-idrac", Field: "sshEnabled"},
		{OrbID: "colo:srv-001-idrac", Field: "racadmEnabled"},
	}
	got := buildIgnored(omissions, servers)
	if len(got) != 2 {
		t.Fatalf("expected 2 ignored entries, got %d", len(got))
	}
	for _, e := range got {
		if e.ServerOrbID != "colo:srv-001" {
			t.Errorf("expected serverOrbId resolved from idrac orbId, got %q", e.ServerOrbID)
		}
	}
}

func TestBuildIgnored_UnknownOrbIDSkipped(t *testing.T) {
	got := buildIgnored([]Omission{{OrbID: "unknown:srv-001-idrac", Field: "sshEnabled"}}, nil)
	if len(got) != 0 {
		t.Errorf("unknown orbId must be silently skipped, got %d entries", len(got))
	}
}

func TestBuildIgnored_IntentValuesStayInSpec(t *testing.T) {
	// Regression: under spec.ignored[], the field's intent value must remain in
	// the manifest so the divergence-reporter can compare against the local
	// override. Pre-2026-06-16 behavior nil'd the field; that broke "ignore
	// continues to surface as divergence" semantics.
	spec := armadav1.ConfigBundleSpec{
		Datacenter: "colo",
		Servers: []armadav1.ServerSpec{{
			OrbID:    "colo:srv-001",
			Hostname: ptrString("host-01"),
			IdracSettings: armadav1.IdracSettingsSpec{
				OrbID:         "colo:srv-001-idrac",
				SSHEnabled:    ptrBool(true),
				RacadmEnabled: ptrBool(true),
			},
		}},
	}
	spec.Ignored = buildIgnored([]Omission{{OrbID: "colo:srv-001-idrac", Field: "sshEnabled"}}, spec.Servers)

	out, err := yaml.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), "sshEnabled: true") {
		t.Errorf("sshEnabled intent value must remain in spec YAML even when ignored.\nYAML:\n%s", out)
	}
	if !strings.Contains(string(out), "ignored:") {
		t.Errorf("spec.ignored list missing from YAML.\nYAML:\n%s", out)
	}
}

// --- Unit tests for mapClusterBackup / childless ClusterBackup filtering ---

func TestMapClusterBackup_ChildlessReturnsNil(t *testing.T) {
	src := &ClusterBackupResult{OrbID: "colo:cluster-backup"}
	// No Etcd, Velero, or S3Sync children.
	if got := mapClusterBackup(src); got != nil {
		t.Errorf("expected nil for childless ClusterBackup, got %+v", got)
	}
}

func TestMapClusterBackup_WithEtcdReturnsNonNil(t *testing.T) {
	src := &ClusterBackupResult{
		OrbID: "colo:cluster-backup",
		Etcd:  &EtcdBackupResult{OrbID: "colo:cluster-etcd", Enabled: true, Schedule: "0 3 * * *"},
	}
	got := mapClusterBackup(src)
	if got == nil {
		t.Fatal("expected non-nil for ClusterBackup with etcd child")
	}
	if got.Etcd == nil {
		t.Error("expected Etcd to be populated")
	}
}

func TestMapClusterBackup_WithVeleroReturnsNonNil(t *testing.T) {
	src := &ClusterBackupResult{
		OrbID:  "colo:cluster-backup",
		Velero: &VeleroBackupResult{OrbID: "colo:cluster-velero", Enabled: true},
	}
	if got := mapClusterBackup(src); got == nil {
		t.Error("expected non-nil for ClusterBackup with velero child")
	}
}

func TestMapClusterBackup_WithS3SyncReturnsNonNil(t *testing.T) {
	src := &ClusterBackupResult{
		OrbID:  "colo:cluster-backup",
		S3Sync: &S3SyncResult{OrbID: "colo:cluster-s3sync", Enabled: true},
	}
	if got := mapClusterBackup(src); got == nil {
		t.Error("expected non-nil for ClusterBackup with s3sync child")
	}
}

func TestMapToSpec_ChildlessClusterBackupProducesNilBackup(t *testing.T) {
	dc := DataCenterResult{
		Name:  "colo",
		OrbID: "colo:colo-galleon",
		KubernetesClusters: []KubernetesClusterResult{{
			OrbID:  "colo:cluster-a",
			Name:   "cluster-a",
			Backup: &ClusterBackupResult{OrbID: "colo:cluster-a-backup"},
			// No Etcd, Velero, S3Sync.
		}},
	}
	spec := mapToSpec(dc)
	if len(spec.KubernetesClusters) != 1 {
		t.Fatalf("expected 1 cluster, got %d", len(spec.KubernetesClusters))
	}
	if spec.KubernetesClusters[0].Backup != nil {
		t.Errorf("expected nil Backup for childless ClusterBackup node, got %+v", spec.KubernetesClusters[0].Backup)
	}
}

func TestMapToSpec_ClusterWithEtcdEmitsBackup(t *testing.T) {
	dc := DataCenterResult{
		Name:  "colo",
		OrbID: "colo:colo-galleon",
		KubernetesClusters: []KubernetesClusterResult{{
			OrbID: "colo:cluster-b",
			Name:  "cluster-b",
			Backup: &ClusterBackupResult{
				OrbID: "colo:cluster-b-backup",
				Etcd:  &EtcdBackupResult{OrbID: "colo:cluster-b-etcd", Enabled: true, Schedule: "0 14 * * *"},
			},
		}},
	}
	spec := mapToSpec(dc)
	if spec.KubernetesClusters[0].Backup == nil {
		t.Fatal("expected non-nil Backup for cluster with etcd child")
	}
	if spec.KubernetesClusters[0].Backup.Etcd == nil {
		t.Error("expected Etcd block in spec")
	}
}

func ptrString(s string) *string { return &s }
func ptrBool(b bool) *bool       { return &b }
