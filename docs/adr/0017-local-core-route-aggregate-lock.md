# ADR 0017: Local Core Route Aggregate Lock

- Status: accepted for the Local Core state layer
- Date: 2026-07-25

## Context

Publishing a Route changes more than the Route object. It must atomically
verify the referenced Ready Version and Deployment policy, replace the complete
Route, advance the Function active Route pointer, and increment Function
resource revision. Calling public Catalog, ReleaseStore, and RouteStore methods
in sequence would expose an intermediate state to a concurrent reader.

## Decision

`RouteStore.Publish` uses the fixed Local Core aggregate lock order
`Catalog -> ReleaseStore -> RouteStore`. It validates the complete single-target
Route and its Ready Version/Generation/Policy identity while all three locks
are held, then writes the Route and Function pointer before releasing any lock.
Route reads hold RouteStore's lock, and Catalog reads hold Catalog's lock, so
neither can observe a write in progress.

Local Core retains only the current immutable Route snapshot. Every published
snapshot has `resource_revision=1`; `route_revision` is the Function-scoped
monotonic revision. Route history, pinning, rollback, and GC are deliberately
deferred rather than represented by incomplete placeholder state.

## Consequences

New cross-store mutations must use the same lock order. The M1 Raft FSM will
replace this process-local aggregate transaction with one deterministic Apply
transaction and a single snapshot/state digest. It must preserve the same
CAS, policy-identity, and atomic-pointer invariants.
