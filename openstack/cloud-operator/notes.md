# KUBERNETES OPERATORS

A Kubernetes operator is a software extension that uses Custom Resource Definitions and controllers to manage applications or external infrastructure through the Kubernetes API. Instead of running manual scripts, one can declare state in YAML and the operator continuously reconciles actual state to match it.

## Operator Components

### CRD (Custom Resource Definition)

- Registers a new API group, version, and kind in the Kubernetes API server
- Uses an OpenAPI v3 schema to validate fields before objects are stored in etcd
- Can expose a separate `/status` subresource and custom `kubectl get` printer columns

### Custom Resource (CR)

- An instance of a CRD created by users (stored in etcd just like Pods or Deployments)
- `spec` holds the desired state declared by the user
- `status` holds the observed state written by the controller

### Controller and Reconciler

- Watches one or more resource kinds and runs a `Reconcile(ctx, req)` function
- Level-triggered: inspects the full current state rather than relying on individual change diffs
- Must be idempotent because reconcile can run multiple times for the same object

### Informers, Cache, and Workqueue

- Controllers do not poll the API server on every read
- An informer maintains a local in-memory cache synced via list-and-watch
- Watch events enqueue only the object's `namespace/name` key into a deduplicating, rate-limited workqueue

### Manager

- Runs one or more controllers in a single process
- Shares one informer cache and API client across controllers
- Handles leader election (using `Lease` objects), `/healthz` and `/readyz` probes, and Prometheus `/metrics`

## Reconciliation Loop

1. Fetch the Custom Resource by `namespace/name`.
2. If not found, exit cleanly (object was already deleted).
3. If `DeletionTimestamp` is set, run cleanup and remove the finalizer.
4. Otherwise, ensure the finalizer exists, compare desired state (`spec`) against actual state, create or update what is missing, and write observed state to `.status`.

Return values control scheduling:

- `Result{}, nil`: done until the next watch event
- `Result{RequeueAfter: duration}, nil`: run again after a delay (useful for polling external systems or drift checks)
- `Result{}, err`: retry with exponential backoff

## Finalizers

Finalizers prevent Kubernetes from removing an object before external resources are cleaned up.

- When `kubectl delete` runs on an object with a finalizer, Kubernetes sets `metadata.deletionTimestamp` instead of deleting it from etcd
- The controller sees `DeletionTimestamp != nil`, deletes external resources (disks, networks, VMs), and removes its finalizer string
- Once `metadata.finalizers` is empty, Kubernetes deletes the object
- Cleanup logic should ignore `404 Not Found` so retries remain safe

## Status, Conditions, and Predicates

- Status is updated via `Status().Update()` so user edits to `spec` and controller writes to `status` do not overwrite each other
- Standard `metav1.Condition` fields (`Type`, `Status`, `Reason`, `Message`, `ObservedGeneration`, `LastTransitionTime`) give consistent readiness reporting
- Updating `status` emits an update event; attaching `GenerationChangedPredicate` skips status-only updates because only `spec` changes increment `metadata.generation`

## Building Operators in Go

Most Go operators use `controller-runtime` (the library behind Kubebuilder and Operator SDK) on top of `client-go` and `apimachinery`. Controllers can be unit-tested with `controller-runtime`'s fake client (`WithStatusSubresource`) or integration-tested against a local API server and etcd using `envtest`.

## Common kubectl Commands

List installed CRDs and inspect their schema:

```bash
kubectl get crds
kubectl explain <crd-kind>.spec
```

Inspect a Custom Resource, its conditions, and controller events:

```bash
kubectl get <crd-kind>
kubectl describe <crd-kind> <name>
```

Check controller logs and metrics:

```bash
kubectl logs -n <namespace> deploy/<operator-deploy> -f
```
