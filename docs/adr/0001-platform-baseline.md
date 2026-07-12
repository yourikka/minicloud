# ADR 0001: Platform and compatibility baseline

- Status: Accepted
- Date: 2026-07-12

## Context

MiniCloud v1.0 requires multiple independently runnable Go processes, a
versioned WASI Command ABI, deterministic control-plane state, and replaceable
Raft, artifact, runtime, and queue adapters. Compatibility choices must be
locked before runtime and persistence work begins.

## Decision

- Use one Go module, `github.com/yourikka/minicloud`, targeting Go 1.26.
- Keep executable wiring in `cmd/` and implementation packages in `internal/`.
- Use manual constructor injection. Interfaces are declared by consuming
  packages and implemented by adapters.
- Keep control-plane metadata separate from artifact bytes, invocations,
  observations, logs, and traces as required by the specification.
- Use SHA-256 with an explicit domain and schema version for every protocol
  digest. JSON inputs are canonicalized with RFC 8785 before hashing.
- Reserve `wasi-command-v1` as the only v1 ABI and `none` as the only v1 custom
  Host API profile. The exact WASM runtime build, WASI import profile, and
  feature allowlist will be locked by a later runtime ADR before M0 execution.
- The reference release environment is Linux amd64. Development profiles must
  remain portable to Linux, macOS, and Windows; non-Linux validator isolation
  will be documented as weaker where OS quotas are unavailable.

## Consequences

The repository does not split into multiple Go modules prematurely. Public API
is limited to the future Go ABI SDK; all platform implementation details remain
internal. Persistence and RPC schemas must carry explicit version fields, and
runtime upgrades must invalidate compiled-module cache keys and rerun the ABI
and security suites.
