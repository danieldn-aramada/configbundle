# Backup Reference

> **When to load this file:** Read this before working on bc-controller
> (BackupConfig), etcd/Velero backup actuation, or backup status/metrics.

---

## Overview

bc-controller is the **domain controller for all backup types in a cluster**.
cb-controller decomposes a ConfigBundle into a `BackupConfig` child CR per
cluster; bc-controller reconciles it. Two mechanisms exist today — **etcd
snapshots** and **Velero backups** — with S3Sync reserved for later.

The guiding model mirrors serverconfig/iDRAC: a domain controller **observes
the actual state of the resource it manages and reports it** in status and
metrics. For backup, the resource is *the backups themselves* — not the
machinery that produces them.

---

## The three layers

Keep these distinct; conflating them is the classic backup-observability trap.

| Layer | Question | Owner |
|---|---|---|
| **Intent** | what backup policy does orbital want? | ConfigBundle spec |
| **Config** | is the producer configured to match intent? | bc-controller |
| **Outcome** | do fresh, real backups actually exist? | see "ownership depth" below |

---

## Core principle: observe the backups, not the plumbing

serverconfig reports the device's *actual config* (read via Redfish), not "is
the Redfish client wired up." The backup analog: report the **actual backup
inventory** (does a recent snapshot exist? when? how many?), **not** "is the
CronJob configured."

- The observed state bc reports is the **artifacts** (snapshots/backups), read
  live from the authoritative source.
- The producer — etcd `CronJob`, Velero `Schedule` — is an **actuator**, the
  exact analog of "Redfish PATCH." You do not report the state of your
  actuator; you report the state of the thing it acts on.
- "Are backups real" is therefore **not a downstream gap to patch** — it is the
  headline observed state, front and center, like device state for serverconfig.

---

## Ownership depth: own the observation, or defer it

**Criterion:** does the runtime actor have its own control loop + status API?

- **No independent owner → bc owns observation.** Nobody else is watching, so
  bc must be the one to read the store and report.
- **An independent subsystem already observes → bc defers.** Read that
  subsystem through its API; never reimplement or go around it.

This generalizes: the moment a backup type is backed by a real subsystem, it
flips to the "defer" side. The test is not "did we build it" but "is there an
owner already observing outcomes."

---

## Per-mechanism handling

### etcd — full ownership

The etcd snapshot is a CronJob→Job→pod (`etcdctl | az blob upload`) that bc
fully defined. It is a *dumb actuator* with no independent control loop. If bc
doesn't observe the blob store, nobody does.

- **Configure:** maintain the etcd `CronJob` (producer) to match `spec.etcd`.
- **Observe:** list the cluster's blob prefix; report `lastSnapshotTime`,
  `snapshotCount`, `latestSnapshotBytes`.
- **Metrics:** bc **emits** them — it is the sole source
  (`configbundle_backupconfig_status_etcd_*`). Not a duplication of anything.
- **Credentials:** blob **read-only**. (Mirrors serverconfig holding iDRAC
  creds to observe the device — the domain controller holds creds to see its
  resource. Read-scoped is far tamer than serverconfig's device creds.)
- **Depth limit (settled): observe only, for now.**
  - **Retention (prune old snapshots): later.** bc is the natural owner
    (it already lists the inventory), but it needs blob **write/delete** creds —
    a conscious escalation, not a freebie. Deferred until decided.
  - **Integrity verification (download + `etcdctl snapshot status`): deferred,
    and do NOT put it in the reconcile loop.** Freshness says "a file exists,"
    not "it's restorable" — real value — but verification is heavy I/O. If done,
    it belongs at the **job's write-time** (verify right after upload, stamp the
    result), not as bc re-downloading every poll.

### Velero — meta-controller / passthrough

Velero has its own control loop, `Backup` CRDs, status model, and `velero_*`
metrics. It is *the* authority on Velero backups. bc wraps it.

- **Own:** intent→config fidelity. Velero cannot verify orbital's intent — it
  only knows its own `Schedule`. "Does the Velero `Schedule` faithfully reflect
  the ConfigBundle policy (schedule, paused, location, TTL)?" is bc's job,
  always. This is the meta-controller's real value.
- **Defer:** backup outcomes. Source observed state from Velero **`Backup` CRs**
  (native K8s, watchable, no creds), never from Velero's object storage. Defer
  metrics entirely to `velero_*` — do not re-emit. A thin summary (newest
  `Backup` completion time) MAY be reflected into `.status.observed.velero` for
  CR-level uniformity, but that is a convenience read of Velero's own truth.
- **Credentials:** none for object storage. Pure K8s RBAC (read `Backup` CRs).

---

## Observe mechanism (poll vs watch)

bc splits observation by what's watchable — do NOT put everything on one fast
poll "to match serverconfig" (serverconfig's fast poll is a *necessity* because
Redfish is unwatchable; most of bc's targets are K8s objects and don't need it):

- **Producers (etcd `CronJob`, Velero `Schedule`) — WATCH.** bc `Owns()` them
  (ownerReferences set at apply), so an edit or deletion fires a reconcile
  **instantly** — config drift on the producer self-heals without waiting for a
  poll. Watched with a spec-only (`GenerationChangedPredicate`) filter so the
  CronJob's status churn (`status.lastScheduleTime` bumps every run — Layer-3
  runtime) does not spam reconciles. The Velero `Schedule` watch is best-effort:
  skipped if the Velero CRD isn't installed (etcd-only clusters).
- **etcd artifacts (snapshots in blob) — POLL.** The blob store is not
  watchable, so it's read on the periodic re-observe interval
  (`BACKUP_OBSERVE_INTERVAL`). Set it slow (hourly-ish) — backups change
  hourly/daily, not second-scale. This interval is therefore the *blob poll
  cadence*, not a config-drift cadence (watches handle drift instantly).
- **Velero backups (outcomes) — read `Backup` CRs** (K8s-native), defer metrics
  to `velero_*`.

There is deliberately **no separate blob-poll knob**: the single interval is the
blob cadence, and watches make it safe for that interval to be slow. The blob is
re-read on every reconcile (periodic ticks and watch-triggered ones alike); if
Azure call volume ever matters, add a min-interval guard rather than a fast poll.

---

## Status shape

Two conditions, because backup actuation is *indirect* (bc manages the producer,
not the artifacts directly) — so "configured correctly" and "artifacts exist"
are separable, unlike serverconfig where config *is* the outcome.

```yaml
status:
  phase: Applied
  observedGeneration: 3
  lastAppliedAt: "2026-07-10T18:20:00Z"
  conditions:
  - type: Reconciled          # producer configured to match policy (bc's direct control)
    status: "True"
    reason: ScheduleConfigured
    observedGeneration: 3
  - type: BackupsFresh        # the artifacts actually present & recent (observed)
    status: "True"            # False when the newest snapshot is older than policy allows
    reason: RecentSnapshotPresent
    message: "latest etcd snapshot 4h ago; policy daily"
    observedGeneration: 3
  observed:
    etcd:
      lastSnapshotTime: "2026-07-10T03:00:12Z"
      snapshotCount: 7
      latestSnapshotBytes: 104857600
    velero:
      lastBackupTime: "2026-07-10T02:00:00Z"   # reflected from newest Velero Backup CR
```

`Reconciled=False` + `BackupsFresh=True` = producer misconfigured but backups
still landing (rare). `Reconciled=True` + `BackupsFresh=False` = producer looks
right but snapshots are stale → a **runtime** failure bc surfaces (reason
`BackupsStale`) but cannot directly fix; root-cause via the job's logs /
`kube_job_*`.

---

## Metrics shape

bc promotes only its **numeric etcd truth** — snapshot freshness, count, size — as
plain per-cluster gauges (Collector-over-snapshot, refreshed each reconcile): the
reads-only artifact layer bc alone observes. The observed producer *config*
(schedule/location/enabled) is NOT a metric — it lives on the CR `.status`. Per
DOMAIN-CONTROLLER.md §6, a field becomes a metric only when a real alert needs it.

```
# etcd — bc is the sole source
configbundle_backupconfig_status_etcd_last_snapshot_seconds{cluster}  1.7204e9
configbundle_backupconfig_status_etcd_snapshot_count{cluster}         7
configbundle_backupconfig_status_etcd_latest_bytes{cluster}           1.048e8

# liveness (all mechanisms)
configbundle_backupconfig_reconcile_timestamp_seconds{cluster}        1.72e9
```

Alert falls out directly: `time() - configbundle_backupconfig_status_etcd_last_snapshot_seconds > 26h`
= "missed the daily etcd backup." No Pushgateway, no KSM-combing.

**Velero emits no configbundle metrics** — defer to `velero_*`. The
single-pane-of-glass is a Grafana row keyed on `cluster` that joins bc's etcd
signals + `velero_*` + KSM, not bc absorbing everyone's metrics.

---

## Settled Decisions

- **bc observes the backups (artifacts), not the producer (CronJob/Schedule).**
  Observed state = actual backup inventory, read live — mirroring serverconfig
  observing the device, not the Redfish client.
- **Ownership depth follows control-loop ownership.** etcd has no independent
  owner → bc owns observation (reads blob, emits metrics). Velero owns its own
  lifecycle → bc defers (reads `Backup` CRs, defers to `velero_*`).
- **etcd: observe only for now.** Report freshness/count/size with blob
  **read-only** creds. Retention (prune) is deferred — it needs write/delete
  creds. Integrity verification is deferred and must NOT run in the reconcile
  loop (do it at the job's write-time if at all).
- **Velero is not pure passthrough — bc owns intent→`Schedule` fidelity.** Only
  outcomes are deferred.
- **Do NOT reach around Velero to its object storage.** Read a subsystem through
  its API (`Backup` CRs), never around it.
- **Do NOT re-emit `velero_*` or `kube_*` (KSM) metrics.** Those are their
  owners'. bc emits only configbundle-domain signals — for etcd, where it is the
  sole source; for velero, nothing.
- **Two conditions for backup** (`Reconciled` = config fidelity, `BackupsFresh`
  = artifact truth) because actuation is indirect. serverconfig needs only one
  because its config *is* the observed outcome.
- **Poll etcd (blob unwatchable); watch Velero `Backup` CRs (K8s-native).** Do
  not force serverconfig's fast poll cadence onto bc.
- **One observe switch: `BACKUP_OBSERVE_INTERVAL` (>0 = on).** Do NOT reintroduce
  a separate `ETCD_SNAPSHOT_OBSERVE`-style bool — it only proxied "creds mounted,"
  which store construction self-detects (graceful degrade on missing creds).
  Mirrors serverconfig's `IDRAC_OBSERVE_INTERVAL`.
- **Azure creds live in an overlay, never the shared base.** Base ships
  observation off + credential-free; `config/overlays/dev-main` patches the
  interval and mounts `az-storage-creds`.

---

## Implementation status

**etcd artifact-observation is implemented and feature-gated.** The
`etcdBackupStore` interface, the `BackupsFresh` condition, the
`configbundle_backupconfig_status_etcd_*` metrics (Collector-over-snapshot),
`.status.observed.etcd` artifact fields, and the freshness classifier all exist
and are unit-tested with a fake store. It is **gated on a configured store**
(`BackupConfigReconciler.EtcdStore`): nil = observation off, bc behaves as
before (manages the CronJob, no artifact read). Staleness threshold is
`EtcdSnapshotStaleAfter` (default to a daily-ish window when wired).

The concrete **Azure Blob reader** (`NewAzureEtcdStore`, using
`azure-sdk-for-go/sdk/storage/azblob` + `azidentity` environment credential) is
implemented. **Enablement is a single switch: `BACKUP_OBSERVE_INTERVAL`** — set
it (>0) to turn observation on at that cadence; `0s`/unset = off. This mirrors
serverconfig's `IDRAC_OBSERVE_INTERVAL`; there is deliberately **no separate
on/off flag** (an earlier `ETCD_SNAPSHOT_OBSERVE` bool was removed — it merely
proxied "creds are mounted," which the store construction already self-detects).
When the interval is set, bc builds the Azure store from a read-scoped SP mounted
as `AZURE_TENANT_ID`/`AZURE_CLIENT_ID`/`AZURE_CLIENT_SECRET`; missing/invalid
creds → logs and runs without observation rather than crashing (bc still manages
the CronJob and reconciles config via watches).

`ETCD_SNAPSHOT_STALE_AFTER` is a **staleness alarm, not retention** — when the
newest snapshot is older than it, `BackupsFresh` flips to False (`SnapshotStale`).
It never deletes a snapshot. (It is named `STALE_AFTER`, not `MAX_AGE`, precisely
because "max age of a snapshot" reads as a TTL/retention cap, which this is not.)
Retention (expiring old blobs) is not implemented.

The base (`config/default`) ships observation **off** and credential-free — the
Azure creds are environment-specific and never belong in the shared base. Turn
observation on per environment with an overlay that patches the interval and
mounts the creds: `config/overlays/dev-main` sets `BACKUP_OBSERVE_INTERVAL=30m`
+ the `az-storage-creds` secretKeyRefs (`kubectl apply -k config/overlays/dev-main`).

**Watches are implemented.** bc `Owns()` the etcd `CronJob` (always) and the
Velero `Schedule` (when the Velero CRD is installed), both spec-predicate — so
producer config drift is caught event-driven regardless of the interval. The
interval is therefore purely the blob-poll cadence, not a config-drift poll.

**Not yet built:**
- **Velero artifact reflection** from `Backup` CRs into `.status.observed.velero`
  — the velero side still reports producer config only.
- **Retention** and **integrity verification** — explicitly out of scope (see
  the etcd depth limit above).

---

## External references

- serverconfig/iDRAC observation model — the analog this mirrors (device state
  via Redfish; here, backup state via the store).
- [CRD types](./CRD.md) — BackupConfig spec/status shape.
- Velero metrics: `velero_backup_*`; kube-state-metrics: `kube_cronjob_*`,
  `kube_job_*` — the downstream owners bc defers to.

---

## Domain file maintenance

Update this file when:
- A backup mechanism is added (S3Sync) or its ownership model changes.
- The etcd depth limit moves (retention or integrity verification is adopted).
- The status conditions or `configbundle_backupconfig_*` metric shape changes.

Updates must be in the same PR as the code change that prompted them.
