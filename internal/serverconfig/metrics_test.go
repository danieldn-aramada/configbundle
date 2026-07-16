package serverconfig

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/client_golang/prometheus/testutil"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// gather renders a collector's series into a flat "name{k=v,...}" string per
// series so tests can assert on labels via substring. Uses a private registry
// so tests don't touch the global controller-runtime one.
func gather(t *testing.T, c prometheus.Collector) string {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register collector: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var sb strings.Builder
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			sb.WriteString(mf.GetName() + "{")
			for _, l := range m.GetLabel() {
				sb.WriteString(l.GetName() + "=" + l.GetValue() + ",")
			}
			sb.WriteString("}\n")
		}
	}
	return sb.String()
}

// TestReconcileSuccessGauge pins the reconcile_success contract: success → 1,
// failure → 0, and skip/delete → ABSENT (never 0 — absence is how a skipped
// server stays out of `== 0` alerts).
func TestReconcileSuccessGauge(t *testing.T) {
	defer removeReconcileSuccess("gauge-ok")
	defer removeReconcileSuccess("gauge-fail")

	recordReconcileSuccess("gauge-ok", "10.0.0.1", "orb-ok", 1234567890)
	recordReconcileError("gauge-fail", "10.0.0.2", "orb-fail", "RedfishReadFailed")

	if v := testutil.ToFloat64(serverConfigReconcileSuccess.WithLabelValues("gauge-ok", "10.0.0.1", "orb-ok")); v != 1 {
		t.Errorf("gauge-ok = %v, want 1 (converged)", v)
	}
	if v := testutil.ToFloat64(serverConfigReconcileSuccess.WithLabelValues("gauge-fail", "10.0.0.2", "orb-fail")); v != 0 {
		t.Errorf("gauge-fail = %v, want 0 (failed)", v)
	}

	// skip/delete ⇒ series absent, not 0.
	removeReconcileSuccess("gauge-fail")
	if got := gather(t, serverConfigReconcileSuccess); strings.Contains(got, "server=gauge-fail") {
		t.Errorf("expected gauge-fail series absent after remove, got:\n%s", got)
	}
}

// TestSSHEnabledGauge pins the promoted-field contract (§6 Family 2): SSH
// enabled → 1, disabled → 0, and skip/delete/unreadable → ABSENT (never a false
// 0 — absence means "not observed", which must not read as "SSH is off").
func TestSSHEnabledGauge(t *testing.T) {
	defer removeSSHEnabled("ssh-on")
	defer removeSSHEnabled("ssh-off")

	recordSSHEnabled("ssh-on", "10.0.0.3", "orb-on", true)
	recordSSHEnabled("ssh-off", "10.0.0.4", "orb-off", false)

	if v := testutil.ToFloat64(serverConfigSSHEnabled.WithLabelValues("ssh-on", "10.0.0.3", "orb-on")); v != 1 {
		t.Errorf("ssh-on = %v, want 1 (enabled)", v)
	}
	if v := testutil.ToFloat64(serverConfigSSHEnabled.WithLabelValues("ssh-off", "10.0.0.4", "orb-off")); v != 0 {
		t.Errorf("ssh-off = %v, want 0 (disabled)", v)
	}

	// skip/delete/unreadable ⇒ series absent, not a false 0.
	removeSSHEnabled("ssh-off")
	if got := gather(t, serverConfigSSHEnabled); strings.Contains(got, "server=ssh-off") {
		t.Errorf("expected ssh-off series absent after remove, got:\n%s", got)
	}
}

// TestMetricsEndpoint_ServerConfig is the end-to-end surface check for the §6
// reshape: after the reconciler populates the metric state, the promhttp endpoint
// over the real controller-runtime registry renders the reconcile backbone + the
// promoted ssh_enabled gauge, and the removed status_idracsettings_info metric is
// GONE from the endpoint entirely. Mirrors bc's TestMetricsEndpoint_BackupConfig.
func TestMetricsEndpoint_ServerConfig(t *testing.T) {
	defer removeReconcileSuccess("metrics-int")
	defer removeSSHEnabled("metrics-int")

	// Populate the surface the way a converged reconcile with SSH observed on does.
	recordReconcileSuccess("metrics-int", "10.9.9.9", "orb-mi", 1234567890)
	recordSSHEnabled("metrics-int", "10.9.9.9", "orb-mi", true)

	srv := httptest.NewServer(promhttp.HandlerFor(crmetrics.Registry, promhttp.HandlerOpts{}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	page := string(body)

	for _, want := range []string{
		`configbundle_serverconfig_reconcile_success{oob_ip="10.9.9.9",orb_id="orb-mi",server="metrics-int"} 1`,
		`configbundle_serverconfig_reconcile_timestamp_seconds{oob_ip="10.9.9.9",orb_id="orb-mi",server="metrics-int"}`,
		`configbundle_serverconfig_ssh_enabled{oob_ip="10.9.9.9",orb_id="orb-mi",server="metrics-int"} 1`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/metrics endpoint missing %q", want)
		}
	}
	// The removed info metric must not appear anywhere on the endpoint.
	if strings.Contains(page, "configbundle_serverconfig_status_idracsettings_info") {
		t.Errorf("removed info metric configbundle_serverconfig_status_idracsettings_info still present on /metrics")
	}
}
