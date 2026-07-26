# Default HTTP Trigger Boundary

- Status: open design constraint
- Date: 2026-07-26

## Trust Boundary

`/invoke/{name}/{path...}` is a public data-plane boundary. Function name,
escaped path, query, headers, body, Request ID, Idempotency Key, and Invocation
Token are attacker-controlled. The handler resolves the Function only from one
atomically readable Gateway Serving view and never consults mutable Controller
state. It invokes the existing Gateway coordinator exactly once after every
request and authentication check succeeds.

## Invocation Token Verification

Token-mode Triggers store only `digest.Sum(token)` in the ServingSnapshot. The
handler accepts exactly one Bearer credential, bounds it to 4096 bytes, hashes
the presented token, and compares the encoded fixed-length digests with
constant-time comparison. Public-mode Triggers allow anonymous calls, but any
present Authorization value is still removed before constructing the Guest
Envelope.

This fast SHA-256 verifier is appropriate only for the specification's
high-entropy, randomly generated Invocation Tokens. It is not a password
storage scheme and must not be reused for human-chosen credentials. Token
creation and rotation must continue to generate cryptographic random values and
return plaintext only once; those management operations remain to be wired to
the replicated catalog.

## Header and Resource Limits

The handler removes Authorization, Request ID, fixed and dynamically nominated
Hop-by-hop headers, proxy forwarding headers, and internal `X-Minicloud-*`
headers before ABI validation. It then calls the SDK's `EncodeRequest` with the
same effective Body, Header, Query, Path, and metadata limits used by the
runtime. Guest response headers may carry end-to-end application values such as
`Set-Cookie`, but cannot override platform correlation headers or Content-Length.

Concurrency admission is a bounded non-blocking semaphore. Per-Gateway rate
admission uses a process-local token bucket, as required by v1; it deliberately
does not claim a global exact quota. A real HTTP server must additionally set
TLS, `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, `IdleTimeout`, and
`MaxHeaderBytes`. Handler tests cannot substitute for those listener controls.

## Remaining Evidence

The integration test now enters through the public HTTP handler and reaches a
real Go/WASI Guest through ServingSnapshot, Gateway routing, local resolution,
Worker authorization, and the compiled cache. It remains in-process and does
not prove TLS peer properties, network RPC serialization, request disconnect
behavior across a transport, Token rotation propagation, or multi-process LKG
expiry. Those remain separate failure and security boundaries.
