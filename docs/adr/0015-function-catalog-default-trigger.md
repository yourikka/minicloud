# ADR 0015: Deterministic Function Catalog and Default Trigger

- Status: accepted for the transport-neutral control-plane state layer
- Date: 2026-07-25

## Context

Function creation must make its default HTTP invocation entry visible as one
control-plane fact. Creating these objects independently permits a Function to
appear callable without an authentication rule, or permits retry/recovery to
observe a partially initialized Function. The future Raft FSM also needs a
small deterministic state core before it can add Version, Route, Deployment,
and transport concerns.

## Decision

1. `internal/controlplane.Catalog` owns Function identity/name indexes and one
   default `HTTPTrigger` for each Function. `CreateFunction` validates and
   inserts both objects under one mutex after all checks pass. The command
   carries IDs, UTC creation time, Applied Index, and verifier digest; Catalog
   does not read a clock, generate an ID, access storage, or accept a Token
   plaintext.
2. Creation requires `If-None-Match: *`, initial `resource_revision=1`, and
   matching `created_raft_index` values. Function starts `Active` with no
   published Route. The default Trigger is enabled and either uses `token`
   authentication with a SHA-256 verifier digest or explicitly uses `public`
   authentication without one.
3. The initial catalog only transitions Function lifecycle `Active <->
   Disabled`, using the exact expected Function `resource_revision`; successful
   state changes advance it by one. It does not pretend to implement deletion:
   FN-009 requires atomically publishing a disabled empty Route, Tombstoning
   every Trigger, and later waiting on Version and Assignment lifecycle data.
4. Read results and snapshots are defensive copies. Lists are ordered by
   `{created_raft_index,id}`. `StateDigest` serializes labels as sorted pairs
   and uses `control-catalog-state`/`v1`, so map iteration cannot affect a
   future full FSM digest.
5. Catalog accepts at most 1000 Functions, the Local Profile hard safety
   maximum. A full catalog rejects a new unique Function with `overloaded`.
   Function CAS failures expose a typed safe detail set: `revision_kind`,
   `expected_revision`, and `actual_revision`; the future Management adapter
   must pass those values into its public error envelope.

## Consequences

The Catalog is not yet a Raft implementation and does not itself pair its
mutation with the Operation Ledger. The eventual FSM must invoke both in one
committed transaction, preserve the same command values in Snapshot state, and
return the Function/Trigger revisions through the management API. Version
admission, Route CAS, Deployment generation, deletion, and trigger rotation
remain separate slices because their correctness needs those additional state
sets.
