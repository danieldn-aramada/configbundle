package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	armadav1 "github.com/armada/configbundle/api/v1"
)

func newStampFakeClient(t *testing.T, objs ...armadav1.ServerConfig) *fake.ClientBuilder {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := armadav1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	runtimeObjs := make([]runtime.Object, len(objs))
	for i := range objs {
		runtimeObjs[i] = &objs[i]
	}
	_ = runtimeObjs
	builder := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&armadav1.ServerConfig{})
	for i := range objs {
		builder = builder.WithObjects(&objs[i])
	}
	return builder
}

// TestStampBundleProvenance_ExistingServerConfig verifies that stampBundleProvenance
// writes LastAppliedVersion and LastAppliedDigest onto existing ServerConfig statuses.
func TestStampBundleProvenance_ExistingServerConfig(t *testing.T) {
	hostname := "r09-u06.colo-galleon"
	sc := armadav1.ServerConfig{
		ObjectMeta: metav1.ObjectMeta{Name: hostname},
		Spec:       armadav1.ServerConfigSpec{ServiceTag: "CFRHDX3"},
	}
	c := newStampFakeClient(t, sc).Build()

	s := &ConsumeServer{Client: c}
	spec := armadav1.ConfigBundleSpec{
		Datacenter: "colo-galleon",
		Servers: []armadav1.ServerSpec{
			{Hostname: strPtr(hostname), ServiceTag: "CFRHDX3"},
		},
	}

	s.stampBundleProvenance(context.Background(), spec, "v42", "sha256:deadbeef")

	var got armadav1.ServerConfig
	if err := c.Get(context.Background(), types.NamespacedName{Name: hostname}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.LastAppliedBundle == nil {
		t.Fatal("LastAppliedBundle is nil, want populated")
	}
	if got.Status.LastAppliedBundle.Version != "v42" {
		t.Errorf("LastAppliedBundle.Version = %q, want %q", got.Status.LastAppliedBundle.Version, "v42")
	}
	if got.Status.LastAppliedBundle.Digest != "sha256:deadbeef" {
		t.Errorf("LastAppliedBundle.Digest = %q, want %q", got.Status.LastAppliedBundle.Digest, "sha256:deadbeef")
	}
	if got.Status.LastAppliedBundle.AppliedAt == nil {
		t.Errorf("LastAppliedBundle.AppliedAt is nil, want a timestamp")
	}
}

// TestStampBundleProvenance_MissingServerConfig verifies that stampBundleProvenance
// does not error when a ServerConfig does not yet exist (new server, first dispatch).
func TestStampBundleProvenance_MissingServerConfig(t *testing.T) {
	c := newStampFakeClient(t).Build()

	s := &ConsumeServer{Client: c}
	spec := armadav1.ConfigBundleSpec{
		Datacenter: "colo-galleon",
		Servers: []armadav1.ServerSpec{
			{Hostname: strPtr("r09-u99.colo-galleon"), ServiceTag: "NEWST01"},
		},
	}

	// Must not panic or error — NotFound is silently ignored.
	s.stampBundleProvenance(context.Background(), spec, "v1", "sha256:abc")
}

// TestStampBundleProvenance_EmptyTag verifies no status writes occur when tag is empty.
func TestStampBundleProvenance_EmptyTag(t *testing.T) {
	hostname := "r09-u06.colo-galleon"
	sc := armadav1.ServerConfig{
		ObjectMeta: metav1.ObjectMeta{Name: hostname},
		Spec:       armadav1.ServerConfigSpec{ServiceTag: "CFRHDX3"},
	}
	c := newStampFakeClient(t, sc).Build()

	s := &ConsumeServer{Client: c}
	spec := armadav1.ConfigBundleSpec{
		Servers: []armadav1.ServerSpec{
			{Hostname: strPtr(hostname)},
		},
	}

	s.stampBundleProvenance(context.Background(), spec, "", "")

	var got armadav1.ServerConfig
	if err := c.Get(context.Background(), types.NamespacedName{Name: hostname}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.LastAppliedBundle != nil {
		t.Errorf("expected LastAppliedBundle to remain nil on empty tag, got %+v", got.Status.LastAppliedBundle)
	}
}

func strPtr(s string) *string { return &s }
