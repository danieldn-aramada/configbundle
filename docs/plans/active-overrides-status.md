# Plan: status.activeOverrides[]

Surface active local:* overrides on the ConfigBundle CR status so operators can answer "what is currently overridden on this Galleon?" with a single `kubectl describe configbundle`.

## Problem

The divergence reporter computes the override set on every reconcile and POSTs it to orb, but never writes it back to the CR. Operators have to parse raw `managedFields` to see what's locally overridden.

## New type

```go
// ActiveOverrideEntry records a single field currently owned by a local:*
// manager whose live value differs from the last-applied bundle intent.
type ActiveOverrideEntry struct {
    OrbID         string       `json:"orbId"`
    Field         string       `json:"field"`
    Type          string       `json:"type"`
    IntendedValue string       `json:"intendedValue,omitempty"`  // JSON-encoded
    OverrideValue string       `json:"overrideValue,omitempty"`  // JSON-encoded
    Manager       string       `json:"manager"`
    Since         *metav1.Time `json:"since,omitempty"`
}
```

**Why strings for values:** CRD schemas can't express `interface{}` cleanly — kubebuilder emits `x-kubernetes-preserve-unknown-fields: true` which makes `kubectl describe` print `{}`. JSON-encoding at write time is explicit, schema-valid, and directly readable.

**Why `+listType=atomic`:** The reporter always replaces the full set (same as the POST payload). No SSA merge granularity needed; only one writer.

## Placement

Top-level on `ConfigBundleStatus`, not nested under `divergenceReporting`.

```go
// +optional
// +listType=atomic
ActiveOverrides []ActiveOverrideEntry `json:"activeOverrides,omitempty"`
```

`divergenceReporting` is internal dedup plumbing. `activeOverrides` is operator-facing and needs its own section in `kubectl describe`.

## Write location

Extend `writeReportingStatus` to accept `[]OverrideEntry` and write `ActiveOverrides` in the same `Status().Update` call.

| Path | What gets written |
|---|---|
| After successful `postToOrb` | Full current override set |
| Steady-state-quiet (empty confirmed) | Empty `[]ActiveOverrideEntry{}` |
| Exact-hash dedup fast-return | Nothing — existing value already correct |

Updated signature:
```go
func (r *DivergenceReporter) writeReportingStatus(
    ctx context.Context,
    name, hash string,
    count int,
    overrides []OverrideEntry,
) error
```

Add converter `overrideEntriesToStatus([]OverrideEntry) []ActiveOverrideEntry` in `divergence_reporter.go`.

## Consistency

Bounded staleness under the debounce window (default 5s) — same lag orb UI already sees. Not strictly transactional with `managedFields`. Cold start: nil until first POST.

## When reporter is disabled

Clear `activeOverrides` to nil via a one-shot `Runnable` in `SetupWithManager`. A stale non-nil list with a disabled reporter is misleading. Do NOT clear `DivergenceReporting` — dedup state remains valid if re-enabled.

## Files to change

- `api/v1/configbundle_types.go` — add `ActiveOverrideEntry`; add `ActiveOverrides` to `ConfigBundleStatus`
- `internal/controller/divergence_reporter_controller.go` — extend `writeReportingStatus`; add disabled-Runnable
- `internal/controller/divergence_reporter.go` — add `overrideEntriesToStatus` converter
- `docs/reference/EDGE.md` — add `status.activeOverrides[]` to the divergence tracking section

## Open question

Should `spec.ignored[]` entries (active `local:*` claim, cb-controller bows out intentionally) appear in `activeOverrides`? `extractOverrides` currently includes them since it walks all `local:*` managed fields. Decide before implementing: same list, or separate `activeIgnored[]` slice.
