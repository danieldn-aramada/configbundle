# CLAUDE.md

## Project Overview

**configbundle** — A Go library and set of services that packages Orbital's datacenter export as a signed OCI artifact and delivers it to Galleon edge clusters.

**Problem:** Galleon edge clusters need to receive and apply a consistent, verifiable snapshot of their intended configuration from the cloud CMDB (Orbital). Orbital produces the source of truth but has no delivery mechanism that works across disconnected or air-gapped edges.

**Non-goals:**
- configbundle does not implement or replicate any CMDB or graph logic — it calls Orbital's export API
- configbundle does not push OCI artifacts — Orbital is the sole OCI producer; configbundle returns bytes to Orbital via the enricher API
- configbundle does not push configuration to Galleons — the edge always pulls

## Stack

- **Language:** Go 1.26.4 (module: `github.com/armada/configbundle`; go.mod pinned to 1.26.4 to match orbital and sibling controllers — homebrew must be at ≥1.26.4)
- **Framework:** kubebuilder / controller-runtime (CRD definitions, controllers)
- **Deployment:** AKS (cloud: bundler service); Galleon Mgmt Cluster (ConfigBundle Controller)
- **Key libraries:** `k8s.io/client-go`, `sigs.k8s.io/controller-runtime`
- **Registry:** ACR (cloud OCI registry), Zot (edge OCI mirror)

## Architecture Notes

- **Edge always pulls; cloud never pushes.** No cloud component initiates a connection to a Galleon. The edge registry polls ACR; orb is the single OCI consumer at the edge. No exceptions.
- **Orbital is the sole OCI producer.** The bundler returns bytes to Orbital via the enricher API. Orbital signs once and pushes once. Configbundle never holds OCI write credentials.
- **Enrichment is all-or-nothing.** A non-2xx response from the bundler causes Orbital to mark the publish failed and push nothing. Partial artifacts are never produced.
- **Orbital never imports configbundle.** Dependency flows one way: configbundle calls Orbital's GraphQL API. No reverse imports.
- **CMDB is not in the reconciliation path.** After a ConfigBundle lands on a Galleon, Orbital has no further role. ConfigBundle Controller and X Config Controllers run locally and reconcile from the CRD.
- **Orb is the single artifact ingress at the edge.** Orb pulls from Zot, cosign-verifies, imports graph layers to DGraph, then dispatches each remaining layer to registered consumers by media type. CB Controller is a consumer — it receives the manifest layer via `POST /dispatch` (content-routed by `Content-Type`). The manifest is the sole layer dispatched to CB Controller; there is no separate mapping layer. CB Controller never holds OCI credentials.
- **Orb owns Dgraph import.** Configbundle never calls orb's import API. Orb is responsible for getting graph data into its own database. The ConfigBundle CR is the handoff artifact — orb reacts to it independently.

## Current State

**Phase:** Prototype
**Active work:** Monorepo consolidation complete. sc-controller and bc-controller sources are now folded into this repo under `cmd/{serverconfig,backupconfig}/` and `internal/{serverconfig,backupconfig}/`; the `replace ../configbundle` directives are gone. All four services (cb-controller, cb-bundler, sc-controller, bc-controller) ship from one Go module and one Dockerfile with four targets. Deploy is one `kubectl apply -k config/default/` — CRDs + 3 Deployments + per-controller RBAC. This follows the cert-manager / cluster-api pattern for related controllers with shared types.
**Next priority:** Spike 8 (full pipeline e2e); PrometheusRule + Alertmanager wiring for drift gauges; orbital-side cleanup of orphan Ignore resolutions after edge handback.

*Update this section at each session wrap-up.*

## Model & Workflow Guide

**Default model: Sonnet.** Use Opus only at specific decision points. Opus sessions should be short and design-focused — then hand back to Sonnet to implement.

| Sonnet | Opus |
|---|---|
| Implementation, bug fixes, library code, service handlers | Architecture decisions, security design, spike planning |
| Anything with a settled decision | Tasks touching 3+ domains simultaneously |
| Known-spec features | New systems being designed for the first time |

### When to suggest switching to Opus

Proactively suggest before proceeding if: (1) design work with no settled decision, (2) task touches 3+ domains, (3) security-sensitive design, (4) planning a new spike for the first time, (5) user says `discuss:` or `thoughts:` with significant design implications.

**Signal:** *"This is a design decision with long-term consequences — consider switching to Opus (`/effort max`) before I implement anything."*

### Spike lifecycle checkpoints

1. **Before starting a new spike** → `/plan` or Opus design session; read ROADMAP.md spike definition
2. **After implementing a complex spike** → consider Opus review against architectural invariants before marking done
3. **Before wrapping up** → check if any decisions belong in the relevant domain file

### Session hygiene

Start a new session after each natural milestone (feature done, spike complete, bug fixed). Don't try to span a full spike in one session — compaction loses precision.

## Reference Index

Read the relevant topic doc before starting work in that area. Each doc's `## Settled Decisions` section carries the current rules. **When a decision is made, add a bullet to the relevant topic doc — not to CLAUDE.md. Do NOT create separate ADR files.**

| Working on | Read |
|---|---|
| Building/changing a domain controller — spec↔status shape, observe→status+metrics, conditions, watch-vs-poll | `docs/reference/DOMAIN-CONTROLLER.md` |
| Full catalog of published Prometheus metrics (names, types, labels) for sc/bc controllers | `docs/reference/METRICS.md` |
| Bundler HTTP service, enricher API, Orbital GraphQL integration | `docs/reference/API.md` |
| OCI artifact structure, layers, media types, signing, tags | `docs/reference/BUNDLE.md` |
| CRD types, ConfigBundle CR, kubebuilder annotations, SSA | `docs/reference/CRD.md` |
| bc-controller, etcd/Velero backups, backup status + metrics, observe model | `docs/reference/BACKUP.md` |
| Edge dispatch, cosign verification, divergence, takeover, reclaim | `docs/reference/EDGE.md` |
| Orbital GraphQL data model, bundler query logic, local overrides | `docs/reference/ORBITAL.md` |
| OCI bundler pipeline, ConfigBundle integration | `~/armada/orbital/docs/configbundle-integration.md` |
| Planning or starting any spike | `ROADMAP.md` |

## Local Development

```bash
make up                # start minikube + install CRDs
make run-controller    # terminal 1 — controller on :8095
make run-bundler       # terminal 2 — bundler on :8020
make down              # stop minikube
```

### Running tests

```bash
make test              # unit + envtest (requires envtest binaries: make setup-envtest)
make test-e2e-local    # e2e against running controller (requires make install + make run-controller)
```

## Repository Structure

`api/v1/` — CRD types (ConfigBundle, ServerConfig, BackupConfig). `bundle/` — OCI media type constants. `cmd/` — entry points (controller, bundler, serverconfig, backupconfig). `internal/controller/` — cb-controller logic (ConsumeServer, Reconciler, DivergenceReporter, takeover, reclaim). `internal/bundler/` — bundler HTTP service. `internal/serverconfig/` + `internal/backupconfig/` — sibling controllers. `config/` — kubebuilder manifests. `docs/reference/` — topic docs with inline Settled Decisions. `docs/playbooks/` — task recipes. `docs/runbooks/` — operational troubleshooting. `test/` — e2e tests.

## Working Style

- Don't add comments that just restate what the code does
- Don't refactor code that wasn't part of the request — ask first
- Don't add third-party packages without asking first
- Only touch files relevant to the task
- Don't add TODOs or placeholder comments
- Don't add error handling for scenarios that can't happen
- Before marking a task done: check whether any decisions belong in the relevant domain file (see Reference Index above). Domain-specific decisions go in domain files — only cross-cutting decisions go in CLAUDE.md.
- At PR phase: if the diff introduces a settled decision, update the relevant domain file in the same commit — do not defer
- **Write tests alongside every behavioral change** — when you add a field, persist data, change an API response, or introduce an interface, include tests asserting the new behavior in the same response. Do not wait to be asked.
- **Run tests after writing them** — always run `make test` after writing new tests. If tests fail, diagnose and fix before reporting done. Do not hand back failing tests.
- **Test at the lowest isolatable level** — unit (no services, `testing.T` table-driven) → envtest (K8s API, Ginkgo) → e2e (running cluster). Choose the lowest level where the behavior is fully exercised. Unit tests for pure logic (parsing, filtering); envtest for K8s apply/watch behavior.
- **Any persistence requires a round-trip test** — if data is written to the K8s API or any file: write a test that writes, reads back, and asserts. Persistence bugs are invisible without this.
- **Interfaces at external boundaries** — OCI clients, HTTP clients, and other I/O-bound dependencies must be injected via interfaces so tests can substitute fakes. Never make external calls non-injectable.

### Conversation conventions

- **`thoughts:` / `discuss:`** — respond conversationally only; do not write code or edit files
- **`propose:`** — write a design proposal for review; do not write code
- **`critique:`** — cold critical read of the specified document; switch to Opus before starting; evaluate arguments, consistency, contradictions, and voice; output structured findings with priority ratings; do not edit unless asked
- **`challenge:`** — stress-test a design thesis; be adversarial
- **`validate:`** — confirm reasoning against docs and knowledge base; flag exceptions
- No prefix — implement
- Run `/wrap-up` at the end of a session to update Current State, save memories, and get a commit message.

## Go Conventions

- **Error wrapping** — use `fmt.Errorf("...: %w", err)`; never discard or log-and-return
- **Context** — always the first argument: `func Foo(ctx context.Context, ...)`
- **Constructors** — named `New[Type]`, e.g. `NewConsumeServer`, `NewDivergenceReporter`
- **`cmd/` is thin** — entry points only; all logic lives in `internal/`
- **Tests** — table-driven with `t.Run`; avoid test helpers that obscure failure sites
- No `init()` functions
- No global variables
- No `panic()` outside of `main()`

## Settled Decisions

These are cross-cutting platform decisions. **Domain-specific decisions live in the domain files listed above — that is where new decisions belong when you document them.**

- **Go for all components** — consistency with the Armada platform stack
- **Orbital is the sole OCI producer** — bundler returns bytes to Orbital via the enricher API; it never pushes to ACR directly
- **apiVersion: armada.ai/v1** — for all CRD types defined in this repo
- **No separate edge agent** — CB Controller is a passive consumer; orb dispatches to it
- **Orb is the single artifact ingress at the edge** — CB Controller never pulls from ACR and never needs OCI registry credentials
- **Local overrides are at ConfigBundle CR level only** — child CRs are derived state; never an override surface
- **Local override field manager is `local:admin` for MVP** — single fixed string; post-MVP will address per-person managers
- **CB Controller validates synchronously, applies asynchronously** — bad payloads return 4xx; valid payloads return 200; K8s apply runs in background
- **CB Bundler deploys as a sidecar container in the Orbital pod for prototype/MVP** — separate container image, shared pod network, enricher URL `http://localhost:8020/bundle`
- **ConfigBundle is a separate project, built after orbital** — orbital's APIs are the contract; do not add ConfigBundle awareness to orbital
- **Local dev defaults must point to local services** — `ORBITAL_BASE_URL`, `BUNDLER_PORT`, etc. all default to local values in config structs. Production credentials must never appear as code defaults.
- **Single base URL for orbital — `ORBITAL_BASE_URL`** — cb-bundler derives `/graphql` and `/api/v1/...` from this one root. Must include orbital's base path (e.g. AKS: `http://localhost:8001/orbital`, local: `http://localhost:8001`). Do NOT reintroduce separate `ORBITAL_GRAPHQL_URL`/`ORBITAL_API_URL` env vars — that two-URL design caused base-path drift across local/AKS that took two rounds of debugging to surface. See `cmd/bundler/main.go` and `internal/bundler/orbital.go` for the helpers (`graphqlURL()`, `divergencesURL()`).
- **Single `Dockerfile`, two targets** — `--target controller` and `--target bundler` produce two images from one Dockerfile with a shared builder stage
- **`api/v1/` types mirror the orbital GraphQL schema** — Go type = `<OrbitalType>Spec`; JSON edges verbatim orbital (`idracSettings`, `kubernetesClusters`, `backup`, `velero`, `etcd`, `s3Sync`). No ad-hoc names, no `*TypeName` string constants.
- **Prom namespace is `configbundle_*`, never `armada_*`** — taxonomy `configbundle_<subsystem>_<subject>_<measure>`. `armada_*` claimed a brand namespace no other Armada service shares.
- **One live read per reconcile → fan out to Prom AND `.Status` independently** — metrics must NOT derive from status writes (RetryOnConflict race would silence gauges). `.Status.observed` is live-read too, never spec-copy. Downstream operational metrics come from their owners (`velero_*`, `kube_*` via KSM) — controllers publish only pipeline-domain metrics.
- **Always SSA-apply in reconcile loops; do NOT gate the apply on a pre-diff.** SSA is idempotent — the convention (cert-manager, cluster-api, kubebuilder samples) is always-apply. Pre-diff-then-maybe-apply silently skips metadata reconciliation (OwnerReferences, labels, finalizers) whenever spec matches; the delta computation is only load-bearing for spec fields. Use the delta output for the `status.recentPatches` summary, never to gate the apply. Pinned by `TestReconcile_BackfillsOwnerReferenceOnPreExistingSubResources` in backupconfig.
- **Sub-resources created by our controllers MUST carry `SetControllerReference` to their parent CR.** Cluster-scoped parent → namespaced child is a valid K8s pattern (cert-manager's `ClusterIssuer` → `Certificate`). Without OwnerReferences, deleting the parent leaves orphan Velero Schedules and CronJobs firing against blob storage. Rename-during-parent-lifetime still needs manual cleanup — OwnerReferences only cover the parent-delete case.

## Development Status

Early-stage prototype. The Go module is initialized at `github.com/armada/configbundle`.
