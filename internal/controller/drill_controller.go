// Package controller reconciles Drills.
//
// A Drill moves through phases, and every phase change is written to the
// Drill's status before the next thing happens:
//
//	Pending ──checks──► Injecting ──action──► Observing ──window over──► Succeeded
//	   │                 (intent saved)          │
//	   └─► Rejected (unsafe, not allowed)        └─guard trips──► Aborted
//
// A controller can be stopped and restarted at any moment, and a reconcile can
// run twice for the same change. So the operator never decides the same thing
// twice: it records which pods it will delete (or the replica count it will
// restore) first, and a retry repeats that recorded intent.
package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	drillsv1 "github.com/builtbyshailendra/drill-operator/api/v1alpha1"
)

const (
	// AllowLabel opts a namespace in to drills. Without it the operator
	// refuses to touch anything there.
	AllowLabel = "drills.builtbyshailendra.dev/allowed"
	// Finalizer holds a running Drill's deletion until anything it changed
	// has been put back.
	Finalizer = "drills.builtbyshailendra.dev/restore"

	condProgressing = "Progressing"
	condGuardHeld   = "GuardHeld"
)

// EventRecorder is the part of the events.k8s.io/v1 recorder the operator uses.
type EventRecorder interface {
	Eventf(regarding runtime.Object, related runtime.Object, eventtype, reason, action, note string, args ...interface{})
}

// Reconciler drives Drills. Guard reads Prometheus (or anything else).
type Reconciler struct {
	client.Client
	Recorder EventRecorder
	Guard    GuardReader
	Now      func() time.Time
}

// +kubebuilder:rbac:groups=drills.builtbyshailendra.dev,resources=drills,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=drills.builtbyshailendra.dev,resources=drills/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=drills.builtbyshailendra.dev,resources=drills/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// SetupWithManager registers the controller. extra predicates narrow which
// Drills it sees (the tests use one to keep some Drills for direct calls).
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager, extra ...predicate.Predicate) error {
	if r.Now == nil {
		r.Now = time.Now
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&drillsv1.Drill{}, builder.WithPredicates(extra...)).
		Named("drill").
		Complete(r)
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.Now == nil {
		r.Now = time.Now
	}
	var d drillsv1.Drill
	if err := r.Get(ctx, req.NamespacedName, &d); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !d.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, &d)
	}
	switch d.Status.Phase {
	case "", drillsv1.Pending:
		return r.start(ctx, &d)
	case drillsv1.Injecting:
		return r.inject(ctx, &d)
	case drillsv1.Observing:
		return r.observe(ctx, &d)
	default: // finished
		return ctrl.Result{}, r.dropFinalizer(ctx, &d)
	}
}

// start checks that the drill is allowed and safe, then records its intent.
func (r *Reconciler) start(ctx context.Context, d *drillsv1.Drill) (ctrl.Result, error) {
	var ns corev1.Namespace
	if err := r.Get(ctx, types.NamespacedName{Name: d.Namespace}, &ns); err != nil {
		return ctrl.Result{}, err
	}
	if ns.Labels[AllowLabel] != "true" {
		return r.reject(ctx, d, "NamespaceNotAllowed", "namespace %s is not labelled %s=true", d.Namespace, AllowLabel)
	}
	dep, err := r.deployment(ctx, d)
	if apierrors.IsNotFound(err) {
		return r.reject(ctx, d, "TargetNotFound", "deployment %s not found", d.Spec.Target.Deployment)
	} else if err != nil {
		return ctrl.Result{}, err
	}
	pods, err := r.readyPods(ctx, dep)
	if err != nil {
		return ctrl.Result{}, err
	}
	ready, guard := int32(len(pods)), d.Spec.Guard

	// Steady state first: a drill on a service that's already unwell proves
	// nothing and makes things worse.
	if ready < guard.MinReadyPods {
		return r.reject(ctx, d, "SteadyStateNotMet", "only %d pods are ready, below the guard's %d", ready, guard.MinReadyPods)
	}
	if guard.Prometheus != nil {
		v, err := r.Guard.Read(ctx, guard.Prometheus)
		if err != nil {
			return r.reject(ctx, d, "GuardUnavailable", "can't check the guard before starting: %v", err)
		}
		if over, err := above(v, guard.Prometheus.MaxValue); err != nil || over {
			return r.reject(ctx, d, "SteadyStateNotMet", "%s is %g, above %s already", guard.Prometheus.Query, v, guard.Prometheus.MaxValue)
		}
	}

	// Blast radius: the action alone must not break the guard.
	count := d.Spec.Action.Count
	if ready-count < guard.MinReadyPods {
		return r.reject(ctx, d, "BlastRadius", "removing %d of %d ready pods would leave %d, below the guard's %d",
			count, ready, ready-count, guard.MinReadyPods)
	}
	replicas := int32(1)
	if dep.Spec.Replicas != nil {
		replicas = *dep.Spec.Replicas
	}
	if d.Spec.Action.Type == drillsv1.ScaleDown && count > replicas {
		return r.reject(ctx, d, "BlastRadius", "can't remove %d replicas from %d", count, replicas)
	}

	// Record intent before acting. From here on the finalizer guarantees a
	// ScaleDown is undone even if someone deletes the Drill mid-run.
	if controllerutil.AddFinalizer(d, Finalizer) {
		if err := r.Update(ctx, d); err != nil {
			return ctrl.Result{}, err
		}
	}
	switch d.Spec.Action.Type {
	case drillsv1.DeletePods:
		sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
		for _, p := range pods[:count] {
			d.Status.Victims = append(d.Status.Victims, drillsv1.Victim{Name: p.Name, UID: string(p.UID)})
		}
	case drillsv1.ScaleDown:
		d.Status.OriginalReplicas = &replicas
	}
	d.Status.Phase = drillsv1.Injecting
	d.Status.Message = fmt.Sprintf("%s %d on deployment %s", d.Spec.Action.Type, count, dep.Name)
	meta.SetStatusCondition(&d.Status.Conditions, metav1.Condition{Type: condProgressing, Status: metav1.ConditionTrue,
		Reason: "Injecting", Message: d.Status.Message, ObservedGeneration: d.Generation})
	if err := r.Status().Update(ctx, d); err != nil {
		return ctrl.Result{}, err
	}
	return r.inject(ctx, d)
}

// inject applies the recorded intent. Safe to run more than once.
func (r *Reconciler) inject(ctx context.Context, d *drillsv1.Drill) (ctrl.Result, error) {
	switch d.Spec.Action.Type {
	case drillsv1.DeletePods:
		for _, v := range d.Status.Victims {
			uid := types.UID(v.UID)
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: v.Name, Namespace: d.Namespace}}
			err := r.Delete(ctx, pod, client.Preconditions{UID: &uid})
			if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
				return ctrl.Result{}, err // a conflict means a different pod now has that name: leave it
			}
		}
	case drillsv1.ScaleDown:
		dep, err := r.deployment(ctx, d)
		if err != nil {
			return r.fail(ctx, d, "TargetNotFound", "deployment vanished before the action: %v", err)
		}
		want := *d.Status.OriginalReplicas - d.Spec.Action.Count // an absolute value: repeating it is harmless
		dep.Spec.Replicas = &want
		if err := r.Update(ctx, dep); err != nil {
			return ctrl.Result{}, err
		}
	}
	now := metav1.NewTime(r.Now())
	d.Status.Phase, d.Status.ActionAt = drillsv1.Observing, &now
	meta.SetStatusCondition(&d.Status.Conditions, metav1.Condition{Type: condProgressing, Status: metav1.ConditionTrue,
		Reason: "Observing", Message: d.Status.Message + "; watching the guard", ObservedGeneration: d.Generation})
	if err := r.Status().Update(ctx, d); err != nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Eventf(d, nil, corev1.EventTypeNormal, "ActionApplied", "Inject", "%s", d.Status.Message)
	return ctrl.Result{RequeueAfter: d.Spec.CheckEvery.Duration}, nil
}

// observe checks the guard, then waits, aborts, or finishes.
func (r *Reconciler) observe(ctx context.Context, d *drillsv1.Drill) (ctrl.Result, error) {
	breach, err := r.check(ctx, d)
	if err != nil {
		return ctrl.Result{}, err
	}
	if breach != "" {
		if err := r.restore(ctx, d); err != nil {
			return ctrl.Result{}, err
		}
		meta.SetStatusCondition(&d.Status.Conditions, metav1.Condition{Type: condGuardHeld, Status: metav1.ConditionFalse,
			Reason: "GuardTripped", Message: breach, ObservedGeneration: d.Generation})
		r.Recorder.Eventf(d, nil, corev1.EventTypeWarning, "GuardTripped", "Abort", "%s", breach)
		return ctrl.Result{}, r.finish(ctx, d, drillsv1.Aborted, "GuardTripped", "stopped early: "+breach)
	}
	elapsed := r.Now().Sub(d.Status.ActionAt.Time)
	if elapsed >= d.Spec.ObserveFor.Duration {
		if err := r.restore(ctx, d); err != nil {
			return ctrl.Result{}, err
		}
		meta.SetStatusCondition(&d.Status.Conditions, metav1.Condition{Type: condGuardHeld, Status: metav1.ConditionTrue,
			Reason: "GuardHeld", Message: fmt.Sprintf("the guard held for %s", d.Spec.ObserveFor.Duration), ObservedGeneration: d.Generation})
		r.Recorder.Eventf(d, nil, corev1.EventTypeNormal, "DrillPassed", "Complete", "the guard held for %s", d.Spec.ObserveFor.Duration)
		return ctrl.Result{}, r.finish(ctx, d, drillsv1.Succeeded, "GuardHeld", "the guard held for the whole window")
	}
	if err := r.Status().Update(ctx, d); err != nil { // keeps lowest/highest current
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: min(d.Spec.CheckEvery.Duration, d.Spec.ObserveFor.Duration-elapsed)}, nil
}

// check measures the guard and returns why it tripped, or "".
func (r *Reconciler) check(ctx context.Context, d *drillsv1.Drill) (string, error) {
	dep, err := r.deployment(ctx, d)
	if apierrors.IsNotFound(err) {
		return "the target deployment was deleted", nil
	} else if err != nil {
		return "", err
	}
	pods, err := r.readyPods(ctx, dep)
	if err != nil {
		return "", err
	}
	ready := int32(len(pods))
	if d.Status.LowestReadyPods == nil || ready < *d.Status.LowestReadyPods {
		d.Status.LowestReadyPods = &ready
	}
	if ready < d.Spec.Guard.MinReadyPods {
		return fmt.Sprintf("%d ready pods, below the guard's %d", ready, d.Spec.Guard.MinReadyPods), nil
	}
	if pg := d.Spec.Guard.Prometheus; pg != nil {
		v, err := r.Guard.Read(ctx, pg)
		if err != nil {
			// A guard you can't read is a guard that isn't protecting you.
			return fmt.Sprintf("can't read the guard: %v", err), nil
		}
		if h, perr := parseValue(d.Status.HighestValue); perr != nil || v > h {
			d.Status.HighestValue = formatValue(v)
		}
		if over, err := above(v, pg.MaxValue); err != nil || over {
			return fmt.Sprintf("%s is %g, above %s", pg.Query, v, pg.MaxValue), nil
		}
	}
	return "", nil
}

// restore undoes a ScaleDown. Idempotent: it sets an absolute replica count
// and records that it did.
func (r *Reconciler) restore(ctx context.Context, d *drillsv1.Drill) error {
	if d.Spec.Action.Type != drillsv1.ScaleDown || d.Status.Restored || d.Status.OriginalReplicas == nil {
		return nil
	}
	dep, err := r.deployment(ctx, d)
	if apierrors.IsNotFound(err) {
		d.Status.Restored = true // nothing left to put back
		return nil
	} else if err != nil {
		return err
	}
	dep.Spec.Replicas = d.Status.OriginalReplicas
	if err := r.Update(ctx, dep); err != nil {
		return err
	}
	d.Status.Restored = true
	r.Recorder.Eventf(d, dep, corev1.EventTypeNormal, "Restored", "Restore", "replicas back to %d", *d.Status.OriginalReplicas)
	return nil
}

func (r *Reconciler) finish(ctx context.Context, d *drillsv1.Drill, phase drillsv1.Phase, reason, msg string) error {
	now := metav1.NewTime(r.Now())
	d.Status.Phase, d.Status.Message, d.Status.FinishedAt = phase, msg, &now
	meta.SetStatusCondition(&d.Status.Conditions, metav1.Condition{Type: condProgressing, Status: metav1.ConditionFalse,
		Reason: reason, Message: msg, ObservedGeneration: d.Generation})
	if err := r.Status().Update(ctx, d); err != nil {
		return err
	}
	drillsFinished.WithLabelValues(string(phase)).Inc()
	return r.dropFinalizer(ctx, d)
}

func (r *Reconciler) reject(ctx context.Context, d *drillsv1.Drill, reason, format string, args ...any) (ctrl.Result, error) {
	msg := fmt.Sprintf(format, args...)
	r.Recorder.Eventf(d, nil, corev1.EventTypeWarning, reason, "Reject", "%s", msg)
	return ctrl.Result{}, r.finish(ctx, d, drillsv1.Rejected, reason, msg)
}

func (r *Reconciler) fail(ctx context.Context, d *drillsv1.Drill, reason, format string, args ...any) (ctrl.Result, error) {
	msg := fmt.Sprintf(format, args...)
	r.Recorder.Eventf(d, nil, corev1.EventTypeWarning, reason, "Fail", "%s", msg)
	return ctrl.Result{}, r.finish(ctx, d, drillsv1.Failed, reason, msg)
}

// finalize runs when a Drill is deleted: put things back, then let it go.
func (r *Reconciler) finalize(ctx context.Context, d *drillsv1.Drill) error {
	if !controllerutil.ContainsFinalizer(d, Finalizer) {
		return nil
	}
	if !d.Status.Phase.Finished() {
		if err := r.restore(ctx, d); err != nil {
			return err
		}
		r.Recorder.Eventf(d, nil, corev1.EventTypeNormal, "Cancelled", "Cancel", "the drill was deleted while %s", d.Status.Phase)
	}
	return r.dropFinalizer(ctx, d)
}

func (r *Reconciler) dropFinalizer(ctx context.Context, d *drillsv1.Drill) error {
	if controllerutil.RemoveFinalizer(d, Finalizer) {
		return r.Update(ctx, d)
	}
	return nil
}

func (r *Reconciler) deployment(ctx context.Context, d *drillsv1.Drill) (*appsv1.Deployment, error) {
	var dep appsv1.Deployment
	err := r.Get(ctx, types.NamespacedName{Namespace: d.Namespace, Name: d.Spec.Target.Deployment}, &dep)
	return &dep, err
}

// readyPods lists the Deployment's pods that are Ready and not on their way out.
func (r *Reconciler) readyPods(ctx context.Context, dep *appsv1.Deployment) ([]corev1.Pod, error) {
	if dep.Spec.Selector == nil {
		return nil, errors.New("deployment has no selector")
	}
	sel, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return nil, err
	}
	var list corev1.PodList
	if err := r.List(ctx, &list, client.InNamespace(dep.Namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, err
	}
	var ready []corev1.Pod
	for _, p := range list.Items {
		if p.DeletionTimestamp != nil {
			continue
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				ready = append(ready, p)
			}
		}
	}
	return ready, nil
}
