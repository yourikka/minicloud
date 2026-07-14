GO ?= go

.PHONY: all build test test-integration test-race test-race-integration fmt vet tidy coverage-check

all: fmt vet test build

build:
	$(GO) build ./...

test:
	$(GO) test ./...

test-integration:
	$(GO) test -tags=integration ./...

test-race:
	$(GO) test -race ./...

test-race-integration:
	$(GO) test -race -tags=integration ./...

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

coverage-check:
	$(GO) run ./cmd/coveragecheck -spec MiniCloud-Spec-v1.0.md -manifest coverage/requirements.json
