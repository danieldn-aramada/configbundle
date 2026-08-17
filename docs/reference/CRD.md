# CRD Reference

> **When to load this file:** Read this before working on CRD type definitions, ConfigBundle CR structure, kubebuilder annotations, or SSA field managers.

---

## Overview

configbundle defines `ConfigBundle` and `ServerConfig` CRDs in `api/v1/`. The ConfigBundle CR is the handoff artifact between the ConfigBundle Controller (which writes the CR after pulling and verifying a signed OCI artifact) and domain controllers (ServerConfig controller, etc.) that actuate child CRs on the Galleon Mgmt Cluster.

---

## Settled Decisions

- **apiVersion: `armada.ai/v1`** — all CRD types in this repo use this group. Do not use `configbundle.armada.ai` or any other variant.
- **SSA everywhere** — the ConfigBundle Controller applies all resources via Server-Side Apply. No direct creates or full-object updates.
- **Field managers are fixed** — `configbundle-controller` for all SSA writes by the controller. Local admin overrides use `local:<admin-id>` on the ConfigBundle CR only, never on child CRs.
- **ownerReferences on child CRs** — deleting a `ConfigBundle` CR cascades to domain child CRs (ServerConfig, BackupConfig).
- **Unknown fields are ignored** — the controller must not fail on manifest fields it doesn't recognize. Forward-compatible.
- **Server identity model** — `serviceTag` in spec, `strings.ToLower(hostname)` as the K8s resource name. `hostname` and `oobIP` are mandatory; bundler skips servers missing either. `serviceTag` is repeated in spec because the resource name isn't reliably reconstructable from it.
- **All 8 orbital `IdracSettings` fields are desired-state spec fields** — `firmwareVersion` means "we intend this version," not a snapshot of current hardware.
- **Overridable leaves are pointers with `omitempty`** — `IdracSpec` booleans are `*bool`, string leaves are `*string,omitempty`. A nil leaf means "no manager has set this field." Callers must dereference safely. `ServerSpec.ServiceTag` stays plain string — it's the listMapKey and always required. Do NOT drop `omitempty` here: nilifying admin-owned fields before the apply is what makes partial SSA work.
- **Bool fields on the CRD do NOT use `omitempty` unless they are pointers** — omitempty on a plain bool omits `false` (Go zero value) from the SSA patch, making the controller unable to enforce `false` as desired state.
- **ConfigBundle and ServerConfig are cluster-scoped; per-bundle ConfigMaps and Secrets stay namespaced.** cb-controller writes children to `CHILD_NAMESPACE` (default `configbundle-system`). Invariant: one ConfigBundle per datacenter per cluster. Not enforced by admission validation yet.
- **Every level that orbital identifies as a ConfigItem carries its own `OrbID` field on the CRD type** (e.g. `IdracSpec.OrbID`). Bundler queries orbital and populates; controller reads directly. New nested types (BIOS, NIC, ...) follow the same pattern.
- **No mapping OCI layer, no path→orbId translation on the wire.** Do NOT reintroduce a separate mapping layer — an earlier design shipped one as a second OCI layer and produced a persistent 409 race (mapping dispatch arrived before `Status.LastAppliedDigest` was written). Saturating orbIds on the CR closes the race by deletion.
- **ServerConfig cluster identity uses two keys: label + annotation.** `serverconfig.armada.ai/cluster-name=<KubernetesCluster.name>` for kubectl filtering and sc-controller watch scoping; `serverconfig.armada.ai/cluster-orb-id=<full-orbId>` annotation for stable cross-boundary identity. Do NOT use only the annotation — it is not filterable via label selectors. Do NOT try to put the raw orbId in the label — colons are invalid in K8s label values.

---

## ConfigBundle CR structure

```yaml
apiVersion: armada.ai/v1
kind: ConfigBundle
metadata:
  name: colo-galleon
  namespace: configbundle-system
spec:
  datacenter: colo
  servers:
    - serviceTag: 3RK3V64
      hostname: colo-r740-01
      oobIP: 10.10.1.45
      idracSettings:
        firmwareVersion: "7.20.10.05"
        sshEnabled: false
        ipmiEnabled: false
        lockdownModeEnabled: false
        osToIdracPassThroughEnabled: false
        usbManagementPortEnabled: true
        dhcpEnabled: false
        racadmEnabled: true
status:
  phase: Applied            # Pending | Applying | Applied | Failed
  observedGeneration: 3     # metadata.generation the controller has reconciled
  conditions:
    - type: Reconciled      # decomposition to child CRs complete
  lastAppliedDigest: sha256:abc123...
  lastOrbImportID: "abc123-def456-..."   # X-Orb-Import-ID for orb correlation
  lastAppliedAt: "2026-05-26T12:00:00Z"

  # Written only by the divergence-reporter. See EDGE.md for the
  # dedup + steady-state-quiet semantics that read from these fields.
  divergenceReporting:
    lastPostedAt: "2026-05-26T12:00:30Z"
    lastPostedHash: "sha256hex..."       # exact-match dedup key
    lastPostedOverrideCount: 0           # pointer int: nil=never posted, *0=posted empty, *N=posted N
```

## ServerConfig child CR structure

Created by the Decomposition Reconciler via SSA WITH ForceOwnership. Named `strings.ToLower(server.Hostname)`.

```yaml
apiVersion: armada.ai/v1
kind: ServerConfig
metadata:
  name: colo-r740-01
  namespace: configbundle-system
  ownerReferences:
    - apiVersion: armada.ai/v1
      kind: ConfigBundle
      name: colo-galleon
      controller: true
      blockOwnerDeletion: true
spec:
  serviceTag: 3RK3V64
  hostname: colo-r740-01
  oobIP: 10.10.1.45
  idracSettings:
    firmwareVersion: "7.20.10.05"
    sshEnabled: false
    ipmiEnabled: false
    lockdownModeEnabled: false
    osToIdracPassThroughEnabled: false
    usbManagementPortEnabled: true
    dhcpEnabled: false
    racadmEnabled: true
```

---

## Conventions

- Package path: `api/v1/` — top-level, importable by Orbital and other consumers
- Generated code lives alongside the types: `zz_generated.deepcopy.go`
- Run `make generate && make manifests` after any type change — never hand-edit generated files
- CRD YAML output goes to `config/crd/bases/` (kubebuilder default)

---

## Kubebuilder marker conventions

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cb
// +kubebuilder:printcolumn:name="Datacenter",type=string,JSONPath=`.spec.datacenter`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
```

For lists that need per-entry field ownership in SSA (required for divergence tracking):
```go
// +listType=map
// +listMapKey=serviceTag
Servers []ServerSpec `json:"servers,omitempty"`
```

**Why `+listType=map` matters:** Without it, SSA treats `servers[]` as atomic — the entire array is owned by a single manager. If a local admin overrides one field in one server entry, they take ownership of the entire `servers[]` array. The Puller's next apply would then conflict on the whole array, not just that one field. With `+listType=map +listMapKey=serviceTag`, each server entry is independently trackable by `serviceTag` and managers can own fields within individual entries separately. This annotation is a prerequisite for Spike 7 (Divergence Reporter).

Reference: https://kubernetes.io/docs/reference/using-api/server-side-apply/#custom-resources-and-server-side-apply

---

## SSA field manager model

| Actor | Field manager | ForceOwnership | Target |
|---|---|---|---|
| Puller (ctrl.Runnable) | `configbundle-controller` | **No** | ConfigBundle CR spec |
| Decomposition Reconciler | `configbundle-controller` | **Yes** | Child CRs (ServerConfig, etc.) |
| Edge admin | `local:<admin-id>` | — | ConfigBundle CR spec only |

Puller applies WITHOUT ForceOwnership so SSA preserves locally-owned fields on the ConfigBundle CR. Decomposition Reconciler applies WITH ForceOwnership because child CRs are derived state — they always faithfully reflect the ConfigBundle CR.

---

## SSA conflict resolution (empirically verified)

Source: Daniel's minikube experiments, April 2026.

When a manager applies a manifest containing a field owned by another manager, the API returns 409 (FieldManagerConflict). Three resolution options:

| Resolution | What the upstream does | Result |
|---|---|---|
| **#1 Force** | Re-apply with `--force-conflicts` | Upstream wins. Other manager's claim stripped from managedFields. |
| **#2 Give up management** | Re-apply omitting the conflicting field | Apply succeeds. Upstream loses ownership of that field. Value stays as the other manager set it. |
| **#3 Become shared manager** | Re-apply including the field with the **same value** the other manager set | Apply succeeds. Both managers co-own the field. Neither can change it unilaterally — any attempt to change the shared field by either owner returns a new conflict. |

**Critical: no partial apply.** If upstream sends a manifest with even one conflicting field, the **entire apply fails**. Fields that upstream legitimately owns are NOT updated. Verified: upstream applied `biosProfile=performance, powerLimit=500w` where local-user owned `powerLimit`. Apply failed 409. `biosProfile` was NOT updated to "performance" — it stayed "standard". Nothing was applied.

**Releasing ownership:** Apply an empty manifest that omits the field (or its parent object entirely). An empty `data: {}` body does NOT release ownership of nested fields — the parent must be omitted entirely (apply with no `data` key). Verified on minikube.

**Shared manager deadlock:** Once co-owned (Resolution #3), neither owner can change the value without the other first releasing ownership. Use with caution.

These behaviors map to the cloud admin resolution actions in ORBITAL.md:
- Force → Resolution #1
- Accept (incorporate local value upstream) → Resolution #3, then local-user releases ownership
- Ignore → Resolution #2 (upstream omits the field from future bundles)

---

## Gotchas

- **Never hand-edit `zz_generated.deepcopy.go`** — overwritten by `make generate`.
- **Status subresource** — status updates go through the status subresource endpoint. Use a separate SSA patch on the status subresource after updating spec.
- **Bool fields and omitempty** — removing omitempty is intentional on desired-state bools. Do not add it back. See Key decisions above.
- **`+listType=map` prerequisite** — `servers[]` does not yet have this annotation. Do not implement the Divergence Reporter until it is added and `make generate && make manifests` has been run and verified.
- **No partial apply — Puller uses Resolution #2 (omit contested entries)** — The Puller inspects `managedFields` before applying. Any server entry with any field owned by `local:admin` is omitted from the SSA patch entirely. With `+listType=map`, omitting a full entry (not just the contested field) is safe: the admin's intent for that server is preserved, and the Puller still updates uncontested server entries without conflict. `ForceOwnership` is not used on the ConfigBundle CR.

---

## External references

- [kubebuilder book](https://book.kubebuilder.io/)
- [SSA field ownership model](./EDGE.md)

---

## Domain file maintenance

Update this file when:
- The ConfigBundle spec or status schema changes
- A new CRD type is added
- An SSA field manager is added or renamed
- A kubebuilder marker convention is established

Updates must be in the same PR as the code change that prompted them.
