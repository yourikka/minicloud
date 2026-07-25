# ADR 0018: Local Controller Admission Boundary

- Status: accepted for the Local Core MVP
- Date: 2026-07-25

## Context

The Catalog and ReleaseStore deliberately accept only explicit command inputs.
They cannot read wall time, create IDs, open Artifact files, or run Validator
subprocesses without losing the deterministic apply contract required by the
future Raft FSM. Conversely, leaving those activities only in ad-hoc callers
would make Local Core unable to resume an interrupted validation safely.

Version validation has a fence (`ValidationID`) after `Uploaded -> Validating`.
Within the current Local Core process, a canceled management request or a
child-process crash must not create a new fence that lets an old report write a
newer Version state. Controller restart recovery requires durable state and is
therefore deferred to the M1 FSM.

## Decision

1. `internal/localcontroller` owns fresh Catalog and ReleaseStore instances and
   is their Local Core write entry point. It injects only the side-effecting or
   non-deterministic boundaries: Artifact CAS, Validator, command metadata,
   and ID generation.
2. `MonotonicCommandSource` assigns process-local increasing Applied Index and
   UTC timestamp values at the boundary. These values make each domain command
   complete but are explicitly not Raft log indexes and do not survive restart.
   `Controller` independently rejects a configured source that regresses either
   value. The M1 adapter will replace this source with committed log metadata.
3. Version creation first verifies the CAS Artifact, then commits `Uploaded`,
   persists one `ValidationID`, and runs the Validator without holding a state
   lock. A valid report atomically commits `Ready` with Generation 1; a valid
   negative report commits `Failed` without a Deployment.
4. A known Validator deadline or validator-output resource limit is converted
   to a bounded safe negative report and commits `Failed`, satisfying the
   Local Core portion of ART-010. Child crash, temporary CAS failure, overload,
   malformed child output, and caller cancellation retain `Validating` with
   its existing fence. `ResumePendingValidation` reuses that exact ID.
5. CAS blobs are not deleted as compensation. A successfully published but
   temporarily unreferenced digest is shared content and remains available for
   the subsequent persisted Version write or retry.
6. The controller rejects sub-millisecond resource timeouts before creating a
   Version because Generation 1 effective-policy canonicalization uses whole
   milliseconds. This prevents an Uploaded Version that can never produce a
   valid policy.

## Consequences

Local Core now has a synchronous, recoverable create-function/upload/admit
path but does not yet expose HTTP management operations, durable Controller
state, cross-restart operation replay, Raft recovery, or a serving Worker
reconcile loop. In particular, the existing terminal Operation Ledger cannot
represent an in-flight validation transaction; that guarantee remains a future
FSM transaction with a durable pending-operation intent.
