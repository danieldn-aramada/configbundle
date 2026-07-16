package serverconfig

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	armadav1 "github.com/armada/configbundle/api/v1"
)

// ConditionReconciled is the canonical condition type written by this
// controller. Type=True ⇒ live state matches intent (after PATCH, or already
// matching on read). Type=False ⇒ a step failed; Reason names which.
const ConditionReconciled = "Reconciled"

// iDRAC attribute keys for the toggles this controller manages. Add new
// entries here as more settings are wired in, and extend computeIdracDeltas
// + unmanagedFields + policyBlockedFields + the KnownIdracFields registry
// below.
const (
	attrSSHEnable    = "SSH.1.Enable"
	attrRacadmEnable = "Racadm.1.Enable"
	attrIPMIEnable   = "IPMILan.1.Enable"
)

// KnownIdracFields enumerates every CRD JSON field name this controller
// recognizes for reconciliation. Used at startup to warn about unknown
// entries in IDRAC_FIELD_ALLOWLIST and to gate which intents the controller
// will act on. The names match the JSON tags on armadav1.IdracSettingsSpec.
var KnownIdracFields = []string{"sshEnabled", "racadmEnabled", "ipmiEnabled"}

// UnknownAllowlistEntries returns any entries in `allowed` that don't name
// a field this controller knows how to reconcile. Used by main.go to warn
// about typos at startup.
func UnknownAllowlistEntries(allowed map[string]bool) []string {
	known := map[string]bool{}
	for _, f := range KnownIdracFields {
		known[f] = true
	}
	var unknown []string
	for k := range allowed {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	return unknown
}

// enabledStr / disabledStr are the canonical Attributes-API values for a
// bool-flavored toggle on Dell iDRAC. iDRAC sometimes capitalizes inconsistently
// across firmware versions, so reads are compared case-insensitively.
const (
	enabledStr  = "Enabled"
	disabledStr = "Disabled"
)

func boolToEnableStr(b bool) string {
	if b {
		return enabledStr
	}
	return disabledStr
}

func enableStrToBool(s string) bool {
	return strings.EqualFold(s, enabledStr)
}

// computeIdracDeltas returns the subset of (attribute → intent-string) pairs
// that need PATCHing — i.e., spec sets an intent, the field is allowed by
// policy, AND live differs from intent. Empty result means "nothing to do."
// Pure function: no K8s, no HTTP — straightforward to unit-test.
func computeIdracDeltas(spec armadav1.IdracSettingsSpec, live map[string]any, allowed map[string]bool) map[string]string {
	out := map[string]string{}
	if spec.SSHEnabled != nil && allowed["sshEnabled"] {
		intentStr := boolToEnableStr(*spec.SSHEnabled)
		if liveStr, _ := live[attrSSHEnable].(string); !strings.EqualFold(liveStr, intentStr) {
			out[attrSSHEnable] = intentStr
		}
	}
	if spec.RacadmEnabled != nil && allowed["racadmEnabled"] {
		intentStr := boolToEnableStr(*spec.RacadmEnabled)
		if liveStr, _ := live[attrRacadmEnable].(string); !strings.EqualFold(liveStr, intentStr) {
			out[attrRacadmEnable] = intentStr
		}
	}
	if spec.IPMIEnabled != nil && allowed["ipmiEnabled"] {
		intentStr := boolToEnableStr(*spec.IPMIEnabled)
		if liveStr, _ := live[attrIPMIEnable].(string); !strings.EqualFold(liveStr, intentStr) {
			out[attrIPMIEnable] = intentStr
		}
	}
	return out
}

// hasReconcilableIntent reports whether spec specifies any setting that is
// BOTH (a) recognized by this controller and (b) permitted by the field
// allowlist. Used to short-circuit before loading creds or touching the
// network. Note: fields with intent set but blocked by policy don't count —
// the early-return log surfaces them via policyBlockedFields().
func hasReconcilableIntent(spec armadav1.IdracSettingsSpec, allowed map[string]bool) bool {
	return (spec.SSHEnabled != nil && allowed["sshEnabled"]) ||
		(spec.RacadmEnabled != nil && allowed["racadmEnabled"]) ||
		(spec.IPMIEnabled != nil && allowed["ipmiEnabled"])
}

// unmanagedFields lists the managed-field names whose intent is nil on the
// given spec. Order is stable (declaration order). Returned in the summary
// log so operators can see which fields the controller deliberately ignored
// (no intent) vs. fields it acted on.
func unmanagedFields(spec armadav1.IdracSettingsSpec) []string {
	var out []string
	if spec.SSHEnabled == nil {
		out = append(out, "sshEnabled")
	}
	if spec.RacadmEnabled == nil {
		out = append(out, "racadmEnabled")
	}
	if spec.IPMIEnabled == nil {
		out = append(out, "ipmiEnabled")
	}
	return out
}

// policyBlockedFields lists fields where intent IS set in the spec but the
// field is NOT in the allow set. Logged so operators can see why a field
// they expected to be reconciled didn't fire.
func policyBlockedFields(spec armadav1.IdracSettingsSpec, allowed map[string]bool) []string {
	var out []string
	if spec.SSHEnabled != nil && !allowed["sshEnabled"] {
		out = append(out, "sshEnabled")
	}
	if spec.RacadmEnabled != nil && !allowed["racadmEnabled"] {
		out = append(out, "racadmEnabled")
	}
	if spec.IPMIEnabled != nil && !allowed["ipmiEnabled"] {
		out = append(out, "ipmiEnabled")
	}
	return out
}

// ServerConfigReconciler watches ServerConfig CRs and reconciles spec.idrac
// against the iDRAC at spec.oobIP via Redfish. Prototype scope: only the
// SSH.ProtocolEnabled setting is wired through.
type ServerConfigReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// AllowedOobIPs is the set of OOB IPs the controller will reconcile against.
	// CRs whose spec.oobIP is not in this set are logged and skipped without any
	// status writes. Empty set = nothing is reconciled (paranoid default).
	AllowedOobIPs map[string]bool

	// CredentialsNamespace + CredentialsSecretName name the Secret carrying
	// `username` + `password` keys used for Redfish basic auth. The same Secret
	// is used for every iDRAC in this prototype.
	CredentialsNamespace  string
	CredentialsSecretName string

	// AllowedFields is the set of CRD JSON field names the controller is
	// permitted to reconcile. Fields with intent set on a CR but not in this
	// set are dropped from the PATCH (logged as `policyBlocked`). Empty set =
	// paranoid (nothing reconciled). Same shape as AllowedOobIPs.
	AllowedFields map[string]bool

	// ObserveInterval is the cadence at which the reconciler re-polls iDRAC
	// even when nothing on the CR has changed. Drives drift detection: without
	// it, an out-of-band iDRAC change (someone toggles via the web UI) would
	// stay invisible until the next spec change. Zero = no periodic poll
	// (event-driven only).
	ObserveInterval time.Duration

	// Recorder emits per-action Kubernetes Events (PATCH landed, PATCH failed,
	// etc.) so operators can see the action history via `kubectl describe sc
	// <name>` alongside the K8s-native Events section. Replaces the bounded
	// status.recentPatches[] we used to maintain — Events are the K8s norm
	// for "what happened when," complete with TTL, dedup (count/lastTimestamp),
	// and cross-resource correlation via involvedObject.
	Recorder record.EventRecorder
}

func (r *ServerConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// GenerationChangedPredicate fires reconcile on Create + spec changes only.
	// Status updates and managedFields-only changes don't bump generation, so
	// they don't re-fire. Combined with our "log once per change" semantic,
	// this keeps the log clean: one log per CR at startup (cache sync), then
	// silence until spec.idrac.* actually changes.
	return ctrl.NewControllerManagedBy(mgr).
		For(&armadav1.ServerConfig{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("serverconfig").
		Complete(r)
}

func (r *ServerConfigReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx).WithName("serverconfig")

	var sc armadav1.ServerConfig
	if err := r.Get(ctx, req.NamespacedName, &sc); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("serverconfig deleted")
			// Drop the deleted server from the ssh_enabled and reconcile-success
			// gauges so it stops appearing.
			removeSSHEnabled(req.Name)
			removeReconcileSuccess(req.Name)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	// No oobIP means nothing actionable — surface the skip on status so
	// `kubectl describe sc <name>` explains the blank live state.
	if sc.Spec.OobIP == nil || *sc.Spec.OobIP == "" {
		logger.V(1).Info("no oobIP on serverconfig; skipping",
			"serviceTag", sc.Spec.ServiceTag)
		r.setStatusSkipped(ctx, &sc, "NoOobIP",
			"spec.oobIP is empty — no target to reconcile against. Populate spec.oobIP to enable reconciliation.")
		return reconcile.Result{}, nil
	}
	oobIP := *sc.Spec.OobIP

	// Allowlist enforcement — short-circuit before fetching credentials or
	// touching the network. Status is written so operators can distinguish
	// "controller broken" from "this CR is deliberately not managed by this
	// controller instance" without reading logs.
	if !r.AllowedOobIPs[oobIP] {
		logger.V(1).Info("change detected but oobIP not on allowlist, ignoring",
			"oobIP", oobIP,
			"intent.sshEnabled", boolPtr(sc.Spec.IdracSettings.SSHEnabled),
			"intent.racadmEnabled", boolPtr(sc.Spec.IdracSettings.RacadmEnabled),
			"intent.ipmiEnabled", boolPtr(sc.Spec.IdracSettings.IPMIEnabled))
		r.setStatusSkipped(ctx, &sc, "NotInOobAllowlist",
			fmt.Sprintf("oobIP %s is not in IDRAC_OOB_ALLOWLIST — this ServerConfig is not managed by this controller instance.", oobIP))
		return reconcile.Result{}, nil
	}

	// Nothing reconcilable on this CR — either no intent set, or all intent
	// fields are blocked by the field allowlist. Surface it as a deliberate
	// skip (Phase=Skipped, Reconciled=Unknown), consistent with the other skip
	// gates above — a benign "nothing to enforce," not a fault.
	if !hasReconcilableIntent(sc.Spec.IdracSettings, r.AllowedFields) {
		logger.V(1).Info("no reconcilable intent on serverconfig; skipping",
			"oobIP", oobIP,
			"unmanaged", unmanagedFields(sc.Spec.IdracSettings),
			"policyBlocked", policyBlockedFields(sc.Spec.IdracSettings, r.AllowedFields))
		r.setStatusSkipped(ctx, &sc, "NoManagedFields",
			"no allowlisted iDRAC field has intent set — nothing to reconcile")
		return reconcile.Result{}, nil
	}

	// Load shared credentials.
	creds, err := loadIdracCredentials(ctx, r.Client, r.CredentialsNamespace, r.CredentialsSecretName)
	if err != nil {
		// Treat "Secret not found" as a config error, not a transient one —
		// controller-runtime's exponential backoff would otherwise spam the
		// log with stack traces 8×/sec until the operator creates the Secret.
		// Long requeue lets us recover automatically without the noise.
		if apierrors.IsNotFound(err) {
			logger.Info("iDRAC credentials Secret not found; deferring reconcile (create the Secret to proceed)",
				"secret", r.CredentialsNamespace+"/"+r.CredentialsSecretName)
			r.setStatusFailed(ctx, &sc, "MissingCredentials", err.Error())
			recordReconcileError(sc.Name, oobIP, sc.Spec.OrbID, "MissingCredentials")
			return reconcile.Result{RequeueAfter: 1 * time.Minute}, nil
		}
		logger.Error(err, "load iDRAC credentials")
		r.setStatusFailed(ctx, &sc, "CredentialsLoadFailed", err.Error())
		recordReconcileError(sc.Name, oobIP, sc.Spec.OrbID, "CredentialsLoadFailed")
		return reconcile.Result{}, fmt.Errorf("load credentials: %w", err)
	}

	// Read live iDRAC attributes, compute deltas across every managed field.
	rc := newRedfishClient(oobIP, creds.Username, creds.Password)
	attrs, err := rc.GetAttributes(ctx)
	if err != nil {
		logger.Error(err, "read iDRAC Attributes", "oobIP", oobIP)
		r.setStatusFailed(ctx, &sc, "RedfishReadFailed", err.Error())
		recordReconcileError(sc.Name, oobIP, sc.Spec.OrbID, "RedfishReadFailed")
		return reconcile.Result{}, fmt.Errorf("read Attributes from %s: %w", oobIP, err)
	}

	// Promote the observed SSH-enabled state to a metric
	// (configbundle_serverconfig_ssh_enabled) — SSH is a security-relevant toggle
	// worth alerting on; the rest of idracSettings stays on .status. Read straight
	// from the live Redfish attributes, sourced here (not from the CR status
	// write) so a status-write conflict can never blank the metric. Report what
	// this poll actually read off the device, raw, before any drift correction
	// below. If SSH can't be read, drop the series (absent = unknown, never a
	// false 0).
	if raw, ok := attrs[attrSSHEnable].(string); ok {
		recordSSHEnabled(sc.Name, oobIP, sc.Spec.OrbID, enableStrToBool(raw))
	} else {
		removeSSHEnabled(sc.Name)
	}
	r.recordObserved(ctx, &sc, attrs)

	deltas := computeIdracDeltas(sc.Spec.IdracSettings, attrs, r.AllowedFields)
	unmanaged := unmanagedFields(sc.Spec.IdracSettings)
	blocked := policyBlockedFields(sc.Spec.IdracSettings, r.AllowedFields)

	if len(deltas) == 0 {
		logger.V(1).Info("reconciled (no PATCH needed)",
			"oobIP", oobIP,
			"unmanaged", unmanaged,
			"policyBlocked", blocked)
		// Steady-state: keep the Reconciled=True condition's LastTransitionTime
		// stable (K8s norm — LTT only moves on Status flip). bump ObservedGeneration
		// and LastAppliedAt so tooling sees "controller is caught up and still
		// working." First-time convergence path (isCurrentlyReconciled=false)
		// flips the condition via setStatusApplied.
		if !meta.IsStatusConditionTrue(sc.Status.Conditions, ConditionReconciled) {
			r.setStatusApplied(ctx, &sc)
		} else {
			r.markReconcileSuccess(ctx, &sc)
		}
		recordReconcileSuccess(sc.Name, oobIP, sc.Spec.OrbID, time.Now().Unix())
		return reconcile.Result{RequeueAfter: r.ObserveInterval}, nil
	}

	if err := rc.PatchAttributes(ctx, deltas); err != nil {
		logger.Error(err, "PATCH iDRAC attributes", "oobIP", oobIP, "updates", deltas)
		r.setStatusFailed(ctx, &sc, "RedfishPatchFailed", err.Error())
		recordReconcileError(sc.Name, oobIP, sc.Spec.OrbID, "RedfishPatchFailed")
		return reconcile.Result{}, fmt.Errorf("PATCH attributes on %s: %w", oobIP, err)
	}
	logger.Info("reconciled (PATCH applied)",
		"oobIP", oobIP,
		"unmanaged", unmanaged,
		"policyBlocked", blocked,
		"updates", deltas)
	// Emit a Kubernetes Event with the specific PATCH detail — the K8s-native
	// "action history" surface (see `kubectl describe sc <name>` Events). The
	// observed metric and .status were already recorded above from the raw
	// poll; the corrected value surfaces on the next poll, so observed stays an
	// honest "what we saw" snapshot rather than "what we just asserted."
	patchMsg := fmt.Sprintf("PATCHed %d attribute(s): %s", len(deltas), formatDeltas(deltas))
	r.Recorder.Eventf(&sc, corev1.EventTypeNormal, "Applied", patchMsg)
	r.setStatusApplied(ctx, &sc)
	recordReconcileSuccess(sc.Name, oobIP, sc.Spec.OrbID, time.Now().Unix())
	return reconcile.Result{RequeueAfter: r.ObserveInterval}, nil
}

// formatDeltas renders a small map as a stable, human-readable string for
// status messages: "SSH.1.Enable=Disabled, Racadm.1.Enable=Enabled".
func formatDeltas(d map[string]string) string {
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, d[k]))
	}
	return strings.Join(parts, ", ")
}

// setStatusApplied writes Phase=Applied + Reconciled=True with a stable,
// generic Reason and Message — the K8s norm is that Condition Message
// describes STATE ("we're converged"), not the last ACTION. Per-PATCH
// action detail goes to Kubernetes Events (via r.Recorder), not the
// Condition. Best-effort: status conflicts are logged and dropped; the
// next reconcile will reassert.
func (r *ServerConfigReconciler) setStatusApplied(ctx context.Context, sc *armadav1.ServerConfig) {
	r.writeStatus(ctx, sc, armadav1.ServerConfigPhaseApplied, metav1.ConditionTrue,
		"SettingsApplied", "all managed settings match intent", true /* bumpLastApplied */)
}

// setStatusFailed writes Phase=Diverged + Reconciled=False with a Reason that
// names which step failed (MissingCredentials, RedfishReadFailed, RedfishPatchFailed).
// Does NOT bump LastAppliedAt — the reconcile did not succeed.
//
// Also emits a Warning Event so failures surface to humans via `kubectl
// describe sc <name>` and `kubectl get events`, symmetric with the Normal
// "Applied" events on success. K8s Event aggregation dedups repeats
// (count/lastTimestamp), so a persistently-failing iDRAC yields one aggregated
// event, not a flood — safe to emit on every failing reconcile.
func (r *ServerConfigReconciler) setStatusFailed(ctx context.Context, sc *armadav1.ServerConfig, reason, msg string) {
	r.Recorder.Event(sc, corev1.EventTypeWarning, reason, msg)
	r.writeStatus(ctx, sc, armadav1.ServerConfigPhaseDiverged, metav1.ConditionFalse, reason, msg, false)
}

// setStatusSkipped writes Phase=Skipped + Reconciled=Unknown with a Reason
// describing why the controller deliberately did not reconcile this CR
// (NoOobIP, NotInOobAllowlist).
//
// Unknown, not False, is deliberate: False means "managed, and determined NOT
// converged" (a real problem worth alerting on). A skipped CR isn't managed by
// this controller instance, so convergence is simply not being determined —
// that's the textbook meaning of Unknown. Keeping it out of False means an
// operator alerting on `Reconciled=False` pages only for genuinely-broken
// servers, not for ones deliberately out of scope. (Same reasoning K8s uses
// for NodeReady=Unknown when a node is unreachable.)
// Does NOT bump LastAppliedAt — no apply happened.
func (r *ServerConfigReconciler) setStatusSkipped(ctx context.Context, sc *armadav1.ServerConfig, reason, msg string) {
	// Skipped ⇒ not managed here ⇒ the reconcile_success series must be ABSENT
	// (absent = skip), not 0 (which means "managed and failing"). Drop any prior
	// series for a server that has left the allowlist.
	removeReconcileSuccess(sc.Name)
	r.writeStatus(ctx, sc, armadav1.ServerConfigPhaseSkipped, metav1.ConditionUnknown, reason, msg, false)
}

// writeStatus updates the ServerConfig's Phase + Reconciled condition, and
// optionally bumps LastAppliedAt (success paths only). Wrapped in
// RetryOnConflict so an immediately-prior Get's resourceVersion going stale
// (e.g. just after `kubectl apply` creates the CR and the cache hasn't fully
// synced) doesn't surface a benign optimistic-concurrency 409.
func (r *ServerConfigReconciler) writeStatus(ctx context.Context, sc *armadav1.ServerConfig, phase armadav1.ServerConfigPhase, condStatus metav1.ConditionStatus, reason, msg string, bumpLastApplied bool) {
	logger := log.FromContext(ctx).WithName("serverconfig.status")
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh armadav1.ServerConfig
		if err := r.Get(ctx, client.ObjectKeyFromObject(sc), &fresh); err != nil {
			return err
		}
		fresh.Status.Phase = phase
		fresh.Status.ObservedGeneration = fresh.Generation
		if bumpLastApplied {
			now := metav1.Now()
			fresh.Status.LastAppliedAt = &now
		}
		// meta.SetStatusCondition is the apimachinery-canonical upsert: it moves
		// LastTransitionTime only when Status flips, and always refreshes
		// Reason/Message/ObservedGeneration — exactly the semantics we want.
		// Passing ObservedGeneration records which spec generation this
		// condition reflects (per-condition freshness signal).
		meta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{
			Type:               ConditionReconciled,
			Status:             condStatus,
			Reason:             reason,
			Message:            msg,
			ObservedGeneration: fresh.Generation,
		})
		return r.Status().Update(ctx, &fresh)
	})
	if err != nil {
		logger.Info("status update failed (will retry next reconcile)", "err", err.Error())
	}
}

// recordObserved writes status.idracSettings from the live Redfish
// Attributes map (passed in by the caller from the top-of-reconcile GET).
// Live-read semantics: observed reflects what actually exists on the iDRAC,
// not the intent we PATCHed. Consequences:
//   - out-of-band change flips a value → observed shows the LIVE value,
//     drift visible in `kubectl get sc -o yaml` on the next reconcile
//   - field missing from Attributes response (transient firmware quirk) →
//     that field is skipped for this pass, no series update
//   - field not on the allowlist → not written, even if live has a value
//     (matches the Prom metrics semantic in package-level recordObserved)
//
// Read-modify-write with RetryOnConflict; skipped entirely when the freshly
// projected observed equals what's already in status, so periodic polls in
// steady state are zero-cost. attrs is threaded through instead of re-fetched
// inside the retry loop: a fresh Redfish GET per conflict retry would burn
// iDRAC round-trips on unrelated status races.
func (r *ServerConfigReconciler) recordObserved(ctx context.Context, sc *armadav1.ServerConfig, attrs map[string]any) {
	desired := buildObservedIdrac(attrs, r.AllowedFields)
	if observedIdracEqual(sc.Status.IdracSettings, desired) {
		return
	}
	logger := log.FromContext(ctx).WithName("serverconfig.status")
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh armadav1.ServerConfig
		if err := r.Get(ctx, client.ObjectKeyFromObject(sc), &fresh); err != nil {
			return err
		}
		if observedIdracEqual(fresh.Status.IdracSettings, desired) {
			return nil
		}
		fresh.Status.IdracSettings = desired
		return r.Status().Update(ctx, &fresh)
	})
	if err != nil {
		logger.Info("observed status update failed (will retry next reconcile)", "err", err.Error())
	}
}

// buildObservedIdrac projects the live Redfish attribute map into the
// observed-idrac status shape, gated by the field allowlist. Fields absent
// from attrs (missing from firmware response) or not on the allowlist stay
// nil in the output — the ledger only reflects values the controller manages
// AND observed live on the target. Mirror of package-level recordObserved
// (metrics.go) — both share the same live-attr-derivation shape.
func buildObservedIdrac(attrs map[string]any, allowed map[string]bool) armadav1.ObservedIdracSettingsStatus {
	var out armadav1.ObservedIdracSettingsStatus
	set := func(field, attrKey string, dst **bool) {
		if !allowed[field] {
			return
		}
		raw, ok := attrs[attrKey].(string)
		if !ok {
			return
		}
		b := enableStrToBool(raw)
		*dst = &b
	}
	set("sshEnabled", attrSSHEnable, &out.SSHEnabled)
	set("ipmiEnabled", attrIPMIEnable, &out.IPMIEnabled)
	set("racadmEnabled", attrRacadmEnable, &out.RacadmEnabled)
	return out
}

func observedIdracEqual(a, b armadav1.ObservedIdracSettingsStatus) bool {
	return boolPtrEqual(a.SSHEnabled, b.SSHEnabled) &&
		boolPtrEqual(a.IPMIEnabled, b.IPMIEnabled) &&
		boolPtrEqual(a.RacadmEnabled, b.RacadmEnabled)
}

func boolPtrEqual(a, b *bool) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// markReconcileSuccess bumps status.observedGeneration and status.lastAppliedAt
// on steady-state successful reconciles (Reconciled=True stays True). The
// Reconciled condition is deliberately NOT rewritten here — K8s norm is that
// Condition.LastTransitionTime only moves on Status flip. LastAppliedAt is
// the truthful "controller is still doing work" signal; ObservedGeneration is
// the "controller has caught up to this spec.generation" signal. Skipped
// entirely when both markers already match, so periodic polls in steady state
// produce zero apiserver writes.
func (r *ServerConfigReconciler) markReconcileSuccess(ctx context.Context, sc *armadav1.ServerConfig) {
	needBump := sc.Status.ObservedGeneration != sc.Generation || sc.Status.LastAppliedAt == nil
	if !needBump {
		// Rate-limit LastAppliedAt writes to roughly once per ObserveInterval —
		// otherwise every 5-min drift poll would touch status even when nothing
		// meaningful changed. If the previous LastAppliedAt is older than half
		// the observe interval, bump it; otherwise skip.
		if r.ObserveInterval > 0 && time.Since(sc.Status.LastAppliedAt.Time) > r.ObserveInterval/2 {
			needBump = true
		}
	}
	if !needBump {
		return
	}
	logger := log.FromContext(ctx).WithName("serverconfig.status")
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh armadav1.ServerConfig
		if err := r.Get(ctx, client.ObjectKeyFromObject(sc), &fresh); err != nil {
			return err
		}
		changed := false
		if fresh.Status.ObservedGeneration != fresh.Generation {
			fresh.Status.ObservedGeneration = fresh.Generation
			changed = true
		}
		if fresh.Status.LastAppliedAt == nil || (r.ObserveInterval > 0 && time.Since(fresh.Status.LastAppliedAt.Time) > r.ObserveInterval/2) {
			now := metav1.Now()
			fresh.Status.LastAppliedAt = &now
			changed = true
		}
		if !changed {
			return nil
		}
		return r.Status().Update(ctx, &fresh)
	})
	if err != nil {
		logger.Info("reconcile-marker update failed (will retry next reconcile)", "err", err.Error())
	}
}

func boolPtr(p *bool) string {
	if p == nil {
		return "<nil>"
	}
	if *p {
		return "true"
	}
	return "false"
}
