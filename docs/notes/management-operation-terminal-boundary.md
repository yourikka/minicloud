# Management Operation Terminal Boundary

- Status: resolved for the Local Core MVP
- Date: 2026-07-26

## Problem

`docs/notes/local-core-management-operation-atomicity.md` left one blocking
question open: Version admission cannot be exposed through an HTTP management
endpoint because a validator infrastructure failure leaves a `Validating`
Version with no terminal Operation outcome. `controlplane.Ledger` stores only
terminal results, so the boundary could neither record success nor record
failure, and an exact retry would create a second Version.

The same question applies to every management write. The Ledger requires the
completion record and the resource mutation to enter authoritative state in one
transaction, but Local Core keeps Catalog, ReleaseStore, RouteStore, and Ledger
as independent in-memory state machines.

## Decision

Split every management write into a **terminal control command** and a
**convergence step**, and let only the terminal command enter the Ledger.

1. The terminal command is the smallest resource mutation that is always
   decidable: create the Function and its Trigger, create the `Uploaded`
   Version, publish the Route, or apply the lifecycle CAS. It performs no I/O
   and cannot report "unknown", so `Ledger.CompleteAfter` can wrap it directly.
2. Everything fallible is validated and reserved **before** the mutation:
   Artifact bytes are verified through the CAS, identifiers and command
   metadata are allocated, and the request digest is computed. Capacity
   exhaustion therefore fails before any resource changes.
3. Validator execution is no longer part of the operation. `CreateVersion`
   records `Uploaded`, commits the operation record, and only then drives
   `StartValidation` and admission. A validator infrastructure failure leaves
   the persisted validation fence intact; `ResumePendingValidation` retries it
   from the convergence loop.
4. An exact retry of the same `{principal, namespace, operation_id}` replays the
   retained outcome, which names the Version that already exists. The client
   polls `GET /v1/functions/{name}/versions/{id}` for `Ready` or `Failed`
   instead of inferring admission from the write response.

## Why Not The Alternatives

Recording a pending-operation intent inside the Ledger would add a non-terminal
state to a deterministic FSM primitive whose ADR explicitly restricts it to
terminal outcomes. That change belongs to the M1 Raft FSM, where the intent can
be persisted in the same Apply transaction as the Version transition.

Keeping admission inside the operation and returning `operation_unknown` on a
validator failure was rejected because the Version resource would already
exist while no record names it, which is the exact hazard the Ledger prevents.

## Consequences

Version creation over HTTP returns as soon as the immutable Version exists, so
the response no longer proves `Ready`. The MVP acceptance path performs one
poll after the write. Admission still runs synchronously on the happy path, so
a successful create usually observes `Ready` on the first poll.

This is not a claim of restart durability. Local Core loses resources and
operation records together on process exit; only the M1 Raft FSM makes the
boundary durable.
