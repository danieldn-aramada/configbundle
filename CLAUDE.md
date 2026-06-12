# CLAUDE.md

## Project Overview

**configbundle** — A Go library and set of services that packages Orbital's datacenter export as a signed OCI artifact and delivers it to Galleon edge clusters.

**Problem:** Galleon edge clusters need to receive and apply a consistent, verifiable snapshot of their intended configuration from the cloud CMDB (Orbital). Orbital produces the source of truth but has no delivery mechanism that works across disconnected or air-gapped edges.

**Non-goals:**
- configbundle does not implement or replicate any CMDB or graph logic — it calls Orbital's export API
- configbundle does not push OCI artifacts — Orbital is the sole OCI producer; configbundle returns bytes to Orbital via the enricher API
- configbundle does not push configuration to Galleons — the edge always pulls

## Stack

- **Language:** Go 1.25.5 (module: `github.com/armada/configbundle`; go.mod pinned to 1.25.5 — homebrew install is 1.25.5, do not bump without upgrading homebrew first)
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
- **Orb is the single artifact ingress at the edge.** Orb pulls from Zot, cosign-verifies, imports graph layers to DGraph, then dispatches each remaining layer to registered consumers by media type. CB Controller is a consumer — it receives its manifest layer via `POST /consume` and applies it. CB Controller never holds OCI credentials.
- **Orb owns Dgraph import.** Configbundle never calls orb's import API. Orb is responsible for getting graph data into its own database. The ConfigBundle CR is the handoff artifact — orb reacts to it independently.

## Current State

**Phase:** Prototype
**Active work:** Divergence pipeline feature-complete; pending integration tests and e2e validation
**Next priority:** Integration tests (envtest for reporter + takeover); bundler e2e validation against live Orbital (Spike 3 acceptance); Spike 8 (full pipeline e2e)

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

### Domain files

Read the relevant file before starting work in that area. Each file contains settled decisions, patterns, and gotchas. **When a decision is made, document it in the domain file — not in CLAUDE.md.**

| Working on | File |
|---|---|
| Bundler HTTP service, enricher API, Orbital GraphQL integration | `docs/claude/api-context.md` |
| OCI artifact structure, layers, media types, signing, tags | `docs/claude/bundle-context.md` |
| CRD types, ConfigBundle CR, kubebuilder annotations, SSA | `docs/claude/crd-context.md` |
| Edge agent, Zot registry, cosign verification, divergence | `docs/claude/edge-context.md` |
| Orbital GraphQL data model, bundler query logic, ConfigBundle manifest YAML, local overrides | `docs/claude/orbital-context.md` |
| Planning or starting any spike | `ROADMAP.md` |

See [`docs/claude/_index.md`](docs/claude/_index.md) for the full routing table.

### Decision records

Architecture decisions with full rationale. Read when the context would otherwise be invisible from the code.

| When working on | Read |
|---|---|
| CRD domain routing (which domain file to read) | `docs/decisions/001-domain-routing.md` |
| CRD design for server domain | `docs/decisions/002-crd-design-server.md` |
| Divergence `when` field semantics | `docs/decisions/004-divergence-when-semantics.md` |
| Divergence mapping layer (D2 decision) | `docs/decisions/005-divergence-mapping-layer.md` |
| Takeover pipeline ordering in consume handler | `docs/decisions/006-divergence-takeover-pipeline.md` |
| OCI bundler pipeline, ConfigBundle integration | `~/armada/orbital/docs/configbundle-integration.md` |
| Divergence contract (cb-controller obligations) | `docs/plans/divergence-cb-controller-contract.md` |

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

`api/v1/` — CRD types (ConfigBundle, ServerConfig). `bundle/` — OCI media type constants. `cmd/` — entry points (`main.go` controller, `bundler/main.go`). `internal/controller/` — controller logic (ConsumeServer, Reconciler, DivergenceReporter, takeover). `internal/bundler/` — bundler HTTP service. `config/` — kubebuilder manifests. `docs/claude/` — domain reference files. `docs/decisions/` — ADRs. `docs/plans/` — implementation plans. `test/` — e2e tests.

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
- **Local dev defaults must point to local services** — `ORBITAL_GRAPHQL_URL`, `BUNDLER_PORT`, etc. all default to local values in config structs. Production credentials must never appear as code defaults.
- **Single `Dockerfile`, two targets** — `--target controller` and `--target bundler` produce two images from one Dockerfile with a shared builder stage

## Development Status

Early-stage prototype. The Go module is initialized at `github.com/armada/configbundle`.
