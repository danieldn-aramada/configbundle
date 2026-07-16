package serverconfig

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	armadav1 "github.com/armada/configbundle/api/v1"
)

// newSkipTestReconciler wires a Reconciler with the given ServerConfig CR
// pre-loaded and a fake status subresource. AllowedOobIPs is set to a fixed
// single-entry set so tests can drive both the "in the allowlist" and
// "not in the allowlist" paths deterministically.
func newSkipTestReconciler(t *testing.T, sc *armadav1.ServerConfig) (*ServerConfigReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := armadav1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sc).
		WithStatusSubresource(&armadav1.ServerConfig{}).
		Build()
	r := &ServerConfigReconciler{
		Client:                c,
		Scheme:                scheme,
		AllowedOobIPs:         map[string]bool{"10.20.21.44": true},
		AllowedFields:         allFields(),
		CredentialsNamespace:  "default",
		CredentialsSecretName: "idrac-credentials",
		Recorder:              record.NewFakeRecorder(32),
	}
	return r, c
}

// assertReconciledCondition looks up the Reconciled condition and fails the
// test if status/reason don't match. Message is asserted as a substring so
// wording tweaks don't break the test.
func assertReconciledCondition(t *testing.T, sc *armadav1.ServerConfig, wantStatus metav1.ConditionStatus, wantReason, wantMsgSubstr string) {
	t.Helper()
	for _, c := range sc.Status.Conditions {
		if c.Type != ConditionReconciled {
			continue
		}
		if c.Status != wantStatus {
			t.Errorf("Reconciled.status = %s, want %s", c.Status, wantStatus)
		}
		if c.Reason != wantReason {
			t.Errorf("Reconciled.reason = %q, want %q", c.Reason, wantReason)
		}
		if !strings.Contains(c.Message, wantMsgSubstr) {
			t.Errorf("Reconciled.message = %q, want substring %q", c.Message, wantMsgSubstr)
		}
		return
	}
	t.Fatalf("Reconciled condition not found; conditions = %+v", sc.Status.Conditions)
}

func TestReconcile_SkipsNoOobIP_WritesStatus(t *testing.T) {
	sc := &armadav1.ServerConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "colo-r740-01"},
		Spec: armadav1.ServerConfigSpec{
			ServiceTag: "3RK3V64",
			// OobIP intentionally nil — this is the skip trigger.
		},
	}
	r, c := newSkipTestReconciler(t, sc)

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: sc.Name},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got armadav1.ServerConfig
	if err := c.Get(context.Background(), types.NamespacedName{Name: sc.Name}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Phase != armadav1.ServerConfigPhaseSkipped {
		t.Errorf("Phase = %q, want Skipped", got.Status.Phase)
	}
	assertReconciledCondition(t, &got, metav1.ConditionUnknown, "NoOobIP", "spec.oobIP is empty")
}

func TestReconcile_SkipsOobNotAllowlisted_WritesStatus(t *testing.T) {
	sc := &armadav1.ServerConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "colo-r740-02"},
		Spec: armadav1.ServerConfigSpec{
			ServiceTag: "FQK3V64",
			OobIP:      ptr.To("10.99.99.99"), // NOT in allowlist (allowlist has 10.20.21.44)
			IdracSettings: armadav1.IdracSettingsSpec{
				SSHEnabled: ptr.To(true),
			},
		},
	}
	r, c := newSkipTestReconciler(t, sc)

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: sc.Name},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got armadav1.ServerConfig
	if err := c.Get(context.Background(), types.NamespacedName{Name: sc.Name}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Phase != armadav1.ServerConfigPhaseSkipped {
		t.Errorf("Phase = %q, want Skipped", got.Status.Phase)
	}
	// Message names the offending IP so operators can grep for it.
	assertReconciledCondition(t, &got, metav1.ConditionUnknown, "NotInOobAllowlist", "10.99.99.99")
}

// TestReconcile_SkipStatusClearsOnAllowlistAdmission verifies that once an
// operator adds a previously-skipped oobIP to the allowlist and reconciles
// again, the Skipped phase and NotInOobAllowlist reason don't persist — the
// controller must progress the CR out of Skipped on the very next reconcile.
// This is the "operator fixed the allowlist, did the status catch up?" check.
func TestReconcile_SkipStatusClearsOnAllowlistAdmission(t *testing.T) {
	sc := &armadav1.ServerConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "colo-r740-03"},
		Spec: armadav1.ServerConfigSpec{
			ServiceTag: "7RK9Y21",
			OobIP:      ptr.To("10.20.21.44"), // in the allowlist
			IdracSettings: armadav1.IdracSettingsSpec{
				SSHEnabled: ptr.To(true),
			},
		},
		Status: armadav1.ServerConfigStatus{
			// Simulate a previous reconcile that skipped this CR (before the
			// operator flipped it into the allowlist).
			Phase: armadav1.ServerConfigPhaseSkipped,
			Conditions: []metav1.Condition{{
				Type:               ConditionReconciled,
				Status:             metav1.ConditionUnknown,
				Reason:             "NotInOobAllowlist",
				Message:            "stale — allowlist just changed",
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
	r, c := newSkipTestReconciler(t, sc)

	// Force the reconcile to bail before touching the network: no credentials
	// Secret in the fake client, so credential load will fail with NotFound
	// and the CR will land in Diverged / MissingCredentials — not the point of
	// this test. The point is: Phase must NOT stay Skipped now that the CR
	// passes the allowlist gate.
	_, _ = r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: sc.Name},
	})

	var got armadav1.ServerConfig
	if err := c.Get(context.Background(), types.NamespacedName{Name: sc.Name}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Phase == armadav1.ServerConfigPhaseSkipped {
		t.Errorf("Phase stuck at Skipped after allowlist admission; want progression to Diverged or Applied")
	}
}

// TestReconcile_FailureEmitsWarningEvent verifies errors surface to humans as
// Kubernetes Warning Events (kubectl describe / get events), not just status
// conditions. Drives the missing-credentials path: in-allowlist server, no
// credentials Secret → MissingCredentials failure.
func TestReconcile_FailureEmitsWarningEvent(t *testing.T) {
	sc := &armadav1.ServerConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "colo-r740-09"},
		Spec: armadav1.ServerConfigSpec{
			ServiceTag: "FAIL01",
			OobIP:      ptr.To("10.20.21.44"), // in allowlist → passes the gate, then creds load fails
			IdracSettings: armadav1.IdracSettingsSpec{
				SSHEnabled: ptr.To(true),
			},
		},
	}
	r, _ := newSkipTestReconciler(t, sc)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: sc.Name},
	}); err != nil {
		t.Fatalf("Reconcile returned unexpected error: %v", err)
	}

	fr := r.Recorder.(*record.FakeRecorder)
	select {
	case ev := <-fr.Events:
		if !strings.Contains(ev, "Warning") || !strings.Contains(ev, "MissingCredentials") {
			t.Errorf("expected a Warning MissingCredentials event; got %q", ev)
		}
	default:
		t.Errorf("expected a Warning Event on failure; got none")
	}
}
