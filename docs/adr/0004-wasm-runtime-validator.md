# ADR 0004: WASM runtime and admission validator

- Status: Accepted
- Date: 2026-07-12

## Context

Admission must compile untrusted WebAssembly without running that work in a
Controller or Management API process. The same compatibility profile will
later be used by Workers, so runtime build, engine configuration, WASI surface,
feature set, and memory tiers must be explicit and cache-key inputs.

## Decision

- Lock the runtime to `github.com/tetratelabs/wazero` v1.12.0. The profile name
  is `wazero-core-v2-v1`; tests assert that the reported version matches Go
  build metadata.
- Enable exactly wazero `api.CoreFeaturesV2`: mutable globals, bulk memory,
  multi-value, non-trapping float-to-int conversion, reference types, sign
  extension operations, and SIMD. Experimental Threads, Tail Call, Extended
  Const, Exception Handling, Typed Function References, Shared Memory, and
  Memory64 are not enabled. Component Model, WIT, Preview 2, `wasi:http`, and
  Reactor ABI are outside v1.
- Use the wazero WASI Preview 1 `wasi_snapshot_preview1` module as the frozen
  function-import catalog. Admission accepts only matching function names and
  signatures from that module, rejects imported memory, and requires a
  module-defined `_start` function with signature `() -> ()`.
- Keep compiler and interpreter as distinct profile values. Production uses
  the compiler; the interpreter remains a compatibility test path. Runtime
  config disables custom-section retention and debug info, closes work on
  context cancellation, and applies the selected 64/128/256/512 MiB linear
  memory tier during compilation.
- Compile every admission candidate in a one-shot `minicloud-validator`
  process. The parent sends one versioned, size-bounded frame over stdin,
  verifies the artifact digest again in the child, accepts one strict bounded
  JSON report on stdout, and kills the child process group on timeout or caller
  cancellation. At most two validations run concurrently; excess work is
  rejected immediately instead of building an unbounded queue.
- The validator receives an empty constructed environment, a private temporary
  directory, a 30 second wall deadline, a 30 second Linux CPU rlimit, a 512 MiB
  per-file rlimit, and a 64-file descriptor rlimit. `GOMEMLIMIT=384MiB` is a Go
  runtime target, not a hard memory quota.
- Standard Go 1.26.5 `GOOS=wasip1 GOARCH=wasm` is the primary toolchain
  fixture. TinyGo is not enabled in v1 until it has its own locked fixture and
  compatibility suite.

## Security boundary

The child never instantiates the untrusted module during admission because a
Wasm Start Section may execute independently of the `_start` export. It only
uses actual wazero validation and compilation, then inspects public compiled
module metadata. Child stderr is bounded and never copied into public errors.

This phase does not claim complete ART-010 or ART-014 isolation. The Linux
release supervisor must still add cgroup v2 process-memory enforcement and a
quota-backed tmpfs or equivalent aggregate temporary-disk limit. macOS and
Windows development profiles provide process separation and wall deadlines but
do not claim Linux-equivalent resource isolation.

The wazero public `CompiledModule` API enumerates imported functions and
memories, but not every imported global, table, or tag. After wazero validates
the whole binary, admission reads the validated Import section's entry count
and requires it to equal the number of public function and memory definitions.
Any otherwise invisible import kind therefore fails closed without executing
the guest. ART-012 remains open until this validator is wired into the full
Version deployment transition and its multi-node consistency gate.

## Consequences

Runtime upgrades require changing the dependency, reported version or profile,
and rerunning protocol, negative-feature, standard Go fixture, race, security,
and cross-platform suites. Worker compiled-cache keys must include the exact
runtime version, engine, ABI, Host API profile, feature profile, and target
platform. The future Worker runtime must configure WASI with no inherited
arguments, environment, filesystem preopens, or network access and prove those
execution properties separately.
