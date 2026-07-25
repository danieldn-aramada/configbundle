package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	armadav1 "github.com/armada/configbundle/api/v1"
	"github.com/armada/configbundle/bundle"
)

const (
	defaultConsumePort = ":8095"
	defaultRetryMax    = 3
	defaultRetryWait   = time.Second
	maxManifestBytes   = 10 * 1024 * 1024 // 10 MB — matches ORBITAL_ENRICHER_MAX_RESPONSE_BYTES
)

// ConsumeServer is a ctrl.Runnable that exposes POST /dispatch for orb layer dispatch.
// Orb calls this endpoint after pulling and cosign-verifying the OCI artifact, routing
// layers by Content-Type. ConsumeServer validates the payload synchronously and
// applies it to the cluster asynchronously.
type ConsumeServer struct {
	Client    client.Client
	port      string
	namespace string
	retryMax  int
	retryWait time.Duration
	ctx       context.Context // lifecycle context set by Start(); defaults to Background()
	reporter  *DivergenceReporter

	// inFlight tracks ConfigBundles whose applyManifest is currently executing.
	// ReclaimController consults this to defer its replay path when a release
	// event arrives mid-apply — the controller's own ForceOwnership and
	// processTakeover passes strip local:* claims, which would otherwise
	// trigger reclaim and replay a stale manifest on top of a half-landed one.
	// Keyed by ConfigBundle name.
	inFlightMu sync.Mutex
	inFlight   map[string]bool

	// applyFn overrides applyManifest in tests.
	applyFn func(ctx context.Context, body []byte, digest, importID string) error
}

// IsInFlight reports whether applyManifest is currently executing for the
// named ConfigBundle. ReclaimController checks this before doing its replay
// pass to avoid racing with an in-progress consume.
func (s *ConsumeServer) IsInFlight(name string) bool {
	s.inFlightMu.Lock()
	defer s.inFlightMu.Unlock()
	return s.inFlight[name]
}

func (s *ConsumeServer) markInFlight(name string) {
	s.inFlightMu.Lock()
	if s.inFlight == nil {
		s.inFlight = map[string]bool{}
	}
	s.inFlight[name] = true
	s.inFlightMu.Unlock()
}

func (s *ConsumeServer) markComplete(name string) {
	s.inFlightMu.Lock()
	delete(s.inFlight, name)
	s.inFlightMu.Unlock()
}

// ConsumeServerOption configures a ConsumeServer.
type ConsumeServerOption func(*ConsumeServer)

// WithPort sets the TCP address the consume server listens on (default ":8095").
func WithPort(port string) ConsumeServerOption {
	return func(s *ConsumeServer) { s.port = port }
}

// WithNamespace sets the K8s namespace for child resources — ServerConfig CRs,
// the per-bundle mapping ConfigMap, the last-applied-spec ConfigMap. Default
// "configbundle-system". ConfigBundle itself is cluster-scoped and
// has no namespace.
func WithNamespace(ns string) ConsumeServerOption {
	return func(s *ConsumeServer) { s.namespace = ns }
}

// WithDivergenceReporter links a DivergenceReporter so the consume handler can
// record last-applied manifests for divergence comparison.
func WithDivergenceReporter(r *DivergenceReporter) ConsumeServerOption {
	return func(s *ConsumeServer) { s.reporter = r }
}

// WithRetry configures apply retry. maxAttempts is the total number of attempts
// (1 = no retry). backoffBase is the wait before the second attempt; each subsequent
// attempt doubles it (exponential backoff).
func WithRetry(maxAttempts int, backoffBase time.Duration) ConsumeServerOption {
	return func(s *ConsumeServer) {
		s.retryMax = maxAttempts
		s.retryWait = backoffBase
	}
}

func NewConsumeServer(c client.Client, opts ...ConsumeServerOption) *ConsumeServer {
	s := &ConsumeServer{
		Client:    c,
		port:      defaultConsumePort,
		namespace: "configbundle-system",
		retryMax:  defaultRetryMax,
		retryWait: defaultRetryWait,
		ctx:       context.Background(),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// NeedsLeaderElection returns false — all replicas serve /dispatch.
// SSA patches from the same field owner are idempotent; concurrent applies are safe.
func (s *ConsumeServer) NeedsLeaderElection() bool { return false }

// Start implements ctrl.Runnable. Runs until ctx is cancelled.
func (s *ConsumeServer) Start(ctx context.Context) error {
	s.ctx = ctx
	logger := log.FromContext(ctx).WithName("consume-server")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /dispatch", s.handleDispatch)
	srv := &http.Server{Addr: s.port, Handler: mux}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	logger.Info("starting", "port", s.port, "namespace", s.namespace)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("consume server: %w", err)
	}
	return nil
}

// handleDispatch is the POST /dispatch handler. Orb calls this for each layer it dispatches,
// routing by Content-Type. After ADR-011 the only layer cb-controller consumes is the
// manifest layer; the mapping layer was deleted because all orbital identifiers now live
// directly on the ConfigBundle CR.
func (s *ConsumeServer) handleDispatch(w http.ResponseWriter, r *http.Request) {
	ct := r.Header.Get("Content-Type")
	switch ct {
	case bundle.MediaTypeManifest:
		tag := r.Header.Get("X-Orb-Tag")
		digest := r.Header.Get("X-Orb-Digest")
		importID := r.Header.Get("X-Orb-Import-ID")
		s.handleManifestBody(w, r, tag, digest, importID)
	default:
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
	}
}

// handleManifestBody processes the manifest layer. Validation is synchronous (bad payload
// returns 4xx). The K8s apply is asynchronous: the handler returns 200 as soon as the
// payload is accepted, then applies in the background using the server lifecycle context.
func (s *ConsumeServer) handleManifestBody(w http.ResponseWriter, r *http.Request, tag, digest, importID string) {
	logger := log.FromContext(r.Context()).WithName("consume")

	body, err := io.ReadAll(io.LimitReader(r.Body, maxManifestBytes+1))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if len(body) > maxManifestBytes {
		http.Error(w, "manifest exceeds max size", http.StatusRequestEntityTooLarge)
		return
	}

	// Validate synchronously — bad payload is the caller's concern.
	spec, err := parseManifest(body)
	if err != nil {
		http.Error(w, "invalid manifest: "+err.Error(), http.StatusBadRequest)
		return
	}
	if spec.Datacenter == "" {
		http.Error(w, "manifest missing datacenter field", http.StatusBadRequest)
		return
	}

	logger.Info("received dispatch",
		"tag", tag, "digest", digest, "importID", importID, "bytes", len(body),
	)

	apply := s.applyManifest
	if s.applyFn != nil {
		apply = s.applyFn
	}

	// Apply asynchronously — K8s apply latency must not block orb's import pipeline.
	go func() {
		if err := apply(s.ctx, body, digest, importID); err != nil {
			logger.Error(err, "async apply failed", "importID", importID, "digest", digest)
			return
		}
		logger.Info("async apply succeeded", "importID", importID, "digest", digest)
	}()

	w.WriteHeader(http.StatusOK)
}

// handleConsume is kept as an alias for handleManifestBody to support direct
// method calls in existing unit tests. It extracts the headers and delegates.
func (s *ConsumeServer) handleConsume(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Content-Type") != bundle.MediaTypeManifest {
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		return
	}
	tag := r.Header.Get("X-Orb-Tag")
	digest := r.Header.Get("X-Orb-Digest")
	importID := r.Header.Get("X-Orb-Import-ID")
	s.handleManifestBody(w, r, tag, digest, importID)
}

// applyManifest parses the manifest bytes, runs the admin-override-aware SSA pipeline,
// and updates ConfigBundle status. Retries the SSA patch on transient K8s API errors.
func (s *ConsumeServer) applyManifest(ctx context.Context, body []byte, digest, importID string) error {
	spec, err := parseManifest(body)
	if err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	if spec.Datacenter == "" {
		return fmt.Errorf("manifest has empty datacenter field")
	}

	// Mark this CB as having an apply in flight so ReclaimController defers any
	// release events fired by our own ForceOwnership / processTakeover passes.
	// Cleared at the very end (after the durable ConfigMap write) so reclaim's
	// next attempt reads a consistent ConfigMap if it falls back to that path.
	s.markInFlight(spec.Datacenter)
	defer s.markComplete(spec.Datacenter)

	// Update the in-memory last-manifest BEFORE any K8s write that can trigger
	// a reclaim event. ReclaimController prefers this in-memory value over the
	// persisted ConfigMap (which is only written at the end of this function),
	// so a release fired mid-apply replays the FRESH manifest, not the previous
	// one. The persisted ConfigMap remains the durable bootstrap baseline.
	if s.reporter != nil {
		s.reporter.SetLastManifest(spec.Datacenter, spec)
	}

	// Fetch the current CR to read managedFields for omitAdminOwnedServers.
	// ConfigBundle is cluster-scoped — no namespace.
	var cb armadav1.ConfigBundle
	err = s.Client.Get(ctx, types.NamespacedName{Name: spec.Datacenter}, &cb)
	if client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("get ConfigBundle: %w", err)
	}

	// Flag any spec writer that doesn't follow the local:<id> convention.
	// They're silently dropped by omitAdminOwnedFields below; the warning
	// makes the silent drop visible at the runtime where the bug bites.
	warnNonConformingManagers(log.FromContext(ctx).WithName("consume"), spec.Datacenter, cb.ManagedFields)

	// Drop spec.Ignored entries whose target field has no active local:*
	// claim. Per ADR-009, an Ignore directive is meaningless without an
	// override — persisting such entries (e.g. after edge handback, or
	// because orbital hasn't yet cleaned up a stale resolution row) violates
	// the data-model invariant. Apply the spec without them so the CR stays
	// internally consistent.
	claimed := collectLocalClaimedKeys(cb.ManagedFields)
	spec.Ignored = filterActiveIgnored(spec.Ignored, claimed)

	// Omit only "bow-out" leaves: local:*-owned fields where intent value differs
	// from live value AND the field isn't in spec.takeover[]. Values-match fields
	// stay in the apply and get force-claimed below — no steady-state co-ownership.
	// See ADR-008 (companion simplification 2026-06-16).
	patchSpec, err := omitAdminOwnedFields(spec, cb.Spec, cb.ManagedFields)
	if err != nil {
		return fmt.Errorf("compute admin-omitted patch: %w", err)
	}

	// ConfigBundle is cluster-scoped — no namespace in metadata.
	apply := &armadav1.ConfigBundle{
		TypeMeta: metav1.TypeMeta{
			APIVersion: armadav1.GroupVersion.String(),
			Kind:       "ConfigBundle",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: spec.Datacenter,
		},
		Spec: *patchSpec,
	}

	// Retry with exponential backoff on transient K8s API errors.
	var lastErr error
	for attempt := 0; attempt < s.retryMax; attempt++ {
		if attempt > 0 {
			wait := s.retryWait * (1 << (attempt - 1)) // 1s, 2s, 4s …
			select {
			case <-ctx.Done():
				return fmt.Errorf("apply cancelled after %d attempt(s): %w", attempt, ctx.Err())
			case <-time.After(wait):
			}
		}
		// ForceOwnership: silently evict local:*-co-managers on the fields we DID
		// include (intent matched live → no value conflict, ownership transfer
		// is the whole point). Bowed-out fields aren't in `apply.Spec` so they're
		// untouched. ADR-008 release pass cleans up the now-stale local:* claim
		// records afterward.
		lastErr = s.Client.Patch(ctx, apply, client.Apply,
			client.FieldOwner("configbundle-controller"),
			client.ForceOwnership,
		)
		if lastErr == nil {
			break
		}
	}
	// Takeover pass — runs regardless of whether the normal apply succeeded (ADR-006).
	// Force-conflicts reclaims ownership of specific fields from local:admin.
	// The takeover apply re-sends the same patchSpec plus the takeover target
	// leaves; ForceOwnership only effectively claims the takeover targets
	// (everything else is already controller-owned and not in conflict).
	takeoverErr := s.processTakeover(ctx, spec, patchSpec)

	if lastErr != nil && takeoverErr != nil {
		return fmt.Errorf("apply failed: %w; takeover also failed: %v", lastErr, takeoverErr)
	}
	if lastErr != nil {
		return fmt.Errorf("apply ConfigBundle spec (after %d attempt(s)): %w", s.retryMax, lastErr)
	}
	if takeoverErr != nil {
		return fmt.Errorf("takeover: %w", takeoverErr)
	}

	// Reconcile every local:* manager's claim tree so managedFields reflects
	// only meaningful ownership: takeover-target leaves get released, listMap
	// identity residuals get dropped, and managers with no real claims left
	// disappear entirely. Runs unconditionally — the same code path handles
	// takeover cleanup, post-release residuals, and true no-ops (SSA idempotent
	// when the reconstructed body matches current claims). Non-fatal.
	if err := s.reconcileLocalClaims(ctx, spec); err != nil {
		log.FromContext(ctx).WithName("consume").Info("reconcile local:* manager claims failed (non-fatal)", "err", err.Error())
	}

	// Persist the manifest to the per-CR ConfigMap as the durable bootstrap
	// baseline. In-memory was already updated above (before any K8s write that
	// could fire reclaim). Persist failure is non-fatal: in-memory is correct;
	// only post-restart recovery degrades until the next dispatch.
	if s.reporter != nil {
		if err := writeLastAppliedSpec(ctx, s.Client, s.namespace, spec.Datacenter, spec); err != nil {
			log.FromContext(ctx).WithName("consume").Info("persist last-applied-spec failed (non-fatal)", "err", err.Error())
		}
	}

	// Re-read inside a retry loop that handles two distinct races:
	//   - NotFound: the SSA Apply above created the CR via the API server, but
	//     the controller-runtime cache hasn't seen the watch event yet, so
	//     Client.Get returns NotFound. Typically resolves within a few hundred
	//     ms. This is the first-import-after-kubectl-delete race that surfaces
	//     downstream as a 409 from the mapping handler ("ConfigBundle not
	//     found for digest") because lastAppliedDigest never gets set.
	//   - Conflict: the ConfigBundleReconciler writes ObservedGeneration in
	//     response to our SSA patch; concurrent status writes race on
	//     resourceVersion. RetryOnConflict alone would handle this case but
	//     misses NotFound, which is why we use OnError with both predicates.
	var prev metav1.ConditionStatus
	err = retry.OnError(retry.DefaultBackoff, func(e error) bool {
		return apierrors.IsNotFound(e) || apierrors.IsConflict(e)
	}, func() error {
		cur := &armadav1.ConfigBundle{}
		if err := s.Client.Get(ctx, client.ObjectKeyFromObject(apply), cur); err != nil {
			return err
		}
		now := metav1.Now()
		cur.Status.Phase = armadav1.ConfigBundlePhaseApplied
		cur.Status.LastAppliedDigest = digest
		cur.Status.LastOrbImportID = importID
		cur.Status.LastAppliedAt = &now
		prev = setCondition(&cur.Status.Conditions, armadav1.ConditionReconciled,
			metav1.ConditionTrue, "Reconciled", "manifest applied via orb dispatch")
		return s.Client.Status().Update(ctx, cur)
	})
	if err != nil {
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			return fmt.Errorf("update ConfigBundle status gave up after retries: %w", err)
		}
		return fmt.Errorf("update ConfigBundle status: %w", err)
	}
	if prev != metav1.ConditionTrue {
		log.FromContext(ctx).WithName("consume").Info("condition transitioned",
			"type", armadav1.ConditionReconciled, "from", prev, "to", metav1.ConditionTrue,
			"reason", "Reconciled")
	}

	return nil
}

// parseManifest deserialises the ConfigBundle manifest YAML layer into a ConfigBundleSpec.
func parseManifest(data []byte) (armadav1.ConfigBundleSpec, error) {
	var spec armadav1.ConfigBundleSpec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return spec, fmt.Errorf("unmarshal manifest YAML: %w", err)
	}
	return spec, nil
}

// omitAdminOwnedFields returns a typed ConfigBundleSpec with the "bow-out"
// subset of local:*-owned leaves removed: those where the controller's intent
// value differs from the live CR value AND the field is not explicitly in
// `spec.takeover[]`. Other local:*-owned leaves stay in the apply body — the
// controller's force-conflicts pass will silently claim them (no co-ownership
// is allowed in the steady state).
//
// Three cases per local:*-owned leaf:
//   - Field is in spec.takeover[] → KEEP (explicit reject/accept; force-claim
//     even though values disagree).
//   - intent value == live value → KEEP (no conflict; force-claim silently
//     evicts local:* on the next force-conflicts apply).
//   - intent value != live value, no takeover → OMIT (genuine override; bow
//     out, preserve local:*'s ownership; surfaces as divergence in the next
//     report).
//
// Granularity is per-leaf: if admin owns one iDRAC field on a server, the
// controller still updates the rest of that server.
//
// Mechanism: round-trip spec → JSON map → mark-and-delete bow-out paths →
// JSON → typed spec. Leaf pointers (ADR-007) with omitempty serialize as
// absent when nil, which is the SSA-correct way to "not claim this field."
func omitAdminOwnedFields(intent armadav1.ConfigBundleSpec, live armadav1.ConfigBundleSpec, managedFields []metav1.ManagedFieldsEntry) (*armadav1.ConfigBundleSpec, error) {
	rawIntent, err := json.Marshal(intent)
	if err != nil {
		return nil, fmt.Errorf("marshal intent: %w", err)
	}
	var intentMap map[string]any
	if err := json.Unmarshal(rawIntent, &intentMap); err != nil {
		return nil, fmt.Errorf("unmarshal intent: %w", err)
	}
	rawLive, err := json.Marshal(live)
	if err != nil {
		return nil, fmt.Errorf("marshal live: %w", err)
	}
	var liveMap map[string]any
	if err := json.Unmarshal(rawLive, &liveMap); err != nil {
		return nil, fmt.Errorf("unmarshal live: %w", err)
	}

	// Takeover set: (serverOrbId|field) — KEEP these in apply even when values
	// disagree (force-claim on the next force-conflicts apply). Used for
	// Accept/Reject resolutions.
	//
	// Ignored set: (serverOrbId|field) — OMIT these from apply unconditionally,
	// even when values match (so auto-claim doesn't silently evict the local
	// manager). Used for Ignore resolutions. takeoverSet takes precedence if
	// somehow both lists name the same field (shouldn't happen — orbital writes
	// one resolution row per field, mutually exclusive actions).
	takeoverSet := map[string]bool{}
	for _, t := range intent.Takeover {
		takeoverSet[takeoverKey(t.ServerOrbID, t.Field)] = true
	}
	ignoredSet := map[string]bool{}
	for _, ig := range intent.Ignored {
		k := takeoverKey(ig.ServerOrbID, ig.Field)
		if !takeoverSet[k] {
			ignoredSet[k] = true
		}
	}

	for _, entry := range managedFields {
		// Match any "local:<id>" field manager — per-person identities, not just "local:admin".
		if !strings.HasPrefix(entry.Manager, "local:") || entry.FieldsV1 == nil {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal(entry.FieldsV1.Raw, &fields); err != nil {
			continue
		}
		// FieldsV1 root holds {"f:spec": {...}}; descend into the spec subtree.
		specOwned, _ := fields["f:spec"].(map[string]any)
		if specOwned != nil {
			deleteOwnedPaths(intentMap, liveMap, specOwned, "", takeoverSet, ignoredSet)
		}
	}

	filtered, err := json.Marshal(intentMap)
	if err != nil {
		return nil, fmt.Errorf("marshal filtered spec: %w", err)
	}
	var result armadav1.ConfigBundleSpec
	if err := json.Unmarshal(filtered, &result); err != nil {
		return nil, fmt.Errorf("unmarshal filtered spec: %w", err)
	}
	return &result, nil
}

// takeoverKey builds the per-(server, field) key used in the takeover set.
// serverOrbID is empty for top-level (non-per-server) takeover entries.
func takeoverKey(serverOrbID, field string) string {
	return serverOrbID + "|" + field
}

// deleteOwnedPaths walks an SSA FieldsV1 subtree and deletes leaves from
// intentMap based on the four-way rule:
//   - in ignoredSet → omit unconditionally (Ignore: never claim, regardless of value)
//   - in takeoverSet → keep (Accept/Reject: force-claim regardless of value)
//   - values match → keep (auto-claim silently evicts local:*)
//   - values differ → omit (bow out, preserve override)
//
// scope tracks the server-orbId context as we descend into spec.servers[]:
// empty at top-level; set to the orbId when we enter a k:{"orbId":"X"} subtree.
// Used to build the takeoverSet/ignoredSet key consistent with how spec.takeover[]
// and spec.ignored[] are keyed (serverOrbId+field).
//
// FieldsV1 encoding rules:
//   - "f:fieldName": {}     → admin owns the entire field
//   - "f:fieldName": {...}  → admin owns leaves within; recurse
//   - "k:{...}" keys appear only under an "f:listField" parent (handled below)
func deleteOwnedPaths(intentMap, liveMap, owned map[string]any, scope string, takeoverSet, ignoredSet map[string]bool) {
	for ownedKey, ownedVal := range owned {
		if !strings.HasPrefix(ownedKey, "f:") {
			continue
		}
		field := strings.TrimPrefix(ownedKey, "f:")
		ownedSubtree, _ := ownedVal.(map[string]any)
		if len(ownedSubtree) == 0 {
			// Leaf claim. Four-way decision.
			key := takeoverKey(scope, field)
			if ignoredSet[key] {
				delete(intentMap, field)
				continue
			}
			if takeoverSet[key] {
				continue
			}
			liveVal := liveMap[field]
			intentVal := intentMap[field]
			if !leafValuesEqual(intentVal, liveVal) {
				delete(intentMap, field)
			}
			continue
		}
		// Non-leaf claim: descend.
		switch iv := intentMap[field].(type) {
		case map[string]any:
			lv, _ := liveMap[field].(map[string]any)
			deleteOwnedPaths(iv, nullSafeMap(lv), ownedSubtree, scope, takeoverSet, ignoredSet)
		case []any:
			lvList, _ := liveMap[field].([]any)
			intentMap[field] = filterOwnedFromList(iv, lvList, ownedSubtree, takeoverSet, ignoredSet)
		}
	}
}

// collectLocalClaimedKeys walks managedFields and returns the set of
// "<serverOrbId>|<field>" keys for every leaf field owned by at least one
// local:* manager. Top-level (non-per-server) claims use "" as the
// serverOrbId, matching takeoverKey() — so the result is directly usable
// against an IgnoredEntry/TakeoverEntry tuple of (ServerOrbID, Field).
//
// Structural FieldsV1 markers (entry-presence ".", listMapKey "f:orbId")
// produce harmless keys that won't false-match any IgnoredEntry — Field on
// IgnoredEntry is a real spec leaf name (e.g. "racadmEnabled"), never "."
// or "orbId" as a stand-alone Ignore target.
func collectLocalClaimedKeys(fields []metav1.ManagedFieldsEntry) map[string]bool {
	out := map[string]bool{}
	for _, e := range fields {
		if !strings.HasPrefix(e.Manager, "local:") || e.FieldsV1 == nil {
			continue
		}
		var tree map[string]any
		if err := json.Unmarshal(e.FieldsV1.Raw, &tree); err != nil {
			continue
		}
		specTree, _ := tree["f:spec"].(map[string]any)
		if specTree == nil {
			continue
		}
		walkLocalClaims(specTree, "", out)
	}
	return out
}

// walkLocalClaims descends a FieldsV1 subtree (rooted at f:spec or below) and
// emits "<scope>|<field>" keys for every leaf claim. scope is the current
// server orbId (empty at top level), updated when descending into a
// k:{"orbId":"X"} list-map entry.
func walkLocalClaims(node map[string]any, scope string, out map[string]bool) {
	for k, v := range node {
		sub, _ := v.(map[string]any)
		switch {
		case strings.HasPrefix(k, "f:"):
			field := strings.TrimPrefix(k, "f:")
			if len(sub) == 0 {
				out[scope+"|"+field] = true
				continue
			}
			walkLocalClaims(sub, scope, out)
		case strings.HasPrefix(k, "k:"):
			raw := strings.TrimPrefix(k, "k:")
			var key map[string]string
			if err := json.Unmarshal([]byte(raw), &key); err != nil {
				continue
			}
			id := key["orbId"]
			if len(sub) > 0 {
				walkLocalClaims(sub, id, out)
			}
		}
	}
}

// filterActiveIgnored drops IgnoredEntry rows whose (ServerOrbID, Field) has
// no corresponding local:* claim. Under ADR-009, Ignore is meaningless without
// an active override.
func filterActiveIgnored(entries []armadav1.IgnoredEntry, claimed map[string]bool) []armadav1.IgnoredEntry {
	if len(entries) == 0 {
		return entries
	}
	kept := make([]armadav1.IgnoredEntry, 0, len(entries))
	for _, ig := range entries {
		if claimed[ig.ServerOrbID+"|"+ig.Field] {
			kept = append(kept, ig)
		}
	}
	return kept
}

// nullSafeMap returns a non-nil map for safe key access during descent.
func nullSafeMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// leafValuesEqual compares two decoded JSON leaf values (bools, strings, numbers,
// nil) for equality. Used to decide bow-out vs force-claim on local:*-owned leaves.
func leafValuesEqual(a, b any) bool {
	// reflect.DeepEqual handles nil, bool, string, json.Number, and matching types.
	// JSON numbers decode as float64 — same on both sides since both came through
	// json.Unmarshal of the same go struct shape.
	if a == nil && b == nil {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// filterOwnedFromList applies bow-out deletions inside a list field whose
// SSA strategy is listType=map. owned holds "k:{...}" entries identifying the
// list elements admin claims. For each claimed entry we descend with the
// scope set to the orbId so deleteOwnedPaths can consult the takeover/ignored
// sets.
//
// Elements where admin owns the entire entry (empty subtree) are dropped only
// when none of their fields are in takeoverSet — otherwise they're kept so
// the takeover pass can force-claim them.
func filterOwnedFromList(target []any, live []any, owned map[string]any, takeoverSet, ignoredSet map[string]bool) []any {
	// Build orbId → live entry index for quick lookup during descent.
	liveByOrbID := map[string]map[string]any{}
	for _, item := range live {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if id, _ := entry["orbId"].(string); id != "" {
			liveByOrbID[id] = entry
		}
	}

	out := make([]any, 0, len(target))
	for _, item := range target {
		entry, ok := item.(map[string]any)
		if !ok {
			out = append(out, item)
			continue
		}
		// Scope for the takeover key is this entry's orbId (listMapKey).
		orbID, _ := entry["orbId"].(string)
		liveEntry := liveByOrbID[orbID]

		drop := false
		for ownedKey, ownedVal := range owned {
			if !strings.HasPrefix(ownedKey, "k:") {
				continue
			}
			var keyMap map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(ownedKey, "k:")), &keyMap); err != nil {
				continue
			}
			if !entryMatchesListKey(entry, keyMap) {
				continue
			}
			ownedSubtree, _ := ownedVal.(map[string]any)
			if len(ownedSubtree) == 0 {
				// admin owns the entire entry. With no per-leaf takeover/value
				// data to consult here, fall back to the pre-simplification
				// behavior: drop. Rare path — granular `f:fieldname` claims
				// per the SSA encoding are the common case.
				drop = true
				break
			}
			deleteOwnedPaths(entry, nullSafeMap(liveEntry), ownedSubtree, orbID, takeoverSet, ignoredSet)
			// Restore listMapKey fields. Admin co-owns the key (SSA always claims
			// it on the apply), but the entry must still carry its key to remain
			// a valid associative-list element.
			for k, v := range keyMap {
				entry[k] = v
			}
			break
		}
		if !drop {
			out = append(out, entry)
		}
	}
	return out
}

func entryMatchesListKey(entry map[string]any, keyMap map[string]any) bool {
	for k, v := range keyMap {
		if entry[k] != v {
			return false
		}
	}
	return true
}

// setCondition upserts a metav1.Condition and returns the previous Status
// (empty string if the condition did not exist). Callers can compare the
// returned value to detect transitions for logging.
//
// LastTransitionTime is updated only when Status actually changes, per the
// metav1.Condition contract.
func setCondition(conditions *[]metav1.Condition, condType string, status metav1.ConditionStatus, reason, message string) metav1.ConditionStatus {
	now := metav1.Now()
	for i, c := range *conditions {
		if c.Type == condType {
			prev := c.Status
			(*conditions)[i].Reason = reason
			(*conditions)[i].Message = message
			if prev != status {
				(*conditions)[i].Status = status
				(*conditions)[i].LastTransitionTime = now
			}
			return prev
		}
	}
	*conditions = append(*conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})
	return ""
}
