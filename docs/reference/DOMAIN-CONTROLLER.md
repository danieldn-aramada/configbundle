# Domain Controller Template

A standard for building the controllers that manage one class of resource on
behalf of the cloud. `serverconfig` (iDRAC) and `backupconfig` (etcd + Velero
backups) are the first two; this doc is the shape every future one should follow.

Individual controllers may deviate — the differences are real and sanctioned
(see [Sanctioned deviations](#sanctioned-deviations)). But they all present the
same approach.

---

## The approach, in one line

> A **domain controller** owns one class of resource that lives **outside**
> Kubernetes. It continuously drives that resource toward the intent declared in
> `.spec`, and reflects the resource's observed reality back into `.status` and
> the metrics pipeline. **Intent flows down; observed truth flows up** — so the
> gap between desired and actual stays visible in both Kubernetes and the cloud.

Two-sentence version, for a slide:

> A domain controller is a two-way bridge for one class of out-of-cluster
> resource: it reconciles the resource toward the intent in `.spec`, and from a
> single continuous observation it records the resource's actual state on
> `.status` — the one observed surface, readable on-cluster and from the cloud —
> while projecting reconcile health and any alertable fields into the metrics
> pipeline. Intent flows down, observed truth flows up — the cloud sees edge
> reality without ever reaching into the cluster.

Everything below is the mechanics of that one idea.

---

## 1. `.spec` is intent; `.status` is observed

The `spec`/`status` split **is** the desired-vs-observed split — that is the
label. Do not add an `observed:` (or `atProvider:`-style) wrapper under status;
`status` already means "observed." (Core Kubernetes never wraps: `Pod.status.podIP`,
`Deployment.status.replicas` sit flat.)

- **Mirror every *observable* spec field under the same status path.**
  `spec.fooSettings.fieldA` (desired) ↔ `status.fooSettings.fieldA` (observed).
  Same shape → drift is a generic field diff (`diff(spec.foo, status.foo)`), so
  one reflection-based differ serves every controller. This is the single
  biggest reason the shape is standardized: **shared machinery.**
- **Status is a superset, not a bijection.** It also carries observation-only
  fields with no spec twin — freshness, health, counts, timestamps. These are
  often the *most valuable* signals. (`Deployment.status.availableReplicas` has
  no `spec` counterpart; neither does an etcd `lastSnapshotTime`.)
- **A spec field that isn't observable has no twin** — write-only, one-shot, or
  no read-back. Don't fabricate an empty mirror.

```yaml
apiVersion: armada.ai/v1
kind: FooConfig
spec:
  fooSettings:
    fieldA: <desired>          # observable → mirrored into status
    fieldB: <desired>
    # a write-only / one-shot field would have NO status twin
status:
  observedGeneration: 5
  conditions:                  # see §4
    - type: Reconciled
      status: "True"
  lastAppliedAt: <ts>          # cross-cutting observation-only → top level
  fooSettings:
    fieldA: <observed_last>    # 1:1 mirror of spec.fooSettings.fieldA
    fieldB: <observed_last>
    lastObservedAt: <ts>       # domain observation-only → in the block, no spec twin
    health: <...>
```

## 2. Observation is direct; one read feeds two surfaces

Every domain controller **directly observes** its external system — it polls the
system and reports what it sees. There is no lesser, second-hand form of
observation; `backupconfig` reading Azure blob state is exactly as direct as
`serverconfig` reading Redfish attributes.

**One live read per reconcile fans out to `.status` AND metrics, independently.**

```
              ┌── .status.fooSettings   (Kubernetes-native view: kubectl, GitOps)
observe() ────┤
              └── configbundle_foo_*     (cloud view: metrics pipeline → Prometheus)
```

- **Metrics MUST NOT be derived from `.status`.** Both come from the same read.
  (Deriving metrics from status couples them to a status write that can lose a
  `RetryOnConflict` race — silently dropping gauges. Keep them independent.)
- `.status` is the K8s surface; metrics are the cloud surface. Neither is the
  other's source of truth — the *observation* is.

## 3. Classify each external system: writes-and-reads vs reads-only

A controller may touch more than one external system. Classify each:

| Relationship | Meaning | Status role |
|---|---|---|
| **writes-and-reads** | the controller both actuates and observes it | **config fidelity** → mirror the spec fields (`spec.X` ↔ `status.X`) |
| **reads-only** | the controller observes it but something else writes it | **outcome / monitoring** → observation-only fields, no spec twin |

> **The number of independent health signals (conditions and/or Prometheus
> alerts) equals the number of independently-failing observed layers** — not a
> fixed count.

- A controller that only *writes-and-reads* one system needs **one** signal:
  observed == desired, done. (Its config *is* the outcome.)
- A controller that also *reads-only* a downstream outcome needs a **second**
  signal for that outcome, because config-fidelity and outcome can diverge
  (config perfect, downstream actor failed). Where that second verdict lives —
  a CR condition vs. a PrometheusRule on the raw metric — is a per-controller
  choice; the raw observed fact belongs on `.status` either way.

## 4. Reconcile behavior

- **Watch in-band, poll out-of-band.** Kubernetes-native inputs (the CR, owned
  sub-objects) are *watchable* → use `For`/`Owns`, react event-driven and
  instantly. External systems (iDRAC, blob storage) are *out-of-band* — K8s has
  no visibility, nothing pushes events → **poll** on an interval. The interval
  is the switch for out-of-band observation; when it's unset the controller is
  purely watch-driven. (Do not add a redundant on/off flag beside the interval.)
- **Always SSA-apply; do not gate the apply on a pre-diff.** SSA is idempotent.
  Use the computed delta for the status summary / events, never to skip the
  apply (skipping silently drops metadata reconciliation — owner refs, labels).
- **Reconcile drift, then back off.** On observing drift, attempt to converge.
  On failure, return the error so controller-runtime applies exponential backoff
  and retries — same as any well-behaved controller.
- **Surface errors; never swallow them.** A managed resource that is unreachable
  or misconfigured is a fault the operator must see (`Reconciled=False` + a
  Warning event). Distinguish it from *deliberately out of scope* (see Unknown,
  below).

## 5. Status conventions

**Conditions** — via `meta.SetStatusCondition` (the apimachinery-canonical
upsert). Tri-state, PascalCase types, per-condition `observedGeneration`.

| `status` | Meaning |
|---|---|
| `True` | managed and converged |
| `False` | managed and **determined not converged** — a real fault, alert on this |
| `Unknown` | not being determined — deliberately skipped / out of scope |

- **`Skipped` → `Unknown`, never `False`.** A CR the controller intentionally
  doesn't manage (not allow-listed, missing prerequisite) isn't broken. Keeping
  it out of `False` means an operator alerting on `Reconciled=False` pages only
  for genuinely-broken resources. (Same reasoning as `NodeReady=Unknown`.)
- **`LastTransitionTime` moves only on a status *flip*** (K8s norm). It lies for
  "still True, but the controller just did more work." So carry a separate
  **`lastAppliedAt`** timestamp — the truthful "is the controller still working?"
  signal — bumped on every successful reconcile.
- **Condition `message` describes state** ("all managed settings match intent"),
  not the last action. **Per-action history goes to Kubernetes Events**, not the
  condition — a `Normal` event on apply, a `Warning` event on failure. Events
  aggregate/dedup, so emitting on every failing reconcile yields one aggregated
  event, not a flood.

**Phases** (`.status.phase`) are a coarse human-readable rollup
(`Pending`/`Applied`/`Diverged`/`Skipped`). Conditions are the machine signal;
phase is the at-a-glance one.

## 6. Metrics conventions

Namespace `configbundle_*`, taxonomy `configbundle_<kind>_<subject>_<measure>`.
Never `armada_*`. One live read fans out to status AND metrics (§2) — metrics are
never derived from status.

### Metrics are the alertable projection — not the observed twin

The full observed state already has a home: `.status` (§1–3), read with `kubectl`
on-cluster and remotely by the cloud. **Metrics are not a second copy of that
twin.** A controller emits only:

1. **Reconcile outcome** — is the loop healthy, and did *this object* converge?
2. **The specific observed fields you actually alert or graph on** — promoted one
   at a time, only when a real alert/dashboard needs them.

**Intent (`.spec`) is NOT a metric** — it's declarative input the cloud authored
and pushed down (the orbital artifact); echoing it back is redundant. No
`configbundle_<kind>_spec_*`, no PromQL `spec != status` (the controller already
computes drift — that *is* reconciliation).

This is how the mature external-resource operators do it: **cert-manager**
promotes exactly two of a Certificate's fields to metrics (the expiry + renewal
timestamps) and leaves everything else on `.status`; **Crossplane** — whose entire
job is managing external resources — keeps per-object state off `/metrics`
altogether, emitting only aggregate pipeline metrics. Do not dump the whole
observed config into an info metric; that is what `.status` is for.

### Family 1 — reconcile outcome (the backbone)

This is the load-bearing surface — invest here first.

**Fleet health is free.** controller-runtime registers, per *controller*, with
zero code:
- `controller_runtime_reconcile_total{controller,result}` — `result` ∈
  `success`/`error`/`requeue`/`requeue_after` (the RED workhorse);
- `controller_runtime_reconcile_errors_total{controller}`, the
  `_reconcile_time_seconds` histogram, and the `workqueue_*` family (depth,
  latency, retries).

Use these for rate/errors/duration and saturation — **do not reinvent them.**

**What isn't free is per-object identity** — controller-runtime deliberately puts
no `{name}` on anything (unbounded-cardinality guard). That gap is exactly what a
domain controller fills, with three per-object series keyed by identity only:

- `configbundle_<kind>_reconcile_success{name}` — `1` converged, `0` failed,
  **absent = deliberately skipped** (not allow-listed / prerequisite missing) or
  never reconciled. Alert `== 0`; skips are absent so they never page. This is the
  "*which* object is broken" signal.
- `configbundle_<kind>_reconcile_timestamp_seconds{name}` — Unix time of the last
  *success*; `time() - <this>` = how long it's been broken or stuck.
- `configbundle_<kind>_reconciliation_errors_total{name,reason}` — `reason` a
  bounded enum; `increase()` surfaces the dominant failure mode.

**Boolean + absent, not a condition state-set.** The ecosystem convention
(kube-state-metrics, Flux, cert-manager) is a state-set gauge —
`{condition="Ready",status="True|False|Unknown"}`, one series per status value,
`1` on the active one. We deliberately don't: `absent = skip` already delivers
"skips never page" at a third of the series, and the distinction the state-set's
`Unknown` buys (failed vs unreachable) doesn't change alerting — both page — while
the detail lives on the CR anyway (next point). **Do NOT emit conditions as
state-set metrics.**

**Failure reason is NOT in the metric.** A boolean has no room; the reason lives
in the CR's `Reconciled` condition + Events (`kubectl describe`). The metric says
"this one's wrong"; the CR says why.

### Family 2 — observed fields, promoted one at a time

A field graduates from `.status` to a metric **only when there is a real alert or
dashboard behind it** — never "surface everything just in case." The author picks
*which* fields; the template fixes the *shape*:

- **Boolean field** → `configbundle_<kind>_<field>{name}`, a plain `0/1` gauge —
  the value *is* the boolean, identity in labels. Alert `== 1` / `== 0`. Example:
  `configbundle_serverconfig_ssh_enabled{server}` (SSH is a security-relevant
  toggle worth alerting on; the *rest* of idracSettings stays on `.status`,
  un-metricked).
- **Numeric field** (count, size, timestamp) →
  `configbundle_<kind>_<subject>_<measure>{name}`, the number in the *value*.
  Timestamps as absolute Unix seconds — alert `time() - <metric> > threshold`, so
  the threshold is tunable in the rule, not baked into the exporter
  (cert-manager's expiry pattern). Examples: backupconfig's
  `status_etcd_last_snapshot_seconds`, `_snapshot_count`, `_latest_bytes`.

**Never put a value in a label** — you can't `time() - <label>` or `<label> == 0`.
**Never an info metric** enumerating all observed fields as labels — that was the
old shape; it duplicated the `.status` twin and metricked fields nobody alerts on.

### Cardinality

Per-object metrics (a `name` label) are what tell the cloud *which* resource is
affected — worth the cost, and safe here because:
- **Labels are identity only** (`name`/`cluster`) — never a value, timestamp, or
  unbounded id. That is the one true cardinality lever.
- **Identity labels can go beyond the CR name — but only stable identity keys.**
  Two legitimate additions: an **operational address** the responder acts on
  (serverconfig's `oob_ip`, the iDRAC address — lets an alert name the box), and the
  **uniform `orb_id`** (`spec.orbId`, the immutable orbital node id — the join key
  to the CMDB and divergence reports, and the one label that lets queries span
  controllers). Both are 1:1 with the object → zero added series. `orb_id` SHOULD be
  on every domain controller's metrics; `oob_ip`-style addresses are per-controller.
  The guard: these are stable **identity** keys, not descriptive/observed attributes
  — firmware, status, schedules stay on `.status` (and an `_info` join if ever
  needed), never as labels. Delete on the stable CR-name key so a rare address
  change can't strand a series.
- **One series per object per metric** — linear in a physically-bounded inventory
  (servers per cluster; ~1 BackupConfig per cluster): tens to low thousands even
  federated, orders below a single Prometheus's comfort zone.
- Dropping the info metric and promoting fields one at a time keeps it *linear*,
  not multiplicative. (Crossplane defers per-object to KSM only because it manages
  tens of thousands of *unbounded* objects — that scale doesn't bind us. If an
  object type ever does explode, offload to KSM-CRS or drop to aggregate-only.)

### Self-emit vs kube-state-metrics: a genuine tradeoff

Both are viable; a shared template presents both rather than mandate one:
- **Self-emit** (configbundle's default): the metric surface lives in-repo with
  the CRD, fed by the same live read (§2); full control of taxonomy.
- **KSM-CRS:** zero code — declarative YAML in the monitoring stack reads the CRD
  status; but the surface then lives in kube-prometheus-stack values, off the
  "controllers own their pipeline metrics" line.

### Stay in your lane

Emit only pipeline-domain metrics. Downstream operational metrics belong to their
owners (`velero_*`, `kube_*` via KSM) — do not re-emit them.

## 7. Sanctioned deviations

The common approach above holds for all domain controllers. These are the axes
on which they legitimately differ — name them, don't hide them:

- **Number of external systems** touched (one vs several), and the
  writes-and-reads / reads-only classification of each (§3).
- **Number of health signals** — falls out of §3, not chosen arbitrarily.
- **Where an outcome verdict lives** — a CR condition vs a PrometheusRule.
- **How much of `.spec` is observable** — a fully-observable resource mirrors
  nearly all of spec; an indirectly-produced one mirrors config-fidelity and
  carries the outcome as observation-only fields.

## 8. Reference implementations

- **`serverconfig` (iDRAC)** — the simplest shape. One external system, which it
  **writes-and-reads** (PATCHes an attribute, then reads it back). `.spec` mirrors
  almost entirely into `.status`; one condition (`Reconciled`); polls Redfish
  (out-of-band) on the observe interval.
- **`backupconfig` (etcd + Velero)** — a hybrid. **Writes-and-reads** the producer
  (the etcd `CronJob` / Velero `Schedule` — watched in-band, config mirrored into
  status) *and* **reads-only** the artifact store (Azure blob — polled
  out-of-band, surfaced as observation-only fields like `lastSnapshotTime`). The
  second, reads-only layer is why it carries an outcome signal beyond config
  fidelity. See [`BACKUP.md`](BACKUP.md).

## 9. Worked example — serverconfig metrics

Illustrates §6 on the serverconfig reference implementation. Three servers:
`node-a` healthy, `node-b` broken (iDRAC unreachable), `node-c` skipped (not on
the allowlist).

Exposition (`/metrics`):

```
# Family 1 — reconcile outcome (per-object; the backbone). Identity labels: server
# (kubectl), oob_ip (the address on-call reaches), orb_id (spec.orbId — the
# immutable cross-controller join key to orbital / divergence).
configbundle_serverconfig_reconcile_success{oob_ip="10.0.0.11",orb_id="orb-aaa",server="node-a"} 1
configbundle_serverconfig_reconcile_success{oob_ip="10.0.0.12",orb_id="orb-bbb",server="node-b"} 0
# node-c skipped → no series emitted
configbundle_serverconfig_reconcile_timestamp_seconds{oob_ip="10.0.0.11",orb_id="orb-aaa",server="node-a"} 1783900800
configbundle_serverconfig_reconciliation_errors_total{oob_ip="10.0.0.12",orb_id="orb-bbb",reason="RedfishReadFailed",server="node-b"} 7

# Family 2 — one promoted field (SSH), a plain 0/1 gauge; the rest of
# idracSettings stays on .status, not here
configbundle_serverconfig_ssh_enabled{oob_ip="10.0.0.11",orb_id="orb-aaa",server="node-a"} 0
configbundle_serverconfig_ssh_enabled{oob_ip="10.0.0.12",orb_id="orb-bbb",server="node-b"} 1
# node-c skipped → no series

# Plus, free from controller-runtime (per-controller, NOT per-object):
#   controller_runtime_reconcile_total{controller="serverconfig",result="error"} …
```

Queries (paste into Grafana):

| Question | PromQL | Returns |
|---|---|---|
| Which servers are failing *now*? | `configbundle_serverconfig_reconcile_success == 0` | `node-b` (`node-c` skipped → absent → never shows) |
| Alert on *sustained* failure | `configbundle_serverconfig_reconcile_success == 0`, `for: 10m` | one alert per broken server; a transient clears before 10m; skips can't fire |
| How long has it been broken? | `time() - configbundle_serverconfig_reconcile_timestamp_seconds` | seconds since last success (the timestamp only bumps on success) |
| How flaky is it? | `increase(configbundle_serverconfig_reconciliation_errors_total[1h])` | failure count — catches flapping even after `success` flips back to `1` |
| Which servers have SSH on? | `configbundle_serverconfig_ssh_enabled == 1` | `node-b` — a value gauge, alertable directly (`== 1`) |
| Fleet error rate (free) | `sum by (controller) (rate(controller_runtime_reconcile_total{result="error"}[5m]))` | per-controller error rate, no custom metric |

**Where's the full iDRAC state?** On the CR — `.status.idracSettings` (`kubectl get serverconfig node-a -o yaml`), read remotely by the cloud. Only `sshEnabled` is a metric, because only it has an alert behind it. **Where's "drift"?** There is no `spec != status` query. *Converged?* → `reconcile_success`. *Desired?* → the CR `.spec` / orbital, which already has it. Drift is reported (a boolean the controller computed), never re-derived in PromQL.

---

## Settled Decisions

- **`.spec` = intent, `.status` = observed — the prefix is the label.** No
  `observed:`/`atProvider:` wrapper under status; mirror observable spec fields
  at matching paths, and let status be a superset for observation-only truth.
- **Observation is always direct; one read fans out to `.status` and metrics
  independently.** Metrics are never derived from status.
- **Classify each external system writes-and-reads vs reads-only; health signals
  = independently-failing observed layers.** Not a fixed condition count.
- **Watch in-band (K8s objects), poll out-of-band (external systems).** The
  observe interval is the single switch for out-of-band observation.
- **`Skipped` is `Unknown`, not `False`.** False = managed and broken; Unknown =
  deliberately not managed.
- **Condition message = state; per-action history = Kubernetes Events.**
  `lastAppliedAt` (not `LastTransitionTime`) is the "still working" signal.
- **Intent (`.spec`) is not a metric.** It's declarative input the cloud already
  has (the orbital artifact); echoing it back is redundant. Metrics carry only
  what the cloud can't otherwise know: reconcile outcome + observed state. No
  `_spec_` metrics, no PromQL `spec != status` drift (the controller already
  computes drift — that's reconciliation).
- **Reconcile outcome is the backbone — invest there first.** Fleet RED is free
  from controller-runtime (`controller_runtime_reconcile_total{controller,result}`
  + duration histogram + `workqueue_*`); don't reinvent it. Fill the per-object
  gap it deliberately leaves with `reconcile_success{name} 0/1` (**absent =
  skip**), `reconcile_timestamp_seconds{name}`, and
  `reconciliation_errors_total{name,reason}`. **Boolean + absent, NOT a condition
  state-set** (the KSM/Flux/cert-manager shape): absent=skip already gives
  "skips never page" at a third of the series, and failed-vs-unreachable doesn't
  change alerting. Failure *reason* stays on the CR condition + Events, never in a
  metric; do NOT emit conditions as state-set metrics.
- **Metrics are the alertable projection, not the observed twin.** The full
  observed state lives on `.status` (read on-cluster and from the cloud); do NOT
  duplicate it into metrics. A field graduates from `.status` to a metric ONLY
  when a real alert/dashboard needs it (author's discretion) — cert-manager
  promotes ~2 Certificate fields; Crossplane keeps per-object state off `/metrics`
  entirely. Shape: boolean field → plain `0/1` gauge
  `configbundle_<kind>_<field>{name}` (value is the bool); numeric/timestamp →
  value gauge, alert `time() - <metric>`. NEVER an info metric enumerating all
  fields as labels (the old shape — it duplicated `.status` and metricked fields
  nobody alerts on); never a number in a label. Labels are identity only
  (name/cluster) — the one cardinality lever.
- **`orb_id` (`spec.orbId`) is the uniform cross-controller identity label** —
  immutable, 1:1, present on every domain controller's metrics; the join key to the
  CMDB and divergence reports, and the one dimension queries can share across
  controllers. Extra identity labels are per-controller (a CR name for kubectl; an
  operational address like serverconfig's `oob_ip`). Identity keys only — never
  descriptive/observed values (those live on `.status`).
- **Self-emit vs KSM-CRS is a genuine tradeoff** (no tri-state to lose once
  success is a boolean). A shared template presents both; configbundle's default
  is self-emit (in-repo, fed by the one live read).
