# Roadmap

## Development Timeline

```mermaid
%%{init: {'theme': 'base', 'themeVariables': {'doneTaskBkgColor': '#22c55e', 'doneTaskBorderColor': '#16a34a', 'activeTaskBkgColor': '#3b82f6', 'activeTaskBorderColor': '#2563eb', 'taskBkgColor': '#e5e7eb', 'taskBorderColor': '#d1d5db', 'taskTextColor': '#6b7280', 'taskTextDarkColor': '#fff'}}}%%
gantt
    dateFormat YYYY-MM-DD
    axisFormat %b %Y

    section Completed
    Architecture Design (SDD v0.3, CMDB Arch Proposal)    :done, 2026-04-16, 2026-05-24
    Orbital Integration Contract                           :done, 2026-05-08, 2026-05-24

    section Current
    Prototyping                                            :active, 2026-05-24, 2026-07-14

    section Upcoming
    MVP                                                    :2026-07-14, 2026-08-28
```

**Note:** All future dates are subject to change.

---

## Spikes

Each spike is a question to answer. Design spikes are settled decisions that shaped the architecture before implementation started. Implementation spikes are the prototype work items.

| # | Spike | Key Question | Owner | Status | Open items |
|---|---|---|---|---|---|
| D1 | Architecture design | What is the right pattern for cloud-authored, edge-enforced configuration management at the Galleon? | Daniel | ✅ Done (May 24) | CCP-authored, edge-enforced; K8s controller pattern; CMDB not in reconciliation path; 4 invariants from SDD Key Decision 5 |
| D2 | Orbital integration contract | How does configbundle plug into Orbital's OCI publish pipeline as an enricher? | Daniel | ✅ Done (May 24) | `POST /enrich` enricher API; Orbital is sole OCI producer; all-or-nothing enrichment; retry + timeout behavior defined; Orbital enricher endpoint live |
| D3 | Edge agent vs controller | Should the edge agent be a separate sidecar, or can the ConfigBundle Controller absorb the OCI pipeline? | Daniel | ✅ Done (May 24) | No separate agent; controller owns full pipeline (poll → verify → write CR → decompose); orb owns Dgraph import; ConfigBundle CR is the sole handoff artifact |
| 1 | Go module scaffold | Can we get `kubebuilder init` + CI running as a clean starting point? | — | ✅ Done (May 26) | kubebuilder init with `--domain armada.ai --plugins go/v4`; `go.mod` at `github.com/armada/configbundle`; Makefile; CRD generation pipeline. CI (lint + test on PR) not yet wired. |
| 2 | Bundle package | What are the right OCI layer types and media type constants for the importable library? | — | Not started | Top-level `bundle/` package; no cmd/ or internal/ dependencies |
| 3 | Bundler service (`POST /enrich`) | Does the enricher work end-to-end with Orbital's publish pipeline? | — | Not started | Orbital enricher endpoint ready to test against; GraphQL query shape; ConfigBundle manifest YAML structure |
| 4 | ConfigBundle CRD | What is the right schema for the ConfigBundle CR, its status, and the domain child CR types? | — | ✅ Done (May 26) — iDRAC domain | `api/v1/`: ConfigBundle, ServerConfig, IdracSpec (all 8 fields from Orbital IdracSettings). CR naming: hostname (settled). Printer columns, status conditions, phase enum. `clusters` domain (EksaConfig) not started. |
| 5 | Controller — OCI pipeline | Can the controller poll Zot, cosign-verify, import to orb, and write the ConfigBundle CR reliably? | — | ✅ Done (May 28) — HTTPOCIClient stub (oras-go + cosign pending); HTTPOrbClient fully implemented | Puller as `ctrl.Runnable`; **settled design:** (1) poll Zot by tag, (2) skip if digest unchanged, (3) cosign verify, (4) extract config layer + graph layers (data.json.gz, schema.gz), (5) call orb `POST /import/subgraph` — wait for 2xx, abort cycle on failure, (6) apply ConfigBundle CR spec via SSA WITHOUT ForceOwnership — inspect managedFields first, omit fields owned by `local:admin`, (7) write status: ArtifactFetched + SignatureVerified + GraphImported conditions, lastAppliedDigest, lastAppliedAt. New env var: `ORB_ENDPOINT`. **Prerequisites complete:** `+listType=map +listMapKey=serviceTag` added and tested (May 27); SSA no-partial-apply design question resolved (inspect managedFields, omit contested). |
| 6 | Controller — decomposition | Can the controller decompose a ConfigBundle CR into domain child CRs via SSA, respecting local overrides? | — | ✅ Done (May 26) — iDRAC domain | SSA with ForceOwnership on child CRs — **correct as implemented** (local overrides are at ConfigBundle CR level only; child CRs are derived state); ownerReferences + cascade delete verified; hostname-based CR naming; envtest suite (4 cases) + e2e suite (3 cases) passing |
| 7 | Divergence reporting | How does the controller detect and publish field-level divergence from cloud intent? | — | Not started | Divergence Reporter as `ctrl.Runnable`; inspect `managedFields` on **ConfigBundle CR only** (not child CRs); divergence = fields owned by `local:<admin-id>`; compare against OCI artifact for field-level diff; report: field path, owner, since when; publish to DIVERGENCE_REPORT_DEST. **Prerequisite:** `+listType=map +listMapKey=serviceTag` on `servers[]` (shared with Spike 5 — must land before either spike is implemented). Without it, `managedFields` shows ownership of the entire `servers[]` array, not individual server entries or their fields. The Divergence Reporter cannot distinguish "admin overrode sshEnabled on server X" from "admin owns all server config." |

---

## What We've Built

| Item | Completed | Summary |
|---|---|---|
| Architecture Design | Apr 16 – May 24 | SDD v0.3: 5 key design decisions (air-gapped first, graph CMDB, GraphQL API, K8s controller pattern, local override via SSA field managers). CMDB Architectural Proposal (Sedar): eliminated CMDB-driven reconciler in favor of K8s controller pattern; resolved edge agent question (no separate binary). |
| Orbital Integration Contract | May 8 – May 24 | `POST /enrich` enricher API fully specified: request/response schema, retry + timeout behavior, OCI layer structure, base64 encoding, local end-to-end test flow. Orbital's enricher endpoint implemented and live. `configbundle-integration.md` is the source of truth. |
| Project scaffold (Spike 1) | May 26 | kubebuilder v4 init; `go.mod`; Makefile with generate/manifests/install/run targets; CRD generation pipeline via controller-gen. `cmd/main.go` wired for ConfigBundle controller. |
| ConfigBundle + ServerConfig CRDs (Spike 4) | May 26 | `api/v1/`: ConfigBundle (datacenter, servers[], status with phase/conditions/lastAppliedDigest), ServerConfig (serviceTag, hostname, oobIP, idrac), IdracSpec (all 8 desired-state fields from Orbital IdracSettings). CR name = `strings.ToLower(hostname)`. kubebuilder annotations, printer columns, generated CRD YAML. |
| ConfigBundle Controller — decomposition (Spike 6) | May 26 | Reconciler decomposes ConfigBundle into ServerConfig child CRs via SSA (field manager: `configbundle-controller`). ownerReferences set (cascade delete verified on minikube). Desired state enforcement: out-of-band mutations on child CRs are restored. omitempty removed from bool fields so `false` is enforced. Envtest suite (4 cases) + e2e suite (3 cases, `make test-e2e-local`). |
| ConfigBundle Controller — OCI pipeline (Spike 5) | May 28 | Puller as `ctrl.Runnable`: digest-skip, managedFields inspection, orb import before CR write, SSA apply without ForceOwnership. `bundle/` package with OCI media type constants. HTTPOrbClient (stdlib zip + HTTP). HTTPOCIClient stub (oras-go + cosign pending). `GraphImported` condition + condition constants. `ORB_ENDPOINT` env var. Envtest suite (6 cases) + unit tests (parseManifest, adminOwnedServiceTags, omitAdminOwnedServers, setCondition). |

*Full design decisions and architectural rationale: [SDD DCIM & CMDB for Galleon Digital Twin in Atlas.pdf](SDD%20DCIM%20%26%20CMBD%20for%20Galleon%20Digital%20Twin%20in%20Atlas%20%283%29.pdf) · [CMDB_Architectural_Proposal.docx](CMDB_Architectural_Proposal.docx) · [configbundle-integration.md](configbundle-integration.md)*

---

## MVP Definition

> Working draft — scope confirmed once prototype spikes complete.

### Cloud
- ✅ Architecture design — CCP-authored, edge-enforced pattern settled
- ✅ Integration contract — `POST /enrich` API defined and Orbital side live
- ⬜ Bundler service — `POST /enrich` implementation (Spike 3)
- ✅ `api/v1` package — ConfigBundle and child CR type definitions, importable by Orbital (Spike 4)
- ⬜ `bundle/` package — OCI media type constants, importable library (Spike 2)

### Edge (Galleon Mgmt Cluster)
- ⬜ ConfigBundle Controller — OCI pipeline: poll Zot, cosign verify, write ConfigBundle CR (Spike 5)
- ✅ ConfigBundle Controller — decompose ConfigBundle CR into domain child CRs via SSA (Spike 6)
- ⬜ Divergence reporting — field-level divergence detection and signed report publish (Spike 7)

### Explicitly out of scope for v1
- X Config Controllers (ServerConfig, NetworkConfig, etc.) — domain-specific, owned by teams consuming the ConfigBundle CR
- Named server-side enrichers — per-request URLs are sufficient; governance requirement not yet present
- Orbital bearer token enforcement — depends on Orbital Spike 11 (authorization)

### Prerequisites for domain controllers (post-MVP, required before ServerConfig controller ships)
- **iDRAC credentials model** — the ServerConfig controller needs iDRAC credentials to issue Redfish calls. For now: manually provisioned K8s Secret in the controller's namespace. Secret naming convention, rotation strategy, and whether to use per-server or datacenter-wide secrets must be designed before the controller is implemented.

### Post-MVP backlog
- **Local override RBAC** — MVP assumes a single local admin with field manager `local:admin` (fixed string). Post-MVP: define how multiple admins are represented (per-person `local:<admin-id>` managers), how the Puller enumerates them from managedFields, and what RBAC controls who can act as a local field manager on the ConfigBundle CR.

---

## External Integration Dependencies

| System | Role | Status |
|---|---|---|
| **Orbital** | Calls `POST /enrich` during publish; provides GraphQL data model for bundler query; sole OCI producer | Integration contract defined; enricher endpoint live |
| **Zot** | Edge OCI registry — ConfigBundle Controller polls this; never ACR directly | Deployment model TBD (per Galleon) |
| **cosign** | Signs artifacts (Orbital side); verifies on Galleon (controller side, air-gapped) | Public key distribution to Galleons: TBD |
| **orb** | Reacts to ConfigBundle CR being written to etcd; owns Dgraph import independently | `/import` endpoint design: orb's concern |
| **ACR** | Cloud OCI registry; Zot polls ACR on connectivity | Azure-native; existing Armada infrastructure |
