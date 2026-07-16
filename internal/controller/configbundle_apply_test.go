package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"

	armadav1 "github.com/armada/configbundle/api/v1"
)

// TestSpecOnlyUnstructured_StripsStatus pins the hardening that prevents the
// parent's child-apply from coupling to the child's status schema. The parent
// owns child spec + metadata, never status; sending a (zero-value) status is
// what let a status-schema change (removing status.observed) break every child
// SSA when an older cb-controller still serialized it. The apply payload must
// carry spec + apiVersion/kind + ownerReferences, but NOT status.
func TestSpecOnlyUnstructured_StripsStatus(t *testing.T) {
	sc := &armadav1.ServerConfig{
		TypeMeta:   metav1.TypeMeta{APIVersion: armadav1.GroupVersion.String(), Kind: "ServerConfig"},
		ObjectMeta: metav1.ObjectMeta{Name: "srv-1", OwnerReferences: []metav1.OwnerReference{{Kind: "ConfigBundle", Name: "cb-1"}}},
		Spec:       armadav1.ServerConfigSpec{OrbID: "o1", ServiceTag: "T1"},
		// A populated status the parent must NOT send on apply.
		Status: armadav1.ServerConfigStatus{
			IdracSettings: armadav1.ObservedIdracSettingsStatus{SSHEnabled: ptr.To(true)},
		},
	}

	u, err := specOnlyUnstructured(sc)
	if err != nil {
		t.Fatalf("specOnlyUnstructured: %v", err)
	}

	if _, found, _ := unstructured.NestedFieldNoCopy(u.Object, "status"); found {
		t.Errorf("status must be stripped from the child apply payload; got: %v", u.Object["status"])
	}
	if _, found, _ := unstructured.NestedMap(u.Object, "spec"); !found {
		t.Error("spec must be preserved")
	}
	if u.GetAPIVersion() != armadav1.GroupVersion.String() || u.GetKind() != "ServerConfig" {
		t.Errorf("apiVersion/kind must be preserved for SSA; got %q/%q", u.GetAPIVersion(), u.GetKind())
	}
	if len(u.GetOwnerReferences()) != 1 {
		t.Errorf("ownerReferences must be preserved (child GC); got %d", len(u.GetOwnerReferences()))
	}
}
