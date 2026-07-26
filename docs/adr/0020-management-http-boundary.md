# ADR 0020: Local Core Management HTTP Boundary

- Status: accepted for the Local Core MVP
- Date: 2026-07-26

## Context

The Local Core process could execute the full publish path only through its Go
API. The MVP exit criteria require an external management client to create a
Function, upload an Artifact, admit a Version, publish a Route, and query the
result of a write whose response was lost. The boundary must keep the Operation
Ledger contract, the stable problem codes, and the secret-handling rules that
the state layer already enforces.

## Decision

1. `internal/managementhttp` adapts `localcontroller.Controller` to a
   versioned `/v1` API served by the existing hardened `gatewayhttp.Server`.
   The management listener is separate from the invocation listener and is
   disabled until a static management token is configured (NFR-SEC-006).
2. Authentication is one static Bearer token compared as a SHA-256 digest in
   constant time. The Operation principal is the separately configured
   management subject, never the token or its digest (NFR-SEC-001).
3. Every management write requires the `X-Minicloud-Operation-Id` header. The
   authenticated `{principal, namespace, operation_id}` key and canonical
   request enter the Controller operation boundary, so a lost response is
   recoverable by exact retry or `GET /v1/operations/{id}` (RFT-019, RFT-025).
4. Concurrency preconditions are explicit HTTP conditional headers: creates
   require `If-None-Match: *`; lifecycle, token rotation, and Route publication
   require `If-Match: "N"` carrying the expected resource or active-route
   revision. Responses return the primary resource revision as a strong ETag.
5. MVP management paths address Functions by immutable Function ID rather than
   by name. The retained Operation digest covers the canonical path, and a
   non-reusable ID keeps a future delete/recreate cycle from replaying another
   resource's operation. The spec's name-based paths return with the complete
   management surface and its name-resolution step.
6. Responses are dedicated projections. The Trigger verifier digest and the
   Route hashing salt are never serialized, and credential plaintext exists
   only in the single applied create or rotate response. A replayed credential
   response returns `credential_not_replayable` with the resource identifiers
   and current revision in `details` (NFR-SEC-009).
7. Bodies are bounded to the 1 MiB management JSON limit, validated as strict
   bounded JSON, and decoded with unknown fields rejected. Artifact upload is
   `PUT /v1/artifacts/{digest}`: content-addressed, naturally idempotent, and
   limited by the configured artifact size, so it does not consume Operation
   Ledger capacity.
8. Version creation commits the Uploaded Version and its Operation record
   before driving admission. A validator infrastructure failure leaves a
   `Validating` Version with an intact fence for the convergence loop; the
   client polls the Version state instead of retrying the write
   (`docs/notes/management-operation-terminal-boundary.md`).
9. Unclassified write failures map to `operation_unknown`, telling the client
   to query or retry its persistent Operation ID. Classified problems keep
   their stable code and the shared envelope shape.

## Consequences

A fresh process now completes the whole MVP user flow over HTTP, and
`GET /v1/profile` states explicitly that the profile is single-process and
non-durable. The boundary still omits pagination cursors, audit queries,
deletion, deployment updates, rollback, and name-based paths; those arrive
with their own safety preconditions. The M1 Raft FSM will replace the
process-local operation boundary with a replicated apply transaction without
changing the HTTP contract.
