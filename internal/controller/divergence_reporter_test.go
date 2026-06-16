package controller

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

	armadav1 "github.com/armada/configbundle/api/v1"
	"github.com/armada/configbundle/bundle"
)

func TestExtractAdminPaths_NoAdminEntries(t *testing.T) {
	fields := []metav1.ManagedFieldsEntry{
		{Manager: "configbundle-controller", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{}`)}},
	}
	paths := extractAdminPaths(fields)
	if len(paths) != 0 {
		t.Errorf("expected 0 paths, got %d", len(paths))
	}
}

func TestExtractAdminPaths_SimpleField(t *testing.T) {
	now := metav1.Now()
	fieldsJSON := `{
		"f:spec": {
			"f:datacenter": {}
		}
	}`
	fields := []metav1.ManagedFieldsEntry{
		{
			Manager:  "local:admin",
			Time:     &now,
			FieldsV1: &metav1.FieldsV1{Raw: []byte(fieldsJSON)},
		},
	}
	paths := extractAdminPaths(fields)
	if len(paths) != 1 {
		t.Fatalf("expected 1 path, got %d: %+v", len(paths), paths)
	}
	if paths[0].path != "spec.datacenter" {
		t.Errorf("expected spec.datacenter, got %q", paths[0].path)
	}
}

func TestExtractAdminPaths_NestedServerField(t *testing.T) {
	now := metav1.Now()
	fieldsJSON := `{
		"f:spec": {
			"f:servers": {
				"k:{\"orbId\":\"colo:srv-3rk3v64\"}": {
					"f:idrac": {
						"f:sshEnabled": {}
					}
				}
			}
		}
	}`
	fields := []metav1.ManagedFieldsEntry{
		{
			Manager:  "local:admin",
			Time:     &now,
			FieldsV1: &metav1.FieldsV1{Raw: []byte(fieldsJSON)},
		},
	}
	paths := extractAdminPaths(fields)
	if len(paths) != 1 {
		t.Fatalf("expected 1 path, got %d: %+v", len(paths), paths)
	}
	expected := "spec.servers[orbId=colo:srv-3rk3v64].idrac.sshEnabled"
	if paths[0].path != expected {
		t.Errorf("expected %q, got %q", expected, paths[0].path)
	}
}

func TestExtractAdminPaths_MultipleFields(t *testing.T) {
	now := metav1.Now()
	fieldsJSON := `{
		"f:spec": {
			"f:servers": {
				"k:{\"orbId\":\"colo:srv-3rk3v64\"}": {
					"f:idrac": {
						"f:sshEnabled": {},
						"f:ipmiEnabled": {}
					}
				}
			}
		}
	}`
	fields := []metav1.ManagedFieldsEntry{
		{
			Manager:  "local:admin",
			Time:     &now,
			FieldsV1: &metav1.FieldsV1{Raw: []byte(fieldsJSON)},
		},
	}
	paths := extractAdminPaths(fields)
	if len(paths) != 2 {
		t.Fatalf("expected 2 paths, got %d: %+v", len(paths), paths)
	}
	pathSet := map[string]bool{}
	for _, p := range paths {
		pathSet[p.path] = true
	}
	for _, want := range []string{
		"spec.servers[orbId=colo:srv-3rk3v64].idrac.sshEnabled",
		"spec.servers[orbId=colo:srv-3rk3v64].idrac.ipmiEnabled",
	} {
		if !pathSet[want] {
			t.Errorf("missing expected path %q, got %v", want, pathSet)
		}
	}
}

func TestSplitPath(t *testing.T) {
	tests := []struct {
		input string
		want  []pathPart
	}{
		{"spec.datacenter", []pathPart{{field: "spec"}, {field: "datacenter"}}},
		{
			"spec.servers[orbId=colo:srv-3rk3v64].idrac.sshEnabled",
			[]pathPart{
				{field: "spec"},
				{field: "servers"},
				{selector: "colo:srv-3rk3v64"},
				{field: "idrac"},
				{field: "sshEnabled"},
			},
		},
	}
	for _, tt := range tests {
		got := splitPath(tt.input)
		if len(got) != len(tt.want) {
			t.Errorf("splitPath(%q): got %d parts, want %d: %+v", tt.input, len(got), len(tt.want), got)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("splitPath(%q)[%d]: got %+v, want %+v", tt.input, i, got[i], tt.want[i])
			}
		}
	}
}

func TestResolveValue(t *testing.T) {
	spec := armadav1.ConfigBundleSpec{
		OrbID:      "colo:colo",
		Datacenter: "colo",
		Servers: []armadav1.ServerSpec{
			{
				OrbID:      "colo:srv-3rk3v64",
				ServiceTag: "3RK3V64",
				Hostname:   ptr.To("host-01"),
				OobIP:      ptr.To("10.10.1.45"),
				Idrac: armadav1.IdracSpec{
					SSHEnabled:      ptr.To(false),
					FirmwareVersion: ptr.To("7.20.10.05"),
				},
			},
		},
	}

	tests := []struct {
		path string
		want interface{}
	}{
		{"spec.datacenter", "colo"},
		{"spec.servers[orbId=colo:srv-3rk3v64].hostname", ptr.To("host-01")},
		{"spec.servers[orbId=colo:srv-3rk3v64].idrac.sshEnabled", ptr.To(false)},
		{"spec.servers[orbId=colo:srv-3rk3v64].idrac.firmwareVersion", ptr.To("7.20.10.05")},
		{"spec.servers[orbId=colo:srv-nonexist].hostname", nil},
	}

	for _, tt := range tests {
		got := resolveValue(spec, tt.path)
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("resolveValue(%q): got %v (%T), want %v (%T)", tt.path, got, got, tt.want, tt.want)
		}
	}
}

func testMapping(t *testing.T) *bundle.MappingPayload {
	t.Helper()
	m, err := ParseMapping([]byte(`{
		"bundleDigest": "sha256:abc",
		"rules": [
			{"listField": "spec.servers", "itemKey": "orbId", "field": "idrac", "type": "IdracSettings", "orbIdSuffix": "-idrac"}
		]
	}`))
	if err != nil {
		t.Fatalf("ParseMapping: %v", err)
	}
	return m
}

func TestExtractOverrides_NoDivergence(t *testing.T) {
	spec := armadav1.ConfigBundleSpec{
		OrbID:      "colo:colo",
		Datacenter: "colo",
		Servers: []armadav1.ServerSpec{
			{
				OrbID:      "colo:srv-3rk3v64",
				ServiceTag: "3RK3V64",
				Hostname:   ptr.To("host-01"),
				Idrac:      armadav1.IdracSpec{SSHEnabled: ptr.To(false)},
			},
		},
	}

	now := metav1.Now()
	fieldsJSON := `{
		"f:spec": {
			"f:servers": {
				"k:{\"orbId\":\"colo:srv-3rk3v64\"}": {
					"f:idrac": {
						"f:sshEnabled": {}
					}
				}
			}
		}
	}`

	cb := &armadav1.ConfigBundle{
		ObjectMeta: metav1.ObjectMeta{
			Name: "colo",
			ManagedFields: []metav1.ManagedFieldsEntry{
				{
					Manager:  "local:admin",
					Time:     &now,
					FieldsV1: &metav1.FieldsV1{Raw: []byte(fieldsJSON)},
				},
			},
		},
		Spec:   spec,
		Status: armadav1.ConfigBundleStatus{LastAppliedDigest: "sha256:abc"},
	}

	r := &DivergenceReporter{
		lastManifests: map[string]armadav1.ConfigBundleSpec{"colo": spec},
	}

	overrides := r.extractOverrides(cb, testMapping(t), spec)
	if len(overrides) != 0 {
		t.Errorf("expected 0 overrides (no divergence), got %d: %+v", len(overrides), overrides)
	}
}

func TestExtractOverrides_WithDivergence(t *testing.T) {
	intended := armadav1.ConfigBundleSpec{
		OrbID:      "colo:colo",
		Datacenter: "colo",
		Servers: []armadav1.ServerSpec{
			{
				OrbID:      "colo:srv-3rk3v64",
				ServiceTag: "3RK3V64",
				Hostname:   ptr.To("host-01"),
				Idrac:      armadav1.IdracSpec{SSHEnabled: ptr.To(false)},
			},
		},
	}

	current := armadav1.ConfigBundleSpec{
		OrbID:      "colo:colo",
		Datacenter: "colo",
		Servers: []armadav1.ServerSpec{
			{
				OrbID:      "colo:srv-3rk3v64",
				ServiceTag: "3RK3V64",
				Hostname:   ptr.To("host-01"),
				Idrac:      armadav1.IdracSpec{SSHEnabled: ptr.To(true)},
			},
		},
	}

	now := metav1.Now()
	fieldsJSON := `{
		"f:spec": {
			"f:servers": {
				"k:{\"orbId\":\"colo:srv-3rk3v64\"}": {
					"f:idrac": {
						"f:sshEnabled": {}
					}
				}
			}
		}
	}`

	cb := &armadav1.ConfigBundle{
		ObjectMeta: metav1.ObjectMeta{
			Name: "colo",
			ManagedFields: []metav1.ManagedFieldsEntry{
				{
					Manager:  "local:admin",
					Time:     &now,
					FieldsV1: &metav1.FieldsV1{Raw: []byte(fieldsJSON)},
				},
			},
		},
		Spec:   current,
		Status: armadav1.ConfigBundleStatus{LastAppliedDigest: "sha256:abc"},
	}

	r := &DivergenceReporter{
		lastManifests: map[string]armadav1.ConfigBundleSpec{"colo": intended},
	}

	overrides := r.extractOverrides(cb, testMapping(t), intended)
	if len(overrides) != 1 {
		t.Fatalf("expected 1 override, got %d: %+v", len(overrides), overrides)
	}

	o := overrides[0]
	if o.OrbID != "colo:srv-3rk3v64-idrac" {
		t.Errorf("orbId: got %q, want colo:srv-3rk3v64-idrac", o.OrbID)
	}
	if o.Field != "sshEnabled" {
		t.Errorf("field: got %q, want sshEnabled", o.Field)
	}
	if o.Type != "IdracSettings" {
		t.Errorf("type: got %q, want IdracSettings", o.Type)
	}
	if v, ok := o.IntendedValue.(*bool); !ok || v == nil || *v != false {
		t.Errorf("intendedValue: got %v (%T)", o.IntendedValue, o.IntendedValue)
	}
	if v, ok := o.OverrideValue.(*bool); !ok || v == nil || *v != true {
		t.Errorf("overrideValue: got %v (%T)", o.OverrideValue, o.OverrideValue)
	}
	if o.Who != "local:admin" {
		t.Errorf("who: got %q", o.Who)
	}
}

// TestExtractOverrides_IgnoreResolution_NoLoop covers the "ignore" resolution case.
// After orbital marks a (orbId, field) as ignore, the bundler omits it from the
// manifest. The reporter must NOT keep re-reporting that field — otherwise orbital
// would receive the same divergence every tick and the loop never closes.
//
// Setup: lastManifests has SSHEnabled=nil (bundler omitted it). cb.Spec has admin's
// override sshEnabled=true. managedFields says local:admin owns the field.
// Expected: zero overrides reported (the field is absent from the intent).
func TestExtractOverrides_IgnoreResolution_NoLoop(t *testing.T) {
	// Manifest WITHOUT the sshEnabled field (omitted by bundler).
	intended := armadav1.ConfigBundleSpec{
		OrbID:      "colo:colo",
		Datacenter: "colo",
		Servers: []armadav1.ServerSpec{
			{
				OrbID:      "colo:srv-3rk3v64",
				ServiceTag: "3RK3V64",
				Hostname:   ptr.To("host-01"),
				// Idrac.SSHEnabled intentionally nil — orbital resolved as ignore.
				Idrac: armadav1.IdracSpec{RacadmEnabled: ptr.To(false)},
			},
		},
	}

	// Current CR state: admin has set sshEnabled=true.
	current := armadav1.ConfigBundleSpec{
		OrbID:      "colo:colo",
		Datacenter: "colo",
		Servers: []armadav1.ServerSpec{
			{
				OrbID:      "colo:srv-3rk3v64",
				ServiceTag: "3RK3V64",
				Hostname:   ptr.To("host-01"),
				Idrac: armadav1.IdracSpec{
					SSHEnabled:    ptr.To(true), // admin override
					RacadmEnabled: ptr.To(false),
				},
			},
		},
	}

	now := metav1.Now()
	fieldsJSON := `{
		"f:spec": {
			"f:servers": {
				"k:{\"orbId\":\"colo:srv-3rk3v64\"}": {
					"f:idrac": {
						"f:sshEnabled": {}
					}
				}
			}
		}
	}`

	cb := &armadav1.ConfigBundle{
		ObjectMeta: metav1.ObjectMeta{
			Name: "colo",
			ManagedFields: []metav1.ManagedFieldsEntry{
				{
					Manager:  "local:admin",
					Time:     &now,
					FieldsV1: &metav1.FieldsV1{Raw: []byte(fieldsJSON)},
				},
			},
		},
		Spec:   current,
		Status: armadav1.ConfigBundleStatus{LastAppliedDigest: "sha256:abc"},
	}

	r := &DivergenceReporter{
		lastManifests: map[string]armadav1.ConfigBundleSpec{"colo": intended},
	}

	overrides := r.extractOverrides(cb, testMapping(t), intended)
	if len(overrides) != 0 {
		t.Errorf("expected 0 overrides (field not in manifest = ignore-resolved), got %d: %+v", len(overrides), overrides)
	}
}

func TestIntendedAbsent(t *testing.T) {
	var nilBool *bool
	var nilStr *string
	var nilMap map[string]int

	tests := []struct {
		name string
		v    interface{}
		want bool
	}{
		{"nil interface", nil, true},
		{"typed-nil *bool", nilBool, true},
		{"typed-nil *string", nilStr, true},
		{"nil map", nilMap, true},
		{"non-nil *bool", ptr.To(true), false},
		{"non-nil *string", ptr.To("x"), false},
		{"plain bool", true, false},
		{"plain string", "x", false},
	}
	for _, tt := range tests {
		if got := intendedAbsent(tt.v); got != tt.want {
			t.Errorf("intendedAbsent(%s): got %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestFieldKeyToPath(t *testing.T) {
	tests := []struct {
		prefix string
		key    string
		want   string
	}{
		{"", "f:spec", "spec"},
		{"spec", "f:datacenter", "spec.datacenter"},
		{"spec.servers", `k:{"orbId":"colo:srv-3rk3v64"}`, "spec.servers[orbId=colo:srv-3rk3v64]"},
		{"", "v:something", ""},
	}
	for _, tt := range tests {
		got := fieldKeyToPath(tt.prefix, tt.key)
		if got != tt.want {
			t.Errorf("fieldKeyToPath(%q, %q): got %q, want %q", tt.prefix, tt.key, got, tt.want)
		}
	}
}

func TestDivergencePayload_JSON(t *testing.T) {
	payload := DivergencePayload{
		Overrides: []OverrideEntry{
			{
				OrbID:         "colo:srv-001-idrac",
				Field:         "sshEnabled",
				Type:          "IdracSettings",
				IntendedValue: false,
				OverrideValue: true,
				Who:           "local:admin",
				When:          time.Now().Format(time.RFC3339),
			},
		},
	}

	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded DivergencePayload
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.Overrides) != 1 {
		t.Fatalf("overrides: got %d", len(decoded.Overrides))
	}
	if decoded.Overrides[0].OrbID != "colo:srv-001-idrac" {
		t.Errorf("orbId: got %q", decoded.Overrides[0].OrbID)
	}
	if decoded.Overrides[0].Field != "sshEnabled" {
		t.Errorf("field: got %q", decoded.Overrides[0].Field)
	}
	if decoded.Overrides[0].Type != "IdracSettings" {
		t.Errorf("type: got %q", decoded.Overrides[0].Type)
	}

	// Suppress unused import warning — runtime is used by managedFields FieldsV1.
	_ = runtime.RawExtension{}
}

// --- localManagersChanged unit tests ---

func TestLocalManagersChanged_NoLocalManagers(t *testing.T) {
	old := []metav1.ManagedFieldsEntry{
		{Manager: "configbundle-controller", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{}`)}},
	}
	new := []metav1.ManagedFieldsEntry{
		{Manager: "configbundle-controller", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:spec":{}}`)}},
	}
	if localManagersChanged(old, new) {
		t.Error("expected false when no local:* managers exist in either slice")
	}
}

func TestLocalManagersChanged_LocalManagerAppears(t *testing.T) {
	old := []metav1.ManagedFieldsEntry{
		{Manager: "configbundle-controller", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{}`)}},
	}
	new := []metav1.ManagedFieldsEntry{
		{Manager: "configbundle-controller", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{}`)}},
		{Manager: "local:admin", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:spec":{}}`)}},
	}
	if !localManagersChanged(old, new) {
		t.Error("expected true when local:admin appears in new but not old")
	}
}

func TestLocalManagersChanged_LocalManagerChanges(t *testing.T) {
	old := []metav1.ManagedFieldsEntry{
		{Manager: "local:admin", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:servers":{}}}`)}},
	}
	new := []metav1.ManagedFieldsEntry{
		{Manager: "local:admin", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:servers":{"f:sshEnabled":{}}}}`)}},
	}
	if !localManagersChanged(old, new) {
		t.Error("expected true when local:admin FieldsV1 content changes")
	}
}

func TestLocalManagersChanged_LocalManagerUnchanged(t *testing.T) {
	raw := []byte(`{"f:spec":{"f:sshEnabled":{}}}`)
	old := []metav1.ManagedFieldsEntry{
		{Manager: "local:admin", FieldsV1: &metav1.FieldsV1{Raw: raw}},
	}
	new := []metav1.ManagedFieldsEntry{
		{Manager: "local:admin", FieldsV1: &metav1.FieldsV1{Raw: raw}},
	}
	if localManagersChanged(old, new) {
		t.Error("expected false when local:admin entry is byte-identical")
	}
}

// --- contentHash unit tests ---

func TestContentHash_SamePayloadSameHash(t *testing.T) {
	p := DivergencePayload{
		Overrides: []OverrideEntry{
			{OrbID: "colo:srv-001-idrac", Field: "sshEnabled", Type: "IdracSettings", Who: "local:admin"},
		},
	}
	h1 := contentHash(p)
	h2 := contentHash(p)
	if h1 != h2 {
		t.Error("same payload should produce same hash")
	}
}

func TestContentHash_DifferentOrderSameHash(t *testing.T) {
	p1 := DivergencePayload{
		Overrides: []OverrideEntry{
			{OrbID: "aaa", Field: "f1"},
			{OrbID: "bbb", Field: "f2"},
		},
	}
	p2 := DivergencePayload{
		Overrides: []OverrideEntry{
			{OrbID: "bbb", Field: "f2"},
			{OrbID: "aaa", Field: "f1"},
		},
	}
	if contentHash(p1) != contentHash(p2) {
		t.Error("overrides in different order should produce the same hash")
	}
}

func TestContentHash_DifferentPayloadDifferentHash(t *testing.T) {
	p1 := DivergencePayload{
		Overrides: []OverrideEntry{
			{OrbID: "colo:srv-001-idrac", Field: "sshEnabled"},
		},
	}
	p2 := DivergencePayload{
		Overrides: []OverrideEntry{
			{OrbID: "colo:srv-001-idrac", Field: "ipmiEnabled"},
		},
	}
	if contentHash(p1) == contentHash(p2) {
		t.Error("different payloads should produce different hashes")
	}
}
