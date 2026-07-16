# ADR 0009: Route hash compatibility contract

- Status: Accepted
- Date: 2026-07-16

## Context

The v1 specification requires weighted Route selection to use the
`sha256-bps-v1` compatibility contract. The existing Route model only checked
weights and advertised the non-specification value `sha256-v1`; no component
implemented the stable bucket or Target selection algorithm.

A hash name alone is insufficient for interoperability. Every implementation
must agree on field framing, integer encoding, Target order, bucket arithmetic,
and interval boundaries. Concatenating variable-length fields without framing
also allows distinct Function/Affinity pairs to share the same preimage.

## Decision

- Replace the unsupported `sha256-v1` identifier with the exact
  `sha256-bps-v1` value. A Route carrying the old value is invalid rather than
  silently interpreted under a different algorithm.
- Add `internal/routing.Select` as a pure deterministic selector. It validates
  the complete Route, rejects a disabled Route as `function_disabled`, and
  returns the routing Digest, Bucket, and a defensive copy of the selected
  Target.
- Build the SHA-256 preimage from these fields in order:

  1. Domain: the UTF-8 bytes of `sha256-bps-v1`;
  2. Function ID UTF-8 bytes;
  3. Route Revision as an unsigned 64-bit big-endian value;
  4. the 128-bit Salt bytes;
  5. the original Affinity Key bytes.

  Every field, including the fixed-width Revision and Salt, is preceded by its
  unsigned 32-bit big-endian byte length. The Affinity Key is not decoded,
  normalized, case-folded, or otherwise transformed by the selector.
- Interpret the first eight digest bytes as an unsigned big-endian integer
  `u`. Compute `floor(u * 10000 / 2^64)` with integer multiply-high; floating
  point and `% 10000` are not compatible alternatives.
- Clone and sort Targets by UTF-8 byte order of `version_id`, then numeric
  `deployment_generation`. Select the first cumulative interval whose upper
  bound is strictly greater than the Bucket. Caller order is never mutated.

## Fixed vectors

The executable fixtures in `internal/routing/route_test.go` publish these
cross-implementation vectors:

| Vector | Function / Revision | Salt (hex) | Affinity | Digest | Bucket | Target |
| --- | --- | --- | --- | --- | ---: | --- |
| single | `fn_01` / 1 | `000102030405060708090a0b0c0d0e0f` | UTF-8 `request-123` | `sha256:4cfbeb9d996250b898acc908d8d2a2ae155072414244771547c803d78a2586b6` | 3007 | `ver_01` / 1 |
| multi/raw | `fn_payments` / 42 | `00112233445566778899aabbccddeeff` | hex `637573746f6d65723a00ff` | `sha256:c8f2c66d28ad9fd37d92fe0af3317b023852c580c3780d1b8eba7b86e4f2b942` | 7849 | `ver_b` / 2 |

The multi/raw input deliberately supplies Targets out of order. Their sorted
intervals are `ver_a/1=[0,4500)`, `ver_a/3=[4500,7500)`, and
`ver_b/2=[7500,10000)`.

A separate deterministic fixture hashes 10,000 distinct Affinity Keys against
a 90/10 Route and requires the canary count to remain within 10% plus or minus
1.5 percentage points. This is local algorithm evidence, not the cross-Gateway
benchmark required for acceptance.

## Remaining boundaries

This decision does not implement Gateway Affinity extraction, ServingSnapshot
application, Endpoint least-inflight selection, activation waiting, Route
mutation, or Raft revision compare-and-swap. It also does not enable P1
multi-Target Routes in Core mode. Consequently `RTE-003` remains `planned`
until the Gateway and multi-process acceptance paths use this selector and
prove that a selected Version is never silently replaced when its Endpoints
are unavailable.
