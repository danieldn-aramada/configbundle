package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
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

	// applyFn overrides applyManifest in tests.
	applyFn func(ctx context.Context, body []byte, digest, importID string) error
}

// ConsumeServerOption configures a ConsumeServer.
type ConsumeServerOption func(*ConsumeServer)

// WithPort sets the TCP address the consume server listens on (default ":8095").
func WithPort(port string) ConsumeServerOption {
	return func(s *ConsumeServer) { s.port = port }
}

// WithNamespace sets the K8s namespace for ConfigBundle CRs (default "configbundle-system").
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
// routing by Content-Type. Supported types: manifest (sync validate, async apply) and
// mapping (sync validate, store in ConfigMap).
func (s *ConsumeServer) handleDispatch(w http.ResponseWriter, r *http.Request) {
	ct := r.Header.Get("Content-Type")
	switch ct {
	case bundle.MediaTypeManifest:
		tag := r.Header.Get("X-Orb-Tag")
		digest := r.Header.Get("X-Orb-Digest")
		importID := r.Header.Get("X-Orb-Import-ID")
		s.handleManifestBody(w, r, tag, digest, importID)
	case bundle.MediaTypeMapping:
		digest := r.Header.Get("X-Orb-Digest")
		if digest == "" {
			http.Error(w, "X-Orb-Digest header required", http.StatusBadRequest)
			return
		}
		s.handleMappingBody(w, r, digest)
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

// handleMappingBody processes the mapping layer from orb's dispatch pipeline.
// It validates the mapping, finds the ConfigBundle with the matching digest, and
// writes the mapping to a ConfigMap owned by that CR.
func (s *ConsumeServer) handleMappingBody(w http.ResponseWriter, r *http.Request, digest string) {
	logger := log.FromContext(r.Context()).WithName("mapping")

	body, err := io.ReadAll(io.LimitReader(r.Body, maxManifestBytes+1))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if len(body) > maxManifestBytes {
		http.Error(w, "mapping exceeds max size", http.StatusRequestEntityTooLarge)
		return
	}

	if _, err := ParseMapping(body); err != nil {
		http.Error(w, "invalid mapping: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Find the ConfigBundle whose LastAppliedDigest matches the provided digest.
	var cbList armadav1.ConfigBundleList
	if err := s.Client.List(r.Context(), &cbList, client.InNamespace(s.namespace)); err != nil {
		http.Error(w, "list ConfigBundles: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var cb *armadav1.ConfigBundle
	for i := range cbList.Items {
		if cbList.Items[i].Status.LastAppliedDigest == digest {
			cb = &cbList.Items[i]
			break
		}
	}
	if cb == nil {
		http.Error(w, "ConfigBundle not found for digest; manifest may not have been applied yet", http.StatusConflict)
		return
	}

	ownerRef := metav1.OwnerReference{
		APIVersion:         armadav1.GroupVersion.String(),
		Kind:               "ConfigBundle",
		Name:               cb.Name,
		UID:                cb.UID,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}

	if err := writeMappingConfigMap(r.Context(), s.Client, s.namespace, cb.Name, digest, body, ownerRef); err != nil {
		http.Error(w, "write mapping ConfigMap: "+err.Error(), http.StatusInternalServerError)
		return
	}

	logger.Info("stored mapping", "digest", digest, "configbundle", cb.Name)
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

	// Fetch the current CR to read managedFields for omitAdminOwnedServers.
	var cb armadav1.ConfigBundle
	err = s.Client.Get(ctx, types.NamespacedName{
		Name:      spec.Datacenter,
		Namespace: s.namespace,
	}, &cb)
	if client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("get ConfigBundle: %w", err)
	}

	// Flag any spec writer that doesn't follow the local:<id> convention.
	// They're silently dropped by omitAdminOwnedFields below; the warning
	// makes the silent drop visible at the runtime where the bug bites.
	warnNonConformingManagers(log.FromContext(ctx).WithName("consume"), spec.Datacenter, cb.ManagedFields)

	// Omit leaf fields owned by any local:<id> manager to avoid SSA 409 conflicts.
	// Granularity is per-leaf: a single overridden field does not exempt the
	// rest of the server from controller updates. See ADR-007.
	patchSpec, err := omitAdminOwnedFields(spec, cb.ManagedFields)
	if err != nil {
		return fmt.Errorf("compute admin-omitted patch: %w", err)
	}

	apply := &armadav1.ConfigBundle{
		TypeMeta: metav1.TypeMeta{
			APIVersion: armadav1.GroupVersion.String(),
			Kind:       "ConfigBundle",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.Datacenter,
			Namespace: s.namespace,
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
		lastErr = s.Client.Patch(ctx, apply, client.Apply,
			client.FieldOwner("configbundle-controller"),
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

	// Record the last-applied manifest for divergence comparison — both in
	// memory (fast path for the reporter) and durably in the CR's ConfigMap
	// (survives controller restart). Persist failure is non-fatal: the
	// in-memory state is still correct; only post-restart recovery degrades.
	if s.reporter != nil {
		s.reporter.SetLastManifest(spec.Datacenter, spec)
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

// omitAdminOwnedFields returns a typed ConfigBundleSpec with all leaf fields
// owned by local:admin removed. The result is the SSA-safe patch the controller
// can apply with field manager "configbundle-controller" without 409-conflicting
// on admin-owned fields.
//
// Granularity is per-leaf, not per-server entry: if admin owns one iDRAC field
// on a server, the controller still updates that server's hostname/oobIP and the
// other iDRAC fields. Only the specific leaves admin owns are excluded.
//
// Mechanism: round-trip spec → JSON map → delete admin-owned paths → JSON →
// typed spec. Because all leaf fields in IdracSpec/ServerSpec are *bool/*string
// with omitempty (ADR-007), deleted leaves decode as nil and are absent from
// the serialized SSA apply.
func omitAdminOwnedFields(spec armadav1.ConfigBundleSpec, managedFields []metav1.ManagedFieldsEntry) (*armadav1.ConfigBundleSpec, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("marshal spec: %w", err)
	}
	var specMap map[string]any
	if err := json.Unmarshal(raw, &specMap); err != nil {
		return nil, fmt.Errorf("unmarshal spec: %w", err)
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
			deleteOwnedPaths(specMap, specOwned)
		}
	}
	filtered, err := json.Marshal(specMap)
	if err != nil {
		return nil, fmt.Errorf("marshal filtered spec: %w", err)
	}
	var result armadav1.ConfigBundleSpec
	if err := json.Unmarshal(filtered, &result); err != nil {
		return nil, fmt.Errorf("unmarshal filtered spec: %w", err)
	}
	return &result, nil
}

// deleteOwnedPaths walks an SSA FieldsV1 subtree and deletes corresponding
// leaves from target (a decoded spec subtree). FieldsV1 encoding rules:
//   - "f:fieldName": {}     → admin owns the entire field; delete target[fieldName]
//   - "f:fieldName": {...}  → admin owns leaves within; recurse
//   - "k:{...}" keys appear only under an "f:listField" parent (handled below)
func deleteOwnedPaths(target map[string]any, owned map[string]any) {
	for ownedKey, ownedVal := range owned {
		if !strings.HasPrefix(ownedKey, "f:") {
			continue
		}
		field := strings.TrimPrefix(ownedKey, "f:")
		ownedSubtree, _ := ownedVal.(map[string]any)
		if len(ownedSubtree) == 0 {
			delete(target, field)
			continue
		}
		switch tv := target[field].(type) {
		case map[string]any:
			deleteOwnedPaths(tv, ownedSubtree)
		case []any:
			target[field] = filterOwnedFromList(tv, ownedSubtree)
		}
	}
}

// filterOwnedFromList applies admin-owned deletions inside a list field whose
// SSA strategy is listType=map. owned holds "k:{...}" entries identifying the
// list elements admin claims. For entries admin owns entirely (empty subtree),
// the element is dropped from the returned slice; for partial ownership, the
// element is kept with admin-owned leaves deleted.
func filterOwnedFromList(target []any, owned map[string]any) []any {
	out := make([]any, 0, len(target))
	for _, item := range target {
		entry, ok := item.(map[string]any)
		if !ok {
			out = append(out, item)
			continue
		}
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
				drop = true
				break
			}
			deleteOwnedPaths(entry, ownedSubtree)
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
