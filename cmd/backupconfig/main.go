// backupconfig-controller — watches BackupConfig CRs and reconciles Velero
// Schedule CRDs + an etcd-backup CronJob in the same cluster. Mirror of
// serverconfig-controller for the backup domain.
//
// Flag surface mirrors cb-controller's kubebuilder-scaffolded shape:
// operator-standard settings are FLAGS; domain config (VELERO_*, ETCD_*,
// BACKUP_OBSERVE_INTERVAL) stays in envconfig.
package main

import (
	"crypto/tls"
	"flag"
	"os"
	"time"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/kelseyhightower/envconfig"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	armadav1 "github.com/armada/configbundle/api/v1"
	"github.com/armada/configbundle/internal/backupconfig"
)

// Config holds domain configuration. Operator-standard settings (bind
// addresses, leader-elect, TLS) live as flags — see main() below.
type Config struct {
	// VeleroNamespace is where the controller writes Velero Schedule CRDs. The
	// Velero install conventionally lives in `velero` on the management cluster.
	VeleroNamespace string `envconfig:"VELERO_NAMESPACE" default:"velero"`

	// EtcdBackupNamespace is where the controller writes the etcd-backup CronJob.
	// `kube-system` is the conventional home for control-plane infrastructure.
	EtcdBackupNamespace string `envconfig:"ETCD_BACKUP_NAMESPACE" default:"kube-system"`

	// EtcdctlImage is the container image that runs `etcdctl snapshot save`
	// in the CronJob's initContainer. Default pins the same version the
	// existing hand-crafted etcd-backup on colo-dev-main uses.
	EtcdctlImage string `envconfig:"ETCD_BACKUP_ETCDCTL_IMAGE" default:"armadaeksatest.azurecr.io/etcdctl:3.5.15"`

	// UploadImage is the container image that uploads the snapshot to Azure
	// Blob storage. Default pins the same azure-cli version the existing
	// etcd-backup uses.
	UploadImage string `envconfig:"ETCD_BACKUP_UPLOAD_IMAGE" default:"armadaeksatest.azurecr.io/azure-cli:2.67.0"`

	// CredentialSecret is the K8s Secret name (in EtcdBackupNamespace)
	// holding Azure service-principal credentials. Data keys required:
	// client-id, client-secret, tenant-id.
	CredentialSecret string `envconfig:"ETCD_BACKUP_CRED_SECRET" default:"az-storage-creds"`

	// ObserveInterval is the single observe switch — the cadence at which bc
	// re-observes non-watchable state, chiefly the etcd backup store (blob) for
	// snapshot presence/freshness. Set it (>0) to turn artifact observation ON;
	// unset (0, the default, `go run`-safe for local dev) leaves bc purely
	// watch-driven: it still reconciles the CronJob/Schedule (config drift is
	// caught instantly by the Owns watches), it just never polls the blob.
	// Mirrors serverconfig's IDRAC_OBSERVE_INTERVAL — one interval, no separate
	// on/off flag. Production sets a slow value (30m–1h) via overlay, alongside
	// the AZURE_* creds mount.
	ObserveInterval time.Duration `envconfig:"BACKUP_OBSERVE_INTERVAL" default:"0s"`

	// S3SyncNamespace is where the controller writes the S3 Sync CronJob.
	S3SyncNamespace string `envconfig:"S3SYNC_NAMESPACE" default:"default"`

	// S3SyncRcloneImage is the container image that runs rclone sync.
	S3SyncRcloneImage string `envconfig:"S3SYNC_RCLONE_IMAGE" default:"armadaeksatest.azurecr.io/rclone:1.68.2"`

	// S3SyncCredSecret is the K8s Secret name (in S3SyncNamespace) holding
	// S3 credentials. Data keys required: source-access-key, source-secret-key,
	// dest-access-key, dest-secret-key.
	S3SyncCredSecret string `envconfig:"S3SYNC_CRED_SECRET" default:"s3-creds"`

	// EtcdSnapshotStaleAfter is the staleness threshold for the BackupsFresh
	// condition — when the NEWEST snapshot is older than this, the condition
	// flips to False (SnapshotStale). It is a health alarm only and never
	// deletes anything (NOT retention). Only consulted when observation is on
	// (ObserveInterval>0).
	EtcdSnapshotStaleAfter time.Duration `envconfig:"ETCD_SNAPSHOT_STALE_AFTER" default:"26h"`
}

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(armadav1.AddToScheme(scheme))
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8096 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8094", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics server.")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Disable HTTP/2 unless explicitly enabled — protects against the HTTP/2
	// Stream Cancellation and Rapid Reset CVEs. Matches cb-controller.
	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, func(c *tls.Config) { c.NextProtos = []string{"http/1.1"} })
	}

	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}
	if secureMetrics {
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	var cfg Config
	if err := envconfig.Process("", &cfg); err != nil {
		setupLog.Error(err, "Failed to load config")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "bc.configbundle.armada.ai",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	setupLog.Info("backup target namespaces",
		"velero", cfg.VeleroNamespace,
		"etcd", cfg.EtcdBackupNamespace,
		"etcdctlImage", cfg.EtcdctlImage,
		"uploadImage", cfg.UploadImage,
		"credentialSecret", cfg.CredentialSecret,
		"s3sync", cfg.S3SyncNamespace,
		"s3syncRcloneImage", cfg.S3SyncRcloneImage,
		"s3syncCredSecret", cfg.S3SyncCredSecret)

	reconciler := &backupconfig.BackupConfigReconciler{
		Client:              mgr.GetClient(),
		Scheme:              mgr.GetScheme(),
		VeleroNamespace:     cfg.VeleroNamespace,
		EtcdBackupNamespace: cfg.EtcdBackupNamespace,
		EtcdctlImage:        cfg.EtcdctlImage,
		UploadImage:         cfg.UploadImage,
		CredentialSecret:    cfg.CredentialSecret,
		ObserveInterval:     cfg.ObserveInterval,
		S3SyncNamespace:     cfg.S3SyncNamespace,
		S3SyncRcloneImage:   cfg.S3SyncRcloneImage,
		S3SyncCredSecret:    cfg.S3SyncCredSecret,
		Recorder:            mgr.GetEventRecorderFor("backupconfig-controller"),
	}

	// etcd artifact-observation follows the observe interval — setting it (>0)
	// turns blob observation on. It degrades gracefully: if the storage
	// credential is missing/invalid we log and run WITHOUT it rather than crash;
	// bc still manages the CronJob and reconciles config via watches.
	if cfg.ObserveInterval > 0 {
		store, err := backupconfig.NewAzureEtcdStore()
		if err != nil {
			setupLog.Error(err, "artifact-observation requested (BACKUP_OBSERVE_INTERVAL set) but storage credential unavailable; running WITHOUT it")
		} else {
			reconciler.EtcdStore = store
			reconciler.EtcdSnapshotStaleAfter = cfg.EtcdSnapshotStaleAfter
			setupLog.Info("etcd artifact-observation enabled", "interval", cfg.ObserveInterval, "staleAfter", cfg.EtcdSnapshotStaleAfter)
		}
	} else {
		setupLog.Info("etcd artifact-observation disabled (watch-only); set BACKUP_OBSERVE_INTERVAL>0 with AZURE_* creds to enable")
	}

	if err := reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "backupconfig")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager", "probe", probeAddr, "metrics", metricsAddr, "secureMetrics", secureMetrics, "leaderElection", enableLeaderElection)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
