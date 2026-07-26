# Assignment Intent Authority

- Status: Local Core primitive implemented; replicated FSM integration pending
- Date: 2026-07-26

## Planner Journal Is Not Assignment Authority

The deterministic Scheduler Planner retains decisions only until explicit
acknowledgement. Its Assignment index is therefore an idempotency journal, not
the durable proof that an Assignment ID was globally consumed. Reusing that
index as desired state would allow an acknowledged or restored ID to be reused
and would make a successful Plan look like a committed Worker side effect.

`controlplane.AssignmentStore` owns the Local Core desired-state primitive.
It retains every identity, including cancelled records, and accepts no delete
or reactivate transition. The outer Operation Ledger owns command replay; the
Store rejects every repeated Assignment ID, even when its payload is equal.

## Commit And Scaling Boundary

An Assignment is installed only while holding the aggregate lock order
Catalog, ReleaseStore, RouteStore, then AssignmentStore. The placement must
match the current Active Function, enabled Ready Route target, admitted Version
runtime, Deployment Generation, and Effective Policy digest. Creation also
uses an exact Deployment ScalingRevision CAS and counts only Assigned records
for that target, so concurrent Reconcile attempts cannot exceed
DesiredReplicas.

Local Controller allocates command metadata and commits the Intent before a
future Worker Reconciler may call Prepare. Cancellation uses the inverse safe
ordering: commit `Cancelled`, publish a ServingSnapshot that excludes the
Assignment, then send Worker Cancel. A failed Worker RPC never rolls desired
state back to `Assigned`.

## Remaining Deterministic Transaction

The current Worker Registry combines persistent session high-water state with
process-local heartbeat observations. It cannot yet join Assignment creation
in a deterministic Raft Apply transaction. The Local Store can verify the
exact Worker Session carried by Planner output and the Worker Agent will reject
a stale fence, but a cluster implementation still needs persistent Worker
Session/Intent state so Session CAS, resource reservation, Assignment creation,
snapshot restore, and the global State Digest share one FSM boundary.

Until that boundary exists, `CreatedRaftIndex` and `LastAppliedIndex` in Local
Core are process-local command positions, not claims of quorum commitment.
