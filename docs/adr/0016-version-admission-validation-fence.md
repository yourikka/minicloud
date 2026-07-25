# ADR 0016: Version Admission Validation Fence

- Status: accepted for the Local Core state layer
- Date: 2026-07-25

## Context

Artifact validation executes in a recoverable subprocess. Its result can arrive
after a Controller retry, cancellation, or later lifecycle transition. A local
goroutine, process ID, or wall-clock deadline cannot identify which result is
still allowed to change persisted Version state after a restart.

The specification also requires the `Validating -> Ready` transition to create
the first immutable Deployment Generation in the same Raft command. Recording
Ready before the policy or recording a Deployment after Ready would expose a
routeable Version without a complete execution fence.

## Decision

1. `ReleaseStore` creates only immutable `Uploaded` Versions. The command must
   carry the Version ID, Artifact/Manifest digests, UTC metadata, Applied Index,
   fixed runtime profile, and initial `admission_epoch=1`; it cannot create a
   Ready Version directly.
2. `StartValidation` transitions exactly `Uploaded -> Validating`, increments
   Version resource revision, and retains a `VersionAdmissionAttempt` with the
   command-supplied Validation ID. The attempt is part of `ReleaseSnapshot`.
3. `CompleteValidation` requires both the exact current resource revision and
   retained Validation ID. A mismatched or late report returns
   `stale_generation` without changing state. An invalid report produces only
   a bounded safe `Failed` error and no Deployment.
4. A valid report must match the Version Artifact digest, size, and frozen
   runtime profile. The same completion command creates `Ready` and exactly
   one Generation 1 Deployment. Local Core fixes this Generation to manual
   `min=max=desired=1`, zero Ready replicas, and an effective policy digest
   recomputed from Version inputs, approved capability subset, and fixed log
   bound.
5. The pending attempt is removed only after a valid completion has fully
   passed deployment validation and committed its new state. A malformed
   deployment therefore leaves the Version `Validating` with its Fence intact
   for a safe retry.
6. Local Core generates globally unique Version IDs, a stricter form of the
   per-Function uniqueness required by v1, and fixes admission to the wazero
   compiler engine. This keeps the Version ID used by the Route and Deployment
   models unambiguous until a composite identity is introduced deliberately.

## Consequences

`ReleaseStore` binds Version creation to an existing Function Catalog identity,
but it remains a Local Core primitive. The M1 FSM must atomically compose the
Function Catalog, Release Store, Operation Ledger, audit outbox, snapshot, and
State Digest. The Validator driver remains outside Apply: it reads a retained
attempt, validates the CAS Artifact in a child process, then submits only the
bounded report result as deterministic command input.
