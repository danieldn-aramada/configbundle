package controller

import (
	"testing"

	armadav1 "github.com/armada/configbundle/api/v1"
)

func TestSetFieldOnServer_IdracFields(t *testing.T) {
	src := &armadav1.ServerSpec{
		ServiceTag: "3RK3V64",
		Hostname:   "host-01",
		OobIP:      "10.10.1.45",
		Idrac: armadav1.IdracSpec{
			FirmwareVersion:             "7.20.10.05",
			SSHEnabled:                  true,
			IPMIEnabled:                 false,
			LockdownModeEnabled:         true,
			OsToIdracPassThroughEnabled: true,
			UsbManagementPortEnabled:    false,
			DHCPEnabled:                 true,
			RacadmEnabled:               false,
		},
	}

	tests := []struct {
		field string
		check func(dst *armadav1.ServerSpec) bool
	}{
		{"sshEnabled", func(d *armadav1.ServerSpec) bool { return d.Idrac.SSHEnabled == true }},
		{"ipmiEnabled", func(d *armadav1.ServerSpec) bool { return d.Idrac.IPMIEnabled == false }},
		{"lockdownModeEnabled", func(d *armadav1.ServerSpec) bool { return d.Idrac.LockdownModeEnabled == true }},
		{"osToIdracPassThroughEnabled", func(d *armadav1.ServerSpec) bool { return d.Idrac.OsToIdracPassThroughEnabled == true }},
		{"usbManagementPortEnabled", func(d *armadav1.ServerSpec) bool { return d.Idrac.UsbManagementPortEnabled == false }},
		{"dhcpEnabled", func(d *armadav1.ServerSpec) bool { return d.Idrac.DHCPEnabled == true }},
		{"racadmEnabled", func(d *armadav1.ServerSpec) bool { return d.Idrac.RacadmEnabled == false }},
		{"firmwareVersion", func(d *armadav1.ServerSpec) bool { return d.Idrac.FirmwareVersion == "7.20.10.05" }},
		{"hostname", func(d *armadav1.ServerSpec) bool { return d.Hostname == "host-01" }},
		{"oobIP", func(d *armadav1.ServerSpec) bool { return d.OobIP == "10.10.1.45" }},
	}

	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			dst := &armadav1.ServerSpec{ServiceTag: "3RK3V64"}
			if err := setFieldOnServer(dst, src, tt.field); err != nil {
				t.Fatalf("setFieldOnServer(%q): %v", tt.field, err)
			}
			if !tt.check(dst) {
				t.Errorf("field %q not correctly set on dst", tt.field)
			}
			// Verify serviceTag was not overwritten
			if dst.ServiceTag != "3RK3V64" {
				t.Errorf("serviceTag changed: got %q", dst.ServiceTag)
			}
		})
	}
}

func TestSetFieldOnServer_UnknownField(t *testing.T) {
	dst := &armadav1.ServerSpec{}
	src := &armadav1.ServerSpec{}
	err := setFieldOnServer(dst, src, "nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
}

func TestSetFieldOnServer_MinimalPatch(t *testing.T) {
	src := &armadav1.ServerSpec{
		ServiceTag: "3RK3V64",
		Hostname:   "host-01",
		OobIP:      "10.10.1.45",
		Idrac: armadav1.IdracSpec{
			SSHEnabled:    true,
			IPMIEnabled:   true,
			RacadmEnabled: true,
		},
	}

	// Only set sshEnabled — verify other fields remain zero
	dst := &armadav1.ServerSpec{ServiceTag: "3RK3V64"}
	if err := setFieldOnServer(dst, src, "sshEnabled"); err != nil {
		t.Fatal(err)
	}

	if dst.Idrac.SSHEnabled != true {
		t.Error("sshEnabled should be true")
	}
	if dst.Idrac.IPMIEnabled != false {
		t.Error("ipmiEnabled should remain false (zero value)")
	}
	if dst.Idrac.RacadmEnabled != false {
		t.Error("racadmEnabled should remain false (zero value)")
	}
	if dst.Hostname != "" {
		t.Error("hostname should remain empty")
	}
}
