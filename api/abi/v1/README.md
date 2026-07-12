# wasi-command-v1

Each invocation creates a fresh WASI Command instance. The command reads one
`RequestEnvelope` JSON value from stdin, writes one `ResponseEnvelope` JSON
value to stdout, and writes untrusted guest logs only to stderr.

The schemas describe the wire shape. Cross-field and decoded-size rules that
JSON Schema cannot express, including canonical padded Base64, aggregate Header
and Query bytes, case-normalized Header collisions, and body-forbidden status
codes, are enforced by `sdk/go/minicloudabi` and the Worker runtime.

Only ABI version `1.0` is supported. These schemas do not declare WASI Preview
2, the Component Model, WIT, `wasi:http`, or a reusable Reactor ABI.
