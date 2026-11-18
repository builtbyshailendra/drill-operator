# drill-operator

A Kubernetes operator for failure drills. A `Drill` resource says: break this Deployment a little (delete pods, or remove replicas), watch a guard (ready pods, a Prometheus query), and stop early, putting things back, if the guard trips.

Status: built step by step, locally. Not published. Tested against a real kube-apiserver and etcd (envtest); never run on a real cluster.

## Build log

### Step 1: the Drill API

`api/v1alpha1/drill_types.go` defines the resource; `controller-gen` (pinned with Go's `tool` directive) generates the deep-copy code and the CRD in `config/crd`.

Validation lives in the CRD, so the **API server** enforces it on every `kubectl apply`, before the operator sees anything:
- OpenAPI schema: action is `DeletePods` or `ScaleDown`, count 1–10, guard ≥ 0, Prometheus URL is http(s), the limit is a number.
- **CEL rules** (`x-kubernetes-validations`): `checkEvery ≤ observeFor`, `observeFor ≤ 1h`, `checkEvery ≥ 100ms`, and `self == oldSelf` on the spec, a *transition rule* that makes a Drill immutable once created.

Tests start envtest (kube-apiserver 1.37.0 and etcd 3.7.0, Windows builds) and check 10 bad Drills are rejected with the right message, that defaults are applied, and that changing a spec is refused.

What broke: the first run rejected a *valid* Drill with "checkEvery must be at least 100ms". `metav1.Duration` is a struct, and `omitempty` never drops a struct, so the Go client sent `"0s"`: the default didn't apply and the CEL rule fired. Optional fields with defaults are pointers in Kubernetes APIs for this reason; `observeFor` and `checkEvery` are now `*metav1.Duration`.
