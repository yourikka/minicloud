# ADR 0003: wasi-command-v1 wire contract

- Status: Accepted
- Date: 2026-07-12

## Context

Gateway, Worker, Validator, runtime, fixtures, and guest SDK must parse the same
bounded invocation representation. Standard `encoding/json` behavior alone is
not sufficient because it accepts duplicate object keys by replacement and can
turn JSON nulls into Go zero values.

## Decision

- ABI version `1.0` is a WASI Command protocol with one stdin Request JSON value
  and one stdout Response JSON value. Every invocation gets a fresh instance.
- Publish separate Draft 2020-12 request and response schemas under
  `api/abi/v1`, and a public Go package at `sdk/go/minicloudabi`.
- Read no more than 1.5 MiB before parsing; reject invalid UTF-8, more than 32
  JSON container levels, duplicate keys, unknown fields, trailing values, and
  non-string nulls before typed normalization.
- Require canonical padded standard Base64 and enforce the 1 MiB body limit on
  decoded bytes. Metadata, Header, Query, Method, and Path use the limits in the
  specification.
- Normalize request methods to uppercase. Merge differently cased request
  Header names in wire order, but reject differently cased response names.
- Reject connection-level response Headers, CRLF/NUL field values, status below
  200 or above 599, and bodies on HEAD, 204, or 304 responses.
- Treat `deadline_unix_ms` as guest-visible context only. The future Worker must
  enforce a separately received remaining duration with a local monotonic
  clock; this SDK field never grants more execution time.

## Consequences

Callers must use `DecodeRequest`, `EncodeRequest`, `DecodeResponse`, and
`EncodeResponse` rather than plain `json.Unmarshal` when handling untrusted ABI
data. Gateway and Worker integration remains necessary before ABI-002 and
ABI-009 through ABI-013 can be marked covered. This decision does not add WASI
Preview 2, Component Model, WIT, `wasi:http`, or Reactor support.
