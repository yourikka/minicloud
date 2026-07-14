# ADR 0005: Worker WASI Command execution runtime

- Status: Accepted
- Date: 2026-07-14

## Context

The Worker must reuse compiled code without reusing guest state. It also has to
apply the same compatibility policy as admission, enforce ABI and resource
bounds before allocation grows without limit, and keep guest diagnostics out
of platform errors.

## Decision

- Put the exact wazero build, CoreFeaturesV2 selection, compiler/interpreter
  names, fixed memory tiers, binary section inspection, WASI import allowlist,
  and command entrypoint checks in `internal/wasmprofile`. Validator and Worker
  both use this package, so a module cannot become Ready under one profile and
  execute under a looser one.
- Reject a WebAssembly Start Section. A v1 command has one module-defined
  `_start` export with signature `() -> ()`; no guest code runs before that
  entrypoint.
- Compile once per Program, then create a new anonymous wazero Module and a new
  ModuleConfig for every invocation. Module instances and all of their globals,
  memories, descriptors, and handles are closed after success, exit, Trap, or
  cancellation. Compiled code remains reusable until Program close.
- Construct stdin by validating and encoding the typed SDK Request. Override
  its guest-visible Unix deadline with the earlier of the host timeout and
  parent deadline. stdout is a hard in-memory 1.5 MiB writer and is decoded as
  exactly one strict SDK Response. An overflow cancels the invocation and wins
  error classification even if the guest ignores the WASI write error.
- Store at most 256 KiB of stderr per invocation and 16 KiB per line. Extra
  bytes are counted and discarded while writes still return success, so guest
  logging cannot block or fail the business response.
- Do not configure arguments, environment variables, filesystem preopens, or
  socket capabilities. Build the wazero execution context from a clean
  background context and copy only cancellation and the earlier local
  deadline, preventing experimental socket configuration from arriving through
  an RPC context value.
- Use the host wall and monotonic clocks, `crypto/rand.Reader`, `runtime.Gosched`,
  and an invocation-local cancellable nanosleep. wazero's deterministic default
  random and fake clocks are not part of the production profile.
- Bound the Worker runtime to four active invocations and 64 queued calls by
  default. Each Program additionally allows two active and 16 queued calls.
  A call obtains its Program permit before a Worker permit, so a hot Program's
  queued calls cannot reserve otherwise idle Worker capacity from other
  Programs.
  Queue overflow returns `overloaded`; queue deadline returns
  `function_timeout`. These defaults bound the default linear-memory commitment
  to four 128 MiB guests, excluding runtime RSS and compiled code.
- Program and Engine close first reject new operations, then wait for active
  operations through a context-aware drain channel. A canceled wait returns
  promptly, and a failed wazero release remains retryable instead of being
  reported as closed.
- Map output overflow to `output_limit`, invalid stdout to
  `invalid_function_response`, local cancellation/deadline to
  `function_timeout`, and non-zero exit or residual Trap to `function_trap`.
  Public errors never include wazero Trap text, guest stdout, DWARF paths, or
  panic stacks. Guest stderr is returned through its separate bounded field.
- Measure compile, queue, instantiate, and execute durations independently.

## Remaining boundaries

A wall deadline is not a CPU or fuel quota and a linear-memory tier is not a
Worker RSS limit. The future Worker Supervisor must still enforce the
`runtime_cancel_grace` unhealthy/restart transition if a Runtime or Host call
does not stop, and must add Worker-wide log rate/buffer accounting. Artifact
and compiled-cache capacity/eviction, per-entry admission, observability export,
and process RSS budgets are also separate work. Requirements depending on
those boundaries remain planned.

TinyGo, WASI Preview 2, Component Model, WIT, `wasi:http`, Reactor, filesystem,
temporary directory, custom Host API, and outbound network profiles remain
disabled.

## Consequences

The standard Go 1.26.5 fixture now executes end to end and proves fresh global
state, empty args/environment, no host filesystem preopen, strict response
handling, bounded stderr, output overflow, non-zero exit, panic, infinite-loop
deadline, cancellable sleep, and successful invocation after every failure.
Runtime upgrades must rerun this suite under both normal and race builds.
The suite is selected explicitly with the `integration` build tag.
