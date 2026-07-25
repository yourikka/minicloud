# MiniCloud Local Core MVP

## Purpose

This document defines the first runnable MiniCloud delivery without weakening
the v1 architecture contract in `MiniCloud-Spec-v1.0.md`. Local Core is a
single-Controller development profile, not a claim of high availability or a
substitute for the M1 distributed control plane.

## Delivered User Flow

The MVP must complete this bounded synchronous path:

1. A management client creates a Function and its default HTTP Trigger.
2. The client uploads a built `.wasm` Artifact into content-addressed storage.
3. The Controller records an immutable Version, validates it in the isolated
   Validator process, and records either `Ready` or `Failed`.
4. A successful `Ready` transition atomically creates Deployment Generation 1
   with a checked effective policy.
5. An administrator publishes one enabled Route Target with weight 10000 for a
   Ready Version and Generation.
6. The Gateway applies the complete serving view and invokes a prepared Worker
   using the existing WASI Command ABI.

The path retains strict artifact digest checks, validator isolation, resource
bounds, Function resource-revision CAS, persistent-operation semantics at the
state-layer boundary, and all stable problem codes already implemented.

## Local Profile Boundary

Local Core runs one Controller process and does not claim quorum, leader
failover, durable Raft recovery, or multi-node linearizable reads. Every new
control command nevertheless carries deterministic state inputs such as IDs,
UTC timestamps, and Applied Index so that the same domain types can be moved
into the M1 Raft FSM without changing their semantics.

The Management API must surface this profile explicitly. It must not describe
one local process as a replicated control plane or promise availability across
Controller restart until the Raft storage and snapshot gates are implemented.

## Explicitly Enabled

| Area | Local Core behavior |
| --- | --- |
| Function | Create, get, list, enable, and disable with exact resource-revision CAS. |
| Trigger | One default HTTP Trigger per Function; `token` stores only a verifier digest and `public` stores none. |
| Artifact | Content-addressed upload, size/digest checking, and verified Worker loading. |
| Version | Immutable metadata, isolated validation, `Uploaded -> Validating -> Ready/Failed`, and one initial Deployment Generation. |
| Deployment | Manual, single-replica desired state; Generation 1 effective policy is immutable. |
| Route | One enabled Target at 10000 basis points, or a disabled empty Route. |
| Invocation | Synchronous WASI Command invocation through the complete serving view. |

## Deferred, Not Removed

| Area | MVP behavior and future owner |
| --- | --- |
| Raft, WAL, Snapshot, quorum reads | Not exposed as available; M1 control-plane runtime. |
| Multi-Worker placement and recovery | Fixed single Worker; M1 Scheduler/Reconcile. |
| Multiple Route Targets, weighted rollout, rollback history | Validation and selector contracts remain; M2 routing features. |
| Autoscaling and scale to zero | Fixed manual replica count; M2 autoscaling. |
| Async invocation, Cron, Event Source | Not enabled; M2 durable data-plane services. |
| Function/Version deletion and generation GC | Not enabled until Route, Assignment, LKG, and async drain dependencies can be checked atomically. |
| Token rotation | Stored verifier model remains; Management rotation endpoint waits for the complete operation/FSM transaction. |

An unavailable feature must return a stable documented error from its API
boundary. It must never create partial state, silently fall back to a weaker
mode, or claim that the deferred guarantee has been met.

## Compatibility Rules

- Do not remove fields from `internal/model`, alter digest domains, or replace
  transport-neutral control commands with process-local values.
- Keep the Coverage Manifest entries `planned` until their full requirement and
  end-to-end evidence exist. Local Core evidence may be added only as partial
  evidence with its remaining gates stated explicitly.
- Keep secrets out of Function, Version, Deployment, Operation, State Digest,
  logs, and diagnostic snapshots. Verifier digests are the only Trigger
  authentication value allowed in persistent state.
- Do not implement destructive lifecycle endpoints merely to improve API
  coverage. Their cross-resource safety preconditions are part of the feature.

## Exit Criteria

Local Core is ready for a runnable MVP demonstration when a fresh process can
complete the delivered user flow with a valid Go-built WASM module, reject an
invalid module into a queryable `Failed` Version, reject stale Function and
Route writes, and perform one authenticated synchronous invocation. The test
suite must prove the normal and rejection paths without claiming M1/M2
guarantees.
