package serverconfig

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Metrics for the reconciler. Per docs/reference/DOMAIN-CONTROLLER.md §6, metrics
// are the ALERTABLE PROJECTION, not the observed twin — the full observed iDRAC
// state lives on the CR .status, not here. We emit two families:
//   - reconcile OUTCOME: reconcile_success (per-object 0/1, absent = skip),
//     last-success timestamp, error counter (the backbone; fleet RED comes free
//     from controller_runtime_*);
//   - one PROMOTED field: ssh_enabled — SSH is a security-relevant toggle worth
//     alerting on directly, so it graduates from .status to a metric. The rest of
//     idracSettings stays on .status.
//
// One live Redfish read fans out to these gauges AND to CR status independently;
// metrics never block on a status write. Labels are identity only, all 1:1 with
// the object (so zero added cardinality):
//   - {server}  — the CR name / hostname (for kubectl correlation)
//   - {oob_ip}  — the OOB/iDRAC address operators act on, so an alert names the
//     box responders actually reach
//   - {orb_id}  — spec.orbId, the immutable orbital node identity; the uniform
//     cross-controller join key to the CMDB and divergence reports.
//
// Removals delete by {server} (partial match), so a changed oob_ip can't leave a
// stale series. orb_id is immutable, so it never churns.
var (
	// serverConfigReconcileTimestamp mirrors the bc-controller pattern:
	// updated on every successful reconcile; alerts fire when the
	// (time() - <this>) gap exceeds an expected cadence, catching a stuck /
	// dead sc-controller even when spec has not changed.
	serverConfigReconcileTimestamp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "configbundle_serverconfig_reconcile_timestamp_seconds",
		Help: "Unix timestamp of the last successful reconcile of this ServerConfig CR. Absent series = never reconciled.",
	}, []string{"server", "oob_ip", "orb_id"})

	// serverConfigReconcileErrors counts reconcile failures per CR, labelled
	// by a bounded set of reason strings (MissingCredentials, CredentialsLoadFailed,
	// RedfishReadFailed, RedfishPatchFailed). Cloud rate() queries surface which
	// failure mode is dominant across the fleet.
	serverConfigReconcileErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "configbundle_serverconfig_reconciliation_errors_total",
		Help: "Cumulative reconcile failures per ServerConfig CR, labelled by failure reason.",
	}, []string{"server", "oob_ip", "orb_id", "reason"})

	// serverConfigReconcileSuccess is the per-object "did the last reconcile
	// converge?" level: 1 = converged, 0 = failed. The series is ABSENT for a
	// deliberately-skipped server (not on the OOB allowlist / no OOB IP) or one
	// never reconciled — so `== 0` alerts fire only for genuinely-broken boxes,
	// never for skips. It is the load-bearing "which server is failing?" signal
	// for the cloud. Set from recordReconcileSuccess/recordReconcileError; the
	// series is deleted by removeReconcileSuccess on skip and on CR delete.
	// See docs/reference/DOMAIN-CONTROLLER.md §6.
	serverConfigReconcileSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "configbundle_serverconfig_reconcile_success",
		Help: "1 if this ServerConfig's last reconcile converged, 0 if it failed. Absent = deliberately skipped or never reconciled.",
	}, []string{"server", "oob_ip", "orb_id"})

	// serverConfigSSHEnabled is a PROMOTED observed field (§6 Family 2): the
	// observed SSH-enable state of the server's iDRAC. SSH is security-relevant, so
	// it is alertable directly (`== 1`). The value IS the boolean, so a plain
	// GaugeVec keyed by {server} suffices — no stale-label problem, no
	// Collector-over-snapshot needed (unlike the removed info metric, where the
	// value lived in a label). Set on a successful Redfish read; deleted on skip,
	// CR delete, or when the SSH state could not be read (absent = unknown).
	serverConfigSSHEnabled = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "configbundle_serverconfig_ssh_enabled",
		Help: "1 if SSH is enabled on the server's iDRAC (observed live via Redfish), 0 if disabled. A promoted field metric; the rest of idracSettings lives on the CR .status. Absent = not yet observed / server failing or skipped.",
	}, []string{"server", "oob_ip", "orb_id"})
)

func init() {
	metrics.Registry.MustRegister(
		serverConfigReconcileTimestamp, serverConfigReconcileErrors, serverConfigReconcileSuccess,
		serverConfigSSHEnabled,
	)
}

// recordReconcileSuccess marks a successful reconcile: bumps the last-success
// timestamp and sets the reconcile_success level to 1 for this CR. oobIP is the
// server's OOB/iDRAC address, carried as an identity label so an alert names the
// box operators actually reach.
func recordReconcileSuccess(server, oobIP, orbID string, now int64) {
	labels := prometheus.Labels{"server": server, "oob_ip": oobIP, "orb_id": orbID}
	serverConfigReconcileTimestamp.With(labels).Set(float64(now))
	serverConfigReconcileSuccess.With(labels).Set(1)
}

// recordReconcileError increments the failure counter for the given reason and
// drops the reconcile_success level to 0 (the last reconcile did not converge).
// Reason strings should stay bounded — pick from a fixed enum, do not
// interpolate error messages.
func recordReconcileError(server, oobIP, orbID, reason string) {
	serverConfigReconcileErrors.With(prometheus.Labels{"server": server, "oob_ip": oobIP, "orb_id": orbID, "reason": reason}).Inc()
	serverConfigReconcileSuccess.With(prometheus.Labels{"server": server, "oob_ip": oobIP, "orb_id": orbID}).Set(0)
}

// removeReconcileSuccess deletes a server's reconcile_success series so it
// becomes absent — called when the server is skipped (absent = skip, never 0)
// or its CR is deleted. Deletes by the stable {server} key (partial match), not
// the full label-set, so removal needs no oobIP and a changed OOB address (rare:
// server replacement / onboarding correction) can't leave a stale series.
func removeReconcileSuccess(server string) {
	serverConfigReconcileSuccess.DeletePartialMatch(prometheus.Labels{"server": server})
}

// recordSSHEnabled sets the observed SSH-enabled gauge (1/0) for a server, from
// the live Redfish read. oobIP is carried as an identity label (see
// recordReconcileSuccess).
func recordSSHEnabled(server, oobIP, orbID string, enabled bool) {
	v := 0.0
	if enabled {
		v = 1
	}
	serverConfigSSHEnabled.With(prometheus.Labels{"server": server, "oob_ip": oobIP, "orb_id": orbID}).Set(v)
}

// removeSSHEnabled drops a server's ssh_enabled series so it becomes absent —
// on skip, CR delete, or when the SSH state could not be read (absent = unknown,
// never a false 0). Partial match on {server} — same rationale as
// removeReconcileSuccess.
func removeSSHEnabled(server string) {
	serverConfigSSHEnabled.DeletePartialMatch(prometheus.Labels{"server": server})
}
