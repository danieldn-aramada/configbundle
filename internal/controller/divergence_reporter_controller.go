package controller

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	armadav1 "github.com/armada/configbundle/api/v1"
)

// SetupWithManager registers DivergenceReporter as a controller that watches ConfigBundle CRs,
// a one-shot bootstrap Runnable that rehydrates lastManifests from per-CR ConfigMaps at
// startup (so restarts don't lose the intent baseline), and (when enabled) a periodic
// heartbeat that re-syncs the per-CR posted-hash cache.
func (r *DivergenceReporter) SetupWithManager(mgr ctrl.Manager) error {
	if r.enabled {
		if err := mgr.Add(&lastManifestLoader{reporter: r}); err != nil {
			return fmt.Errorf("register last-manifest loader: %w", err)
		}
	}
	if r.enabled && r.heartbeatInterval > 0 {
		if err := mgr.Add(&divergenceHeartbeat{reporter: r}); err != nil {
			return fmt.Errorf("register divergence heartbeat: %w", err)
		}
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&armadav1.ConfigBundle{}).
		WithEventFilter(r.predicate()).
		Named("divergence-reporter").
		Complete(r)
}

// lastManifestLoader is a one-shot manager.Runnable that rehydrates the
// reporter's in-memory lastManifests map from each ConfigBundle's per-CR
// ConfigMap at startup. Runs once after the manager's cache syncs and returns.
// Without this, every controller restart opens a cold-start window where the
// reporter has no intent baseline and either skips POSTs (after my earlier
// guard) or wipes orb's state (pre-guard) — until the next bundle dispatch.
type lastManifestLoader struct {
	reporter *DivergenceReporter
}

func (l *lastManifestLoader) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("divergence-reporter").WithName("bootstrap")
	var list armadav1.ConfigBundleList
	if err := l.reporter.Client.List(ctx, &list, client.InNamespace(l.reporter.namespace)); err != nil {
		logger.Info("list ConfigBundles failed; lastManifests will rely on next dispatch", "err", err.Error())
		return nil
	}
	loaded := 0
	for _, cb := range list.Items {
		spec, err := readLastAppliedSpec(ctx, l.reporter.Client, l.reporter.namespace, cb.Name)
		if err != nil {
			logger.Info("read last-applied-spec failed", "configbundle", cb.Name, "err", err.Error())
			continue
		}
		if spec == nil {
			continue
		}
		l.reporter.SetLastManifest(cb.Name, *spec)
		loaded++
	}
	logger.Info("rehydrated lastManifests", "configbundles", len(list.Items), "loaded", loaded)
	return nil
}

// divergenceHeartbeat is a manager.Runnable that ticks every reporter.heartbeatInterval,
// lists ConfigBundles in the configured namespace, clears each CR's lastPostedHash entry,
// and triggers a reconcile. Bounds the staleness window for the "orb wipe" failure mode —
// orb's persistent divergence store can be lost (PVC failure, manual wipe, fresh edge
// deploy) and the reporter's in-memory hash cache would otherwise dedup the post
// forever because no managedFields event fires to invalidate it.
type divergenceHeartbeat struct {
	reporter *DivergenceReporter
}

func (h *divergenceHeartbeat) Start(ctx context.Context) error {
	t := time.NewTicker(h.reporter.heartbeatInterval)
	defer t.Stop()
	logger := log.FromContext(ctx).WithName("divergence-reporter").WithName("heartbeat")
	logger.Info("heartbeat started", "interval", h.reporter.heartbeatInterval)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			h.tick(ctx, logger)
		}
	}
}

func (h *divergenceHeartbeat) tick(ctx context.Context, logger logrLogger) {
	var list armadav1.ConfigBundleList
	if err := h.reporter.Client.List(ctx, &list, client.InNamespace(h.reporter.namespace)); err != nil {
		logger.Info("list ConfigBundles failed", "err", err.Error())
		return
	}
	if len(list.Items) == 0 {
		return
	}
	// Clear the dedup cache for every CR so the next reconcile re-posts even if
	// the override set is unchanged. The actual re-post fires below.
	h.reporter.mu.Lock()
	for _, cb := range list.Items {
		delete(h.reporter.lastPostedHash, types.NamespacedName{Name: cb.Name, Namespace: cb.Namespace})
	}
	h.reporter.mu.Unlock()
	// Trigger reconcile for each CR. Direct call bypasses the work queue —
	// acceptable here because we ARE the periodic re-sync; there's no event
	// debouncing to honor.
	for _, cb := range list.Items {
		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: cb.Name, Namespace: cb.Namespace}}
		if _, err := h.reporter.Reconcile(ctx, req); err != nil {
			logger.Info("reconcile failed", "configbundle", cb.Name, "err", err.Error())
		}
	}
	logger.Info("heartbeat tick complete", "configbundles", len(list.Items))
}

// Reconcile is called when a ConfigBundle CR's local:* managed fields change.
// It debounces, reads the mapping ConfigMap, computes the override set, deduplicates
// by content hash, and POSTs to orb's divergence intake.
func (r *DivergenceReporter) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx).WithName("divergence-reporter")

	if !r.enabled {
		return reconcile.Result{}, nil
	}

	r.mu.Lock()
	last := r.lastEventAt[req.NamespacedName]
	r.mu.Unlock()

	// Zero lastEventAt means startup reconcile — elapsed is huge, proceed immediately.
	elapsed := time.Since(last)
	if !last.IsZero() && elapsed < r.debounceWindow {
		return reconcile.Result{RequeueAfter: r.debounceWindow - elapsed}, nil
	}

	var cb armadav1.ConfigBundle
	if err := r.Client.Get(ctx, req.NamespacedName, &cb); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	mapping, err := readMappingConfigMap(ctx, r.Client, req.Namespace, req.Name)
	if err != nil {
		if client.IgnoreNotFound(err) == nil {
			logger.Info("no mapping ConfigMap yet, skipping", "configbundle", req.Name)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("read mapping ConfigMap: %w", err)
	}

	warnNonConformingManagers(logger, cb.Name, cb.ManagedFields)

	r.lastManifestsMu.RLock()
	lastManifest, haveLastManifest := r.lastManifests[cb.Name]
	r.lastManifestsMu.RUnlock()

	// Cold-start guard: without a lastManifest we don't know what the intent
	// values are, so every local:admin claim looks "intent-absent" and
	// extractOverrides returns nil. Posting nil to orb is REPLACE-not-merge —
	// it would wipe orb's last-known good divergence set. Skip until the next
	// orb-import dispatch (consume.go) populates lastManifests for this CR.
	if !haveLastManifest {
		logger.Info("no lastManifest yet (controller cold start, awaiting next bundle import); skipping post to avoid wiping orb's state", "configbundle", req.Name)
		return reconcile.Result{}, nil
	}

	overrides := r.extractOverrides(&cb, mapping, lastManifest)
	payload := DivergencePayload{Overrides: overrides}

	h := contentHash(payload)
	r.mu.Lock()
	sameHash := r.lastPostedHash[req.NamespacedName] == h
	r.mu.Unlock()
	if sameHash {
		logger.Info("override set unchanged, skipping POST", "configbundle", req.Name)
		return reconcile.Result{}, nil
	}

	if err := r.postToOrb(ctx, payload); err != nil {
		return reconcile.Result{}, fmt.Errorf("POST divergence: %w", err)
	}

	r.mu.Lock()
	r.lastPostedHash[req.NamespacedName] = h
	r.mu.Unlock()

	logger.Info("reported divergence", "configbundle", req.Name, "overrides", len(overrides))
	return reconcile.Result{}, nil
}

func (r *DivergenceReporter) predicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			if !localManagersChanged(e.ObjectOld.GetManagedFields(), e.ObjectNew.GetManagedFields()) {
				return false
			}
			key := types.NamespacedName{Name: e.ObjectNew.GetName(), Namespace: e.ObjectNew.GetNamespace()}
			r.mu.Lock()
			r.lastEventAt[key] = time.Now()
			r.mu.Unlock()
			return true
		},
		CreateFunc:  func(_ event.CreateEvent) bool { return false },
		DeleteFunc:  func(_ event.DeleteEvent) bool { return false },
		GenericFunc: func(_ event.GenericEvent) bool { return false },
	}
}

// localManagersChanged reports whether the set of local:* manager fields changed between two managed-field slices.
func localManagersChanged(old, new []metav1.ManagedFieldsEntry) bool {
	extract := func(fields []metav1.ManagedFieldsEntry) map[string][]byte {
		m := make(map[string][]byte)
		for _, e := range fields {
			if strings.HasPrefix(e.Manager, "local:") && e.FieldsV1 != nil {
				m[e.Manager] = e.FieldsV1.Raw
			}
		}
		return m
	}
	oldMap := extract(old)
	newMap := extract(new)
	if len(oldMap) != len(newMap) {
		return true
	}
	for k, v := range oldMap {
		nv, ok := newMap[k]
		if !ok || !bytes.Equal(v, nv) {
			return true
		}
	}
	return false
}
