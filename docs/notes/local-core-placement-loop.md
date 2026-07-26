# Local Core Placement Loop

- Status: resolved for the Local Core MVP
- Date: 2026-07-26

## Problem

Publishing a Route did not make a Function callable. The Reconciler only
consumes committed Assignment intent, and nothing in the process created that
intent: `CommitAssignment` existed but had no production caller, so a
published Route converged to a serving view with zero endpoints and every
invocation returned `no_ready_replica` forever.

The gap was invisible to the existing tests because they injected a
hand-written `AssignmentRecord` directly, exactly as a future M1 Scheduler
would have produced it.

## Constraints

1. SCH-010: Assignment intent must be committed before any Worker
   preparation. The placement step must therefore write through
   `CommitAssignment` and let the Reconciler observe it, never call the
   Worker Agent directly.
2. SCH-001/SCH-012 require a Leader barrier before planning. Local Core has
   no Raft, so the planner receives one trivial immutable barrier
   (`Term 1, Applied Index 1, Ready`). This is documented as a stand-in for
   single-process authority, not a leadership claim.
3. The deterministic Planner retains every decision for idempotent retry. A
   commit rejected by the scaling-revision CAS must release the planned
   Assignment ID, or the next convergence pass would replay the rejected
   decision forever. `Planner.Acknowledge` runs after the commit in both the
   success and failure directions.
4. `RuntimeProfile.compatible` compares the placement memory tier against the
   Worker engine profile with exact equality. A Version whose
   `resource_request.memory_mib` differs from the engine memory limit
   (default 128) is correctly filtered as `runtime_incompatible` and the
   Function reports no ready replica; the demo manifest must request the
   matching tier.

## Decision

`Runtime.Converge` gained one placement step between registry evaluation and
reconciliation. For every consistent `ServingState` it derives the single
servable Route target and compares it against live Assignment intent:

- No live intent: plan one placement on the single local Worker
  (`RequiredSlots: 1`; concurrency-aware budgeting is M1 Scheduler work) and
  commit it under the Deployment scaling-revision CAS.
- Live intent for a superseded target (new Version, Generation, or policy
  digest): cancel it with the record's resource-revision CAS. The replacement
  placement intentionally waits for the next convergence pass so the Worker
  first releases the superseded replica's slots.
- Disabled Function: keep the warm placement but never create a new one; the
  serving projection already fences invocation.

Placement failures join the existing convergence error set and surface
through `OnError` without stopping heartbeat, reconcile, or serving sync.

## Consequences

The management API's publish flow now converges to one invocable replica
without manual intent injection, and republish to a new Version drains the
old replica through the same cancel-then-place path. Multi-replica placement,
capacity-aware slots, anti-affinity, and recovery windows remain M1
Scheduler/Reconcile scope.
