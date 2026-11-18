package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	drillsv1 "github.com/builtbyshailendra/drill-operator/api/v1alpha1"
)

// One real kube-apiserver and etcd (envtest) for the whole package. There is
// no kubelet, scheduler or controller-manager: Deployments don't create pods
// and pods never run. Tests create pods themselves and set their Ready
// condition, which is exactly the control the drills need.
var (
	cfg    *rest.Config
	k8s    client.Client
	scheme = runtime.NewScheme()
)

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Println("KUBEBUILDER_ASSETS is not set; run scripts/test.sh, or:\n" +
			"  export KUBEBUILDER_ASSETS=$(go tool setup-envtest use 1.37.0 -p path)")
		os.Exit(1)
	}
	_ = clientgoscheme.AddToScheme(scheme)
	_ = drillsv1.AddToScheme(scheme)

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
	}
	var err error
	if cfg, err = env.Start(); err != nil {
		fmt.Println("starting envtest:", err)
		os.Exit(1)
	}
	if k8s, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		fmt.Println("client:", err)
		os.Exit(1)
	}

	// The operator under test, running in this process like it would in a pod.
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme,
		Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0"})
	if err != nil {
		fmt.Println("manager:", err)
		os.Exit(1)
	}
	r := &Reconciler{Client: mgr.GetClient(), Recorder: mgr.GetEventRecorder("drill-operator"), Guard: PrometheusReader{}}
	// Drills labelled manual are left alone, so a test can call Reconcile itself.
	skipManual := predicate.NewPredicateFuncs(func(o client.Object) bool { return o.GetLabels()[manualLabel] != "true" })
	if err := r.SetupWithManager(mgr, skipManual); err != nil {
		fmt.Println("controller:", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := mgr.Start(ctx); err != nil {
			fmt.Println("manager stopped:", err)
		}
	}()

	code := m.Run()
	cancel()
	_ = env.Stop()
	os.Exit(code)
}

const manualLabel = "test.drills/manual"

var nsCounter int

// newNamespace makes a fresh namespace, opted in to drills unless optIn is
// false, and deletes it when the test ends.
func newNamespace(t *testing.T, optIn bool) string {
	t.Helper()
	nsCounter++
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("t%d-%d", nsCounter, time.Now().UnixNano()%100000)}}
	if optIn {
		ns.Labels = map[string]string{AllowLabel: "true"}
	}
	if err := k8s.Create(context.Background(), ns); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), ns) })
	return ns.Name
}

// eventually polls cond until it's true or the timeout passes.
func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for: %s", timeout, what)
}
