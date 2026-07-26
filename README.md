# MiniCloud

MiniCloud is a distributed WASM function platform implemented in Go. The
normative product and acceptance requirements are defined in
[`MiniCloud-Spec-v1.0.md`](MiniCloud-Spec-v1.0.md).

The implementation is being delivered in the specification's M0 to M3 order.
The repository currently contains the protocol and deterministic-domain
foundation, the `wasi-command-v1` schemas and Go SDK, and a runnable Local Core
process composition. The process owns the Artifact Store, isolated Validator,
wazero Engine, compiled Cache, Worker Agent and Registry, serving projection,
Gateway, and hardened HTTP listener. It starts with an authoritative empty
serving view and shuts down cleanly on SIGINT or SIGTERM.

This is not yet a complete `v0.1-core` cluster. The authenticated, idempotent
management HTTP boundary remains under construction, so the process cannot yet
create a Function or upload and publish a Version through an external API.

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

## Run The Local Core

Build both the parent process and its disposable validation child:

```sh
mkdir -p bin
go build -trimpath -o bin/minicloud ./cmd/minicloud
go build -trimpath -o bin/minicloud-validator ./cmd/minicloud-validator
```

Start the loopback-only default HTTP listener:

```sh
./bin/minicloud \
  -data-dir .minicloud \
  -validator ./bin/minicloud-validator \
  -listen 127.0.0.1:8080
```

The current empty serving view is observable through the invocation boundary:

```sh
curl -i http://127.0.0.1:8080/invoke/missing/
```

It returns a standard `not_found` Problem response. External listen addresses
require both `-tls-cert` and `-tls-key`. Equivalent configuration can be set
with `MINICLOUD_DATA_DIR`, `MINICLOUD_VALIDATOR`, `MINICLOUD_LISTEN`,
`MINICLOUD_TLS_CERT`, `MINICLOUD_TLS_KEY`, and `MINICLOUD_SYNC_INTERVAL`; flags
take precedence.
