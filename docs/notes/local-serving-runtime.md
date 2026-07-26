# Local Serving Runtime Boundaries

- Status: open design constraint
- Date: 2026-07-25

## Consistent Serving Projection

A Local Core runtime must construct each ServingSnapshot from one consistent
view of Function, default HTTP Trigger, Route, Version, and Deployment state.
Calling the existing per-resource getters in sequence is not sufficient:
another Route publish can occur between reads and produce a snapshot whose
target epoch, deployment generation, or effective-policy digest no longer
belongs together.

The control-plane layer therefore needs a defensive aggregate read that uses
the existing Catalog, ReleaseStore, and RouteStore lock order. Local Controller
may expose that projection, but it must not expose mutable store internals.

## Serving Route Selection

`discovery.Route` deliberately omits `model.Metadata`; it is a serving-safe
projection, not a persisted Route object. Gateway code must not synthesize
Metadata merely to call `routing.Select(model.Route, ...)`. A dedicated,
validated serving-route selector input is required so both the persisted Route
and the checked ServingSnapshot use the same hash contract without pretending
that a projection is a state-machine object.

## Endpoint Resolution and Worker Fence

Even in the MVP single-Worker profile, Gateway invocation must resolve the
published `Endpoint.Address` through an explicit in-process resolver. It must
forward the exact Assignment identity and Discovery Epoch as an
`InvocationFence`; it cannot select the only local Worker implicitly. Unknown
addresses and target mismatches fail closed. There is no cross-target or
cross-endpoint transparent retry after an invocation has been sent.

## Trigger Authentication

The default Trigger stores only a verifier digest. The future in-process
Gateway adapter must verify a presented management-independent trigger token
before Route selection or Worker invocation. A direct local call must not
bypass this boundary. Public and token Trigger behavior require separate
end-to-end tests.

## Full Sync Publication Ownership

Local serving publication must freeze the Function, Trigger, and Route
projection before consulting a Worker candidate source. Candidate collection
is allowed to lag or fail, but it cannot alter the control-plane half of the
snapshot; the Builder then excludes any candidate whose complete fence does
not match that fixed projection.

One Synchronizer serializes every Full Sync built by its Publisher and applied
to its Gateway Store. Those Publisher and Store instances must not have a
second writer. `Publisher.BuildFull` consumes a Sequence before Store apply, so
an apply failure may leave a skipped Sequence; the next recovery publication
must be another Full Sync, never a replay at the consumed position with changed
content. Full Sync accepts a forward Sequence jump and atomically replaces the
entire Function set, preserving fail-closed behavior during recovery.

Worker reconciliation is a separate phase from candidate observation. A
`CandidateSource` must only read committed Assignment intent, Worker inventory,
and installed Authorization; it must not call Prepare, InstallAuthorization, or
Cancel while a serving batch is being built. Combining those side effects with
snapshot construction would require a Begin/Commit/Abort protocol: a failed
Store apply must roll back newly prepared Replicas, while obsolete Replicas can
only be canceled after the replacement view is committed. Keeping observation
read-only avoids that cross-component transaction and leaves Replica cleanup to
the independently retryable Worker reconciler.

## Worker Reconciliation and Candidate Observation

The Local Worker Reconciler serializes control operations for one Worker
session. It reads committed Assignment intent first, joins an existing
non-terminal preparation when necessary, and installs a live-only serving
authorization only after the exact replica identity is Ready. A Cancelled intent
is removed from the candidate registry before the Agent is asked to stop it;
terminal acknowledgement happens only after terminal state is observed.

Candidate observation uses a separate read lock that protects only the bounded
registry of successfully installed authorizations. No lock is held while calling
Assignment, Worker, or Agent interfaces. Each read obtains fresh Worker and
Inventory observations and requires the authorization's Worker session and the
Ready replica identity to match exactly. A stale registry entry therefore cannot
be published after a session transition or replica-state change, even before the
next reconciliation pass removes it.
