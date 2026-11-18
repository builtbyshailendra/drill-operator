package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A Drill is a failure drill against one Deployment: break something on
// purpose, watch a guard, and stop early if the guard says the service is
// suffering. It answers "can this service lose N pods (or replicas) for a
// while and stay healthy?" with evidence instead of hope.
//
// The rules marked XValidation are CEL expressions. The Kubernetes API server
// itself enforces them, so a bad Drill is rejected by kubectl apply, before
// the operator ever sees it.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=dr
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.target.deployment`
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=`.spec.action.type`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Drill struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="a drill can't be changed once created; delete it and create a new one"
	Spec   DrillSpec   `json:"spec"`
	Status DrillStatus `json:"status,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="duration(self.checkEvery) <= duration(self.observeFor)",message="checkEvery must not be longer than observeFor"
// +kubebuilder:validation:XValidation:rule="duration(self.observeFor) <= duration('1h')",message="observeFor can be at most 1h"
// +kubebuilder:validation:XValidation:rule="duration(self.checkEvery) >= duration('100ms')",message="checkEvery must be at least 100ms"
type DrillSpec struct {
	Target Target `json:"target"`
	Action Action `json:"action"`
	Guard  Guard  `json:"guard"`

	// How long to watch the guard after the action. A ScaleDown is undone
	// at the end of this window. A pointer, not a value: metav1.Duration is a
	// struct, so `omitempty` never drops it and a Go client would always send
	// "0s", which skips the default (the first test run caught exactly that).
	// +kubebuilder:default="60s"
	// +optional
	ObserveFor *metav1.Duration `json:"observeFor,omitempty"`

	// How often to check the guard while observing.
	// +kubebuilder:default="5s"
	// +optional
	CheckEvery *metav1.Duration `json:"checkEvery,omitempty"`
}

type Target struct {
	// Name of a Deployment in the drill's namespace. Its pods are chosen
	// with the Deployment's own selector.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Deployment string `json:"deployment"`
}

// +kubebuilder:validation:Enum=DeletePods;ScaleDown
type ActionType string

const (
	// DeletePods deletes ready pods; the Deployment replaces them.
	DeletePods ActionType = "DeletePods"
	// ScaleDown removes replicas for the observation window, then puts them back.
	ScaleDown ActionType = "ScaleDown"
)

type Action struct {
	Type ActionType `json:"type"`

	// Pods to delete, or replicas to remove.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	Count int32 `json:"count"`
}

type Guard struct {
	// The drill aborts if fewer target pods than this are Ready. It also
	// refuses to start if the action alone would take ready pods below it.
	// +kubebuilder:validation:Minimum=0
	MinReadyPods int32 `json:"minReadyPods"`

	// Optional: abort if a Prometheus query goes above a limit.
	// +optional
	Prometheus *PrometheusGuard `json:"prometheus,omitempty"`
}

type PrometheusGuard struct {
	// Base URL of the Prometheus HTTP API, e.g. http://prometheus.monitoring:9090
	// +kubebuilder:validation:Pattern=`^https?://[^\s]+$`
	URL string `json:"url"`

	// An instant query that returns one number, e.g. an error ratio.
	// +kubebuilder:validation:MinLength=1
	Query string `json:"query"`

	// The guard trips when the query's value is above this, e.g. "0.05".
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	MaxValue string `json:"maxValue"`
}

// +kubebuilder:validation:Enum=Pending;Injecting;Observing;Succeeded;Aborted;Rejected;Failed
type Phase string

const (
	Pending   Phase = "Pending"   // created, not started
	Injecting Phase = "Injecting" // intent recorded; the action is being applied
	Observing Phase = "Observing" // action applied; watching the guard
	Succeeded Phase = "Succeeded" // the guard held for the whole window
	Aborted   Phase = "Aborted"   // the guard tripped; the drill stopped early
	Rejected  Phase = "Rejected"  // never started: unsafe or not allowed
	Failed    Phase = "Failed"    // the operator couldn't carry it out
)

// Finished reports whether a phase is terminal.
func (p Phase) Finished() bool {
	return p == Succeeded || p == Aborted || p == Rejected || p == Failed
}

type DrillStatus struct {
	// +optional
	Phase Phase `json:"phase,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`

	// When the action was applied.
	// +optional
	ActionAt *metav1.Time `json:"actionAt,omitempty"`
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`

	// The pods chosen for deletion. Written before any pod is deleted, so a
	// retried reconcile deletes these same pods and never extra ones.
	// +optional
	Victims []string `json:"victims,omitempty"`

	// The Deployment's replicas before a ScaleDown, to put back.
	// +optional
	OriginalReplicas *int32 `json:"originalReplicas,omitempty"`
	// Whether a ScaleDown has been undone.
	// +optional
	Restored bool `json:"restored,omitempty"`

	// The lowest number of Ready target pods seen while observing.
	// +optional
	LowestReadyPods *int32 `json:"lowestReadyPods,omitempty"`
	// The highest Prometheus guard value seen, as text.
	// +optional
	HighestValue string `json:"highestValue,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true

// DrillList is a list of Drills.
type DrillList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Drill `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Drill{}, &DrillList{})
}
