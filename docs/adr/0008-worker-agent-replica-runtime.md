# ADR 0008: Worker Agent replica and invocation runtime

- Status: Accepted
- Date: 2026-07-15

## Context

The Worker already had four independent local primitives: verified Artifact
loading, a lease-pinned compiled cache, a WASI Command execution engine, and a
boot-local Serving Authorization gate. None of those primitives established a
Ready Replica. A caller could otherwise combine an Invocation Fence with an
unrelated Artifact, check authorization before spending time in a runtime
queue, or release a compiled Program while an accepted call still used it.

The Worker needs one owner for that composition before a versioned internal RPC
or Gateway is added. This owner must remain boot-local: Replica observations,
queues, active calls, and Serving Authorizations are not Raft FSM inputs.

## Decision

- Add `internal/workeragent`. One `Agent` owns the boot-scoped authorization
  `Gate`, the Worker compiled `Cache`, and a bounded map of Replica records.
- `Prepare` atomically binds one authenticated control connection and complete
  `AssignmentIdentity` to a `ModuleSpec` and an `EffectivePolicy`. The Agent
  acquires one Cache Lease and retains it for the whole Ready lifetime. An
  invocation never supplies an Artifact or compilation profile.
- Preparation runs in one Agent-owned, bounded-lifetime goroutine per distinct
  Assignment. Exact concurrent retries wait on the same result. Caller
  cancellation stops waiting but does not cancel the shared command; the first
  caller's earlier Deadline and the configured preparation maximum still bound
  the work. Cancel, control disconnect, Session replacement, and Agent Close
  synchronously cancel preparation.
- The Replica state graph is explicit and rejects every undefined transition.
  `Fetching` covers the Cache operation. The Cache internally performs actual
  byte verification, command-profile validation, and compilation. After it
  returns successfully, the Agent records the `Validating` and `Compiling`
  postconditions before publishing `Ready` under one mutex. Cache timing is the
  authoritative stage timing; the two postcondition states are not claimed as
  separately observable duration telemetry.
- A Replica becomes Ready only after the Artifact bytes and size match,
  wazero Compile/Validate succeeds, the locally recomputed policy digest
  matches the Assignment, and the Cache/Engine runtime profile can enforce the
  policy. Optional v1 host capabilities remain unsupported and fail closed.

## Effective policy

`model.EffectivePolicy` uses the `effective-policy` / `v1` digest domain and
RFC 8785 canonical JSON. Its canonical schema binds Version ID, Admission
Epoch, Deployment Generation, Artifact digest and size, ABI, Host API profile,
runtime feature profile, millisecond timeout, memory/input/output/log limits,
Replica concurrency, and a stably sorted capability set.

The Engine exposes immutable execution limits through the Cache, including its
Worker-wide and per-Program concurrency. Agent construction rejects a global
concurrency bound that exceeds the Engine, and preparation rejects a policy
that exceeds the actual timeout, ABI, log, memory, per-Program, or Agent bounds.
Per-invocation request, response, raw-output, and guest-log limits only tighten
those Engine bounds; a mismatch is not deferred until the first user call.

## Invocation acceptance

Admission order is fixed:

1. validate the local Replica identity and derive one end-to-end Deadline;
2. acquire the Replica slot/queue, then invoke the pinned Lease and acquire its
   Program permit;
3. acquire the Worker Agent slot/queue, then the Engine permit;
4. synchronously recheck context cancellation and pre-accept stop signals;
5. immediately before `InstantiateModule`, hold the Agent mutex, recheck the
   exact Ready Replica, and call `Gate.AuthorizeSync` once;
6. mark the invocation accepted, release the mutex, and create one fresh Guest.

Narrow Replica and Program admission precede Worker-wide admission, so calls
waiting behind one hot Replica or shared compiled Program do not reserve Worker
slots that another Program can use. Every queue has a hard bound. The runtime
Program and Engine queues observe both the call Deadline and Agent/Replica stop
signals until acceptance, allowing Cancel, Session replacement, and Close to
release a queued Lease immediately without cancelling an accepted Guest.

The pre-instantiation check returns no reusable permit. A Fence mismatch on the
invocation path is always the non-specific `stale_assignment` category. If
authorization expires while queued, no Guest is created. Once this check has
succeeded, later expiry, Cancel, disconnect, or Session replacement does not
cancel that call; its original Deadline still bounds completion.

## Lifecycle and lock order

Cancel changes a Ready Replica to Draining and revokes new admission while
accepted calls finish. Session replacement changes old-session Replicas to
Lost. Close rejects new work, drains every Replica, releases every Cache Lease,
and only then closes the Cache. The Engine remains owned by the Worker process
and must be closed after the Agent.

The only nested lock order is Agent mutex then Gate mutex. Lease release and
Cache close never run under the Agent mutex. Invocation paths hold a Lease read
lock when the acceptance callback takes the Agent mutex, so reversing that
order would deadlock.

Terminal observations remain bounded and are removed only after the exact
current control connection acknowledges them through
`AcknowledgeTerminal`. Global Assignment ID uniqueness and non-reuse remain
Controller/Raft invariants; the Worker does not invent a local ID reuse policy.

## Remaining boundaries

This ADR does not add a network protocol, Node Token authentication, Leader
barrier or quorum proof, Raft Assignment Intent, command IDs, command Deadline
encoding, Worker Registry, heartbeat, full drain ACK protocol, Supervisor,
Gateway ServingSnapshot/LKG, or asynchronous Task Lease Fence. Those layers
must call the Agent only after their own authority checks.

The current Cache binds one Engine and one memory tier. A Worker that advertises
multiple runtime/memory profiles needs a future bounded RuntimePool with one
Cache per exact compilation profile. CPU and process RSS enforcement, remote
Artifact transport, and aggregate log backend isolation also remain open.

Consequently the touched RUN, CAP, WRK, DSC, and RPC requirements remain
`planned`. Unit, integration, and race-oriented tests provide only the Worker
local half of their evidence; the specification's multi-process E2E gates are
still required.
