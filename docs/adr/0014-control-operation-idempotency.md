# ADR 0014: Deterministic Control Operation Idempotency

- Status: accepted for the transport-neutral control-plane state layer
- Date: 2026-07-25
- Owners: Controller and Management API

## Context

A Management client can lose its connection after a control write commits but
before it receives the response. Retrying that write must not create another
state transition, and a new request must not silently reuse the old Operation
ID. The result needs to survive Leader changes, so a process-local idempotency
map is insufficient.

The control-plane state must also avoid retaining request bodies, credentials,
or unbounded response payloads. Those values are either sensitive or belong to
the transport/data plane rather than the Raft FSM.

## Decision

1. `internal/controlplane` owns a deterministic `Ledger` keyed by
   `{principal, namespace, operation_id}`. The authenticated principal is an
   input from the future Management boundary, never a user-controlled field.
2. `Request.Digest` hashes only a versioned canonical request contract using
   the `control-operation-request`/`v1` domain: normalized method and path,
   `If-None-Match`/expected resource and active Route revisions, strict JSON
   body presence/value, and optional Artifact digest. Authentication, request
   ID, connection metadata, and retry headers are excluded.
3. A completed record contains the request digest, committed Applied Index,
   command-supplied UTC completion time, terminal safe error when applicable,
   and affected resource/Route revisions. It does not retain the request body,
   credential, verifier, or arbitrary API response body.
4. Reuse with the same digest returns the original retained outcome. Reuse
   with a different digest returns `conflict`. A record that originally issued
   a one-time credential retains only a boolean marker; every later retry
   returns a `credential_not_replayable` disposition with no credential
   recovery path. The disposition carries only the original safe resource IDs;
   the future Management layer resolves their current revisions from the same
   authoritative FSM transaction before constructing its API response.
5. Retention has three explicit states: completed result, expired Tombstone,
   and absent. A Tombstone preserves only key and digest, returns
   `operation_expired`, and prevents ID reuse until its own retention expires.
6. The FSM never reads wall clock time. A future Leader must submit UTC
   `CompletedAt` on the completion command and an explicit UTC cutoff on GC.
   GC converts eligible results to Tombstones and later deletes eligible
   Tombstones. The ledger does not silently evict capacity; it returns
   `overloaded` until a valid GC command creates space.
7. This first ledger uses immutable v1 capacity and TTL constants. A future
   configurable control-plane limit must be replicated FSM configuration and
   included in its snapshot/state digest; it cannot be supplied by individual
   Controller process startup configuration.

## Consequences

The ledger is transport-neutral and concurrency-safe for local adapters, but
full durability arrives only when the future Raft FSM persists its snapshot and
calls completion in the same atomic Apply transaction as the resource change.
It deliberately does not create Function/Route/Version state, perform
authentication, generate IDs, issue credentials, validate Artifact bytes, or
choose a Leader. Those responsibilities remain with the Controller,
Management API, Artifact layer, and Raft adapter.
