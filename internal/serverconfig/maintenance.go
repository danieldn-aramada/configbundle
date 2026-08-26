package serverconfig

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	armadav1 "github.com/armada/configbundle/api/v1"
)

const (
	// maintenanceRequeue is the interval for re-checking blocked eligibility.
	maintenanceRequeue = 30 * time.Second

	// eventMaintenanceBlocked is emitted when an eligibility check fails.
	eventMaintenanceBlocked = "MaintenanceBlocked"
	// eventMaintenanceActive is emitted when the node enters Active phase.
	eventMaintenanceActive = "MaintenanceActive"
	// eventMaintenanceDone is emitted when the sequence completes.
	eventMaintenanceDone = "MaintenanceDone"
)

// reconcileMaintenance runs the maintenance state machine for sc.
// It is called after the iDRAC skip gates and runs independently of the
// iDRAC reconcile path. Returns the Result to use for this reconcile
// (caller merges with the iDRAC result, taking the shorter requeue).
//
// State machine:
//
//	"" (enabled=false or nil) → no-op
//	"" (enabled=true, window open) → Checking
//	Checking → run eligibility checks; fail → blockedReason + requeue; pass → Active
//	Active → hold; enabled=false → clear phase → ""
func (r *ServerConfigReconciler) reconcileMaintenance(ctx context.Context, sc *armadav1.ServerConfig) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName("serverconfig.maintenance")

	m := sc.Spec.Maintenance
	currentPhase := ""
	if sc.Status.Maintenance != nil {
		currentPhase = string(sc.Status.Maintenance.Phase)
	}

	// No active sequence and no request → nothing to do.
	if !maintenanceRequested(m) && currentPhase == "" {
		return ctrl.Result{}, nil
	}

	switch armadav1.MaintenancePhase(currentPhase) {

	case armadav1.MaintenancePhase(""): // entry gate
		if !maintenanceRequested(m) {
			return ctrl.Result{}, nil
		}
		if blocked, reason := windowBlocked(m); blocked {
			logger.V(1).Info("maintenance window not open", "reason", reason)
			return ctrl.Result{RequeueAfter: maintenanceRequeue}, nil
		}
		logger.Info("maintenance requested; entering Checking phase")
		return ctrl.Result{RequeueAfter: time.Second}, r.setMaintenancePhase(ctx, sc, armadav1.MaintenancePhaseChecking, func(ms *armadav1.MaintenanceStatus) {})

	case armadav1.MaintenancePhaseChecking:
		return r.handleChecking(ctx, sc)

	case armadav1.MaintenancePhaseActive:
		return r.handleActive(ctx, sc)

	default:
		logger.Info("unrecognised maintenance phase; ignoring", "phase", currentPhase)
		return ctrl.Result{}, nil
	}
}

// handleChecking runs all eligibility checks. On failure it writes
// blockedReason and requeues. On pass it transitions to Active.
func (r *ServerConfigReconciler) handleChecking(ctx context.Context, sc *armadav1.ServerConfig) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName("serverconfig.maintenance")

	// If maintenance was cleared while in Checking, exit cleanly.
	if !maintenanceRequested(sc.Spec.Maintenance) {
		logger.Info("maintenance cleared while Checking; exiting")
		return ctrl.Result{}, r.clearMaintenance(ctx, sc)
	}

	blockedReason, err := r.runEligibilityChecks(ctx, sc)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("eligibility checks: %w", err)
	}

	if blockedReason != "" {
		logger.Info("maintenance eligibility check failed", "reason", blockedReason)
		msg := fmt.Sprintf("maintenance blocked: %s", blockedReason)
		r.Recorder.Event(sc, corev1.EventTypeWarning, eventMaintenanceBlocked, msg)
		return ctrl.Result{RequeueAfter: maintenanceRequeue}, r.setMaintenancePhase(ctx, sc, armadav1.MaintenancePhaseChecking, func(ms *armadav1.MaintenanceStatus) {
			ms.BlockedReason = &blockedReason
		})
	}

	// All checks passed → Active.
	logger.Info("maintenance eligibility checks passed; entering Active phase")
	r.Recorder.Event(sc, corev1.EventTypeNormal, eventMaintenanceActive, "all eligibility checks passed; node in maintenance Active phase")
	return ctrl.Result{}, r.setMaintenancePhase(ctx, sc, armadav1.MaintenancePhaseActive, func(ms *armadav1.MaintenanceStatus) {
		ms.BlockedReason = nil
	})
}

// handleActive holds the sequence. Waits for enabled=false to exit.
func (r *ServerConfigReconciler) handleActive(ctx context.Context, sc *armadav1.ServerConfig) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName("serverconfig.maintenance")

	if maintenanceRequested(sc.Spec.Maintenance) {
		// Still active — hold.
		return ctrl.Result{RequeueAfter: maintenanceRequeue}, nil
	}

	// enabled cleared → sequence complete.
	logger.Info("maintenance cleared; completing sequence")
	r.Recorder.Event(sc, corev1.EventTypeNormal, eventMaintenanceDone, "maintenance complete; spec.maintenance.enabled cleared")
	return ctrl.Result{}, r.clearMaintenance(ctx, sc)
}

// runEligibilityChecks runs all checks in order. Returns the first
// blockedReason string, or "" if all pass. An error indicates a K8s
// API failure, not an eligibility failure.
func (r *ServerConfigReconciler) runEligibilityChecks(ctx context.Context, sc *armadav1.ServerConfig) (string, error) {
	// Check 1: node must exist in this cluster.
	skip, err := r.checkNodeExists(ctx, sc)
	if err != nil {
		return "", err
	}
	if skip {
		// Server is not a node in this cluster — skip maintenance entirely.
		// Return empty blockedReason so the caller transitions to Active;
		// maintenance on a non-local server is a no-op, not a block.
		return "", nil
	}

	nodeName := *sc.Spec.KubernetesNode.Name

	// Check 2: no concurrent maintenance on a sibling ServerConfig.
	if reason, err := r.checkNoConcurrentMaintenance(ctx, sc); err != nil {
		return "", err
	} else if reason != "" {
		return reason, nil
	}

	role := sc.Spec.KubernetesNode.Role

	// Check 3: role-based guard (CP: EKSA block or etcd quorum; Worker: schedulable peers).
	switch role {
	case armadav1.NodeRoleControlPlane:
		if reason, err := r.checkControlPlaneGuard(ctx); err != nil {
			return "", err
		} else if reason != "" {
			return reason, nil
		}
	case armadav1.NodeRoleWorker:
		if reason, err := r.checkSchedulableWorkers(ctx, nodeName); err != nil {
			return "", err
		} else if reason != "" {
			return reason, nil
		}
	}

	return "", nil
}

// checkNodeExists returns skip=true if the node named in
// spec.kubernetesNode.name does not exist in this cluster (server is not
// a member of the cluster this controller manages). skip=true is NOT a
// block — the controller simply does not apply maintenance to this CR.
func (r *ServerConfigReconciler) checkNodeExists(ctx context.Context, sc *armadav1.ServerConfig) (skip bool, err error) {
	if sc.Spec.KubernetesNode == nil || sc.Spec.KubernetesNode.Name == nil {
		return true, nil
	}
	var node corev1.Node
	err = r.Get(ctx, client.ObjectKey{Name: *sc.Spec.KubernetesNode.Name}, &node)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	return false, err
}

// checkNoConcurrentMaintenance blocks if any sibling ServerConfig in this
// cluster is already in Checking, Active, or Draining phase. Concurrent
// maintenance risks taking too many nodes offline simultaneously.
func (r *ServerConfigReconciler) checkNoConcurrentMaintenance(ctx context.Context, sc *armadav1.ServerConfig) (string, error) {
	var list armadav1.ServerConfigList
	if err := r.List(ctx, &list, client.MatchingLabels{
		armadav1.LabelCluster: r.ClusterName,
	}); err != nil {
		return "", fmt.Errorf("list ServerConfigs: %w", err)
	}
	for _, sibling := range list.Items {
		if sibling.Name == sc.Name {
			continue
		}
		if sibling.Status.Maintenance == nil {
			continue
		}
		switch sibling.Status.Maintenance.Phase {
		case armadav1.MaintenancePhaseChecking,
			armadav1.MaintenancePhaseDraining,
			armadav1.MaintenancePhaseActive:
			reason := fmt.Sprintf("ConcurrentMaintenance: %s is already in %s phase",
				sibling.Name, sibling.Status.Maintenance.Phase)
			return reason, nil
		}
	}
	return "", nil
}

// checkControlPlaneGuard blocks CP maintenance on EKSA clusters entirely,
// and on non-EKSA clusters blocks when draining would break etcd quorum.
func (r *ServerConfigReconciler) checkControlPlaneGuard(ctx context.Context) (string, error) {
	// EKSA detection: eksa-packages namespace presence in the workload cluster.
	var ns corev1.Namespace
	err := r.Get(ctx, client.ObjectKey{Name: "eksa-packages"}, &ns)
	if err == nil {
		return "EksaControlPlaneForbidden: CP maintenance on EKSA clusters is not supported in this version; management cluster access required", nil
	}
	if !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("check eksa-packages namespace: %w", err)
	}

	// Non-EKSA: check etcd quorum. Count Ready CP nodes.
	var nodeList corev1.NodeList
	if err := r.List(ctx, &nodeList, client.MatchingLabels{
		"node-role.kubernetes.io/control-plane": "",
	}); err != nil {
		return "", fmt.Errorf("list control-plane nodes: %w", err)
	}
	total := len(nodeList.Items)
	// Removing one CP node is safe only if (total-1) >= floor(total/2)+1.
	if total-1 < total/2+1 {
		return fmt.Sprintf("EtcdQuorumRisk: draining this CP node would break etcd quorum (%d total CP nodes)", total), nil
	}
	return "", nil
}

// checkSchedulableWorkers blocks if no other schedulable worker would remain
// after taking this node offline.
func (r *ServerConfigReconciler) checkSchedulableWorkers(ctx context.Context, nodeName string) (string, error) {
	var nodeList corev1.NodeList
	if err := r.List(ctx, &nodeList); err != nil {
		return "", fmt.Errorf("list nodes: %w", err)
	}
	schedulable := 0
	for _, n := range nodeList.Items {
		if n.Name == nodeName {
			continue
		}
		if _, isCP := n.Labels["node-role.kubernetes.io/control-plane"]; isCP {
			continue
		}
		if n.Spec.Unschedulable {
			continue
		}
		// Check for node Ready condition.
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				schedulable++
				break
			}
		}
	}
	if schedulable == 0 {
		return "NoSchedulableWorkers: no other schedulable worker node available", nil
	}
	return "", nil
}

// setMaintenancePhase writes the maintenance phase and any additional status
// fields set by the mutate fn. Uses RetryOnConflict.
func (r *ServerConfigReconciler) setMaintenancePhase(ctx context.Context, sc *armadav1.ServerConfig, phase armadav1.MaintenancePhase, mutate func(*armadav1.MaintenanceStatus)) error {
	logger := log.FromContext(ctx).WithName("serverconfig.maintenance")
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh armadav1.ServerConfig
		if err := r.Get(ctx, client.ObjectKeyFromObject(sc), &fresh); err != nil {
			return err
		}
		if fresh.Status.Maintenance == nil {
			fresh.Status.Maintenance = &armadav1.MaintenanceStatus{}
		}
		fresh.Status.Maintenance.Phase = phase
		mutate(fresh.Status.Maintenance)
		if err := r.Status().Update(ctx, &fresh); err != nil {
			logger.V(1).Info("maintenance status update conflict; retrying", "err", err.Error())
			return err
		}
		return nil
	})
}

// clearMaintenance resets status.maintenance to nil, preserving only
// lastMaintenanceAt so history is not lost.
func (r *ServerConfigReconciler) clearMaintenance(ctx context.Context, sc *armadav1.ServerConfig) error {
	logger := log.FromContext(ctx).WithName("serverconfig.maintenance")
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh armadav1.ServerConfig
		if err := r.Get(ctx, client.ObjectKeyFromObject(sc), &fresh); err != nil {
			return err
		}
		now := metav1.Now()
		fresh.Status.Maintenance = &armadav1.MaintenanceStatus{
			LastMaintenanceAt: &now,
		}
		if err := r.Status().Update(ctx, &fresh); err != nil {
			logger.V(1).Info("clear maintenance status conflict; retrying", "err", err.Error())
			return err
		}
		return nil
	})
}

// maintenanceRequested returns true when spec indicates maintenance is enabled
// and the spec struct is present.
func maintenanceRequested(m *armadav1.MaintenanceSpec) bool {
	return m != nil && m.Enabled
}

// windowBlocked returns true if the maintenance window has not yet opened
// or has already expired.
func windowBlocked(m *armadav1.MaintenanceSpec) (bool, string) {
	if m == nil || m.Window == nil {
		return false, ""
	}
	now := time.Now()
	if m.Window.Start != nil && now.Before(m.Window.Start.Time) {
		return true, fmt.Sprintf("window not yet open (starts %s)", m.Window.Start.Time.Format(time.RFC3339))
	}
	if m.Window.End != nil && !now.Before(m.Window.End.Time) {
		return true, fmt.Sprintf("window expired (ended %s)", m.Window.End.Time.Format(time.RFC3339))
	}
	return false, ""
}
