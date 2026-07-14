# ADR 0002: Local artifact content-addressed store

- Status: Accepted
- Date: 2026-07-12

## Context

The Local Profile needs a disk-backed artifact service that never exposes a
partial WASM blob, does not place artifact bytes in Raft, and detects cache or
disk corruption before execution. Uploads are untrusted and must be bounded
before they are fully read or allocated.

## Decision

- Store blobs by lowercase `sha256:<hex>` identity below an `os.Root` filesystem
  boundary. No caller-controlled path component is used.
- Default to a 32 MiB upload limit and reject configuration above the v1 hard
  maximum of 256 MiB.
- Stream at most `limit + 1` bytes into a mode `0600` temporary file while
  hashing. A larger stream fails before the remaining body is consumed.
- Sync and close the temporary file, then publish it with an atomic hard link
  inside the same store root. Existing identities are opened and verified,
  making concurrent identical uploads idempotent without overwriting data.
- Sync the blob directory after publication on the Linux reference platform.
  Directory sync is best effort on Windows, where the development profile has
  weaker crash-durability guarantees.
- Serialize publication, verification, and corrupt removal through 256 digest
  lock stripes. This prevents a slow verifier of an old inode from deleting a
  replacement at the same digest path without creating an unbounded lock map.
  A `Put` that finds and removes a corrupt existing blob publishes its already
  verified temporary file in the same critical section.
- Re-read and verify the digest before returning a blob for execution. A corrupt
  existing blob fails closed, is removed, and has its shard directory synced so
  a later Worker fetch or upload can restore the same digest.
- Generate temporary names from 128 bits of `crypto/rand` by default. Tests may
  inject a deterministic reader; production callers do not supply one.

The publication sequence ends before Raft metadata is committed. The future
publish service must commit Version metadata only after `Put` succeeds. A
published but unreferenced blob is therefore an orphan, not a half Version.

## Consequences

The local adapter requires hard-link support and keeps temporary and blob files
on one filesystem. One process-local `Store` instance owns an Artifact root;
the digest stripes do not coordinate multiple Store instances or processes on
one shared root. Such a deployment needs an inter-process lock or an external
object store with native conditional operations. Temporary cleanup, orphan
grace, global mark-and-sweep, and storage fault injection remain separate
follow-up work and must be complete before ART-006 and ART-008 are marked
covered.
