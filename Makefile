GO ?= go

.PHONY: all build test test-race fmt vet tidy coverage-check

all: fmt vet test build

build:
	$(GO) build ./...

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

coverage-check:
	$(GO) run ./cmd/coveragecheck -spec MiniCloud-Spec-v1.0.md -manifest coverage/requirements.json
