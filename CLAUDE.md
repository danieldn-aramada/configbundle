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
- **Key libraries:** `k8s.io/client-go`, `sigs.k8s.io/controller-runtime`, `oras-project/oras-go`, `sigstore/cosign`
- **Registry:** ACR (cloud OCI registry), Zot (edge OCI mirror)

## Architecture Notes

- **Edge always pulls; cloud never pushes.** No cloud component initiates a connection to a Galleon. The edge registry polls ACR; the ConfigBundle Controller polls the local Zot registry. No exceptions.
- **Orbital is the sole OCI producer.** The bundler returns bytes to Orbital via the enricher API. Orbital signs once and pushes once. Configbundle never holds OCI write credentials.
- **Enrichment is all-or-nothing.** A non-2xx response from the bundler causes Orbital to mark the publish failed and push nothing. Partial artifacts are never produced.
- **Orbital never imports configbundle.** Dependency flows one way: configbundle calls Orbital's GraphQL API. No reverse imports.
- **CMDB is not in the reconciliation path.** After a ConfigBundle lands on a Galleon, Orbital has no further role. ConfigBundle Controller and X Config Controllers run locally and reconcile from the CRD.
- **No separate edge agent.** The ConfigBundle Controller owns the full OCI pipeline at the edge: poll Zot, cosign verify, write ConfigBundle CR, decompose into child CRs. There is no sidecar binary.
- **Orb owns Dgraph import.** Configbundle never calls orb's import API. Orb is responsible for getting graph data into its own database. The ConfigBundle CR is the handoff artifact — orb reacts to it independently.

## Current State

**Phase:** Prototype
**Active work:** (none — end of session)
**Completed this session (May 26, session 2):** Context doc overhaul — crd-context.md rewritten (stale edge-agent refs removed, correct field manager, SSA conflict resolution table, no-partial-apply finding, +listType=map guidance); edge-context.md divergence tracking section corrected (ConfigBundle CR only, not child CRs); Spike 5 and 7 open items updated in ROADMAP with +listType=map as shared prerequisite and no-partial-apply as open design question. RacadmEnabled omitempty bug fixed in api/v1.
**Next priority:** Add `+listType=map +listMapKey=serviceTag` to `servers[]` in ConfigBundleSpec (prerequisite for Spike 5 and 7), then Spike 5 (Puller design — resolve no-partial-apply question first).

*Update this section at each session wrap-up.*

## Model & Workflow Guide

**Default model: Sonnet.** Use Opus only at specific decision points.

| Sonnet | Opus |
|---|---|
| Implementation, bug fixes, library code, service handlers | Architecture decisions, security design, spike planning |
| Anything with a settled decision in CLAUDE.md | Tasks touching 3+ domains simultaneously |
| Known-spec features | New systems being designed for the first time |

**Switch to Opus (`/effort max`) when:**
- Designing a new subsystem or feature with long-term architectural consequences
- Security-sensitive decisions: signing, key management, auth, permissions
- Task touches 3+ domains simultaneously
- Planning a spike that is "Not started" in ROADMAP.md for the first time
- Reviewing a completed spike against architectural invariants

**Signal:** *"This is a design decision with long-term consequences — consider switching to Opus (`/effort max`) before I implement anything."*

**Switch back to Sonnet (`/effort normal`) when:**
- Design is settled and the task is now implementation of a known plan

## Domain Reference Files

**At the start of every task:**
1. Read `docs/claude/_index.md`
2. Identify every domain file relevant to the task
3. Read those files
4. Signal which files you loaded before doing anything else — e.g. *"Reading api-context.md and bundle-context.md before starting."*

Do not skip this step. Do not proceed without signaling. The developer needs to know what context you are operating from.

See [`docs/claude/_index.md`](docs/claude/_index.md) for the full routing table.

| Working on | Read |
|---|---|
| Bundler HTTP service, enricher API, Orbital GraphQL integration | `docs/claude/api-context.md` |
| OCI artifact structure, layers, media types, signing, tags | `docs/claude/bundle-context.md` |
| CRD types, ConfigBundle CR, kubebuilder annotations, SSA | `docs/claude/crd-context.md` |
| Edge agent, edge registry (Zot), cosign verification, divergence | `docs/claude/edge-context.md` |
| Orbital GraphQL data model, bundler query logic, ConfigBundle manifest YAML, local overrides | `docs/claude/orbital-context.md` |
| Planning or starting any spike | `ROADMAP.md` |

*Domain files are created via `/distill` and updated at each PR through the sync rule. Add new domains as subsystems grow.*

## Working Style

- Don't add comments that just restate what the code does
- Don't refactor code that wasn't part of the request — ask first
- Don't add third-party packages without asking first
- Only touch files relevant to the task
- Don't add TODOs or placeholder comments
- Don't add error handling for scenarios that can't happen
- Before marking a task done: check whether any decisions from this session belong in CLAUDE.md or a domain file
- At PR phase: if the diff introduces a settled decision, update the relevant domain file in the same commit — do not defer
- **Library first:** Use top-level importable packages only (e.g. `api/v1alpha1/`, `bundle/`). Do not add `cmd/` or `internal/` until controllers are explicitly being implemented.
- **Write tests alongside every behavioral change** — when you add a field, persist data, change an API response, or introduce an interface, include tests asserting the new behavior in the same response. Do not wait to be asked.
- **Run tests after writing them** — always run `make test` after writing new tests. If tests fail, diagnose and fix before reporting done. Do not hand back failing tests.
- **Test at the lowest isolatable level** — unit (no services, `testing.T` table-driven) → envtest (K8s API, Ginkgo) → e2e (running cluster). Choose the lowest level where the behavior is fully exercised. Unit tests for pure logic (parsing, filtering); envtest for K8s apply/watch behavior.
- **Any persistence requires a round-trip test** — if data is written to the K8s API or any file: write a test that writes, reads back, and asserts. Persistence bugs are invisible without this.
- **Interfaces at external boundaries** — OCI clients, HTTP clients, and other I/O-bound dependencies must be injected via interfaces so tests can substitute fakes. Never make external calls non-injectable.

## Conversation Conventions

- **`thoughts:` / `discuss:`** — respond conversationally only; do not write code or edit files
- **`propose:`** — write a design proposal for review; do not write code
- **`critique:`** — cold critical read of the specified document; switch to Opus (`/effort max`) before starting; evaluate arguments, consistency, contradictions, and voice; output structured findings with priority ratings; do not edit unless asked
- No prefix — implement

## Settled Decisions

Explicitly decided. Do not re-suggest.

- **Go for all components** — consistency with the Armada platform stack
- **Library-first structure** — no `cmd/` or `internal/` until controllers are explicitly being implemented; top-level importable packages only
- **Orbital is the sole OCI producer** — bundler returns bytes to Orbital via the enricher API; it never pushes to ACR directly
- **Enricher URLs are per-request** — not server-side config; acceptable because the publish API requires Azure AD authn/authz and runs on VPN. Named server-side enrichers are a future option if governance requirements change.
- **Monotonic int OCI tags** — v1, v2, v42; after push all references use digest for immutability
- **cosign for signing** — signature stored as OCI referrer artifact on the bundle digest; Galleons hold only the public key; verification works fully air-gapped without reaching ACR
- **apiVersion: armada.ai/v1** — for all CRD types defined in this repo
- **No `AI.md`** — AI session metadata lives in git commit trailers (`AI-model`, `AI-settled`), not a separate file. See ADR-002.
- **No separate edge agent** — the ConfigBundle Controller owns the full edge OCI pipeline (poll Zot → cosign verify → import to orb → write ConfigBundle CR → decompose to child CRs).
- **Puller calls orb `POST /import/subgraph` before writing the ConfigBundle CR** — the Puller extracts `data.json.gz` and `schema.gz` graph layers from the OCI artifact and POSTs them to orb. Waits for 2xx. If orb fails, the cycle aborts — no CR is written. This guarantees config delivery state and Dgraph state are always derived from the same artifact version.
- **Orb does not poll Zot** — orb's sole import interface is `POST /import/subgraph`. The ConfigBundle Controller is the only OCI consumer on the edge. Do not reintroduce independent Zot polling in orb.
- **`ORB_ENDPOINT` env var** — configures orb's base URL (default `http://localhost:8001`). Required for the Puller.
- **Local overrides are at ConfigBundle CR level only** — child CRs (ServerConfig, etc.) are derived state and are never an override surface. `local:<admin-id>` field managers are only valid on the ConfigBundle CR. Do not implement or support local overrides on child CRs.
- **SSA has no partial apply** — if a manifest includes even one field owned by another manager (without `--force-conflicts`), the entire apply fails (409). No fields are updated, including ones the applier legitimately owns. Verified experimentally. This is the key design constraint for the Puller (Spike 5).
- **`+listType=map +listMapKey=serviceTag` is required on `servers[]` before Spike 5 or 7** — without it, SSA treats `servers[]` as atomic. A single admin override on any server field locks the whole array, and combined with no-partial-apply, blocks all server config changes on the next Puller cycle. Add the markers to `ConfigBundleSpec.Servers` and run `make generate && make manifests`.
- **Local override field manager is `local:admin` for MVP** — single fixed string; do not make it dynamic or enumerate multiple managers. Post-MVP RBAC work will address per-person `local:<admin-id>` managers and K8s RBAC enforcement. The Puller identifies local overrides by checking `manager == "local:admin"` in managedFields.
- **Puller stays in the ConfigBundle Controller binary for MVP** — the Puller (`ctrl.Runnable`) and Decomposition Reconciler share a binary. They must share no in-process state — the ConfigBundle CR is their only interface. This makes splitting into separate deployments a future ops decision, not a code rewrite.
- **Puller does not force-override local fields** — the Puller inspects `managedFields`, omits fields owned by `local:admin`, and applies without `ForceOwnership`. The force-upstream action (SDD cloud admin action 2) is deferred post-MVP; it requires the Divergence Reporter to exist first so cloud admins have visibility before forcing.

*Add new settled decisions here whenever a significant architectural choice is made. Include the decision in the same PR as the code that reflects it.*
