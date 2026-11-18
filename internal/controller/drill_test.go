package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	drillsv1 "github.com/builtbyshailendra/drill-operator/api/v1alpha1"
)

var ctx = context.Background()

// --- fixtures ---------------------------------------------------------------

func makeDeployment(t *testing.T, ns, name string, replicas int32) {
	t.Helper()
	labels := map[string]string{"app": name}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "example.invalid/app:1"}}},
			},
		},
	}
	if err := k8s.Create(ctx, dep); err != nil {
		t.Fatal(err)
	}
}

// makePods creates pods app-0..app-(n-1) for the Deployment and marks the
// first `ready` of them Ready. envtest has no kubelet, so the test plays it.
func makePods(t *testing.T, ns, app string, n, ready int) {
	t.Helper()
	for i := range n {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%d", app, i), Namespace: ns, Labels: map[string]string{"app": app}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "example.invalid/app:1"}}},
		}
		if err := k8s.Create(ctx, pod); err != nil {
			t.Fatal(err)
		}
		setReady(t, ns, pod.Name, i < ready)
	}
}

func setReady(t *testing.T, ns, name string, ready bool) {
	t.Helper()
	var pod corev1.Pod
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &pod); err != nil {
		t.Fatal(err)
	}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
	if err := k8s.Status().Update(ctx, &pod); err != nil {
		t.Fatal(err)
	}
}

func podNames(t *testing.T, ns string) []string {
	t.Helper()
	var list corev1.PodList
	if err := k8s.List(ctx, &list, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range list.Items {
		names = append(names, p.Name)
	}
	slices.Sort(names)
	return names
}

func replicasOf(t *testing.T, ns, name string) int32 {
	t.Helper()
	var dep appsv1.Deployment
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &dep); err != nil {
		t.Fatal(err)
	}
	return *dep.Spec.Replicas
}

func newDrill(ns string, action drillsv1.ActionType, count, minReady int32, observe time.Duration) *drillsv1.Drill {
	return &drillsv1.Drill{
		ObjectMeta: metav1.ObjectMeta{Name: "drill", Namespace: ns},
		Spec: drillsv1.DrillSpec{
			Target:     drillsv1.Target{Deployment: "api"},
			Action:     drillsv1.Action{Type: action, Count: count},
			Guard:      drillsv1.Guard{MinReadyPods: minReady},
			ObserveFor: &metav1.Duration{Duration: observe},
			CheckEvery: &metav1.Duration{Duration: 200 * time.Millisecond},
		},
	}
}

func create(t *testing.T, d *drillsv1.Drill) {
	t.Helper()
	if err := k8s.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
}

func getDrill(t *testing.T, ns string) *drillsv1.Drill {
	t.Helper()
	var d drillsv1.Drill
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "drill"}, &d); err != nil {
		t.Fatal(err)
	}
	return &d
}

func waitPhase(t *testing.T, ns string, phase drillsv1.Phase, timeout time.Duration) *drillsv1.Drill {
	t.Helper()
	var d *drillsv1.Drill
	eventually(t, timeout, "phase "+string(phase), func() bool {
		d = getDrill(t, ns)
		return d.Status.Phase == phase
	})
	return d
}

func reasonOf(d *drillsv1.Drill) string {
	if c := meta.FindStatusCondition(d.Status.Conditions, condProgressing); c != nil {
		return c.Reason
	}
	return ""
}

// fakeProm is a Prometheus HTTP API that answers every query with one value
// the test can change.
type fakeProm struct {
	URL   string
	value atomic.Value
}

func newFakeProm(t *testing.T, v string) *fakeProm {
	t.Helper()
	p := &fakeProm{}
	p.value.Store(v)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1763460000,"%s"]}]}}`, p.value.Load())
	}))
	t.Cleanup(srv.Close)
	p.URL = srv.URL
	return p
}

// --- tests ------------------------------------------------------------------

func TestDeletePodsDrillPasses(t *testing.T) {
	ns := newNamespace(t, true)
	makeDeployment(t, ns, "api", 4)
	makePods(t, ns, "api", 4, 4)
	create(t, newDrill(ns, drillsv1.DeletePods, 1, 3, 2*time.Second))

	d := waitPhase(t, ns, drillsv1.Observing, 10*time.Second)
	if len(d.Status.Victims) != 1 || d.Status.Victims[0].Name != "api-0" {
		t.Fatalf("victims = %+v", d.Status.Victims)
	}
	eventually(t, 5*time.Second, "api-0 deleted", func() bool { return !slices.Contains(podNames(t, ns), "api-0") })

	d = waitPhase(t, ns, drillsv1.Succeeded, 10*time.Second)
	if *d.Status.LowestReadyPods != 3 || d.Status.FinishedAt == nil {
		t.Fatalf("status %+v", d.Status)
	}
	// Finishing is two writes (status, then metadata), so the finalizer goes
	// a moment after the phase changes. Asserting both at once was flaky.
	eventually(t, 5*time.Second, "the finalizer to be removed", func() bool { return len(getDrill(t, ns).Finalizers) == 0 })
	if c := meta.FindStatusCondition(d.Status.Conditions, condGuardHeld); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("GuardHeld = %+v", c)
	}
	eventually(t, 5*time.Second, "an ActionApplied event", func() bool {
		var evs eventsv1.EventList
		_ = k8s.List(ctx, &evs, client.InNamespace(ns))
		return slices.ContainsFunc(evs.Items, func(e eventsv1.Event) bool { return e.Reason == "ActionApplied" })
	})
}

func TestRefusals(t *testing.T) {
	cases := []struct {
		name      string
		optIn     bool
		pods      int
		ready     int
		count     int32
		minReady  int32
		reason    string
		promValue string // "" means no Prometheus guard; "down" means unreachable
	}{
		{"namespace not opted in", false, 4, 4, 1, 3, "NamespaceNotAllowed", ""},
		{"would take ready pods below the guard", true, 4, 4, 2, 3, "BlastRadius", ""},
		{"service already below the guard", true, 4, 2, 1, 3, "SteadyStateNotMet", ""},
		{"error rate already too high", true, 4, 4, 1, 3, "SteadyStateNotMet", "0.2"},
		{"can't read the guard", true, 4, 4, 1, 3, "GuardUnavailable", "down"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns := newNamespace(t, tc.optIn)
			makeDeployment(t, ns, "api", int32(tc.pods))
			makePods(t, ns, "api", tc.pods, tc.ready)
			d := newDrill(ns, drillsv1.DeletePods, tc.count, tc.minReady, 2*time.Second)
			switch tc.promValue {
			case "":
			case "down":
				dead := httptest.NewServer(http.NotFoundHandler())
				dead.Close()
				d.Spec.Guard.Prometheus = &drillsv1.PrometheusGuard{URL: dead.URL, Query: "error_ratio", MaxValue: "0.05"}
			default:
				d.Spec.Guard.Prometheus = &drillsv1.PrometheusGuard{URL: newFakeProm(t, tc.promValue).URL, Query: "error_ratio", MaxValue: "0.05"}
			}
			create(t, d)
			got := waitPhase(t, ns, drillsv1.Rejected, 10*time.Second)
			if reasonOf(got) != tc.reason {
				t.Fatalf("reason = %s (%s), want %s", reasonOf(got), got.Status.Message, tc.reason)
			}
			if n := len(podNames(t, ns)); n != tc.pods {
				t.Fatalf("a refused drill touched pods: %d left of %d", n, tc.pods)
			}
		})
	}
}

func TestGuardTripsWhenAnotherPodFails(t *testing.T) {
	ns := newNamespace(t, true)
	makeDeployment(t, ns, "api", 4)
	makePods(t, ns, "api", 4, 4)
	create(t, newDrill(ns, drillsv1.DeletePods, 1, 3, 30*time.Second))
	waitPhase(t, ns, drillsv1.Observing, 10*time.Second)

	setReady(t, ns, "api-2", false) // a second pod fails while one is already gone
	d := waitPhase(t, ns, drillsv1.Aborted, 10*time.Second)
	if *d.Status.LowestReadyPods != 2 || reasonOf(d) != "GuardTripped" {
		t.Fatalf("status %+v", d.Status)
	}
}

func TestScaleDownIsUndoneAfterTheWindow(t *testing.T) {
	ns := newNamespace(t, true)
	makeDeployment(t, ns, "api", 4)
	makePods(t, ns, "api", 4, 4)
	create(t, newDrill(ns, drillsv1.ScaleDown, 2, 1, 2*time.Second))

	waitPhase(t, ns, drillsv1.Observing, 10*time.Second)
	if r := replicasOf(t, ns, "api"); r != 2 {
		t.Fatalf("replicas while observing = %d, want 2", r)
	}
	d := waitPhase(t, ns, drillsv1.Succeeded, 10*time.Second)
	if r := replicasOf(t, ns, "api"); r != 4 || !d.Status.Restored {
		t.Fatalf("replicas after = %d restored=%v, want 4 and true", r, d.Status.Restored)
	}
}

func TestPrometheusGuardAbortsAndRestores(t *testing.T) {
	ns := newNamespace(t, true)
	makeDeployment(t, ns, "api", 4)
	makePods(t, ns, "api", 4, 4)
	prom := newFakeProm(t, "0.01")
	d := newDrill(ns, drillsv1.ScaleDown, 1, 2, 30*time.Second)
	d.Spec.Guard.Prometheus = &drillsv1.PrometheusGuard{URL: prom.URL, Query: "sum(rate(errors[1m])) / sum(rate(requests[1m]))", MaxValue: "0.05"}
	create(t, d)
	waitPhase(t, ns, drillsv1.Observing, 10*time.Second)
	if r := replicasOf(t, ns, "api"); r != 3 {
		t.Fatalf("replicas while observing = %d, want 3", r)
	}

	prom.value.Store("0.21") // errors climb with one replica fewer
	got := waitPhase(t, ns, drillsv1.Aborted, 10*time.Second)
	if r := replicasOf(t, ns, "api"); r != 4 {
		t.Fatalf("replicas after the abort = %d, want 4", r)
	}
	if got.Status.HighestValue != "0.21" {
		t.Fatalf("highest value = %q", got.Status.HighestValue)
	}
}

func TestDeletingARunningDrillPutsReplicasBack(t *testing.T) {
	ns := newNamespace(t, true)
	makeDeployment(t, ns, "api", 4)
	makePods(t, ns, "api", 4, 4)
	create(t, newDrill(ns, drillsv1.ScaleDown, 3, 1, 5*time.Minute))
	d := waitPhase(t, ns, drillsv1.Observing, 10*time.Second)
	if r := replicasOf(t, ns, "api"); r != 1 {
		t.Fatalf("replicas = %d, want 1", r)
	}

	if err := k8s.Delete(ctx, d); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "the drill to be gone", func() bool {
		return apierrors.IsNotFound(k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "drill"}, &drillsv1.Drill{}))
	})
	if r := replicasOf(t, ns, "api"); r != 4 {
		t.Fatalf("replicas after deleting the drill = %d, want 4 (the finalizer should restore them)", r)
	}
}

// A reconcile can run again after the intent was saved but before the status
// moved on (a crash, a conflict). It must repeat the recorded deletion, not
// pick new pods. Here a test-owned reconciler is called directly.
func TestRetriedInjectionDeletesOnlyTheRecordedPods(t *testing.T) {
	ns := newNamespace(t, true)
	makeDeployment(t, ns, "api", 4)
	makePods(t, ns, "api", 4, 4)
	r := &Reconciler{Client: k8s, Recorder: events.NewFakeRecorder(100), Guard: PrometheusReader{}}

	d := newDrill(ns, drillsv1.DeletePods, 1, 2, time.Minute)
	d.Labels = map[string]string{manualLabel: "true"}
	create(t, d)
	var victim corev1.Pod
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "api-2"}, &victim); err != nil {
		t.Fatal(err)
	}
	// As if an earlier reconcile chose api-2, saved that, then died.
	d.Status.Phase = drillsv1.Injecting
	d.Status.Victims = []drillsv1.Victim{{Name: "api-2", UID: string(victim.UID)}}
	if err := k8s.Status().Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "drill"}}
	for range 2 { // and the retry runs twice for good measure
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := podNames(t, ns), []string{"api-0", "api-1", "api-3"}; !slices.Equal(got, want) {
		t.Fatalf("pods = %v, want %v", got, want)
	}
}

// The recorded victim is gone and a new pod took its name. Deleting by name
// alone would kill the replacement; the UID precondition prevents that.
func TestReplacementPodWithTheSameNameIsSpared(t *testing.T) {
	ns := newNamespace(t, true)
	makeDeployment(t, ns, "api", 3)
	makePods(t, ns, "api", 3, 3)
	r := &Reconciler{Client: k8s, Recorder: events.NewFakeRecorder(100), Guard: PrometheusReader{}}

	d := newDrill(ns, drillsv1.DeletePods, 1, 1, time.Minute)
	d.Labels = map[string]string{manualLabel: "true"}
	create(t, d)
	var old corev1.Pod
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "api-1"}, &old); err != nil {
		t.Fatal(err)
	}
	d.Status.Phase = drillsv1.Injecting
	d.Status.Victims = []drillsv1.Victim{{Name: "api-1", UID: string(old.UID)}}
	if err := k8s.Status().Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Delete(ctx, &old); err != nil { // the first attempt got this far...
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "api-1 gone", func() bool { return !slices.Contains(podNames(t, ns), "api-1") })
	replacement := &corev1.Pod{ // ...and a new pod reused the name
		ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: ns, Labels: map[string]string{"app": "api"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "example.invalid/app:1"}}},
	}
	if err := k8s.Create(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "drill"}}); err != nil {
		t.Fatal(err)
	}
	var now corev1.Pod
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "api-1"}, &now); err != nil || now.UID != replacement.UID {
		t.Fatalf("the replacement pod was deleted (err=%v)", err)
	}
	if getDrill(t, ns).Status.Phase != drillsv1.Observing {
		t.Fatal("the drill should have moved on to observing")
	}
}
