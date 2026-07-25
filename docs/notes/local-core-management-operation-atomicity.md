# Local Core Management Operation Atomicity

- Status: open design constraint
- Date: 2026-07-25

## Problem

The Local Core MVP needs an HTTP management boundary, but its write operations
must preserve the Operation Ledger contract. A management client can retry
after a response is lost, so one `{principal, namespace, operation_id}` must
not create a second Function, Version, or Route transition.

`controlplane.Ledger` retains only terminal outcomes. Its ADR requires the
completion record and the affected resource mutation to enter the authoritative
state in one transaction. The current Local Core keeps Catalog, ReleaseStore,
RouteStore, and Ledger as independent in-memory state machines, so calling
`Ledger.Complete` after a resource write is not safe: a full ledger or an
unexpected completion failure would leave a resource that cannot be replayed.
Calling it before the resource write is also not safe because a successful
operation record could describe a mutation that never happened.

## Required Boundary

The management service must not expose a write endpoint until it uses one
Controller-owned operation boundary with these properties:

1. It serializes lookup, request-digest comparison, command metadata
   allocation, resource mutation, and terminal outcome insertion for all
   management writes.
2. It validates and reserves all fallible inputs needed by the resource command
   and Ledger completion before mutating a resource. Capacity exhaustion must
   fail before the resource mutation.
3. A completed same-digest retry returns the stored safe outcome. A different
   request digest returns `conflict`; an expired tombstone returns
   `operation_expired`.
4. A Version request whose validator has a temporary infrastructure failure is
   not terminal. It requires a persisted pending-operation intent tied to the
   existing validation fence before an HTTP API can promise exact retry replay.
   The current terminal-only Ledger cannot represent that intent.

## MVP Scope Decision

The first management HTTP slice may safely expose read-only profile and query
endpoints. It may expose write endpoints only for command types that the new
single-process operation boundary can complete atomically. Version admission
must remain internal until the boundary records pending validation intent, or
until the M1 Raft FSM persists that intent with the Version transition.

This is intentionally not a claim of restart durability. Local Core is an
in-memory development profile: a process crash loses both resources and
operation records together. The later Raft implementation must replace this
boundary with one replicated apply transaction and a durable pending-operation
representation.
