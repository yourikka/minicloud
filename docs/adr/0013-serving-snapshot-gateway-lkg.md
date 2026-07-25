# ADR 0013: Serving Snapshot and Gateway LKG Boundary

- Status: accepted for the transport-neutral snapshot and in-process Gateway layer
- Date: 2026-07-25
- Owners: Controller Discovery and Gateway

## Context

Serving configuration is safe only when Function lifecycle, HTTP Trigger
authentication, Route targets, and Ready Endpoints are observed as one complete
view. Independent partial updates can combine a new authorization policy with
an old Route or expose an Endpoint before its Worker fence is installed. A
Gateway may retain a complete view during a control-plane interruption, but
that availability is bounded by one process-local monotonic expiry.

## Decision

1. `internal/discovery` defines a dedicated service projection. The checksum
   covers Function/Trigger/Route/Endpoint fields, the Token Verifier digest,
   internal Route salt, Discovery Epoch, Serving Sequence, and diagnostic UTC
   generation time using JCS with the `serving-snapshot`/`v1` domain.
2. Snapshot building accepts complete candidate evidence and includes an
   Endpoint only when Assignment identity, Route target identity, Worker
   Session, Ready observation, schedulable intent, normal mode, and installed
   Serving Authorization all match. Semantic sets use stable ordering before
   checksumming.
3. `internal/gatewaydiscovery` applies a Full Sync as one replacement event and
   applies later per-Function snapshots only at the next global Serving
   Sequence. A gap invalidates incremental application until another Full
   Sync. The Publisher starts a higher Discovery Epoch at Sequence 1; a late
   joining Gateway may Full Sync from any current positive global Sequence. Old
   or regressing epochs/revisions are rejected. A Function, Trigger, or Route
   projection also cannot change while its own revision remains unchanged;
   actual Endpoint membership may change under the same control-object
   revisions.
4. All components of one Function snapshot share one monotonic receive point.
   Lookup expires exactly at `serving_max_stale`; zero stale time requires a
   currently connected Watch. Clock regression fails closed permanently for
   the process. Local health may suppress an existing Endpoint but cannot add,
   replace, or extend the snapshot lifetime. The Go `Config` distinguishes the
   default from an explicit zero window: `nil` `MaxStale` uses the five-minute
   default and a non-nil zero duration selects connected-Watch-only service.
5. `internal/gatewayroute` selects only within a Route Target already chosen
   by `routing.Select`. It uses local least-inflight occupancy and canonical
   Assignment-ID Round-robin ties. A copied or repeated release is idempotent,
   and the balancer cannot silently select a different weighted target.

## Hard boundaries recorded

- The snapshot publisher cannot prove current Raft leadership or recent
  majority contact. A future Controller adapter must provide that proof before
  publishing or refreshing a snapshot.
- The Builder consumes the exact installed Authorization Fence but does not
  perform or authenticate the Worker RPC. The Controller must receive the exact
  Worker Authorization ACK before publishing the Endpoint.
- Full Sync transport framing, Watch Cursor encoding, bounded send queues,
  cursor expiry, Gateway ACK membership, and propagation SLOs remain RPC and
  multi-process responsibilities.
- A redacted diagnostic export requires a separate `redacted-diagnostic`
  domain, excludes Token Verifier material, and is never accepted by the live
  Store. This layer exposes no disk restore path.
- Full Sync is represented as one atomic event carrying the current global
  Sequence and all Function snapshots. Empty Full Sync is valid. Incremental
  events replace one complete Function snapshot; deletion is represented by a
  complete disabled/tombstoned snapshot rather than an absent partial update.

## Consequences

Checksum, fencing, sequence continuity, monotonic expiry, endpoint suppression,
and target-local least-inflight selection can be verified without choosing a
wire transport. Quorum refresh, real Gateway/Worker RPC ordering, restart Full
Sync, and end-to-end propagation remain incomplete until their owning layers
exist.
