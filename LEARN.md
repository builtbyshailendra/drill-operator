# Learning notes: drill-operator

## 1. What an operator is

Kubernetes is a set of control loops. You store *desired state* in the API server; a controller watches it and keeps nudging the real world towards it. An **operator** is a controller for your own resource type, a **CRD** (CustomResourceDefinition), so `kubectl apply -f drill.yaml` becomes as natural as applying a Deployment.

Three things to hold on to:
- **Reconcile, don't script.** `Reconcile(ctx, name)` is called with only a name, maybe many times, maybe after a crash, maybe twice for one change. Each call reads the current state and takes one safe step. It must be *idempotent*.
- **Status is the operator's memory.** Anything it must remember between calls goes in `.status` (phase, chosen pods, original replicas). The process itself keeps nothing.
- **Spec is the user's; status is the operator's.** They're written through different endpoints (`/status` is a *subresource*), which is why finishing a Drill takes two writes (see Step 2's flaky test).

## 2. Pieces used here

| Piece | Where | One-line idea |
|---|---|---|
| CRD + OpenAPI schema | `config/crd` (generated) | the API server validates types, enums, ranges |
| CEL validation rules | `+kubebuilder:validation:XValidation` | expressions the API server runs: `duration(self.checkEvery) <= duration(self.observeFor)` |
| transition rule | `self == oldSelf` on spec | compares old and new on update: here, "a Drill can't be changed" |
| defaults | `+kubebuilder:default` | applied by the API server, but only if the field is *absent* (hence pointers) |
| status subresource | `+kubebuilder:subresource:status` | users can't overwrite status; the operator can't accidentally change spec |
| printer columns | `+kubebuilder:printcolumn` | what `kubectl get drills` shows |
| conditions | `meta.SetStatusCondition` | standard `type/status/reason/message` for tools and humans |
| finalizer | `controllerutil.AddFinalizer` | deletion waits until the operator has cleaned up |
| preconditions | `client.Preconditions{UID}` | "delete this pod only if it's still the same pod" |
| `RequeueAfter` | `observe` | come back in N seconds; the timer that drives observation |
| predicates | `SetupWithManager(extra...)` | filter which events reach the controller |
| leader election | `--leader-elect` | two replicas, one acts; a Lease decides which |
| RBAC markers | `+kubebuilder:rbac` | permissions declared next to the code that needs them, generated into a ClusterRole |
| envtest | tests | a real kube-apiserver and etcd, without kubelet or controllers |
| `go tool` | `go.mod` `tool` block | controller-gen and setup-envtest pinned like dependencies |

## 3. Chaos engineering ideas behind the safety rules

- **Steady state first.** If the service is already unhealthy, a drill teaches nothing and hurts users.
- **Minimise blast radius.** Refuse an action that by itself would break the guard.
- **Stop conditions.** Decide in advance what "it's hurting" means (ready pods, error ratio) and stop automatically.
- **Always restore.** A drill that leaves things broken is an incident. Restore at the end, on abort, and on deletion.
- **Opt in.** Drills run only where someone deliberately allowed them.

## 4. Explain it without notes

1. What does `Reconcile` receive, and why can't it assume it's the first call for this change?
2. Why does the operator save the pods it will delete *before* deleting them? What would go wrong otherwise?
3. What does the UID precondition protect against? Describe the exact sequence where it matters.
4. Why were `observeFor` and `checkEvery` changed to pointers? What did the first test run show?
5. What's the difference between the OpenAPI schema checks and the CEL rules? Give an example of each from this CRD.
6. How does `self == oldSelf` work, and why make a Drill immutable?
7. Walk through deleting a Drill while a ScaleDown is in effect.
8. Why does an unreadable Prometheus count as a tripped guard?
9. The flaky test: what was the race, and why was the operator right and the test wrong?
10. How did the end-to-end test find a missing permission? Why would it have gone unnoticed in a real cluster?
11. What doesn't envtest give you, and how did the tests work around it?
12. What would you need before running this on a real cluster?

## 5. Your turn

1. **Schedule:** add `spec.schedule` (cron) so a drill repeats weekly. Where does "next run" live? (Hint: status, and a new Drill per run, or a `DrillSchedule` that creates Drills.)
2. **Pause button:** an annotation that makes a running Drill restore and stop immediately.
3. **Watch instead of poll:** add a watch on pods so the guard reacts within a second of a pod going unready. What's the trade-off?
4. **Run it on kind:** build the image, load it, apply `config/`, and run the sample against a real app. Write down what differed from envtest.
