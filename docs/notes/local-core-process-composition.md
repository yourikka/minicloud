# Local Core Process Composition

- Status: implemented runtime boundary; management boundary remains open
- Date: 2026-07-26

## Resource Ownership

`internal/localcore.Runtime` is the single owner of the Local Profile process
graph. Construction proceeds from the Artifact Store and Validator Client to
the Wasm Engine, compiled Cache, Worker Agent, Controller, Worker Registry,
Reconciler, Serving projection, Gateway, and HTTP server. A construction error
unwinds only resources that were successfully acquired.

The Worker Agent takes ownership of the Cache after its constructor succeeds.
Shutdown therefore stops HTTP serving and the convergence loop first, then
disconnects the live-only control authorization and closes Agent, Engine, and
Artifact Store in that order. A deadline failure leaves the current stage owned
by Runtime so `Close` can be retried with a fresh context.

## Worker Bootstrap And Liveness

The Local Profile does not inject a static `WorkerSnapshot`. Each process boot
creates a new Boot ID and Control Connection ID, commits Session Epoch 1 to a
real `workerregistry.Registry`, registers the exact session, and reports one
complete Ready inventory. The registry remains the source consumed by the
Reconciler, preserving session high-water and liveness checks even in one
process.

One serialized `Converge` operation performs Heartbeat, Evaluate, Reconcile,
and Full Sync. `Run` requires an initial successful Converge before opening the
HTTP serving phase. The empty Full Sync is significant: it changes Gateway
Discovery from an unsynchronized cache into an authoritative empty view, so an
unknown Function returns `not_found` instead of a stale-control-plane error.
The configured convergence period cannot exceed the Registry heartbeat period.

## Deliberate Empty-State Boundary

The runnable process currently starts with no Functions, Versions, Routes, or
Assignments. This is an honest runtime-composition milestone, not a complete
Local Core MVP. CLI flags must not import a Wasm module and synthesize those
records because that would bypass Management Token authentication, Operation ID
deduplication, resource revision preconditions, and the required ordering that
commits Assignment intent before Worker RPC.

The next process slice must add a Controller-owned atomic management operation
boundary described in `local-core-management-operation-atomicity.md`. Only then
may an HTTP management adapter create resources, install the fixed local Worker
Assignment, trigger `Converge`, and claim a usable end-to-end Local Core process.

## Remaining Process Evidence

Current tests use a real TCP listener and the production Registry, Engine,
Cache, Agent, serving projection, Gateway, and HTTP server. They prove initial
empty synchronization, Problem response behavior, cancellation-driven graceful
shutdown, running/closed lifecycle fences, and idempotent final close.

The command entry point and a subprocess test still need to prove signal
handling and executable configuration. A valid Wasm upload-to-invocation
subprocess test remains blocked on the management operation boundary rather than
being replaced with a privileged startup shortcut.
