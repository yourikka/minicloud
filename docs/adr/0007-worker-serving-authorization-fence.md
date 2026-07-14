# ADR 0007: Worker serving authorization fence

- Status: Accepted
- Date: 2026-07-14

## Context

A Ready Replica is not permanent permission to start work. During a Leader
failure or partition, a Gateway may retain a bounded ServingSnapshot LKG while
the Worker independently retains a short Serving Authorization. A new call is
allowed only while both sides remain valid and every routing, Assignment,
Worker session, policy, and Leader epoch field agrees.

Wall-clock timestamps and a sender's monotonic timestamps cannot prove this
window across machines. A Worker must anchor received authorization durations
to elapsed time in its current process boot.

## Decision

- Add `internal/servingauth` as the Worker-side authorization boundary.
  `AcceptAuthoritativeControl` is called only after the peer is authenticated,
  the current Leader barrier is verified, and Worker Registry has committed the
  Session Epoch. A node-role credential alone is insufficient. An Invocation
  RPC can check a Fence but can never raise Worker session or Discovery Epoch
  state.
- Fix `worker_id` and `boot_id` for one Gate lifetime, but accept a new
  `session_epoch` from each authenticated control registration/reconnect. The
  epoch must increase. Switching Session atomically clears authorizations from
  the old Session, while the Boot's highest Discovery Epoch remains monotonic.
- Separate persistent `AssignmentIdentity` from `InvocationFence`.
  `AssignmentIdentity` binds Worker/Boot/Session, Assignment ID, Version,
  Admission Epoch, Deployment Generation, Policy Digest, and Assignment Mode.
  `InvocationFence` adds Discovery Epoch, allowing a healthy new Leader to
  issue a new Fence without pretending that Discovery Epoch is persisted in the
  Assignment.
- Raise `highestDiscoveryEpoch` and invalidate old permissions as soon as a
  structurally valid authoritative connection presents a higher value. This
  happens before its Session Epoch is accepted, so an invalid new registration
  cannot leave older Leader permissions active. Lower Discovery Epoch messages
  can never lower the high-water mark.
- Store one independently refreshable Authorization per Assignment. TTL mode
  records the Worker's local elapsed time at receipt and expires at the exact
  `elapsed >= ttl` boundary. `live_only` mode additionally binds the exact
  authenticated control connection and fails as soon as that connection closes.
  A failed refresh never replaces the record or extends its receipt point.
- Represent the monotonic clock as an injected `func() time.Duration`.
  Production captures one `time.Now` value and uses `time.Since`, which retains
  Go's process-local monotonic component. Tests advance a manual elapsed value.
  A negative initial value rejects construction; any later regression makes the
  Gate permanently fail closed for new work.
- Linearize refresh, revoke, Session/Epoch changes, expiry checks, and new-call
  authorization under one mutex. `AuthorizeSync` compares the complete Fence
  and lifetime; a nil result is the current call's acceptance point and no
  reusable Permit or capability is returned. Later expiry or fencing does not
  cancel that already accepted invocation; it only rejects subsequent calls.
- Reject every structurally valid Worker Invocation Fence mismatch as
  `stale_assignment` without revealing which identity field differed.
  Structurally invalid fields remain `invalid_argument`. A refresh that attempts
  to mutate Version, Admission Epoch, Generation, or Policy returns
  `stale_generation`. `drain-only` Assignments cannot begin synchronous work.
- Bound both tracked Assignment identities and authorization records to 4096
  entries by default and 65536 at the v1 safety maximum. Expired and disconnected
  authorization records are removed after a failed authorization check, but the
  identity remains until explicit Revoke or Session replacement. This prevents
  an Assignment ID from being refreshed with another identity while it is known
  to the Session. Global ID uniqueness and non-reuse remain Controller/Raft
  invariants. No per-authorization Timer or goroutine is created.

## Remaining boundaries

This Gate assumes the caller already authenticated the control connection,
verified the current Leader barrier, and confirmed the committed Session Epoch.
Node tokens, Cluster/role binding, message framing, command IDs, deadlines, and
batch limits belong to the versioned internal RPC layer. The Controller must
still prove quorum before issuing or refreshing a permission; the Worker cannot
infer that proof from a message.

The Gate does not own Replica lifecycle. Artifact verification, Runtime
Compile/Validate, effective policy installation, Ready Observation, Assignment
cancel/drain idempotency, and atomic Inventory replacement remain Worker Agent
work. The future synchronous Worker entrypoint must require both a Ready Replica
and exactly one successful `AuthorizeSync` call per Invocation, after all queue
waiting completes and immediately before creating its Guest. One authorization
check must never start multiple Guests. The asynchronous entrypoint needs an
additional durable Task/Attempt Lease Fence and is not represented by this
synchronous gate.

Gateway ServingSnapshot LKG is an independent monotonic window. The global
configuration layer must still enforce `authorization_ttl <= serving_max_stale`,
the 0-duration `live_only` relationship, and refresh `< ttl/2`. Consequently,
WRK-007..011, CAP-015, DSC-005, RFT-008, RPC-004, and RPC-005 remain incomplete
until their other components and multi-process evidence exist.

## Consequences

Unit and race tests cover exact TTL expiry, refresh anchoring, TTL behavior after
disconnect, exact-connection `live_only`, higher Discovery Epoch with an invalid
Session, higher Session fencing, every Invocation field mismatch, immutable
refresh identity, capacity/revoke behavior, drain-only synchronous rejection,
clock regression, stale disconnects after connection replacement, and concurrent
refresh/authorize operations. The state is intentionally boot-local and is never
serialized or placed in Raft.
