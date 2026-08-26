package serverconfig

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
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

// newMaintenanceReconciler wires a reconciler with maintenance enabled,
// the given ServerConfig, Nodes, and Namespaces pre-loaded in the fake client.
func newMaintenanceReconciler(t *testing.T, clusterName string, objs ...client.Object) (*ServerConfigReconciler, client.Client) {
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
		WithStatusSubresource(&armadav1.ServerConfig{}).
		Build()
	r := &ServerConfigReconciler{
		Client:             c,
		Scheme:             scheme,
		AllowedOobIPs:      map[string]bool{},
		AllowedFields:      allFields(),
		Recorder:           record.NewFakeRecorder(32),
		ClusterName:        clusterName,
		MaintenanceEnabled: true,
	}
	return r, c
}

func workerNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

func cpNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

func scWithMaintenance(name, clusterName, nodeName string, role armadav1.NodeRole) *armadav1.ServerConfig {
	return &armadav1.ServerConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				armadav1.LabelCluster: clusterName,
			},
		},
		Spec: armadav1.ServerConfigSpec{
			ServiceTag: "TESTST1",
			KubernetesNode: &armadav1.KubernetesNodeSpec{
				Name: ptr.To(nodeName),
				Role: role,
			},
			Maintenance: &armadav1.MaintenanceSpec{Enabled: true},
		},
	}
}

// TestMaintenance_NodeNotInCluster verifies that a ServerConfig whose
// spec.kubernetesNode.name is not found in the local cluster passes
// eligibility (skip=true) and transitions to Active.
func TestMaintenance_NodeNotInCluster(t *testing.T) {
	sc := scWithMaintenance("r740-01", "dev-main", "dev-main-worker-1", armadav1.NodeRoleWorker)
	// No Node objects in fake client — node does not exist in this cluster.
	r, c := newMaintenanceReconciler(t, "dev-main", sc)

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
	// Node not found → skip=true → transitions to Active (maintenance is a no-op for non-local servers).
	if got.Status.Maintenance == nil {
		t.Fatal("expected status.maintenance to be set")
	}
	// After first reconcile: phase should be Checking (just entered).
	// After second reconcile: checks pass (skip=true counts as pass) → Active.
	// Run a second reconcile to drive through Checking → Active.
	_, err = r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: sc.Name},
	})
	if err != nil {
		t.Fatalf("Reconcile 2: %v", err)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: sc.Name}, &got); err != nil {
		t.Fatalf("Get 2: %v", err)
	}
	if got.Status.Maintenance == nil || got.Status.Maintenance.Phase != armadav1.MaintenancePhaseActive {
		t.Errorf("phase = %v, want Active", got.Status.Maintenance)
	}
}

// TestMaintenance_ConcurrentMaintenanceBlocked verifies that a second
// ServerConfig in the same cluster is blocked when a sibling is Active.
func TestMaintenance_ConcurrentMaintenanceBlocked(t *testing.T) {
	worker1 := workerNode("dev-main-worker-1")
	worker2 := workerNode("dev-main-worker-2")

	sc1 := scWithMaintenance("r740-01", "dev-main", "dev-main-worker-1", armadav1.NodeRoleWorker)
	sc2 := scWithMaintenance("r740-02", "dev-main", "dev-main-worker-2", armadav1.NodeRoleWorker)
	// Simulate sc1 already Active.
	sc1.Status.Maintenance = &armadav1.MaintenanceStatus{Phase: armadav1.MaintenancePhaseActive}

	r, c := newMaintenanceReconciler(t, "dev-main", sc1, sc2, worker1, worker2)

	// First reconcile of sc2: "" → Checking.
	r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: sc2.Name}}) //nolint:errcheck
	// Second reconcile: Checking → run checks → blocked by sc1.
	r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: sc2.Name}}) //nolint:errcheck

	var got armadav1.ServerConfig
	if err := c.Get(context.Background(), types.NamespacedName{Name: sc2.Name}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Maintenance == nil {
		t.Fatal("expected status.maintenance set")
	}
	if got.Status.Maintenance.Phase != armadav1.MaintenancePhaseChecking {
		t.Errorf("phase = %v, want Checking", got.Status.Maintenance.Phase)
	}
	if got.Status.Maintenance.BlockedReason == nil || *got.Status.Maintenance.BlockedReason == "" {
		t.Error("expected blockedReason to be set")
	}
}

// TestMaintenance_EksaControlPlaneBlocked verifies that a CP ServerConfig
// is blocked when the eksa-packages namespace is present.
func TestMaintenance_EksaControlPlaneBlocked(t *testing.T) {
	cpNodeObj := cpNode("dev-main-cp-1")
	eksaNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "eksa-packages"}}

	sc := scWithMaintenance("r740-cp", "dev-main", "dev-main-cp-1", armadav1.NodeRoleControlPlane)

	r, c := newMaintenanceReconciler(t, "dev-main", sc, cpNodeObj, eksaNS)

	r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: sc.Name}}) //nolint:errcheck
	r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: sc.Name}}) //nolint:errcheck

	var got armadav1.ServerConfig
	if err := c.Get(context.Background(), types.NamespacedName{Name: sc.Name}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Maintenance == nil || got.Status.Maintenance.Phase != armadav1.MaintenancePhaseChecking {
		t.Errorf("phase = %v, want Checking", got.Status.Maintenance)
	}
	if got.Status.Maintenance.BlockedReason == nil {
		t.Fatal("expected blockedReason set")
	}
	reason := *got.Status.Maintenance.BlockedReason
	if len(reason) < 10 || reason[:4] != "Eksa" {
		t.Errorf("blockedReason = %q, want EksaControlPlaneForbidden prefix", reason)
	}
}

// TestMaintenance_NoSchedulableWorkersBlocked verifies that a worker
// ServerConfig is blocked when no other schedulable worker exists.
func TestMaintenance_NoSchedulableWorkersBlocked(t *testing.T) {
	// Only one worker node — no peers remain if we take it.
	workerObj := workerNode("dev-main-worker-1")
	sc := scWithMaintenance("r740-01", "dev-main", "dev-main-worker-1", armadav1.NodeRoleWorker)

	r, c := newMaintenanceReconciler(t, "dev-main", sc, workerObj)

	r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: sc.Name}}) //nolint:errcheck
	r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: sc.Name}}) //nolint:errcheck

	var got armadav1.ServerConfig
	if err := c.Get(context.Background(), types.NamespacedName{Name: sc.Name}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Maintenance == nil || got.Status.Maintenance.Phase != armadav1.MaintenancePhaseChecking {
		t.Errorf("phase = %v, want Checking", got.Status.Maintenance)
	}
	if got.Status.Maintenance.BlockedReason == nil {
		t.Fatal("expected blockedReason set")
	}
	if *got.Status.Maintenance.BlockedReason != "NoSchedulableWorkers: no other schedulable worker node available" {
		t.Errorf("unexpected blockedReason: %q", *got.Status.Maintenance.BlockedReason)
	}
}

// TestMaintenance_WorkerPassesWithPeer verifies that a worker ServerConfig
// transitions to Active when at least one other schedulable worker exists.
func TestMaintenance_WorkerPassesWithPeer(t *testing.T) {
	worker1 := workerNode("dev-main-worker-1")
	worker2 := workerNode("dev-main-worker-2")
	sc := scWithMaintenance("r740-01", "dev-main", "dev-main-worker-1", armadav1.NodeRoleWorker)

	r, c := newMaintenanceReconciler(t, "dev-main", sc, worker1, worker2)

	r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: sc.Name}}) //nolint:errcheck
	r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: sc.Name}}) //nolint:errcheck

	var got armadav1.ServerConfig
	if err := c.Get(context.Background(), types.NamespacedName{Name: sc.Name}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Maintenance == nil || got.Status.Maintenance.Phase != armadav1.MaintenancePhaseActive {
		t.Errorf("phase = %v, want Active", got.Status.Maintenance)
	}
	if got.Status.Maintenance.BlockedReason != nil {
		t.Errorf("expected blockedReason cleared, got %q", *got.Status.Maintenance.BlockedReason)
	}
}

// TestMaintenance_ActiveClearsOnDisable verifies that setting enabled=false
// while Active causes the phase to clear and lastMaintenanceAt to be stamped.
func TestMaintenance_ActiveClearsOnDisable(t *testing.T) {
	worker1 := workerNode("dev-main-worker-1")
	worker2 := workerNode("dev-main-worker-2")
	sc := scWithMaintenance("r740-01", "dev-main", "dev-main-worker-1", armadav1.NodeRoleWorker)
	// Seed already-Active status to skip entry/check path.
	sc.Status.Maintenance = &armadav1.MaintenanceStatus{Phase: armadav1.MaintenancePhaseActive}

	r, c := newMaintenanceReconciler(t, "dev-main", sc, worker1, worker2)

	// Disable maintenance.
	sc.Spec.Maintenance.Enabled = false
	if err := c.Update(context.Background(), sc); err != nil {
		t.Fatalf("Update: %v", err)
	}

	r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: sc.Name}}) //nolint:errcheck

	var got armadav1.ServerConfig
	if err := c.Get(context.Background(), types.NamespacedName{Name: sc.Name}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Maintenance == nil {
		t.Fatal("expected status.maintenance present (lastMaintenanceAt)")
	}
	if got.Status.Maintenance.Phase != "" {
		t.Errorf("phase = %q, want empty (Done)", got.Status.Maintenance.Phase)
	}
	if got.Status.Maintenance.LastMaintenanceAt == nil {
		t.Error("expected lastMaintenanceAt stamped")
	}
}

// TestMaintenance_FeatureFlagDisabled verifies that when MaintenanceEnabled=false
// the reconciler ignores spec.maintenance entirely.
func TestMaintenance_FeatureFlagDisabled(t *testing.T) {
	sc := scWithMaintenance("r740-99", "dev-main", "dev-main-worker-1", armadav1.NodeRoleWorker)
	r, c := newMaintenanceReconciler(t, "dev-main", sc)
	r.MaintenanceEnabled = false

	r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: sc.Name}}) //nolint:errcheck

	var got armadav1.ServerConfig
	if err := c.Get(context.Background(), types.NamespacedName{Name: sc.Name}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Maintenance != nil && got.Status.Maintenance.Phase != "" {
		t.Errorf("expected no maintenance phase when feature disabled; got %q", got.Status.Maintenance.Phase)
	}
}
