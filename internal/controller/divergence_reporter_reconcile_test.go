package controller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	armadav1 "github.com/armada/configbundle/api/v1"
)

// These tests verify the reporter's dedup + steady-state-quiet decisions read
// from cb.Status.DivergenceReporting (the Phase 2 migration from in-memory
// maps to CR status). Each case seeds a CB with a specific prior status and
// asserts whether the reporter POSTs.

func newReconcileTestReporter(t *testing.T, orbURL string, objs ...client.Object) *DivergenceReporter {
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
		WithObjects(objs...).
		WithStatusSubresource(&armadav1.ConfigBundle{}).
		Build()
	return NewDivergenceReporter(c,
		WithDivergenceIntakeURL(orbURL),
		WithDivergenceEnabled(true),
		WithDivergenceDebounce(0),
	)
}

// cbCleanNoOverride builds a CB with no local:* managedFields — the reporter
// will compute overrides=[] on every reconcile.
func cbCleanNoOverride(name string, priorStatus *armadav1.DivergenceReportingStatus) *armadav1.ConfigBundle {
	return &armadav1.ConfigBundle{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: armadav1.ConfigBundleSpec{
			OrbID:      "colo:" + name,
			Datacenter: name,
		},
		Status: armadav1.ConfigBundleStatus{
			DivergenceReporting: priorStatus,
		},
	}
}

// primeLastManifest sets the reporter's in-memory lastManifest so the cold-start
// guard doesn't short-circuit before the dedup logic runs. In production this is
// populated by consume.applyManifest or the bootstrap loader.
func primeLastManifest(r *DivergenceReporter, cb *armadav1.ConfigBundle) {
	r.SetLastManifest(cb.Name, cb.Spec)
}

// captureOrb creates an httptest server that counts POST hits and captures the
// last payload. Returns URL + a *int32 counter + a getter for the last payload.
func captureOrb(t *testing.T) (url string, hits *int32, lastPayload func() *DivergencePayload) {
	t.Helper()
	var counter int32
	var last *DivergencePayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&counter, 1)
		body, _ := io.ReadAll(r.Body)
		p := &DivergencePayload{}
		if err := json.Unmarshal(body, p); err == nil {
			last = p
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &counter, func() *DivergencePayload { return last }
}

func TestReconcile_ColdStart_NilStatus_ForcesPOST(t *testing.T) {
	// prior=nil (never posted) + overrides=[] → the earlier bug's failure mode.
	// New behavior: POST empty once to sync orb, then persist the status so
	// subsequent reconciles hit steady-state quiet.
	url, hits, last := captureOrb(t)
	cb := cbCleanNoOverride("cb-cold", nil)
	r := newReconcileTestReporter(t, url, cb)
	primeLastManifest(r, cb)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: cb.Name},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if atomic.LoadInt32(hits) != 1 {
		t.Errorf("expected exactly 1 POST on cold start, got %d", atomic.LoadInt32(hits))
	}
	if p := last(); p == nil || len(p.Overrides) != 0 {
		t.Errorf("expected empty override payload, got %+v", p)
	}

	// Status must now reflect the POST: hash set, count=*0.
	var fresh armadav1.ConfigBundle
	if err := r.Client.Get(context.Background(), types.NamespacedName{Name: cb.Name}, &fresh); err != nil {
		t.Fatalf("get cb: %v", err)
	}
	if fresh.Status.DivergenceReporting == nil {
		t.Fatal("expected DivergenceReporting to be populated after POST")
	}
	if fresh.Status.DivergenceReporting.LastPostedOverrideCount == nil ||
		*fresh.Status.DivergenceReporting.LastPostedOverrideCount != 0 {
		t.Errorf("expected LastPostedOverrideCount=*0, got %v",
			fresh.Status.DivergenceReporting.LastPostedOverrideCount)
	}
	if fresh.Status.DivergenceReporting.LastPostedHash == "" {
		t.Error("expected LastPostedHash to be set")
	}
}

func TestReconcile_SteadyStateQuiet_SkipsPOST(t *testing.T) {
	// prior=*0, overrides=[] → true steady state, no POST needed.
	url, hits, _ := captureOrb(t)
	zero := 0
	prior := &armadav1.DivergenceReportingStatus{
		LastPostedAt:            &metav1.Time{Time: time.Now().Add(-1 * time.Hour)},
		LastPostedHash:          "0000000000000000000000000000000000000000000000000000000000000000",
		LastPostedOverrideCount: &zero,
	}
	cb := cbCleanNoOverride("cb-quiet", prior)
	r := newReconcileTestReporter(t, url, cb)
	primeLastManifest(r, cb)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: cb.Name},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if atomic.LoadInt32(hits) != 0 {
		t.Errorf("expected 0 POSTs (steady-state quiet), got %d", atomic.LoadInt32(hits))
	}

	var fresh armadav1.ConfigBundle
	if err := r.Client.Get(context.Background(), types.NamespacedName{Name: cb.Name}, &fresh); err != nil {
		t.Fatalf("get cb: %v", err)
	}
	if fresh.Status.DivergenceReporting == nil || fresh.Status.DivergenceReporting.LastCheckedForDivergence == nil {
		t.Error("expected LastCheckedForDivergence to be set after steady-state-quiet reconcile")
	}
}

func TestReconcile_SameHash_SkipsPOST(t *testing.T) {
	// prior.hash matches current payload's hash → dedup skip, no POST.
	url, hits, _ := captureOrb(t)
	cb := cbCleanNoOverride("cb-dedup", nil)
	// Compute what the current payload's hash would be, seed prior with it.
	currentHash := contentHash(DivergencePayload{Overrides: nil})
	zero := 0
	cb.Status.DivergenceReporting = &armadav1.DivergenceReportingStatus{
		LastPostedHash:          currentHash,
		LastPostedOverrideCount: &zero,
	}
	r := newReconcileTestReporter(t, url, cb)
	primeLastManifest(r, cb)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: cb.Name},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if atomic.LoadInt32(hits) != 0 {
		t.Errorf("expected 0 POSTs (hash matches prior), got %d", atomic.LoadInt32(hits))
	}

	var fresh armadav1.ConfigBundle
	if err := r.Client.Get(context.Background(), types.NamespacedName{Name: cb.Name}, &fresh); err != nil {
		t.Fatalf("get cb: %v", err)
	}
	if fresh.Status.DivergenceReporting == nil || fresh.Status.DivergenceReporting.LastCheckedForDivergence == nil {
		t.Error("expected LastCheckedForDivergence to be set after dedup-skip reconcile")
	}
}

func TestReconcile_TransitionToEmpty_POSTs(t *testing.T) {
	// prior=*3 (previously had overrides), overrides=[] now → the "cleared
	// divergence" transition. Must POST empty so orb clears its state.
	url, hits, last := captureOrb(t)
	three := 3
	prior := &armadav1.DivergenceReportingStatus{
		LastPostedAt:            &metav1.Time{Time: time.Now().Add(-1 * time.Hour)},
		LastPostedHash:          "stale-hash-value",
		LastPostedOverrideCount: &three,
	}
	cb := cbCleanNoOverride("cb-transition", prior)
	r := newReconcileTestReporter(t, url, cb)
	primeLastManifest(r, cb)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: cb.Name},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if atomic.LoadInt32(hits) != 1 {
		t.Errorf("expected 1 POST on transition-to-empty, got %d", atomic.LoadInt32(hits))
	}
	if p := last(); p == nil || len(p.Overrides) != 0 {
		t.Errorf("expected empty payload on transition, got %+v", p)
	}

	// Status now reflects the transition: count=*0.
	var fresh armadav1.ConfigBundle
	if err := r.Client.Get(context.Background(), types.NamespacedName{Name: cb.Name}, &fresh); err != nil {
		t.Fatalf("get cb: %v", err)
	}
	if fresh.Status.DivergenceReporting.LastPostedOverrideCount == nil ||
		*fresh.Status.DivergenceReporting.LastPostedOverrideCount != 0 {
		t.Errorf("expected LastPostedOverrideCount=*0 after transition, got %v",
			fresh.Status.DivergenceReporting.LastPostedOverrideCount)
	}
}

func TestClearReportingHash_ClearsHashPreservesCount(t *testing.T) {
	// Heartbeat's clearReportingHash should zero the hash but keep the count
	// intact — the count is still authoritative history, only the hash needs
	// invalidating so the next reconcile's dedup check misses.
	three := 3
	prior := &armadav1.DivergenceReportingStatus{
		LastPostedAt:            &metav1.Time{Time: time.Now()},
		LastPostedHash:          "some-hash",
		LastPostedOverrideCount: &three,
	}
	cb := cbCleanNoOverride("cb-clear", prior)
	url, _, _ := captureOrb(t)
	r := newReconcileTestReporter(t, url, cb)

	if err := r.clearReportingHash(context.Background(), cb.Name); err != nil {
		t.Fatalf("clearReportingHash: %v", err)
	}

	var fresh armadav1.ConfigBundle
	if err := r.Client.Get(context.Background(), types.NamespacedName{Name: cb.Name}, &fresh); err != nil {
		t.Fatalf("get cb: %v", err)
	}
	if fresh.Status.DivergenceReporting.LastPostedHash != "" {
		t.Errorf("expected LastPostedHash cleared, got %q", fresh.Status.DivergenceReporting.LastPostedHash)
	}
	if fresh.Status.DivergenceReporting.LastPostedOverrideCount == nil ||
		*fresh.Status.DivergenceReporting.LastPostedOverrideCount != 3 {
		t.Errorf("expected count preserved as *3, got %v",
			fresh.Status.DivergenceReporting.LastPostedOverrideCount)
	}
}
