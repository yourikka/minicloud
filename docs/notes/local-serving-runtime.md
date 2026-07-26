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
