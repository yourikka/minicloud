# ADR-0010: Transport-Neutral Internal RPC Contract

- Status: accepted for the v1 contract layer
- Date: 2026-07-16
- Owners: Controller, Gateway, and Worker

## Context

The specification requires versioned internal RPC between Controller, Gateway,
and Worker. Every message needs a bounded size, a bounded deadline, and a
per-peer concurrency cap. A receiver must not trust node, role, namespace, or
function identity copied into a user-controlled envelope. Cross-process
deadlines must carry remaining budget only; a receiver may reduce that budget,
but may not extend it.

The transport implementation is not selected yet. Defining a custom frame
format in the contract package would make a later gRPC, HTTP/2, or TCP adapter
an accidental compatibility commitment.

## Decision

`internal/rpc` freezes only transport-neutral primitives:

1. `SchemaVersion` is `minicloud-rpc-v1`. `Header` is version-only and is
   required for both handshakes and messages. `Negotiate` requires exact
   equality and rejects unknown versions before application work begins.
2. The v1 hard bounds are 4 MiB per message, 10 seconds per hop's remaining
   deadline, and 256 concurrent operations per authenticated peer. Concrete
   RPCs may configure lower values. `Limits.Normalize` applies defaults only to
   zero fields; `Limits.Validate` rejects values outside the hard bounds.
3. `ReadMessage` consumes at most `maxBytes+1` bytes and returns
   `ErrMessageTooLarge` before an unbounded remainder can be read. The source
   must already represent one transport-framed message; this helper does not
   invent framing or decide whether a truncated stream is recoverable.
   Transports must run their strict decoder only after this bounded read.
4. `Budget.RemainingNanos` is the sole cross-process deadline field. It is an
   integer duration, not a wall-clock timestamp and not a sender monotonic
   timestamp. `OutboundBudget` derives it from the current context. `WithBudget`
   and `WithRemaining` create a child context using
   `min(incoming, parent remaining, local maximum)` and retain parent
   cancellation.
5. `PeerLimiter` is created per already-authenticated connection. It accepts
   no identity key from an envelope. `Acquire` is cancellation-aware,
   `TryAcquire` is non-blocking, and `Permit.Release` is idempotent, including
   when a caller accidentally copies a permit value. A transport must not use
   blocking `Acquire` as an unbounded receive queue; it should reject at its
   own bounded queue or use `TryAcquire` when fail-fast admission is required.

No explicit goroutine, own timer loop, network socket, TLS configuration, token
verifier, or custom framing format is introduced by this decision. Context
deadline timers are owned by the standard library context returned to the
caller.

## Consequences

The future transport adapter has a small, testable boundary for limits and
deadline propagation and cannot silently grow an unbounded read or peer queue.
The adapter remains responsible for authenticating the connection, attaching
the authenticated identity out of band, framing bytes, and mapping transport
errors to the public problem catalog.

The contract layer does not yet implement RPC-002 Node Token/TLS identity,
RPC-004 Assignment/Cancel/Drain commands, RPC-005 Ready observations, or
RPC-006 Watch Cursor/epoch resynchronization. Those require the transport,
Worker Agent, and ServingSnapshot layers respectively.

## Evidence

- `internal/rpc/rpc.go` implements the version, bounded read, budget, and
  limiter primitives.
- `internal/rpc/rpc_test.go` covers exact and oversized reads, version
  negotiation, limit validation, no-extension deadline clamping, parent
  cancellation, and bounded/idempotent permits.
- The full RPC and multi-process E2E requirements remain `planned` in the
  coverage manifest until a real transport and authenticated peer harness are
  delivered.
