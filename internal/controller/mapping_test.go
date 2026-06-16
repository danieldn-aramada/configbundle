package controller

import (
	"strings"
	"testing"

	"github.com/armada/configbundle/bundle"
)

func newTestMapping(t *testing.T) *bundle.MappingPayload {
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

func TestResolve_IdracField(t *testing.T) {
	m := newTestMapping(t)
	cases := []struct {
		name      string
		path      string
		wantOrbID string
		wantField string
		wantType  string
	}{
		{
			name:      "idrac sshEnabled resolves",
			path:      "spec.servers[orbId=colo:srv-3rk3v64].idrac.sshEnabled",
			wantOrbID: "colo:srv-3rk3v64-idrac",
			wantField: "sshEnabled",
			wantType:  "IdracSettings",
		},
		{
			name:      "different server resolves independently",
			path:      "spec.servers[orbId=colo:srv-jl3pv82].idrac.ipmiEnabled",
			wantOrbID: "colo:srv-jl3pv82-idrac",
			wantField: "ipmiEnabled",
			wantType:  "IdracSettings",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotOrbID, gotField, gotType, err := m.Resolve(tc.path)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tc.path, err)
			}
			if gotOrbID != tc.wantOrbID {
				t.Errorf("orbId: got %q, want %q", gotOrbID, tc.wantOrbID)
			}
			if gotField != tc.wantField {
				t.Errorf("field: got %q, want %q", gotField, tc.wantField)
			}
			if gotType != tc.wantType {
				t.Errorf("type: got %q, want %q", gotType, tc.wantType)
			}
		})
	}
}

func TestResolve_PathNotMatchingRule_Errors(t *testing.T) {
	m := newTestMapping(t)
	cases := []string{
		"status.foo",                                          // not under spec
		"spec.datacenter",                                     // top-level DC field (no rule today)
		"spec.servers[orbId=colo:srv-3rk3v64].hostname",       // top-level Server field (no rule)
		"spec.servers[orbId=colo:srv-3rk3v64].idrac",          // ConfigItem boundary, no leaf
		"spec.servers[orbId=colo:srv-3rk3v64].idrac.foo.bar",  // leaf is deeper than one segment
	}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			_, _, _, err := m.Resolve(p)
			if err == nil {
				t.Fatalf("expected error for %q, got nil", p)
			}
			if !strings.Contains(err.Error(), "no mapping rule matches") {
				t.Errorf("unexpected error message: %v", err)
			}
		})
	}
}

func TestResolveByOrbID_StripsSuffix(t *testing.T) {
	m := newTestMapping(t)
	rule, parent, ok := m.ResolveByOrbID("colo:srv-3rk3v64-idrac")
	if !ok {
		t.Fatal("expected match on -idrac suffix")
	}
	if parent != "colo:srv-3rk3v64" {
		t.Errorf("parent: got %q, want colo:srv-3rk3v64", parent)
	}
	if rule.Type != "IdracSettings" {
		t.Errorf("rule type: got %q, want IdracSettings", rule.Type)
	}
}

func TestResolveByOrbID_UnknownSuffix(t *testing.T) {
	m := newTestMapping(t)
	if _, _, ok := m.ResolveByOrbID("colo:srv-3rk3v64-bios"); ok {
		t.Error("no rule has -bios suffix; expected no match")
	}
	if _, _, ok := m.ResolveByOrbID("not-an-orbid"); ok {
		t.Error("unsuffixed string must not match any rule")
	}
}

func TestParseMapping_EmptyRules(t *testing.T) {
	_, err := ParseMapping([]byte(`{"bundleDigest":"sha256:abc","rules":[]}`))
	if err == nil {
		t.Fatal("expected error for empty rules, got nil")
	}
}

func TestParseMapping_InvalidJSON(t *testing.T) {
	_, err := ParseMapping([]byte(`{not json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestParseMapping_RuleMissingField(t *testing.T) {
	_, err := ParseMapping([]byte(`{
		"bundleDigest":"sha256:abc",
		"rules":[{"listField":"spec.servers","itemKey":"orbId","field":"idrac","type":"IdracSettings"}]
	}`))
	if err == nil {
		t.Fatal("expected error for rule missing orbIdSuffix, got nil")
	}
}
