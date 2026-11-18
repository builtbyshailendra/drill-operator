package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	drillsv1 "github.com/builtbyshailendra/drill-operator/api/v1alpha1"
)

func validDrill(ns string) *drillsv1.Drill {
	return &drillsv1.Drill{
		ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: ns},
		Spec: drillsv1.DrillSpec{
			Target: drillsv1.Target{Deployment: "api"},
			Action: drillsv1.Action{Type: drillsv1.DeletePods, Count: 1},
			Guard:  drillsv1.Guard{MinReadyPods: 2},
		},
	}
}

// The API server, not the operator, rejects these: the CRD's OpenAPI schema
// and CEL rules run inside kube-apiserver on every create and update.
func TestAPIServerRejectsBadDrills(t *testing.T) {
	ns := newNamespace(t, true)
	ctx := context.Background()
	cases := map[string]struct {
		mutate func(*drillsv1.Drill)
		want   string
	}{
		"check more often than you observe": {func(d *drillsv1.Drill) {
			d.Spec.ObserveFor = &metav1.Duration{Duration: 10 * time.Second}
			d.Spec.CheckEvery = &metav1.Duration{Duration: time.Minute}
		}, "checkEvery must not be longer than observeFor"},
		"observe for two hours": {func(d *drillsv1.Drill) {
			d.Spec.ObserveFor = &metav1.Duration{Duration: 2 * time.Hour}
		}, "observeFor can be at most 1h"},
		"check every 10ms": {func(d *drillsv1.Drill) {
			d.Spec.CheckEvery = &metav1.Duration{Duration: 10 * time.Millisecond}
		}, "at least 100ms"},
		"count zero":     {func(d *drillsv1.Drill) { d.Spec.Action.Count = 0 }, "spec.action.count"},
		"count eleven":   {func(d *drillsv1.Drill) { d.Spec.Action.Count = 11 }, "spec.action.count"},
		"unknown action": {func(d *drillsv1.Drill) { d.Spec.Action.Type = "DeleteNamespace" }, "spec.action.type"},
		"negative guard": {func(d *drillsv1.Drill) { d.Spec.Guard.MinReadyPods = -1 }, "spec.guard.minReadyPods"},
		"no target":      {func(d *drillsv1.Drill) { d.Spec.Target.Deployment = "" }, "spec.target.deployment"},
		"prometheus over ftp": {func(d *drillsv1.Drill) {
			d.Spec.Guard.Prometheus = &drillsv1.PrometheusGuard{URL: "ftp://x", Query: "up", MaxValue: "1"}
		}, "spec.guard.prometheus.url"},
		"limit that isn't a number": {func(d *drillsv1.Drill) {
			d.Spec.Guard.Prometheus = &drillsv1.PrometheusGuard{URL: "http://p:9090", Query: "up", MaxValue: "lots"}
		}, "spec.guard.prometheus.maxValue"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			d := validDrill(ns)
			d.GenerateName, d.Name = "bad-", ""
			tc.mutate(d)
			err := k8s.Create(ctx, d)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestDefaultsAndImmutableSpec(t *testing.T) {
	ns := newNamespace(t, false) // not opted in: no operator in this test anyway
	ctx := context.Background()
	d := validDrill(ns)
	if err := k8s.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	if d.Spec.ObserveFor == nil || d.Spec.ObserveFor.Duration != time.Minute ||
		d.Spec.CheckEvery == nil || d.Spec.CheckEvery.Duration != 5*time.Second {
		t.Fatalf("defaults not applied: observeFor=%v checkEvery=%v", d.Spec.ObserveFor, d.Spec.CheckEvery)
	}
	d.Spec.Action.Count = 2
	err := k8s.Update(ctx, d)
	if err == nil || !strings.Contains(err.Error(), "can't be changed once created") {
		t.Fatalf("spec update: err = %v", err)
	}
}
