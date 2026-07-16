package serverconfig

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"k8s.io/utils/ptr"

	armadav1 "github.com/armada/configbundle/api/v1"
)

// TestServerConfigStatus_JSONRoundTrip pins the reshaped status serialization:
// observed iDRAC state lives at status.idracSettings (not the old
// status.observed.idracSettings wrapper), and firmware is folded into
// status.idracSettings.firmwareVersion (not a top-level observedFirmwareVersion).
func TestServerConfigStatus_JSONRoundTrip(t *testing.T) {
	in := armadav1.ServerConfigStatus{
		IdracSettings: armadav1.ObservedIdracSettingsStatus{
			SSHEnabled:      ptr.To(false),
			FirmwareVersion: ptr.To("7.20.10.05"),
		},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(b)
	for _, want := range []string{`"idracSettings":`, `"sshEnabled":false`, `"firmwareVersion":"7.20.10.05"`} {
		if !strings.Contains(js, want) {
			t.Errorf("marshalled status missing %q\ngot: %s", want, js)
		}
	}
	for _, bad := range []string{`"observed":`, `"observedFirmwareVersion":`} {
		if strings.Contains(js, bad) {
			t.Errorf("marshalled status must not contain %q (reshape removed it)\ngot: %s", bad, js)
		}
	}
	var out armadav1.ServerConfigStatus
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.IdracSettings.SSHEnabled == nil || *out.IdracSettings.SSHEnabled {
		t.Errorf("round-trip sshEnabled = %v, want false", out.IdracSettings.SSHEnabled)
	}
	if out.IdracSettings.FirmwareVersion == nil || *out.IdracSettings.FirmwareVersion != "7.20.10.05" {
		t.Errorf("round-trip firmwareVersion = %v, want 7.20.10.05", out.IdracSettings.FirmwareVersion)
	}
}

// allFields returns an allow-everything map for tests that don't exercise the
// field-allowlist gate.
func allFields() map[string]bool {
	out := map[string]bool{}
	for _, f := range KnownIdracFields {
		out[f] = true
	}
	return out
}

func TestComputeIdracDeltas(t *testing.T) {
	cases := []struct {
		name    string
		spec    armadav1.IdracSettingsSpec
		live    map[string]any
		allowed map[string]bool
		want    map[string]string
	}{
		{
			name:    "no intent set, no deltas",
			spec:    armadav1.IdracSettingsSpec{},
			live:    map[string]any{"SSH.1.Enable": "Enabled", "Racadm.1.Enable": "Enabled"},
			allowed: allFields(),
			want:    map[string]string{},
		},
		{
			name:    "ssh intent matches live, no delta",
			spec:    armadav1.IdracSettingsSpec{SSHEnabled: ptr.To(true)},
			live:    map[string]any{"SSH.1.Enable": "Enabled"},
			allowed: allFields(),
			want:    map[string]string{},
		},
		{
			name:    "ssh intent differs, single-field delta",
			spec:    armadav1.IdracSettingsSpec{SSHEnabled: ptr.To(false)},
			live:    map[string]any{"SSH.1.Enable": "Enabled"},
			allowed: allFields(),
			want:    map[string]string{"SSH.1.Enable": "Disabled"},
		},
		{
			name:    "both intents differ, both keyed in single delta",
			spec:    armadav1.IdracSettingsSpec{SSHEnabled: ptr.To(false), RacadmEnabled: ptr.To(false)},
			live:    map[string]any{"SSH.1.Enable": "Enabled", "Racadm.1.Enable": "Enabled"},
			allowed: allFields(),
			want:    map[string]string{"SSH.1.Enable": "Disabled", "Racadm.1.Enable": "Disabled"},
		},
		{
			name: "all three intents differ, all three keyed in single delta",
			spec: armadav1.IdracSettingsSpec{SSHEnabled: ptr.To(false), RacadmEnabled: ptr.To(false), IPMIEnabled: ptr.To(false)},
			live: map[string]any{
				"SSH.1.Enable":     "Enabled",
				"Racadm.1.Enable":  "Enabled",
				"IPMILan.1.Enable": "Enabled",
			},
			allowed: allFields(),
			want: map[string]string{
				"SSH.1.Enable":     "Disabled",
				"Racadm.1.Enable":  "Disabled",
				"IPMILan.1.Enable": "Disabled",
			},
		},
		{
			name:    "ipmi intent differs, single-field delta",
			spec:    armadav1.IdracSettingsSpec{IPMIEnabled: ptr.To(true)},
			live:    map[string]any{"IPMILan.1.Enable": "Disabled"},
			allowed: allFields(),
			want:    map[string]string{"IPMILan.1.Enable": "Enabled"},
		},
		{
			name:    "policy blocks racadm — only ssh in delta",
			spec:    armadav1.IdracSettingsSpec{SSHEnabled: ptr.To(false), RacadmEnabled: ptr.To(false)},
			live:    map[string]any{"SSH.1.Enable": "Enabled", "Racadm.1.Enable": "Enabled"},
			allowed: map[string]bool{"sshEnabled": true}, // racadmEnabled NOT in allowlist
			want:    map[string]string{"SSH.1.Enable": "Disabled"},
		},
		{
			name:    "empty allowlist — all fields blocked, no delta",
			spec:    armadav1.IdracSettingsSpec{SSHEnabled: ptr.To(false), RacadmEnabled: ptr.To(false)},
			live:    map[string]any{"SSH.1.Enable": "Enabled", "Racadm.1.Enable": "Enabled"},
			allowed: map[string]bool{},
			want:    map[string]string{},
		},
		{
			name:    "case-insensitive live value still matches",
			spec:    armadav1.IdracSettingsSpec{SSHEnabled: ptr.To(true)},
			live:    map[string]any{"SSH.1.Enable": "enabled"},
			allowed: allFields(),
			want:    map[string]string{},
		},
		{
			name:    "live attribute missing — treated as not-Enabled",
			spec:    armadav1.IdracSettingsSpec{SSHEnabled: ptr.To(true)},
			live:    map[string]any{},
			allowed: allFields(),
			want:    map[string]string{"SSH.1.Enable": "Enabled"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeIdracDeltas(tc.spec, tc.live, tc.allowed)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHasReconcilableIntent(t *testing.T) {
	cases := []struct {
		name    string
		spec    armadav1.IdracSettingsSpec
		allowed map[string]bool
		want    bool
	}{
		{"empty spec, empty allowlist", armadav1.IdracSettingsSpec{}, map[string]bool{}, false},
		{"empty spec, full allowlist", armadav1.IdracSettingsSpec{}, allFields(), false},
		{"ssh set, ssh allowed", armadav1.IdracSettingsSpec{SSHEnabled: ptr.To(true)}, map[string]bool{"sshEnabled": true}, true},
		{"ssh set, ssh NOT allowed", armadav1.IdracSettingsSpec{SSHEnabled: ptr.To(true)}, map[string]bool{"racadmEnabled": true}, false},
		{"ssh set + racadm set, only ssh allowed", armadav1.IdracSettingsSpec{SSHEnabled: ptr.To(true), RacadmEnabled: ptr.To(true)}, map[string]bool{"sshEnabled": true}, true},
		{"all set, empty allowlist", armadav1.IdracSettingsSpec{SSHEnabled: ptr.To(true), RacadmEnabled: ptr.To(true)}, map[string]bool{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasReconcilableIntent(tc.spec, tc.allowed); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUnmanagedFields(t *testing.T) {
	cases := []struct {
		name string
		spec armadav1.IdracSettingsSpec
		want []string
	}{
		{"empty spec — all unmanaged", armadav1.IdracSettingsSpec{}, []string{"sshEnabled", "racadmEnabled", "ipmiEnabled"}},
		{"only ssh set", armadav1.IdracSettingsSpec{SSHEnabled: ptr.To(true)}, []string{"racadmEnabled", "ipmiEnabled"}},
		{"only racadm set", armadav1.IdracSettingsSpec{RacadmEnabled: ptr.To(false)}, []string{"sshEnabled", "ipmiEnabled"}},
		{"only ipmi set", armadav1.IdracSettingsSpec{IPMIEnabled: ptr.To(true)}, []string{"sshEnabled", "racadmEnabled"}},
		{"all three set — none unmanaged", armadav1.IdracSettingsSpec{SSHEnabled: ptr.To(true), RacadmEnabled: ptr.To(true), IPMIEnabled: ptr.To(true)}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := unmanagedFields(tc.spec)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPolicyBlockedFields(t *testing.T) {
	cases := []struct {
		name    string
		spec    armadav1.IdracSettingsSpec
		allowed map[string]bool
		want    []string
	}{
		{"no intent set — nothing blocked", armadav1.IdracSettingsSpec{}, map[string]bool{}, nil},
		{"intent set, field allowed — not blocked", armadav1.IdracSettingsSpec{SSHEnabled: ptr.To(true)}, map[string]bool{"sshEnabled": true}, nil},
		{"intent set, field NOT allowed — blocked", armadav1.IdracSettingsSpec{SSHEnabled: ptr.To(true)}, map[string]bool{}, []string{"sshEnabled"}},
		{"both intents set, only ssh allowed — racadm blocked", armadav1.IdracSettingsSpec{SSHEnabled: ptr.To(true), RacadmEnabled: ptr.To(true)}, map[string]bool{"sshEnabled": true}, []string{"racadmEnabled"}},
		{"both intents set, empty allowlist — both blocked", armadav1.IdracSettingsSpec{SSHEnabled: ptr.To(true), RacadmEnabled: ptr.To(true)}, map[string]bool{}, []string{"sshEnabled", "racadmEnabled"}},
		{"all three intents set, only racadm allowed — ssh+ipmi blocked", armadav1.IdracSettingsSpec{SSHEnabled: ptr.To(true), RacadmEnabled: ptr.To(true), IPMIEnabled: ptr.To(true)}, map[string]bool{"racadmEnabled": true}, []string{"sshEnabled", "ipmiEnabled"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := policyBlockedFields(tc.spec, tc.allowed)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUnknownAllowlistEntries(t *testing.T) {
	cases := []struct {
		name    string
		allowed map[string]bool
		want    []string
	}{
		{"empty allow set", map[string]bool{}, nil},
		{"all known", map[string]bool{"sshEnabled": true, "racadmEnabled": true}, nil},
		{"one unknown (typo)", map[string]bool{"sshenabled": true}, []string{"sshenabled"}},
		{"mixed known + unknown — only unknown returned", map[string]bool{"sshEnabled": true, "foo": true, "bar": true}, []string{"bar", "foo"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := UnknownAllowlistEntries(tc.allowed)
			if got != nil {
				sort.Strings(got) // already sorted by impl but defensive
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFormatDeltas(t *testing.T) {
	got := formatDeltas(map[string]string{
		"Racadm.1.Enable": "Disabled",
		"SSH.1.Enable":    "Enabled",
	})
	want := "Racadm.1.Enable=Disabled, SSH.1.Enable=Enabled"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestBuildObservedIdrac pins the honest-observed contract: the ledger comes
// from live Redfish attrs, not from spec. Table cases cover:
//   - live present + allowlisted → observed reads live value
//   - live present + NOT allowlisted → observed field stays nil
//   - live absent (missing from firmware response) → observed field stays nil
//   - drift between what "spec would say" and live → observed reports LIVE
func TestBuildObservedIdrac(t *testing.T) {
	allAllowed := map[string]bool{"sshEnabled": true, "ipmiEnabled": true, "racadmEnabled": true}

	cases := []struct {
		name    string
		attrs   map[string]any
		allowed map[string]bool
		wantSSH *bool
		wantIPM *bool
		wantRac *bool
	}{
		{
			name:    "all live values present, all allowlisted",
			attrs:   map[string]any{"SSH.1.Enable": "Enabled", "IPMILan.1.Enable": "Disabled", "Racadm.1.Enable": "Enabled"},
			allowed: allAllowed,
			wantSSH: boolPtrForTest(true),
			wantIPM: boolPtrForTest(false),
			wantRac: boolPtrForTest(true),
		},
		{
			name:    "field not allowlisted is skipped even though live has it",
			attrs:   map[string]any{"SSH.1.Enable": "Enabled", "Racadm.1.Enable": "Enabled"},
			allowed: map[string]bool{"sshEnabled": true}, // no racadm
			wantSSH: boolPtrForTest(true),
			wantRac: nil,
		},
		{
			name:    "field missing from live attrs is skipped (transient firmware quirk)",
			attrs:   map[string]any{"SSH.1.Enable": "Enabled"},
			allowed: allAllowed,
			wantSSH: boolPtrForTest(true),
			wantIPM: nil,
			wantRac: nil,
		},
		{
			name:    "empty attrs → all observed fields nil",
			attrs:   map[string]any{},
			allowed: allAllowed,
			wantSSH: nil,
			wantIPM: nil,
			wantRac: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildObservedIdrac(tc.attrs, tc.allowed)
			if !boolPtrEqualForTest(got.SSHEnabled, tc.wantSSH) {
				t.Errorf("sshEnabled: got %v want %v", got.SSHEnabled, tc.wantSSH)
			}
			if !boolPtrEqualForTest(got.IPMIEnabled, tc.wantIPM) {
				t.Errorf("ipmiEnabled: got %v want %v", got.IPMIEnabled, tc.wantIPM)
			}
			if !boolPtrEqualForTest(got.RacadmEnabled, tc.wantRac) {
				t.Errorf("racadmEnabled: got %v want %v", got.RacadmEnabled, tc.wantRac)
			}
		})
	}
}

func boolPtrForTest(b bool) *bool { return &b }
func boolPtrEqualForTest(a, b *bool) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func TestBoolEnabledStringRoundTrip(t *testing.T) {
	if boolToEnableStr(true) != "Enabled" {
		t.Errorf("true → 'Enabled'")
	}
	if boolToEnableStr(false) != "Disabled" {
		t.Errorf("false → 'Disabled'")
	}
	if !enableStrToBool("Enabled") {
		t.Errorf("'Enabled' → true")
	}
	if !enableStrToBool("enabled") {
		t.Errorf("case-insensitive: 'enabled' → true")
	}
	if enableStrToBool("Disabled") {
		t.Errorf("'Disabled' → false")
	}
	if enableStrToBool("") {
		t.Errorf("empty string → false")
	}
}
