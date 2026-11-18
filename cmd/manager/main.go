// Command manager runs the drill operator.
package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	drillsv1 "github.com/builtbyshailendra/drill-operator/api/v1alpha1"
	"github.com/builtbyshailendra/drill-operator/internal/controller"
)

// Leader election uses a Lease, so only one replica acts at a time. It also
// announces "became leader" with the old core/v1 Events API, separate from
// the events.k8s.io/v1 API the controller uses; the end-to-end test found
// that missing permission.
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func main() {
	var metricsAddr, probeAddr, leaseNS string
	var leaderElect bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "address for /metrics (0 turns it off)")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "address for /healthz and /readyz")
	flag.BoolVar(&leaderElect, "leader-elect", false, "run leader election, for more than one replica")
	flag.StringVar(&leaseNS, "leader-election-namespace", "drill-operator-system", "namespace for the leader-election Lease")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("setup")

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = drillsv1.AddToScheme(scheme)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          leaderElect,
		LeaderElectionID:        "drill-operator.builtbyshailendra.dev",
		LeaderElectionNamespace: leaseNS,
	})
	if err != nil {
		log.Error(err, "creating the manager")
		os.Exit(1)
	}
	r := &controller.Reconciler{Client: mgr.GetClient(), Recorder: mgr.GetEventRecorder("drill-operator"),
		Guard: controller.PrometheusReader{}}
	if err := r.SetupWithManager(mgr); err != nil {
		log.Error(err, "setting up the controller")
		os.Exit(1)
	}
	_ = mgr.AddHealthzCheck("healthz", healthz.Ping)
	_ = mgr.AddReadyzCheck("readyz", healthz.Ping)

	log.Info("starting", "leaderElection", leaderElect)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager stopped")
		os.Exit(1)
	}
}
