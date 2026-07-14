# ADR 0006: Worker verified loader and compiled cache

- Status: Accepted
- Date: 2026-07-14

## Context

The Worker needs to reuse expensive WebAssembly compilations without reusing
guest instances or trusting local artifact bytes. Concurrent cold requests for
one Version must share work, while unrelated cold requests, queued work, cache
occupancy, and shutdown all remain bounded.

The wazero `CompiledModule` API does not expose the native memory occupied by a
compilation. Treating artifact length as native RSS would therefore create a
false memory guarantee.

## Decision

- Build a comparable cache key from Artifact Digest and size, exact wazero
  runtime name and build, ABI, Host API profile, Wasm feature profile, compiler
  or interpreter selection, memory tier, and `GOOS/GOARCH`. The key profile is
  copied from the real `wasmexec.Engine`; callers cannot describe a different
  compiler configuration.
- Re-open each artifact through the CAS verification boundary, read at most the
  declared size plus one byte, and verify metadata, actual length, and SHA-256
  again before compilation. On `artifact.ErrCorrupt`, retry the source once so
  a source with remote fallback can fetch the blob after the local CAS removes
  it. Other failures are not retried.
- Coalesce concurrent misses for the same complete key into one verify/compile
  task. The cache owns that task's context. Each caller may cancel its own wait
  without canceling other callers; the shared task is canceled when every
  waiter leaves or cache shutdown starts. A failed task is removed and may be
  retried by a later request.
- Admit at most two active and 64 queued distinct cold loads by default. The
  active or queued position is reserved before starting a load goroutine; a
  request beyond both bounds receives `ErrLoadQueueFull`. The active position
  remains held through cache publication and any evicted Program release.
- Use exact LRU ordering and evict only entries with no active Lease. A Lease
  reserves one compiled Program until invocation and release finish, including
  a concurrent `Invoke`/`Release` race. If every candidate is pinned, insertion
  fails with `ErrFull` rather than exceeding capacity.
- Bound the compiled cache independently by 10 GiB of charged verified artifact
  bytes and 4096 entries by default. Both values are configurable only within
  v1 hard bounds. Charged bytes are an eviction weight, not a measurement of
  native compiled memory or Worker RSS.
- Expose hit, miss, coalesced-load, queue-rejection, pinned-capacity rejection,
  close-error, entry-count, charged-byte, and in-flight counters. Capacity
  evictions have stable `capacity_bytes` and `capacity_entries` reasons.
- Cache close rejects new acquisitions, cancels cold work, waits for all Leases
  and shared tasks with the caller's context, and then closes compiled Programs.
  A failed release remains available for a later Close retry.

## Remaining boundaries

The local Artifact CAS is persistent storage, not yet a separately bounded
Worker artifact-download cache. The Worker Supervisor must still impose a
process RSS/runtime-overhead budget because neither artifact-byte charging nor
entry count measures wazero native compilation memory. These gaps keep
RUN-004 open.

Artifact load currently measures CAS open, verification, and read as one phase.
Downloading and verification need separate exported Trace/Metrics phases before
RUN-008 is covered. Replica Ready policy installation and reporting, cache-aware
scheduling, independent cache TTLs, deployment-generation activation waves,
and the E2E-030 scale test are also later integration work.

## Consequences

Unit and race tests use the real local CAS and wazero compiler while injecting
only timing probes. They cover one compile for concurrent same-key requests,
independent waiter cancellation, all-waiter cancellation, bounded distinct-key
loads, retry after corruption or transient failure, byte- and entry-driven LRU
reasons, pinned-entry refusal, shutdown draining, and concurrent Lease release.
