# ADR 0012: Worker Registry Session and Heartbeat Boundary

- Status: accepted for the in-process Registry/Leader observation layer
- Date: 2026-07-18
- Owners: Worker Registry and Controller

## Context

The Worker contract separates durable control intent and Session high-water
from volatile liveness observations. Heartbeats must not become one Raft write
per interval, and a Worker must not return to service through a stale Session
or an incomplete Inventory after reconnect or activation.

## Decision

`internal/workerregistry` provides a bounded, mutex-protected local view:

1. `CommitSession` is the explicit high-water step. It accepts exactly
   `max_session_epoch+1`, fences the previous Boot/Session, and clears the old
   Inventory before `Register` accepts the committed Session. Removed Worker
   identities remain in the map and are rejected permanently.
2. `ReportInventory` requires the exact registered Session and validates a
   complete runtime, capacity, labels, and cache observation. It deep-copies
   maps and is the only transition to `Ready`; `Heartbeat` only refreshes a
   local monotonic lease.
3. `Evaluate` derives `Suspect` at the configured 3-heartbeat boundary and
   `Unavailable` at the configured lease boundary. Clock regression fails
   closed, marks active Workers unavailable, and makes `ClockHealthy` false.
4. `Draining`, `Drained`, and `Removed` are orthogonal to Session liveness.
   `Drained` requires no active invocation, drained Assignments, and either all
   known Gateway Fence ACKs or an expired authorization window. Only Drained
   can transition to Removed; Activate forces a newer Session before Ready.

## Hard boundary recorded

This package intentionally does not implement the Raft FSM, Drain/Remove
Generation commands, Gateway RPCs, or Worker Agent replica/authorization
details. The Controller/Raft layer must persist those generations and provide
authenticated full Inventory observations. The Registry exposes only the
immutable, defensively copied Scheduler snapshot needed by the next layer.

## Evidence

- `internal/workerregistry/registry.go` implements Session fencing, bounded
  heartbeats, monotonic threshold derivation, Drain proofs, and defensive
  snapshots.
- `internal/workerregistry/registry_test.go` covers configuration bounds,
  epoch fencing, threshold boundaries, clock regression, Drain/Activate,
  defensive copies, and concurrent calls under the race detector.
