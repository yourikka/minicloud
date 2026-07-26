# MiniCloud

MiniCloud is a distributed WASM function platform implemented in Go. The
normative product and acceptance requirements are defined in
[`MiniCloud-Spec-v1.0.md`](MiniCloud-Spec-v1.0.md).

The implementation is being delivered in the specification's M0 to M3 order.
The repository currently contains the protocol and deterministic-domain
foundation, the `wasi-command-v1` schemas and Go SDK, and a runnable Local Core
process composition. The process owns the Artifact Store, isolated Validator,
wazero Engine, compiled Cache, Worker Agent and Registry, serving projection,
Gateway, hardened HTTP listeners, and an authenticated management API with a
persistent-Operation idempotency boundary. It starts with an authoritative
empty serving view and shuts down cleanly on SIGINT or SIGTERM.

This is not yet a complete `v0.1-core` cluster: it is one process without
Raft, restart durability, or multi-node placement, and the management surface
still omits pagination, audit queries, deletion, and rollback.

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

## Management API

The management boundary listens separately from invocation and stays disabled
until a static token is configured through `MINICLOUD_MANAGEMENT_TOKEN` or
`-management-token-file`:

```sh
MINICLOUD_MANAGEMENT_TOKEN=$(openssl rand -base64 32) \
./bin/minicloud \
  -data-dir .minicloud \
  -validator ./bin/minicloud-validator \
  -listen 127.0.0.1:8080 \
  -management-listen 127.0.0.1:8081
```

Every request requires `Authorization: Bearer <token>`. Every write requires a
persistent `X-Minicloud-Operation-Id` header plus an explicit precondition:
`If-None-Match: *` for creates, or `If-Match: "<revision>"` for lifecycle
changes, token rotation, and Route publication. A lost response is recovered
with the same Operation ID or through `GET /v1/operations/{id}`.

The bounded MVP publish flow:

```sh
AUTH="Authorization: Bearer $TOKEN"
# 1. Create the Function and its default HTTP trigger.
curl -sS -X POST -H "$AUTH" -H 'X-Minicloud-Operation-Id: op-create-1' \
  -H 'If-None-Match: *' -d '{"name":"echo","auth_policy":"public"}' \
  http://127.0.0.1:8081/v1/functions
# 2. Upload the built module into content-addressed storage.
DIGEST="sha256:$(sha256sum echo.wasm | cut -d' ' -f1)"
curl -sS -X PUT -H "$AUTH" --data-binary @echo.wasm \
  "http://127.0.0.1:8081/v1/artifacts/$DIGEST"
# 3. Create an immutable Version; validation runs in the isolated child.
curl -sS -X POST -H "$AUTH" -H 'X-Minicloud-Operation-Id: op-version-1' \
  -H 'If-None-Match: *' -d "{\"artifact_digest\":\"$DIGEST\", ...}" \
  "http://127.0.0.1:8081/v1/functions/$FUNCTION_ID/versions"
# 4. Publish the single-target Route for the Ready Version.
curl -sS -X PUT -H "$AUTH" -H 'X-Minicloud-Operation-Id: op-route-1' \
  -H 'If-Match: "0"' -d "{\"version_id\":\"$VERSION_ID\"}" \
  "http://127.0.0.1:8081/v1/functions/$FUNCTION_ID/route"
# 5. Invoke through the serving view once the local worker converges.
curl -sS http://127.0.0.1:8080/invoke/echo/
```

`GET /v1/profile` states the profile limits explicitly: one process, no
replication, and no durability across restart.
