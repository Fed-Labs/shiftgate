# SHIFT Kubernetes adapter contract

Kubernetes integration is opt-in and does not claim arbitrary pod portability.
The shipped resource is `ShiftMigration`: it names an explicit workload id, a
source and destination machine, and a migration mode (`cold` or `live`). The
controller calls the same control-plane API used by the dashboard and CLI.

## What ships

- finalizers for in-flight migration operations: a deleted `ShiftMigration`
  keeps its finalizer until the cancel succeeds or the migration is verifiably
  terminal, so an in-flight migration can never leak (a leaked active
  migration holds the destination machine's capacity and locks the workload);
- stable `Idempotency-Key` values derived from object UID and generation, and
  re-reconciliation of spec edits: a phase (`Completed`, `Failed`, `Rejected`)
  is terminal only for the generation that produced it, so fixing a rejected
  spec creates a new migration under the new generation's key;
- deletion always makes progress — `DeletionTimestamp` is handled before spec
  validation, so an object edited invalid after admission can still be deleted;
- `https://` required for both `KUBERNETES_HOST` and `SHIFT_CONTROL_URL`; the
  adapter refuses cleartext URLs so the kube and org-scoped control-plane
  bearer tokens never travel unencrypted (same floor as the Slurm client);
- Secret references for the control-plane token; never embed credentials in a
  CR;
- explicit handling for API outage (retried on the next reconcile pass),
  cancellation (surfaced in `status.reason`), and terminal outcomes.

Status is a flat object (see `shiftmigration-crd.yaml`):

```yaml
status:
  phase: Running | Completed | Failed | Rejected
  reason: <human-readable outcome or error; always set>
  observedGeneration: <generation this status was written for>
  operationID: <control-plane migration id>
  lastObservedAt: <RFC3339 timestamp>
```

## Trust model

The adapter authenticates to the Kubernetes API with the pod's bearer token
and to the control plane with an organization bearer token, both over `https://`
only. The control-plane-to-agent hop a migration triggers is a separate leg the
adapter never crosses: that hop is authenticated by placement (the agent's Unix
socket is 0660 / group-restricted, or the control plane presents TLS client
certificates), not by application-level bearer credentials. Keep agents on a
private interface and the control plane's agent channel on a
mutually-authenticated transport; the adapter's security guarantees end at the
control-plane API.

## Not implemented (do not build against these)

The following are known future work and are deliberately absent from the
shipped contract:

- `ShiftWorkload` (workload/agent registration and `checkpointEverySeconds`
  checkpoint policy): the CRD was removed because nothing consumed it and
  scheduled checkpoints would never fire. Do not schedule checkpoint policies
  through this adapter.
- leader election: run exactly one controller replica today (the previous
  `SHIFT_K8S_LEADER_NAME` setting was accepted and ignored; it is gone);
- `status.conditions[]` carrying stage: the status is the flat object above;
- stale ownership-epoch handling.

The CRD below is the stable contract for what does ship. A production
controller can be built with controller-runtime without changing the agent or
control plane.
