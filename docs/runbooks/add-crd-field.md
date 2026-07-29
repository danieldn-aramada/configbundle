# Add a new field to a CRD

You want to add a new field to `ConfigBundle`, `ServerConfig`, or
`BackupConfig` (or one of their nested types like `IdracSettingsSpec`,
`ClusterBackupSpec`, etc.). Here's the full round-trip.

## Naming rule (settled 2026-07-06)

New Go types in `api/v1/` mirror the orbital schema exactly. `Xxx` in
orbital → `XxxSpec` in Go. JSON edge names match orbital verbatim.

If you're adding a field that already exists in orbital's schema
(`~/armada/orbital/schema/schema.graphql`), use that field's name. If
you're adding something orbital doesn't have yet, coordinate with the
orbital team first — the schemas evolve together.

## Steps

### 1. Add the field to the Go type

Example — add `snapshotEncryption` (bool) to `EtcdBackupSpec`:

```go
// api/v1/backupconfig_types.go
type EtcdBackupSpec struct {
    OrbID              string  `json:"orbId"`
    Enabled            *bool   `json:"enabled,omitempty"`
    Schedule           *string `json:"schedule,omitempty"`
    Location           *string `json:"location,omitempty"`
    SnapshotEncryption *bool   `json:"snapshotEncryption,omitempty"`  // NEW
}
```

Pointer type + `omitempty` for admin-overridable fields — see ADR-007
("SSA pointer fields").

### 2. Regenerate deepcopy + manifests

```bash
make generate manifests
```

This updates `api/v1/zz_generated.deepcopy.go` and
`config/crd/bases/*.yaml`. Both must be committed.

### 3. If the field is orbital-driven, wire the bundler

Update `internal/bundler/orbital.go`:

- Add the field to the corresponding `*Result` struct
- Add the field to the GraphQL query (`configBundleQuery`)

Then update `internal/bundler/handler.go`'s mapper (e.g.
`mapClusterBackup`) to project the field from the result into the spec.

### 4. If the field is actuated at the edge, wire the controller

Depending on which CRD you touched:

- `ServerConfig.spec.idracSettings.*` → `internal/serverconfig/`
- `BackupConfig.spec.etcd.*` / `.velero.*` → `internal/backupconfig/`
- `ConfigBundle.spec.*` → `internal/controller/`

Common shape: extend the `Reconcile` function to read the new field and
apply it to the downstream resource (Redfish, Velero Schedule, CronJob).

### 5. Add a test

**Every behavioral change needs a test.** Table-driven unit test when
possible; envtest with Ginkgo for behavior that needs the K8s API.

Example locations:
- `api/v1/*_test.go` — validation, deepcopy round-trips
- `internal/backupconfig/backupconfig_controller_test.go` — reconcile
  behavior on the fake client
- `internal/controller/configbundle_controller_test.go` — decomposition,
  divergence

### 6. Update the sample manifests

- `config/samples/v1_configbundle.yaml` — top-level sample
- `config/samples/v1_serverconfig.yaml` or `v1_backupconfig.yaml` —
  domain-specific sample

Add the new field with a realistic value so the sample serves as
documentation.

### 7. Update the topic doc

If the field is a **new convention** or a **settled decision**, add a
bullet to the relevant `docs/reference/<DOMAIN>.md` `## Settled Decisions`
section:

- CRD types, kubebuilder markers, SSA → `CRD.md`
- Bundler GraphQL, enricher API → `API.md`
- Divergence, edge behavior → `EDGE.md`
- Orbital GraphQL data model, override semantics → `ORBITAL.md`

If it's a plain new field with the same semantic as its siblings, docs
don't need an update — the field name is self-documenting.

### 8. Run the full suite

```bash
make test
```

All packages should be green. Coverage may drop marginally if you added
code without tests — add them.

### 9. Test on minikube end-to-end

```bash
make up
NAMESPACE=default make run-controller &
# (in another terminal, if bundler/velero/etc. side is exercised)
kubectl apply -f config/samples/v1_configbundle.yaml
kubectl get configbundles,serverconfigs.armada.ai,backupconfigs -o yaml
```

Verify the field lands where you expect it — on the ConfigBundle CR, on
the decomposed child CR, and on the downstream resource (Velero Schedule
/ CronJob / iDRAC config).

### 10. Commit

The commit should include:
- `api/v1/*_types.go` change
- `api/v1/zz_generated.deepcopy.go` (regenerated)
- `config/crd/bases/*.yaml` (regenerated)
- `config/samples/*.yaml` (updated)
- `internal/*/*.go` (reconciler + tests)
- Any `docs/reference/<DOMAIN>.md` update

All in the same commit. Reviewer checks docs + code are in lockstep.

## Common mistakes

- **Forgetting to regenerate.** `make generate manifests` — always. If
  `zz_generated.deepcopy.go` or the CRD YAML are out of sync with types,
  the apply will fail at runtime with cryptic messages.
- **Non-pointer type on an admin-overridable field.** SSA doesn't
  distinguish "value unset" from "value = zero" when the Go type is
  bare. Use pointer types (`*bool`, `*string`) with `omitempty` for
  any field an operator might override with `local:admin`.
- **JSON tag doesn't match orbital.** If orbital says `idracSettings`,
  your JSON tag must say `idracSettings` — not `idrac`, not
  `idracsettings`. See the naming-alignment feedback in CLAUDE.md.
- **Bundler forgot to add the GraphQL query.** Types match, orbital has
  the field, but the bundler doesn't ask for it → the field lands empty
  in the CR spec. Look at bundler test coverage; write a mapper test.
