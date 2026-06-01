package controller

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	armadav1 "github.com/armada/configbundle/api/v1"
)

// ---------------------------------------------------------------------------
// parseManifest
// ---------------------------------------------------------------------------

func TestParseManifest(t *testing.T) {
	tests := []struct {
		name        string
		yaml        string
		wantDC      string
		wantServers int
		wantErr     bool
	}{
		{
			name: "valid manifest with one server",
			yaml: `
datacenter: colo
servers:
  - serviceTag: "3RK3V64"
    hostname: colo-r740-01
    oobIP: "10.10.1.45"
    idrac:
      sshEnabled: true
`,
			wantDC:      "colo",
			wantServers: 1,
		},
		{
			name: "valid manifest with no servers",
			yaml: `
datacenter: colo
`,
			wantDC:      "colo",
			wantServers: 0,
		},
		{
			name: "multiple servers",
			yaml: `
datacenter: colo
servers:
  - serviceTag: "AAA0001"
    hostname: colo-r740-01
    oobIP: "10.10.1.45"
  - serviceTag: "BBB0002"
    hostname: colo-r740-02
    oobIP: "10.10.1.46"
`,
			wantDC:      "colo",
			wantServers: 2,
		},
		{
			name:    "invalid yaml returns error",
			yaml:    `{bad yaml: [`,
			wantErr: true,
		},
		{
			name:        "empty manifest",
			yaml:        ``,
			wantDC:      "",
			wantServers: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, err := parseManifest([]byte(tt.yaml))
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseManifest() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if spec.Datacenter != tt.wantDC {
				t.Errorf("Datacenter = %q, want %q", spec.Datacenter, tt.wantDC)
			}
			if len(spec.Servers) != tt.wantServers {
				t.Errorf("len(Servers) = %d, want %d", len(spec.Servers), tt.wantServers)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// adminOwnedServiceTags
// ---------------------------------------------------------------------------

func TestAdminOwnedServiceTags(t *testing.T) {
	// buildEntry constructs a ManagedFieldsEntry with the given manager and
	// a fieldsV1 JSON that marks the listed serviceTags as owned.
	buildEntry := func(manager string, serviceTags ...string) metav1.ManagedFieldsEntry {
		serverFields := map[string]interface{}{}
		for _, tag := range serviceTags {
			keyJSON, _ := json.Marshal(map[string]string{"serviceTag": tag})
			serverFields["k:"+string(keyJSON)] = map[string]interface{}{
				".":          map[string]interface{}{},
				"f:hostname": map[string]interface{}{},
			}
		}
		raw, _ := json.Marshal(map[string]interface{}{
			"f:spec": map[string]interface{}{
				"f:servers": serverFields,
			},
		})
		return metav1.ManagedFieldsEntry{
			Manager:  manager,
			FieldsV1: &metav1.FieldsV1{Raw: raw},
		}
	}

	tests := []struct {
		name          string
		managedFields []metav1.ManagedFieldsEntry
		wantOwned     []string
		wantNotOwned  []string
	}{
		{
			name:          "empty managedFields returns empty set",
			managedFields: nil,
			wantNotOwned:  []string{"3RK3V64"},
		},
		{
			name: "local:admin owns one server entry",
			managedFields: []metav1.ManagedFieldsEntry{
				buildEntry("local:admin", "3RK3V64"),
			},
			wantOwned:    []string{"3RK3V64"},
			wantNotOwned: []string{"FQK3V64"},
		},
		{
			name: "local:admin owns two server entries",
			managedFields: []metav1.ManagedFieldsEntry{
				buildEntry("local:admin", "AAA0001", "BBB0002"),
			},
			wantOwned: []string{"AAA0001", "BBB0002"},
		},
		{
			name: "other managers are ignored",
			managedFields: []metav1.ManagedFieldsEntry{
				buildEntry("configbundle-controller", "3RK3V64"),
			},
			wantNotOwned: []string{"3RK3V64"},
		},
		{
			name: "only local:admin entries contribute",
			managedFields: []metav1.ManagedFieldsEntry{
				buildEntry("configbundle-controller", "AAA0001"),
				buildEntry("local:admin", "BBB0002"),
			},
			wantOwned:    []string{"BBB0002"},
			wantNotOwned: []string{"AAA0001"},
		},
		{
			name: "nil FieldsV1 is skipped without panic",
			managedFields: []metav1.ManagedFieldsEntry{
				{Manager: "local:admin", FieldsV1: nil},
			},
			wantNotOwned: []string{"3RK3V64"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := adminOwnedServiceTags(tt.managedFields)
			for _, tag := range tt.wantOwned {
				if !got[tag] {
					t.Errorf("expected %q to be owned, but it was not; owned = %v", tag, got)
				}
			}
			for _, tag := range tt.wantNotOwned {
				if got[tag] {
					t.Errorf("expected %q NOT to be owned, but it was; owned = %v", tag, got)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// omitAdminOwnedServers
// ---------------------------------------------------------------------------

func TestOmitAdminOwnedServers(t *testing.T) {
	buildManagedFields := func(adminTags ...string) []metav1.ManagedFieldsEntry {
		serverFields := map[string]interface{}{}
		for _, tag := range adminTags {
			keyJSON, _ := json.Marshal(map[string]string{"serviceTag": tag})
			serverFields["k:"+string(keyJSON)] = map[string]interface{}{".": map[string]interface{}{}}
		}
		raw, _ := json.Marshal(map[string]interface{}{
			"f:spec": map[string]interface{}{"f:servers": serverFields},
		})
		return []metav1.ManagedFieldsEntry{
			{Manager: "local:admin", FieldsV1: &metav1.FieldsV1{Raw: raw}},
		}
	}

	servers := func(tags ...string) []armadav1.ServerSpec {
		out := make([]armadav1.ServerSpec, len(tags))
		for i, t := range tags {
			out[i] = armadav1.ServerSpec{ServiceTag: t, Hostname: "host-" + t, OobIP: "10.0.0.1"}
		}
		return out
	}

	tests := []struct {
		name        string
		specServers []string // input serviceTags
		adminOwned  []string // which tags local:admin owns
		wantServers []string // expected remaining tags
	}{
		{
			name:        "no admin overrides — all servers pass through",
			specServers: []string{"AAA", "BBB"},
			adminOwned:  nil,
			wantServers: []string{"AAA", "BBB"},
		},
		{
			name:        "admin owns one entry — it is omitted",
			specServers: []string{"AAA", "BBB"},
			adminOwned:  []string{"AAA"},
			wantServers: []string{"BBB"},
		},
		{
			name:        "admin owns all entries — result is empty",
			specServers: []string{"AAA", "BBB"},
			adminOwned:  []string{"AAA", "BBB"},
			wantServers: []string{},
		},
		{
			name:        "admin owns entry not present in spec — no effect",
			specServers: []string{"AAA"},
			adminOwned:  []string{"ZZZ"},
			wantServers: []string{"AAA"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := armadav1.ConfigBundleSpec{
				Datacenter: "colo",
				Servers:    servers(tt.specServers...),
			}
			var mf []metav1.ManagedFieldsEntry
			if len(tt.adminOwned) > 0 {
				mf = buildManagedFields(tt.adminOwned...)
			}
			got := omitAdminOwnedServers(spec, mf)

			if len(got.Servers) != len(tt.wantServers) {
				t.Fatalf("len(Servers) = %d, want %d; got tags: %v",
					len(got.Servers), len(tt.wantServers), serviceTags(got.Servers))
			}
			for i, want := range tt.wantServers {
				if got.Servers[i].ServiceTag != want {
					t.Errorf("Servers[%d].ServiceTag = %q, want %q", i, got.Servers[i].ServiceTag, want)
				}
			}
		})
	}
}

func serviceTags(servers []armadav1.ServerSpec) []string {
	out := make([]string, len(servers))
	for i, s := range servers {
		out[i] = s.ServiceTag
	}
	return out
}

// ---------------------------------------------------------------------------
// setCondition
// ---------------------------------------------------------------------------

func TestSetCondition(t *testing.T) {
	t.Run("appends new condition when absent", func(t *testing.T) {
		var conditions []metav1.Condition
		setCondition(&conditions, armadav1.ConditionArtifactFetched, metav1.ConditionTrue, "Fetched", "ok")
		if len(conditions) != 1 {
			t.Fatalf("len = %d, want 1", len(conditions))
		}
		if conditions[0].Type != armadav1.ConditionArtifactFetched {
			t.Errorf("Type = %q, want %q", conditions[0].Type, armadav1.ConditionArtifactFetched)
		}
		if conditions[0].Status != metav1.ConditionTrue {
			t.Errorf("Status = %q, want True", conditions[0].Status)
		}
	})

	t.Run("updates existing condition in place", func(t *testing.T) {
		conditions := []metav1.Condition{
			{Type: armadav1.ConditionArtifactFetched, Status: metav1.ConditionFalse, Reason: "old"},
		}
		setCondition(&conditions, armadav1.ConditionArtifactFetched, metav1.ConditionTrue, "Fetched", "ok")
		if len(conditions) != 1 {
			t.Fatalf("len = %d, want 1 (must update in place, not append)", len(conditions))
		}
		if conditions[0].Status != metav1.ConditionTrue {
			t.Errorf("Status = %q, want True", conditions[0].Status)
		}
		if conditions[0].Reason != "Fetched" {
			t.Errorf("Reason = %q, want Fetched", conditions[0].Reason)
		}
	})

	t.Run("does not disturb other conditions", func(t *testing.T) {
		conditions := []metav1.Condition{
			{Type: armadav1.ConditionReconciled, Status: metav1.ConditionTrue, Reason: "Done"},
		}
		setCondition(&conditions, armadav1.ConditionArtifactFetched, metav1.ConditionTrue, "Fetched", "ok")
		if len(conditions) != 2 {
			t.Fatalf("len = %d, want 2", len(conditions))
		}
		if conditions[0].Type != armadav1.ConditionReconciled {
			t.Errorf("existing condition disturbed: Type = %q", conditions[0].Type)
		}
	})
}
