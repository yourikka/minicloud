# MiniCloud

MiniCloud is a distributed WASM function platform implemented in Go. The
normative product and acceptance requirements are defined in
[`MiniCloud-Spec-v1.0.md`](MiniCloud-Spec-v1.0.md).

The implementation is being delivered in the specification's M0 to M3 order.
The repository currently contains the protocol and deterministic-domain
foundation, the `wasi-command-v1` schemas and Go SDK, the Local Profile
artifact CAS, and a one-shot wazero admission validator with a parent-process
watchdog. A Worker-side WASI Command execution core now provides fresh guest
instances, strict ABI I/O, deadlines, bounded logs/output, and default-deny host
resources. Its verified loader coalesces cold loads and owns a bounded,
lease-pinned compiled LRU cache. A boot-local Serving Authorization gate fences
old Worker sessions and Leader epochs before synchronous work starts. A Worker
Agent now binds those pieces into policy-verified Ready Replicas and performs
post-queue, pre-instantiation authorization for synchronous calls. It does not
yet claim a runnable `v0.1-core` cluster.

## Development

Go 1.26.5 or newer is required. Earlier Go 1.26 patch releases contain a
standard-library `os.Root` vulnerability reachable by the local artifact CAS.

```sh
make test
make test-integration
make test-race
make test-race-integration
make build
```

Requirement coverage is tracked in `coverage/requirements.json` and checked
against the specification with `make coverage-check`.
