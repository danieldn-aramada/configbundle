# Metrics Reference

Every Prometheus metric published by the configbundle domain controllers, in the
style of kube-state-metrics' per-resource metric docs. Only **serverconfig** and
**backupconfig** emit `configbundle_*` metrics; both also expose free framework
metrics on the same endpoint — see [Framework metrics](#framework-metrics-free)
for the reconcile + workqueue families an operator uses to triage the controller
itself.

Namespace is `configbundle_*`, taxonomy `configbundle_<kind>_<subject>_<measure>`.
For *why* each metric has the shape it does (boolean+absent, where the freshness
verdict lives, why the full observed state is NOT here), see
[`DOMAIN-CONTROLLER.md`](DOMAIN-CONTROLLER.md) §6.

Metrics are the **alertable projection**, not the observed twin — the full
observed state lives on the CR `.status`. Two families: **reconcile outcome** (the
backbone — is the loop healthy, did *this* object converge?) and **promoted
observed fields** (only those with a real alert/graph behind them).

---

## serverconfig (sc-controller)

Identity labels (all 1:1 with the object → no added cardinality): `server` = the
ServerConfig CR name (hostname, for kubectl); `oob_ip` = the OOB/iDRAC address
operators act on, so an alert names the box directly; `orb_id` = `spec.orbId`, the
immutable orbital node identity — the uniform cross-controller join key to the CMDB
and divergence reports.

| Metric name | Type | Labels | Description |
| --- | --- | --- | --- |
| `configbundle_serverconfig_reconcile_success` | Gauge | `server`, `oob_ip`, `orb_id` | `1` = last reconcile converged, `0` = failed. **Absent = deliberately skipped** (not on the OOB allowlist / no OOB IP) or never reconciled — so `== 0` alerts never fire for skips. |
| `configbundle_serverconfig_reconcile_timestamp_seconds` | Gauge | `server`, `oob_ip`, `orb_id` | Unix time of the last *successful* reconcile. `time() - <this>` = how long since it last worked. |
| `configbundle_serverconfig_reconciliation_errors_total` | Counter | `server`, `oob_ip`, `orb_id`, `reason` | Cumulative reconcile failures. `reason` ∈ `MissingCredentials`, `CredentialsLoadFailed`, `RedfishReadFailed`, `RedfishPatchFailed`. |
| `configbundle_serverconfig_ssh_enabled` | Gauge | `server`, `oob_ip`, `orb_id` | `1` if SSH is enabled on the server's iDRAC, `0` if disabled. A **promoted field metric** — SSH is security-relevant, so it's alertable directly (`== 1`); the rest of idracSettings stays on `.status`, not here. **Emitted only after a successful Redfish read** — absent while a server is failing or skipped. |

## backupconfig (bc-controller)

Identity labels (1:1 → no added cardinality): `cluster` = the BackupConfig CR name
(one per Kubernetes cluster); `orb_id` = `spec.orbId`, the immutable orbital node
identity — the uniform cross-controller join key to the CMDB and divergence reports.

| Metric name | Type | Labels | Description |
| --- | --- | --- | --- |
| `configbundle_backupconfig_reconcile_success` | Gauge | `cluster`, `orb_id` | `1` converged / `0` failed / **absent = skipped** (no velero or etcd block) or never reconciled. |
| `configbundle_backupconfig_reconcile_timestamp_seconds` | Gauge | `cluster`, `orb_id` | Unix time of the last *successful* reconcile. |
| `configbundle_backupconfig_reconciliation_errors_total` | Counter | `cluster`, `orb_id`, `reason` | Cumulative reconcile failures. `reason` ∈ `VeleroPatchFailed`, `EtcdPatchFailed`. |
| `configbundle_backup_velero_schedule_present` | Gauge | `cluster`, `orb_id` | `1` = live Velero Schedule exists on-cluster, `0` = spec asks for it but it's missing, **absent = not managed**. |
| `configbundle_backup_etcd_cronjob_present` | Gauge | `cluster`, `orb_id` | `1` / `0` / absent — same semantics for the live etcd CronJob. |
| `configbundle_backupconfig_status_etcd_last_snapshot_seconds` | Gauge | `cluster`, `orb_id` | Unix time of the newest etcd snapshot in the backup store. The freshness signal `EtcdSnapshotStale` alerts on. **Emitted only when artifact observation is enabled** (`BACKUP_OBSERVE_INTERVAL` set + `AZURE_*` creds). |
| `configbundle_backupconfig_status_etcd_snapshot_count` | Gauge | `cluster`, `orb_id` | Number of etcd snapshot objects under the cluster's prefix (`0` = empty store). **Observation-on only.** |
| `configbundle_backupconfig_status_etcd_latest_bytes` | Gauge | `cluster`, `orb_id` | Size in bytes of the newest etcd snapshot. **Observation-on only.** |

---

## Framework metrics

Exposed automatically on the same `/metrics` endpoint — **one endpoint, three
sources**: `controller_runtime_*` is the **controller-runtime** library;
`workqueue_*` and `rest_client_*` are **client-go** (controller-runtime only
registers them on its registry — that's why they aren't `controller_runtime_`-
prefixed); `go_*`/`process_*` are **client_golang**'s default collectors. All are
**per-controller** (label `controller="serverconfig"` / `"backupconfig"`; workqueue
uses `name` / `controller_name`) and carry **no object identity**. Rule of thumb:
`configbundle_*` tells you *which object* is wrong; these tell you whether *the
controller itself* is healthy. We do **not** rename these — the shared names are
what every K8s dashboard/runbook already knows.

### Reconcile loop (controller-runtime)

| Metric | Type | Labels | Operator use |
| --- | --- | --- | --- |
| `controller_runtime_reconcile_total` | Counter | `controller`, `result` | Throughput + outcome. `result` ∈ `success`/`error`/`requeue`/`requeue_after`; error *rate* = `result="error"` over total. |
| `controller_runtime_reconcile_errors_total` | Counter | `controller` | Total returned errors. Prefer `reconcile_total{result="error"}` for rate — the two can diverge (controller-runtime #2922). |
| `controller_runtime_terminal_reconcile_errors_total` | Counter | `controller` | Errors **not** requeued (gave up) — work being dropped, not retried. |
| `controller_runtime_reconcile_panics_total` | Counter | `controller` | Reconciler panics; should stay flat at `0`. |
| `controller_runtime_reconcile_time_seconds` | Histogram | `controller` | Reconcile duration (`_bucket`/`_sum`/`_count`) → p99 latency. |
| `controller_runtime_active_workers` | Gauge | `controller` | Workers reconciling right now. |
| `controller_runtime_max_concurrent_reconciles` | Gauge | `controller` | Concurrency ceiling; `active/max ≈ 1` sustained = saturated. |

### Work queue (client-go) 

| Metric | Type | Labels | Operator use |
| --- | --- | --- | --- |
| `workqueue_depth` | Gauge | `name` | Backlog; climbing-and-not-draining = falling behind or wedged. |
| `workqueue_adds_total` | Counter | `name` | Enqueue rate (how much work is arriving). |
| `workqueue_queue_duration_seconds` | Histogram | `name` | Wait from enqueue to pickup (event → processing latency). |
| `workqueue_work_duration_seconds` | Histogram | `name` | Processing time (mirrors reconcile duration). |
| `workqueue_retries_total` | Counter | `name` | Retry churn; high = repeated failure / flapping. |
| `workqueue_unfinished_work_seconds` | Gauge | `name` | Age of in-flight work not yet done; spikes when an item is stuck. |
| `workqueue_longest_running_processor_seconds` | Gauge | `name` | Longest single in-flight reconcile — catches one wedged item. |

Also on the endpoint, not tabled here: `rest_client_requests_total{code,method,host}`
+ `rest_client_request_duration_seconds` (client-go → is the apiserver reachable /
throttling?), and `process_*`/`go_*` (RSS, FDs, goroutines,
`process_start_time_seconds` for crashloop detection). Exact set varies ±a metric by
controller-runtime version — confirm on a live scrape:
`curl -s <addr>/metrics | grep -E '^(controller_runtime|workqueue|rest_client|process|go)_' | sort -u`.

No `configbundle` PrometheusRule fires on these yet — they're here for ad-hoc
troubleshooting; fleet-level alerts (controller error-rate, stalled controller,
p99 latency, queue depth) are a planned follow-up.

---

## Notes

- **`orb_id` is the shared cross-controller label** — every domain controller
  carries `orb_id` (= `spec.orbId`), so a query can span sc + bc and join to the
  CMDB / divergence data by orbital identity. The K8s-native names differ by design
  (sc `server` + `oob_ip`, bc `cluster`) — those are for kubectl / on-call, not
  cross-system correlation.
- **Prefix quirk in bc** — the two producer-presence gauges are
  `configbundle_backup_*`, while everything else is `configbundle_backupconfig_*`.
  Pre-existing; a candidate to normalize to `configbundle_backupconfig_*`.
- **No info metrics; the full observed state lives on `.status`.** A field appears
  here only when it has a real alert/graph behind it (e.g. `ssh_enabled`); the rest
  of the observed config is read from the CR, not Prometheus. Numeric truth (counts,
  timestamps, sizes) is always a gauge *value*, never a label — so you can
  `time() - <metric>` and `<metric> == 0`.
- **Alerting rules** over these metrics live in
  [`config/prometheus/rules/prometheus-rule.yaml`](../../config/prometheus/rules/prometheus-rule.yaml).
  (The Grafana dashboard under `config/prometheus/dashboards/` is unmaintained and
  still queries the removed iDRAC info metric — ignore until refreshed.)

*Regenerate the tables when metrics change — grep `Name: "configbundle` and
`NewDesc(` under `internal/*/metrics.go`.*
