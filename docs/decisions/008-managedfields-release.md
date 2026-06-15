# ADR-008: Release stale ownership claims via SSA-as-manager

**Date:** 2026-06-15
**Status:** accepted

---

## Context

The takeover pipeline (ADR-006) applies controller intent to fields the cloud admin marked for force-resolution. The mechanism is K8s Server-Side Apply with `client.ForceOwnership`. For the **reject** case (cloud intent value differs from local override), force-conflicts works: a value conflict triggers ownership transfer, `local:admin`'s claim on the field is removed, the field becomes solely owned by `configbundle-controller`.

The **accept** case is different. Orbital's accept-divergence semantic means the cloud admin has decided to adopt the local override into intent. After the next export, the manifest's intent value for that field matches the local override. When `processTakeover` does the force-conflicts Apply, K8s SSA sees no value conflict (both managers want the same value), and **force-conflicts is a no-op for ownership transfer.** The result is **shared ownership** — both `local:admin` and `configbundle-controller` appear in the field's managedFields entries.

Shared ownership is functionally benign — the divergence-reporter short-circuits on `reflect.DeepEqual(intended, override)` (`divergence_reporter.go:145`) so the loop converges. But it violates orbital's divergence-semantic contract:

> Cloud admin is the superuser. Accept = orbital adopts override into intent. Reject = orbital intent wins. Ignore = orbital disengages.
>
> **Only `ignore` retains local ownership. Accept and Reject MUST release the local claim.**

Without an explicit release pass, an accept-resolved field looks identical in managedFields to an actively-divergent field — `local:admin` still listed as a co-owner. This breaks local-admin tooling that introspects ownership (`kubectl get cb ... --show-managed-fields | grep 'local:'` is the documented way to audit pending overrides).

## Decision

After a successful takeover Apply, run a second step (`releaseOtherClaims`) that uses the **K8s-documented "transferring ownership between managers" protocol** ([upstream docs](https://kubernetes.io/docs/reference/using-api/server-side-apply/#transferring-ownership-between-managers)) to release stale claims. The protocol has three steps:

1. Manager A (`local:admin`) owns field F.
2. Manager B (`configbundle-controller`) Applies F with the **same value** → both managers share ownership of F.
3. Manager A re-Applies a body that **omits F** → SSA's release-on-omit rule strips A's claim on F. A's other claims (fields still in the new body) persist. B becomes sole owner.

Steps 1–2 happen earlier in `processTakeover` (the force-conflicts Apply). This step runs step 3 for every non-self manager that currently claims any takeover target. cb-controller submits the Apply with `client.FieldOwner(<other manager>)` — K8s does not authenticate the FieldManager string, and this delegation pattern is the recommended mechanism when a controller mediates ownership transfer.

The Apply body is reconstructed: for each non-self manager that claims at least one takeover-target field, build a `ConfigBundle` body containing all of that manager's currently-claimed fields with current live values, EXCEPT the takeover targets. Submit as `client.Apply` with `FieldOwner=<that manager's name>`. The release-on-omit rule then strips just the takeover-target claims.

## Rationale

**Why not just rely on shared ownership.**

Functionally fine for orbital's loop, but breaks the project's stated ownership semantic. Orbital UI and `kubectl` introspection treat managedFields as the source of truth for "who claims this field." A `local:admin` entry that survives a cloud-admin accept is a stale claim with no semantic owner — confusing for operators, breaks `kubectl get cb -o yaml --show-managed-fields | grep 'local:'` workflows.

**Why SSA-as-manager (the documented protocol) rather than JSON Patch on managedFields.**

A JSON Patch with `op: remove` against `/metadata/managedFields/N/fieldsV1/...` also achieves the strip and is shorter to implement. We considered it and rejected it: the K8s docs explicitly warn against editing managedFields directly, and the documented mechanism for the exact use case (transferring ownership between managers) is precisely what we need. SSA-as-manager works through the K8s API server's intended protocol — same audit trail K8s expects, same code path other controllers use, no special cases for managedFields handling.

The tradeoff is implementation complexity: the Apply body must be reconstructed from each manager's current claims (live spec values at the paths they own), then submitted as that manager. The body must:

- Include the listMapKey (`orbId`) for each server entry the manager claims (so SSA matches the existing list element).
- Include all the manager's *other* claimed fields with their current live values (so release-on-omit doesn't strip claims the manager legitimately retains — pending divergences, Ignore-resolved fields).
- OMIT the takeover-target fields (so release-on-omit strips just those).
- NOT include any field the manager doesn't own (so this Apply doesn't extend the manager's claims). In particular, do **not** include `serviceTag` even though it's CRD-Required: K8s validates Required-fields against the merged final state, not the individual Apply body. cb-controller's own claim on `serviceTag` keeps it present in the object.

The Apply must use an `unstructured.Unstructured` body rather than a typed `armadav1.ConfigBundle` — the typed struct would serialize `spec.datacenter` as an empty string (no `json:omitempty`), which would extend the manager's claims to a field cb-controller already owns and fail with a conflict.

**Why this preserves orbital's three-action semantic.**

| Action | Cloud intent | Local claim after takeover |
|---|---|---|
| **Accept** | Adopts override value | Released (this fix) |
| **Reject** | Keeps intent value, overrides edge | Released (force-conflicts in step 1, value differs) |
| **Ignore** | Disengages from field | Preserved — field is omitted from the bundle entirely, never appears in `spec.takeover[]`, never reaches this release pass |

Pending (un-resolved) divergences are also preserved: a field with no resolution row in orbital is not in `spec.takeover[]`, so the manager's claim stays intact.

**Non-fatal on failure.**

If the release fails (rare: concurrent reconcile, CRD validation regression), `processTakeover` logs the error but returns success. The takeover apply itself succeeded — values are correct. Reporting the release failure as a hard error would mask the successful value transfer. The next bundle re-runs takeover, which re-attempts the release.

## Consequences

**Positive:**

- Orbital's divergence semantic is enforced at the K8s ownership layer: Accept and Reject release local claims; Ignore preserves them.
- Local-admin tooling (`kubectl get cb ... --show-managed-fields`) becomes a reliable audit of pending overrides — surviving `local:*` entries indicate actual unresolved or Ignore-resolved overrides.
- Uses K8s's documented protocol — no direct managedFields manipulation, no special audit caveats.

**Negative:**

- The controller submits Apply requests as other field managers. K8s allows this (FieldManager is a free string), but the audit trail will show actions attributed to `local:admin` that were physically performed by `configbundle-controller`'s ServiceAccount. This is documented K8s protocol; not a security issue because the ServiceAccount's RBAC governs what writes are allowed, not the FieldManager string. Operators reading audit logs should know that `local:*` Applies originating from cb-controller's SA are ownership-release operations.
- Reconstructing the Apply body requires walking the manager's claimed-paths tree and looking up live values. ~150 lines of map traversal code in `reconstructApplyExcluding` and helpers. Unit-tested in `takeover_test.go`.

**Neutral:**

- Adds N extra Apply calls per dispatch (one per non-self manager that claims a takeover target). Latency cost is small relative to the SSA pass already running. Most dispatches will have 0 or 1 such managers.

## Implementation

`internal/controller/takeover.go`:
- `processTakeover` calls `releaseOtherClaims` after the force-conflicts Apply.
- `releaseOtherClaims` re-fetches the CR, builds an exclusion set from `spec.takeover[]`, walks managedFields for each non-self non-status manager, reconstructs their Apply body via `reconstructApplyExcluding`, and submits as `client.Apply` with `FieldOwner=<that manager's name>`.
- The Apply uses `unstructured.Unstructured` to avoid the typed-struct serialization issue with `spec.datacenter`.

Reconstruction helpers (all in `takeover.go`):
- `reconstructApplyExcluding` — top-level spec rebuild.
- `reconstructServerList` — list-map of servers; preserves the orbId listMapKey.
- `reconstructServerEntry` — single server entry, handles top-level fields and recurses into idrac.
- `reconstructIdracExcluding` — leaf-level idrac field rebuild with the exclusion check.

Tests: `TestReconstructApplyExcluding` in `takeover_test.go` covers:
- Surgical release: target claim omitted, other claims preserved with live values.
- Manager claims only the takeover target: idrac key absent in output, orbId preserved (listMapKey requirement).
- Manager doesn't claim any takeover target: `touched=false` (caller skips the Apply).
- Top-level Server field as takeover target.

Verified end-to-end on minikube 2026-06-15:

**Before takeover:**
```
local:admin claims: {".":{}, "f:idrac":{"f:dhcpEnabled":{}, "f:sshEnabled":{}}, "f:orbId":{}}
```

**Dispatch manifest with `sshEnabled` in `spec.takeover[]`** → controller logs:
```
INFO  takeover  takeover queued                       field=sshEnabled
INFO  takeover  released claims via SSA-as-manager    manager=local:admin
INFO  consume   async apply succeeded
```

**After takeover:**
```
local:admin claims: {".":{}, "f:idrac":{"f:dhcpEnabled":{}}, "f:orbId":{}}
```

`sshEnabled` released. `dhcpEnabled` preserved. No new fields added (no `serviceTag` injection). `f:idrac` still present because `dhcpEnabled` still lives under it.

## Related

- ADR-006 — Takeover pipeline ordering. This ADR extends ADR-006 with the release step.
- ADR-005 — Mapping layer. Unaffected.
- Orbital plan `docs/plans/cb-controller-retry-on-conflict.md` — the unrelated RetryOnConflict fix that addresses the 409 on mapping dispatch.
- K8s docs — [Server-Side Apply: Transferring ownership between managers](https://kubernetes.io/docs/reference/using-api/server-side-apply/#transferring-ownership-between-managers)
