# ADR 0011: Deterministic Scheduler Planner Boundary

- Status: accepted for the transport-neutral planning layer
- Date: 2026-07-18
- Owners: Controller and Scheduler

## Context

The specification requires the Scheduler to reject incompatible or unsafe
Workers before considering cache locality or load. It also requires a current
Leader barrier, stable placement decisions, and idempotent Reconcile retries.
The repository does not yet contain a Raft FSM, Worker Registry persistence, or
an authenticated Controller-to-Worker transport. Implementing those concerns
inside a local scheduler would make a successful unit test look like a
distributed commit that has not happened.

## Decision

`internal/scheduler` owns only deterministic placement decisions over an
immutable Worker Inventory snapshot:

1. A request must carry the control-plane supplied Command ID, globally unique
   Assignment ID, immutable Version/Artifact/Policy identity, exact Runtime
   profile, resource request, and optional hard label constraints. The Planner
   never generates or repairs IDs.
2. A Worker is hard-filtered when its observation is invalid, its intent is not
   `Schedulable`, its session is not `Ready`, its Runtime/ABI/Feature/Platform
   profile differs, required labels do not match, or its remaining memory/slots
   cannot satisfy the request. Each rejection is returned as a stable reason.
3. Among eligible Workers, the order is deterministic: compiled-cache hint,
   artifact-cache hint, anti-affinity spread, lower allocated-slot ratio, lower
   allocated-memory ratio, then Worker ID/Boot ID/Session Epoch. Cache hints are
   advisory and cannot bypass any hard filter.
4. `InstallBarrier` accepts only a positive, committed Leader term/index and
   rejects regressions. `InvalidateBarrier` withdraws authority when leadership
   or quorum is lost. `Plan` has no external side effect until this barrier
   is installed. Repeated Command IDs return the same decision; a new Command
   for an already planned Assignment returns `already_satisfied` only when the
   immutable placement payload is identical.
5. Idempotency records have a hard bound and are removed only through explicit
   `Acknowledge`. The Planner never silently evicts a result that may still be
   retried, and its retained state is protected by one mutex.

## Hard boundary recorded

The Planner cannot prove that a caller is the current Raft Leader, that the
Barrier was actually committed, that a Worker observation came from an
authenticated connection, or that an Assignment ID is globally non-reusable.
Those proofs remain Controller/Raft/RPC responsibilities. The `Ready` bit in
`LeaderBarrier` is therefore evidence supplied by that future layer, not a
claim made by this package. Likewise, the Planner does not submit an Assignment
Intent or send a Worker RPC; the caller must persist the Intent first and reuse
the returned Command/Assignment identity when it sends the command.

## Consequences

The selection and idempotency rules can be tested without network or wall-clock
state, and bounded inventory/decision inputs make resource behavior explicit.
The current layer does not yet cover SCH-001, SCH-006/007, SCH-010, RPC-002,
RPC-004, or multi-process E2E: those require Raft leadership, Worker Registry,
authenticated transport, retry persistence, and a real Reconcile loop.

## Evidence

- `internal/scheduler/types.go` validates immutable inputs and explains all hard
  filter reasons.
- `internal/scheduler/planner.go` implements barrier fencing, stable ranking,
  bounded idempotency, and explicit ACK reclamation.
- `internal/scheduler/planner_test.go` covers barrier gating, hard filters,
  cache-before-load ordering, no-eligible explanations, duplicate/conflicting
  commands, ACK capacity reclamation, regression rejection, and 100-way
  concurrent retries.
