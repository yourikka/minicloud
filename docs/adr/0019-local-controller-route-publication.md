# ADR 0019: Local Controller Route Publication

- Status: accepted for the Local Core MVP
- Date: 2026-07-25

## Context

A validated Version is not callable until its Function has a Route that binds
the exact Version admission epoch, Deployment generation, and effective-policy
digest. The Local Core feature gate permits only one enabled Target at 10000
basis points. It still requires a complete immutable Route snapshot and the
Function active-route pointer to advance atomically under an exact route CAS.

The existing `RouteStore` already owns the aggregate transition and fixed lock
order (`Catalog -> ReleaseStore -> RouteStore`). A controller-side rewrite of
that validation would duplicate a correctness boundary and could introduce an
observable partial state.

## Decision

1. `localcontroller.Controller` owns a `RouteStore` built from the same Catalog
   and ReleaseStore as admission. It exposes `PublishRoute` and `GetRoute`; it
   does not expose the mutable store.
2. `PublishRoute` accepts Function ID, Ready Version ID, and the exact expected
   active-route revision. It reads the Version and Generation 1 Deployment,
   rejects missing, non-Ready, cross-Function, or inactive targets, and derives
   all target identity fields from persisted values. Callers cannot supply a
   policy digest, epoch, generation, or weight.
3. The controller generates a Route ID, Salt ID, and 16-byte salt at the I/O
   boundary. Salt generation is injectable for tests and cryptographically
   random by default; the complete snapshot then enters `RouteStore.Publish` as
   one command with explicit UTC metadata and Applied Index.
4. Concurrent calls carrying the same expected revision rely on the RouteStore
   CAS. Exactly one may publish; the other receives `revision_conflict` and no
   route state changes.

## Consequences

Local Core can now drive the bounded path from a CAS Artifact, through isolated
validation and an immutable Deployment, to a ready single-target Route. It
still does not assign a Worker, produce a ServingSnapshot, expose an HTTP API,
or promise route persistence across process restart. Multi-target weighted
rollout, rollback history, disabled-route mutation, and Route operation replay
remain later work.
