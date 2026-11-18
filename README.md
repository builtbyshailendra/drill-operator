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

### Step 2: the reconciler

`internal/controller` moves a Drill through its phases, writing each one to the Drill's status before doing the next thing:

```text
Pending ──checks──► Injecting ──action──► Observing ──window over──► Succeeded (ScaleDown undone)
   │                (intent saved)           │
   └─► Rejected                              └─ guard trips ──► Aborted (ScaleDown undone)
```

Before anything is touched, a Drill is **rejected** if its namespace isn't labelled `drills.builtbyshailendra.dev/allowed=true`, the target doesn't exist, the service is already below its guard (steady state first), the Prometheus guard can't be read, or the action alone would take ready pods below the guard (blast radius).

Choices worth explaining:
- **Intent before action.** The pods to delete (with their UIDs) and the replica count to restore are saved in `status` first. A reconcile that runs again (after a crash or a write conflict) repeats that recorded intent instead of choosing new pods.
- **Delete with a UID precondition.** If a recorded pod is already gone and a new pod took its name, the delete fails with a conflict and the new pod is left alone.
- **A finalizer** holds a running Drill's deletion until a ScaleDown is undone.
- **A guard that can't be read counts as tripped.** No data from Prometheus stops the drill; an empty query result is an error, not zero.
- **Timers, not watches.** While observing, the controller re-queues itself every `checkEvery` rather than watching pods.

Tests (envtest, the controller running in the test process; the tests play the kubelet by creating pods and setting their Ready condition): a DeletePods drill that passes; five refusals that touch no pods; another pod failing mid-drill (aborted, lowest ready 2); a ScaleDown undone after its window; a Prometheus guard (a fake Prometheus server) going from 0.01 to 0.21 (aborted, replicas restored); deleting a running Drill (finalizer restores replicas); a retried injection deleting only the recorded pod; and a replacement pod with a reused name surviving. Removing the UID precondition makes the last test fail, so it really tests that.

What broke: one test was flaky (1 failure in 5 full runs). Finishing a Drill takes two writes, status and then metadata (to remove the finalizer), and the test sometimes read the Drill between them. The operator was right; the test now waits for the finalizer to go. After the fix: 10 full runs in a row passed.

### Step 3: the operator binary, RBAC from markers, and an end-to-end run

- `cmd/manager`: the operator: leader election (a Lease), `/metrics` (including `drill_operator_drills_finished_total{phase}`), `/healthz` and `/readyz`.
- `config/rbac/role.yaml` is **generated** from `+kubebuilder:rbac` markers next to the code that needs each permission. `config/manager` has a Namespace, ServiceAccount and a 2-replica Deployment (non-root, read-only root filesystem, all capabilities dropped); `config/samples/drill.yaml` is a ScaleDown drill with an error-ratio guard; there's a Dockerfile. **None of these has been applied to a real cluster or built**: there's no Docker daemon or cluster on this laptop.
- `test/e2e`: builds the real binary and runs it **as its own process**, logged in as a user bound to *only* the generated ClusterRole, with leader election on. It runs a ScaleDown drill to `Succeeded`, checks replicas are back, reads the metric from `/metrics`, finds the Lease, shows `kubectl get drills` (envtest ships kubectl):

  ```text
  NAME               TARGET   ACTION      PHASE       AGE
  lose-one-replica   api      ScaleDown   Succeeded   2s
  ```

  and fails if the operator's log contains any "forbidden".

What broke: the first end-to-end run failed on exactly that check. Leader election announces "became leader" with the **old** core/v1 Events API, while the controller uses `events.k8s.io/v1`, and the role only allowed the new one. On a real cluster the event would have been silently dropped. Added the marker, regenerated the role. Removing `events.k8s.io` from the role by hand also makes the test fail, so it checks the controller's own permissions too.
