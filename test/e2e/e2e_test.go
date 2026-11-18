// Package e2e runs the compiled operator as its own process against envtest,
// logged in as a user that has only the generated ClusterRole. If an RBAC
// marker is missing, this is where it shows up.
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	drillsv1 "github.com/builtbyshailendra/drill-operator/api/v1alpha1"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestOperatorBinaryWithOnlyItsOwnRBAC(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS is not set; run scripts/test.sh")
	}
	ctx := context.Background()
	root := filepath.Join("..", "..")

	bin := filepath.Join(t.TempDir(), "manager.exe")
	build := exec.Command("go", "build", "-o", bin, "./cmd/manager")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join(root, "config", "crd")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = env.Stop() }()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = drillsv1.AddToScheme(scheme)
	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	// The generated ClusterRole, bound to a group, and a user in that group.
	raw, err := os.ReadFile(filepath.Join(root, "config", "rbac", "role.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var role rbacv1.ClusterRole
	if err := yaml.Unmarshal(bytes.TrimPrefix(raw, []byte("---\n")), &role); err != nil {
		t.Fatal(err)
	}
	must(t, admin.Create(ctx, &role))
	must(t, admin.Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "drill-operator"},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: "Group", Name: "drill-operators", APIGroup: "rbac.authorization.k8s.io"}},
	}))
	user, err := env.AddUser(envtest.User{Name: "drill-operator", Groups: []string{"drill-operators"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	kubeconfig, err := user.KubeConfig()
	if err != nil {
		t.Fatal(err)
	}
	kcPath := filepath.Join(t.TempDir(), "kubeconfig")
	must(t, os.WriteFile(kcPath, kubeconfig, 0o600))

	must(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "drill-operator-system"}}))
	must(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "payments",
		Labels: map[string]string{"drills.builtbyshailendra.dev/allowed": "true"}}}))

	metricsPort, probePort := freePort(t), freePort(t)
	logs := &syncBuf{}
	op := exec.Command(bin, "--leader-elect", "--leader-election-namespace=drill-operator-system",
		fmt.Sprintf("--metrics-bind-address=127.0.0.1:%d", metricsPort),
		fmt.Sprintf("--health-probe-bind-address=127.0.0.1:%d", probePort))
	op.Env = append(os.Environ(), "KUBECONFIG="+kcPath)
	op.Stdout, op.Stderr = logs, logs
	must(t, op.Start())
	defer func() { _ = op.Process.Kill(); _, _ = op.Process.Wait() }()
	waitFor(t, 30*time.Second, "/readyz", func() bool {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/readyz", probePort))
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})

	// A Deployment with three ready pods, and a drill that removes one replica.
	replicas := int32(3)
	labels := map[string]string{"app": "api"}
	podSpec := corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "example.invalid/app:1"}}}
	must(t, admin.Create(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "payments"},
		Spec: appsv1.DeploymentSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: podSpec}}}))
	for i := range 3 {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("api-%d", i), Namespace: "payments", Labels: labels}, Spec: podSpec}
		must(t, admin.Create(ctx, pod))
		pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
		must(t, admin.Status().Update(ctx, pod))
	}
	must(t, admin.Create(ctx, &drillsv1.Drill{ObjectMeta: metav1.ObjectMeta{Name: "lose-one-replica", Namespace: "payments"},
		Spec: drillsv1.DrillSpec{Target: drillsv1.Target{Deployment: "api"},
			Action:     drillsv1.Action{Type: drillsv1.ScaleDown, Count: 1},
			Guard:      drillsv1.Guard{MinReadyPods: 1},
			ObserveFor: &metav1.Duration{Duration: 2 * time.Second}, CheckEvery: &metav1.Duration{Duration: 250 * time.Millisecond}}}))

	var d drillsv1.Drill
	waitFor(t, 30*time.Second, "the drill to succeed", func() bool {
		_ = admin.Get(ctx, types.NamespacedName{Namespace: "payments", Name: "lose-one-replica"}, &d)
		return d.Status.Phase == drillsv1.Succeeded || d.Status.Phase.Finished()
	})
	if d.Status.Phase != drillsv1.Succeeded {
		t.Fatalf("phase %s: %s\noperator log:\n%s", d.Status.Phase, d.Status.Message, logs.String())
	}
	var dep appsv1.Deployment
	must(t, admin.Get(ctx, types.NamespacedName{Namespace: "payments", Name: "api"}, &dep))
	if *dep.Spec.Replicas != 3 {
		t.Fatalf("replicas = %d after the drill, want 3", *dep.Spec.Replicas)
	}

	// What an operator would see with kubectl (the printer columns).
	adminUser, err := env.AddUser(envtest.User{Name: "admin", Groups: []string{"system:masters"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	kubectl, err := adminUser.Kubectl()
	if err != nil {
		t.Fatal(err)
	}
	stdout, _, err := kubectl.Run("get", "drills", "-n", "payments")
	if err != nil {
		t.Fatal(err)
	}
	table, _ := io.ReadAll(stdout)
	t.Logf("kubectl get drills -n payments\n%s", table)
	if !strings.Contains(string(table), "Succeeded") || !strings.Contains(string(table), "ScaleDown") {
		t.Fatalf("kubectl output:\n%s", table)
	}

	// Metrics, leader election, and no permission errors anywhere.
	var metricsText string
	waitFor(t, 10*time.Second, "the drills metric", func() bool {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", metricsPort))
		if err != nil {
			return false
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		metricsText = string(b)
		return strings.Contains(metricsText, `drill_operator_drills_finished_total{phase="Succeeded"} 1`)
	})
	var lease coordinationv1.Lease
	must(t, admin.Get(ctx, types.NamespacedName{Namespace: "drill-operator-system", Name: "drill-operator.builtbyshailendra.dev"}, &lease))
	time.Sleep(time.Second) // let any rejected event writes reach the log
	if strings.Contains(strings.ToLower(logs.String()), "forbidden") {
		t.Fatalf("the operator hit a permission error, so the ClusterRole is missing something:\n%s", logs.String())
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
