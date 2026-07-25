package controller

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
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
	// ConfigBundle is cluster-scoped — list cluster-wide.
	var list armadav1.ConfigBundleList
	if err := l.reporter.Client.List(ctx, &list); err != nil {
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
	// ConfigBundle is cluster-scoped — list cluster-wide.
	var list armadav1.ConfigBundleList
	if err := h.reporter.Client.List(ctx, &list); err != nil {
		logger.Info("list ConfigBundles failed", "err", err.Error())
		return
	}
	if len(list.Items) == 0 {
		return
	}
	// Only force-repost CBs that actually have local:* claims. The heartbeat's
	// purpose is recovering from an orb state wipe — but if a CB has no local
	// overrides, orb's view is "empty" with or without a wipe, so re-posting
	// an empty payload would be noise. CBs with claims get their status-side
	// dedup hash cleared so Reconcile re-POSTs even if the override set is
	// unchanged.
	var withClaims []armadav1.ConfigBundle
	for _, cb := range list.Items {
		if len(extractAdminPaths(cb.ManagedFields)) > 0 {
			withClaims = append(withClaims, cb)
		}
	}
	for _, cb := range withClaims {
		if err := h.reporter.clearReportingHash(ctx, cb.Name); err != nil {
			logger.Info("clear reporting hash failed", "configbundle", cb.Name, "err", err.Error())
			// Fall through to reconcile — the hash mismatch on next reconcile
			// will happen anyway when we recompute. The clearReportingHash write
			// is a belt-and-suspenders reset for the reader.
		}
	}
	// Trigger reconcile for each CR with claims. Direct call bypasses the work
	// queue — acceptable here because we ARE the periodic re-sync; there's no
	// event debouncing to honor.
	for _, cb := range withClaims {
		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: cb.Name}}
		if _, err := h.reporter.Reconcile(ctx, req); err != nil {
			logger.Error(err, "reconcile failed", "configbundle", cb.Name)
		}
	}
	// Silent at steady state — only log ticks that actually re-posted. Matches
	// the A+B steady-state-quiet pattern used by the Reconcile POST path. The
	// "heartbeat started" line at Start() handles liveness; per-tick noise on
	// an idle cluster is just log spam.
	if len(withClaims) > 0 {
		logger.Info("heartbeat tick complete", "configbundles", len(list.Items), "withClaims", len(withClaims))
	}
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

	overrides := r.extractOverrides(&cb, lastManifest)
	payload := DivergencePayload{Overrides: overrides}
	h := contentHash(payload)

	// Dedup + steady-state-quiet read from CR status. Nil DivergenceReporting
	// unambiguously means "no POST has ever landed for this CB" — treat it as
	// unknown and force a POST on this reconcile (biased toward orb-sync
	// correctness over one avoidable POST). The pointer LastPostedOverrideCount
	// distinguishes "never posted" (nil) from "posted empty" (*0), which fixes
	// the earlier cold-start bug where an empty in-memory map silently claimed
	// "last post was empty" after a restart.
	prior := cb.Status.DivergenceReporting

	// Exact-hash dedup: if we've already told orb this exact payload, skip.
	if prior != nil && prior.LastPostedHash == h {
		logger.V(1).Info("override set unchanged, skipping POST", "configbundle", req.Name)
		if err := r.touchCheckedAt(ctx, req.Name); err != nil {
			logger.Info("update lastCheckedForDivergence failed (will retry)", "configbundle", req.Name, "err", err.Error())
		}
		return reconcile.Result{}, nil
	}

	// Steady-state quiet: current empty AND the CR's status confirms our last
	// POST was also empty (LastPostedOverrideCount is *0). Skip the redundant
	// POST — orb already knows the state is empty.
	if len(overrides) == 0 && prior != nil && prior.LastPostedOverrideCount != nil && *prior.LastPostedOverrideCount == 0 {
		// Still update the hash so subsequent same-payload reconciles hit the
		// dedup fast-path above without re-hashing history.
		if prior.LastPostedHash != h {
			if err := r.writeReportingStatus(ctx, req.Name, h, 0); err != nil {
				logger.Info("update reporting status failed (will retry)", "configbundle", req.Name, "err", err.Error())
			}
		} else {
			if err := r.touchCheckedAt(ctx, req.Name); err != nil {
				logger.Info("update lastCheckedForDivergence failed (will retry)", "configbundle", req.Name, "err", err.Error())
			}
		}
		return reconcile.Result{}, nil
	}

	if err := r.postToOrb(ctx, payload); err != nil {
		logger.Error(err, "POST divergence failed", "configbundle", req.Name, "url", r.intakeURL)
		return reconcile.Result{}, fmt.Errorf("POST divergence: %w", err)
	}

	if err := r.writeReportingStatus(ctx, req.Name, h, len(overrides)); err != nil {
		// POST succeeded but status write failed — log and move on. Next
		// reconcile will re-POST (hash won't match), which is safe because
		// orb's intake is replace-not-merge and idempotent for identical
		// payloads.
		logger.Info("update reporting status failed (next reconcile will re-POST)", "configbundle", req.Name, "err", err.Error())
	}

	if len(overrides) > 0 {
		logger.Info("reported divergence", "configbundle", req.Name, "overrides", len(overrides))
	} else {
		// Empty payload we sent because prior was nil (cold start) or
		// non-empty (transition-to-empty). Both are meaningful — log distinctly
		// from the steady-state quiet path above.
		logger.Info("cleared divergence (override set went empty or first post-restart POST)", "configbundle", req.Name)
	}
	return reconcile.Result{}, nil
}

// writeReportingStatus persists the just-POSTed hash + override count to
// cb.status.divergenceReporting. Wrapped in RetryOnConflict because multiple
// writers (ConsumeServer, ConfigBundleReconciler, this reporter) may race on
// the same CR's status subresource. Best-effort: a failed write means the next
// reconcile will re-POST (hash won't match), which is safe because orb's
// intake is replace-not-merge and idempotent for identical payloads.
func (r *DivergenceReporter) writeReportingStatus(ctx context.Context, name, hash string, count int) error {
	countCopy := count
	now := metav1.Now()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh armadav1.ConfigBundle
		if err := r.Client.Get(ctx, types.NamespacedName{Name: name}, &fresh); err != nil {
			return client.IgnoreNotFound(err)
		}
		fresh.Status.DivergenceReporting = &armadav1.DivergenceReportingStatus{
			LastCheckedForDivergence: &now,
			LastPostedAt:             &now,
			LastPostedHash:           hash,
			LastPostedOverrideCount:  &countCopy,
		}
		return r.Client.Status().Update(ctx, &fresh)
	})
}

// touchCheckedAt updates only LastCheckedForDivergence without touching the
// POST-related fields. Called on dedup early-return paths where the reporter
// evaluated the override set but correctly skipped the POST.
func (r *DivergenceReporter) touchCheckedAt(ctx context.Context, name string) error {
	now := metav1.Now()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh armadav1.ConfigBundle
		if err := r.Client.Get(ctx, types.NamespacedName{Name: name}, &fresh); err != nil {
			return client.IgnoreNotFound(err)
		}
		if fresh.Status.DivergenceReporting == nil {
			fresh.Status.DivergenceReporting = &armadav1.DivergenceReportingStatus{}
		}
		fresh.Status.DivergenceReporting.LastCheckedForDivergence = &now
		return r.Client.Status().Update(ctx, &fresh)
	})
}

// clearReportingHash zeroes out the LastPostedHash field of cb.status.divergenceReporting
// so the next reconcile's hash check misses and a POST fires. Called by the heartbeat
// for CBs that have local:* claims — the tick's whole purpose is to force a re-sync
// against a possibly-wiped orb store.
//
// Leaves LastPostedOverrideCount intact: if the last known state was "posted N overrides,"
// we still know that, and the next reconcile will POST current state (whatever it is).
// Wrapped in RetryOnConflict for the same multi-writer reason as writeReportingStatus.
func (r *DivergenceReporter) clearReportingHash(ctx context.Context, name string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh armadav1.ConfigBundle
		if err := r.Client.Get(ctx, types.NamespacedName{Name: name}, &fresh); err != nil {
			return client.IgnoreNotFound(err)
		}
		if fresh.Status.DivergenceReporting == nil {
			// Never posted → nothing to clear. Next reconcile will POST anyway
			// because prior == nil takes the force-post branch.
			return nil
		}
		if fresh.Status.DivergenceReporting.LastPostedHash == "" {
			return nil
		}
		fresh.Status.DivergenceReporting.LastPostedHash = ""
		return r.Client.Status().Update(ctx, &fresh)
	})
}

func (r *DivergenceReporter) predicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			if !localManagersChanged(e.ObjectOld.GetManagedFields(), e.ObjectNew.GetManagedFields()) {
				return false
			}
			// ConfigBundle is cluster-scoped — Namespace is always "".
			key := types.NamespacedName{Name: e.ObjectNew.GetName()}
			r.mu.Lock()
			r.lastEventAt[key] = time.Now()
			r.mu.Unlock()
			return true
		},
		// Fire on Create so a controller restart / rollout immediately re-establishes
		// the divergence picture on orb. The reconcile path is naturally cold-start-safe:
		//   - lastEventAt is zero for these → debounce check passes (treated as startup)
		//   - lastPostedHash is empty → POST fires unconditionally (no dedup race)
		//   - lastManifest guard skips the POST if bootstrap hasn't loaded it yet
		// This closes the up-to-heartbeat-interval observability gap that would
		// otherwise exist after every rollout.
		CreateFunc:  func(_ event.CreateEvent) bool { return true },
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
