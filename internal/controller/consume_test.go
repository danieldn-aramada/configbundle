package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	armadav1 "github.com/armada/configbundle/api/v1"
	"github.com/armada/configbundle/bundle"
)

// --- Unit tests for handleConsume (manifest path via handleDispatch) ---

func TestHandleConsume_WrongMediaType(t *testing.T) {
	s := NewConsumeServer(nil)
	req := httptest.NewRequest(http.MethodPost, "/consume", bytes.NewBufferString("body"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleConsume(w, req)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415, got %d", w.Code)
	}
}

func TestHandleConsume_OversizedBody(t *testing.T) {
	s := NewConsumeServer(nil)
	big := bytes.Repeat([]byte("x"), maxManifestBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/consume", bytes.NewReader(big))
	req.Header.Set("Content-Type", bundle.MediaTypeManifest)
	w := httptest.NewRecorder()
	s.handleConsume(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", w.Code)
	}
}

func TestHandleConsume_InvalidManifest(t *testing.T) {
	s := NewConsumeServer(nil)
	req := httptest.NewRequest(http.MethodPost, "/consume", bytes.NewBufferString(":\tbad yaml"))
	req.Header.Set("Content-Type", bundle.MediaTypeManifest)
	w := httptest.NewRecorder()
	s.handleConsume(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleConsume_EmptyDatacenter(t *testing.T) {
	s := NewConsumeServer(nil)
	req := httptest.NewRequest(http.MethodPost, "/consume", bytes.NewBufferString("servers: []"))
	req.Header.Set("Content-Type", bundle.MediaTypeManifest)
	w := httptest.NewRecorder()
	s.handleConsume(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleConsume_ApplyFnError(t *testing.T) {
	s := NewConsumeServer(nil)
	done := make(chan struct{})
	s.applyFn = func(_ context.Context, _ []byte, _, _, _ string) error {
		defer close(done)
		return fmt.Errorf("k8s unavailable")
	}
	req := httptest.NewRequest(http.MethodPost, "/consume", bytes.NewBufferString("datacenter: colo"))
	req.Header.Set("Content-Type", bundle.MediaTypeManifest)
	w := httptest.NewRecorder()
	s.handleConsume(w, req)
	// Apply errors do not affect the HTTP response — they surface via CR status conditions.
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	<-done // wait for async apply
}

func TestHandleConsume_Success(t *testing.T) {
	s := NewConsumeServer(nil)
	var gotBody []byte
	var gotTag, gotDigest, gotImportID string
	done := make(chan struct{})
	s.applyFn = func(_ context.Context, body []byte, tag, digest, importID string) error {
		gotBody = body
		gotTag = tag
		gotDigest = digest
		gotImportID = importID
		close(done)
		return nil
	}
	body := []byte("datacenter: colo")
	req := httptest.NewRequest(http.MethodPost, "/consume", bytes.NewReader(body))
	req.Header.Set("Content-Type", bundle.MediaTypeManifest)
	req.Header.Set("X-Orb-Tag", "v38")
	req.Header.Set("X-Orb-Digest", "sha256:abc")
	req.Header.Set("X-Orb-Import-ID", "uuid-123")
	w := httptest.NewRecorder()
	s.handleConsume(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	<-done // wait for async apply
	if string(gotBody) != string(body) {
		t.Errorf("body mismatch: got %q", gotBody)
	}
	if gotTag != "v38" {
		t.Errorf("tag mismatch: got %q", gotTag)
	}
	if gotDigest != "sha256:abc" {
		t.Errorf("digest mismatch: got %q", gotDigest)
	}
	if gotImportID != "uuid-123" {
		t.Errorf("importID mismatch: got %q", gotImportID)
	}
}

// --- handleDispatch routing tests ---

func TestHandleDispatch_UnsupportedMediaType(t *testing.T) {
	s := NewConsumeServer(nil)
	req := httptest.NewRequest(http.MethodPost, "/dispatch", bytes.NewBufferString("{}"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleDispatch(w, req)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415, got %d", w.Code)
	}
}

func TestHandleDispatch_ManifestRouted(t *testing.T) {
	s := NewConsumeServer(nil)
	done := make(chan struct{})
	s.applyFn = func(_ context.Context, _ []byte, _, _, _ string) error {
		close(done)
		return nil
	}
	req := httptest.NewRequest(http.MethodPost, "/dispatch", bytes.NewBufferString("datacenter: colo"))
	req.Header.Set("Content-Type", bundle.MediaTypeManifest)
	req.Header.Set("X-Orb-Digest", "sha256:abc")
	w := httptest.NewRecorder()
	s.handleDispatch(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	<-done // wait for async apply to confirm routing reached the manifest handler
}

// --- Unit tests for applyManifest helpers ---

func TestParseManifest(t *testing.T) {
	cases := []struct {
		name        string
		input       []byte
		wantDC      string
		wantServers int
		wantErr     bool
	}{
		{
			name:        "valid single server",
			input:       []byte("datacenter: colo\nservers:\n- serviceTag: 3RK3V64\n  hostname: r740\n  oobIP: 10.0.0.1\n"),
			wantDC:      "colo",
			wantServers: 1,
		},
		{
			name:        "no servers",
			input:       []byte("datacenter: colo\n"),
			wantDC:      "colo",
			wantServers: 0,
		},
		{
			name:    "invalid YAML",
			input:   []byte(":\tbad yaml\n"),
			wantErr: true,
		},
		{
			name:        "empty input",
			input:       []byte{},
			wantDC:      "",
			wantServers: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := parseManifest(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if spec.Datacenter != tc.wantDC {
				t.Errorf("datacenter: want %q got %q", tc.wantDC, spec.Datacenter)
			}
			if len(spec.Servers) != tc.wantServers {
				t.Errorf("servers: want %d got %d", tc.wantServers, len(spec.Servers))
			}
		})
	}
}

// --- Unit tests for omitAdminOwnedFields (post-simplification semantics) ---
//
// The simplification: cb-controller bows out only when local:* claims a field
// AND intent value != live value AND the field is NOT in spec.takeover[].
// When values match, the field stays in the apply body so the force-conflicts
// pass can silently claim sole ownership (no steady-state co-ownership).
//
// Regression class: a refactor that drops the value-comparison or takeover-set
// awareness in `omitAdminOwnedFields` would re-introduce either co-ownership
// (no auto-claim on match) or the user-reported batch-Reject bug (Accept on
// a sibling field invalidating Reject on this field).

func managedFieldsClaim(t *testing.T, manager string, fieldsV1 map[string]any) []metav1.ManagedFieldsEntry {
	t.Helper()
	raw, err := json.Marshal(fieldsV1)
	if err != nil {
		t.Fatalf("marshal fieldsV1: %v", err)
	}
	return []metav1.ManagedFieldsEntry{{
		Manager:    manager,
		Operation:  metav1.ManagedFieldsOperationApply,
		APIVersion: "armada.ai/v1",
		FieldsV1:   &metav1.FieldsV1{Raw: raw},
	}}
}

func TestOmitAdminOwnedFields_BowsOutOnValueMismatch(t *testing.T) {
	// intent.sshEnabled=false, live.sshEnabled=true (genuine override) → omitted.
	intent := armadav1.ConfigBundleSpec{
		Datacenter: "colo:test",
		Servers: []armadav1.ServerSpec{{
			OrbID:         "colo:srv-1",
			ServiceTag:    "T1",
			IdracSettings: armadav1.IdracSettingsSpec{SSHEnabled: ptr.To(false)},
		}},
	}
	live := armadav1.ConfigBundleSpec{
		Datacenter: "colo:test",
		Servers: []armadav1.ServerSpec{{
			OrbID:         "colo:srv-1",
			ServiceTag:    "T1",
			IdracSettings: armadav1.IdracSettingsSpec{SSHEnabled: ptr.To(true)},
		}},
	}
	mf := managedFieldsClaim(t, "local:admin", map[string]any{
		"f:spec": map[string]any{
			"f:servers": map[string]any{
				`k:{"orbId":"colo:srv-1"}`: map[string]any{
					"f:idracSettings": map[string]any{"f:sshEnabled": map[string]any{}},
				},
			},
		},
	})

	out, err := omitAdminOwnedFields(intent, live, mf)
	if err != nil {
		t.Fatalf("omit: %v", err)
	}
	if len(out.Servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(out.Servers))
	}
	if out.Servers[0].IdracSettings.SSHEnabled != nil {
		t.Errorf("sshEnabled should be omitted (genuine override, values differ), got %v", *out.Servers[0].IdracSettings.SSHEnabled)
	}
}

func TestOmitAdminOwnedFields_KeepsOnValueMatchForAutoClaim(t *testing.T) {
	// intent.sshEnabled=true, live.sshEnabled=true, local:admin owns it.
	// Values match → KEEP in apply so the force-conflicts pass silently claims.
	intent := armadav1.ConfigBundleSpec{
		Datacenter: "colo:test",
		Servers: []armadav1.ServerSpec{{
			OrbID:         "colo:srv-1",
			ServiceTag:    "T1",
			IdracSettings: armadav1.IdracSettingsSpec{SSHEnabled: ptr.To(true)},
		}},
	}
	live := armadav1.ConfigBundleSpec{
		Datacenter: "colo:test",
		Servers: []armadav1.ServerSpec{{
			OrbID:         "colo:srv-1",
			ServiceTag:    "T1",
			IdracSettings: armadav1.IdracSettingsSpec{SSHEnabled: ptr.To(true)},
		}},
	}
	mf := managedFieldsClaim(t, "local:admin", map[string]any{
		"f:spec": map[string]any{
			"f:servers": map[string]any{
				`k:{"orbId":"colo:srv-1"}`: map[string]any{
					"f:idracSettings": map[string]any{"f:sshEnabled": map[string]any{}},
				},
			},
		},
	})

	out, err := omitAdminOwnedFields(intent, live, mf)
	if err != nil {
		t.Fatalf("omit: %v", err)
	}
	if len(out.Servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(out.Servers))
	}
	if out.Servers[0].IdracSettings.SSHEnabled == nil || *out.Servers[0].IdracSettings.SSHEnabled != true {
		t.Errorf("sshEnabled should be KEPT when values match (auto-claim case); got %v",
			out.Servers[0].IdracSettings.SSHEnabled)
	}
}

func TestOmitAdminOwnedFields_KeepsTakeoverTargetEvenOnMismatch(t *testing.T) {
	// intent.sshEnabled=false, live.sshEnabled=true, in takeover → KEEP for force.
	intent := armadav1.ConfigBundleSpec{
		Datacenter: "colo:test",
		Servers: []armadav1.ServerSpec{{
			OrbID:         "colo:srv-1",
			ServiceTag:    "T1",
			IdracSettings: armadav1.IdracSettingsSpec{SSHEnabled: ptr.To(false)},
		}},
		Takeover: []armadav1.TakeoverEntry{{
			OrbID: "colo:srv-1-idrac", ServerOrbID: "colo:srv-1", Field: "sshEnabled",
		}},
	}
	live := armadav1.ConfigBundleSpec{
		Datacenter: "colo:test",
		Servers: []armadav1.ServerSpec{{
			OrbID:         "colo:srv-1",
			ServiceTag:    "T1",
			IdracSettings: armadav1.IdracSettingsSpec{SSHEnabled: ptr.To(true)},
		}},
	}
	mf := managedFieldsClaim(t, "local:admin", map[string]any{
		"f:spec": map[string]any{
			"f:servers": map[string]any{
				`k:{"orbId":"colo:srv-1"}`: map[string]any{
					"f:idracSettings": map[string]any{"f:sshEnabled": map[string]any{}},
				},
			},
		},
	})

	out, err := omitAdminOwnedFields(intent, live, mf)
	if err != nil {
		t.Fatalf("omit: %v", err)
	}
	if out.Servers[0].IdracSettings.SSHEnabled == nil || *out.Servers[0].IdracSettings.SSHEnabled != false {
		t.Errorf("takeover-target sshEnabled should be KEPT for force-apply, got %v",
			out.Servers[0].IdracSettings.SSHEnabled)
	}
}

func TestOmitAdminOwnedFields_IgnoredAlwaysOmittedEvenOnValueMatch(t *testing.T) {
	// Regression: under Ignore semantics, cb-controller MUST NOT claim the field
	// even when values happen to match. A simplification that only checked
	// values-match for auto-claim would silently evict local:* (broken Ignore).
	intent := armadav1.ConfigBundleSpec{
		Datacenter: "colo:test",
		Servers: []armadav1.ServerSpec{{
			OrbID:         "colo:srv-1",
			ServiceTag:    "T1",
			IdracSettings: armadav1.IdracSettingsSpec{RacadmEnabled: ptr.To(true)},
		}},
		Ignored: []armadav1.IgnoredEntry{{
			OrbID: "colo:srv-1-idrac", ServerOrbID: "colo:srv-1", Field: "racadmEnabled",
		}},
	}
	// Live edge happens to match cloud intent (admin set both to true).
	live := armadav1.ConfigBundleSpec{
		Datacenter: "colo:test",
		Servers: []armadav1.ServerSpec{{
			OrbID:         "colo:srv-1",
			ServiceTag:    "T1",
			IdracSettings: armadav1.IdracSettingsSpec{RacadmEnabled: ptr.To(true)},
		}},
	}
	mf := managedFieldsClaim(t, "local:admin", map[string]any{
		"f:spec": map[string]any{
			"f:servers": map[string]any{
				`k:{"orbId":"colo:srv-1"}`: map[string]any{
					"f:idracSettings": map[string]any{"f:racadmEnabled": map[string]any{}},
				},
			},
		},
	})

	out, err := omitAdminOwnedFields(intent, live, mf)
	if err != nil {
		t.Fatalf("omit: %v", err)
	}
	if len(out.Servers) > 0 && out.Servers[0].IdracSettings.RacadmEnabled != nil {
		t.Errorf("ignored field must be OMITTED from apply body regardless of value match (Ignore preserves local ownership); got %v",
			*out.Servers[0].IdracSettings.RacadmEnabled)
	}
}

func TestOmitAdminOwnedFields_IgnoredWithoutLocalClaim_StaysInApplyForReclaim(t *testing.T) {
	// Under ADR-009, an Ignored field with no active local:* claim is reclaimed
	// by cb-controller with intent value. The Ignore directive is meaningless
	// without an active override — the operator either never claimed the field,
	// or released it (handback). Either way, intent should land.
	intent := armadav1.ConfigBundleSpec{
		Datacenter: "colo:test",
		Servers: []armadav1.ServerSpec{{
			OrbID:         "colo:srv-1",
			ServiceTag:    "T1",
			IdracSettings: armadav1.IdracSettingsSpec{RacadmEnabled: ptr.To(true)},
		}},
		Ignored: []armadav1.IgnoredEntry{{
			OrbID: "colo:srv-1-idrac", ServerOrbID: "colo:srv-1", Field: "racadmEnabled",
		}},
	}
	live := armadav1.ConfigBundleSpec{
		Datacenter: "colo:test",
		Servers: []armadav1.ServerSpec{{
			OrbID:         "colo:srv-1",
			ServiceTag:    "T1",
			IdracSettings: armadav1.IdracSettingsSpec{RacadmEnabled: ptr.To(true)},
		}},
	}
	// No managedFields entries for local:* — controller currently sole owner.
	out, err := omitAdminOwnedFields(intent, live, nil)
	if err != nil {
		t.Fatalf("omit: %v", err)
	}
	if len(out.Servers) == 0 || out.Servers[0].IdracSettings.RacadmEnabled == nil {
		t.Fatalf("ignored field with no local:* claim must be RETAINED in apply body (ADR-009 reclaim semantic); got nil")
	}
	if *out.Servers[0].IdracSettings.RacadmEnabled != true {
		t.Errorf("expected intent value true to flow through; got %v", *out.Servers[0].IdracSettings.RacadmEnabled)
	}
}

// TestOmitAdminOwnedFields_BatchSiblingsOnSameServer pins the original
// user-reported bug: Reject sshEnabled + Accept ipmiEnabled on the same
// IdracSettings. Reject is in takeover; Accept's mutation already set intent
// = override (true), so values match for ipmiEnabled and it should be kept.
// Both fields must end up in the apply body — neither silently bowed out.
func TestOmitAdminOwnedFields_BatchSiblingsOnSameServer(t *testing.T) {
	intent := armadav1.ConfigBundleSpec{
		Datacenter: "colo:test",
		Servers: []armadav1.ServerSpec{{
			OrbID:      "colo:srv-1",
			ServiceTag: "T1",
			IdracSettings: armadav1.IdracSettingsSpec{
				SSHEnabled:  ptr.To(false), // reject: keep intent at false
				IPMIEnabled: ptr.To(true),  // accept: intent updated to override
			},
		}},
		Takeover: []armadav1.TakeoverEntry{{
			OrbID: "colo:srv-1-idrac", ServerOrbID: "colo:srv-1", Field: "sshEnabled",
		}},
	}
	live := armadav1.ConfigBundleSpec{
		Datacenter: "colo:test",
		Servers: []armadav1.ServerSpec{{
			OrbID:      "colo:srv-1",
			ServiceTag: "T1",
			IdracSettings: armadav1.IdracSettingsSpec{
				SSHEnabled:  ptr.To(true), // override still at edge
				IPMIEnabled: ptr.To(true), // override matches new intent post-Accept
			},
		}},
	}
	mf := managedFieldsClaim(t, "local:admin", map[string]any{
		"f:spec": map[string]any{
			"f:servers": map[string]any{
				`k:{"orbId":"colo:srv-1"}`: map[string]any{
					"f:idracSettings": map[string]any{
						"f:sshEnabled":  map[string]any{},
						"f:ipmiEnabled": map[string]any{},
					},
				},
			},
		},
	})

	out, err := omitAdminOwnedFields(intent, live, mf)
	if err != nil {
		t.Fatalf("omit: %v", err)
	}
	srv := out.Servers[0]
	if srv.IdracSettings.SSHEnabled == nil || *srv.IdracSettings.SSHEnabled != false {
		t.Errorf("sshEnabled (in takeover) must stay in apply; got %v", srv.IdracSettings.SSHEnabled)
	}
	if srv.IdracSettings.IPMIEnabled == nil || *srv.IdracSettings.IPMIEnabled != true {
		t.Errorf("ipmiEnabled (values match post-Accept) must stay for auto-claim; got %v", srv.IdracSettings.IPMIEnabled)
	}
}

// --- Unit tests for collectLocalClaimedKeys + filterActiveIgnored (ADR-009) ---

func TestCollectLocalClaimedKeys_LocalIdracClaim(t *testing.T) {
	mf := managedFieldsClaim(t, "local:admin", map[string]any{
		"f:spec": map[string]any{
			"f:servers": map[string]any{
				`k:{"orbId":"colo:srv-1"}`: map[string]any{
					"f:idracSettings": map[string]any{"f:racadmEnabled": map[string]any{}},
				},
			},
		},
	})
	got := collectLocalClaimedKeys(mf)
	if !got["colo:srv-1|racadmEnabled"] {
		t.Errorf("expected key colo:srv-1|racadmEnabled, got %v", got)
	}
}

func TestCollectLocalClaimedKeys_IgnoresControllerManager(t *testing.T) {
	mf := managedFieldsClaim(t, "configbundle-controller", map[string]any{
		"f:spec": map[string]any{
			"f:servers": map[string]any{
				`k:{"orbId":"colo:srv-1"}`: map[string]any{
					"f:idracSettings": map[string]any{"f:racadmEnabled": map[string]any{}},
				},
			},
		},
	})
	got := collectLocalClaimedKeys(mf)
	if len(got) != 0 {
		t.Errorf("controller-owned fields must not appear; got %v", got)
	}
}

func TestFilterActiveIgnored_DropsStaleEntry(t *testing.T) {
	entries := []armadav1.IgnoredEntry{
		{ServerOrbID: "colo:srv-1", Field: "racadmEnabled"},
		{ServerOrbID: "colo:srv-2", Field: "sshEnabled"},
	}
	claimed := map[string]bool{
		"colo:srv-2|sshEnabled": true,
	}
	got := filterActiveIgnored(entries, claimed)
	if len(got) != 1 {
		t.Fatalf("expected 1 kept entry, got %d: %v", len(got), got)
	}
	if got[0].ServerOrbID != "colo:srv-2" {
		t.Errorf("wrong entry kept: %v", got[0])
	}
}

func TestFilterActiveIgnored_AllStale(t *testing.T) {
	entries := []armadav1.IgnoredEntry{
		{ServerOrbID: "colo:srv-1", Field: "racadmEnabled"},
	}
	got := filterActiveIgnored(entries, map[string]bool{})
	if len(got) != 0 {
		t.Errorf("expected all entries dropped (no claims); got %v", got)
	}
}

func TestFilterActiveIgnored_EmptyInputPassthrough(t *testing.T) {
	got := filterActiveIgnored(nil, map[string]bool{"colo:srv-1|sshEnabled": true})
	if got != nil {
		t.Errorf("nil input should pass through unchanged; got %v", got)
	}
}
