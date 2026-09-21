.PHONY: build test e2e lint fmt hooks

# Build stamp. Both are compiled into the binary and reported by
# `syncthis version`; a build outside a git checkout falls back to the module
# version the go tool records.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null)
LDFLAGS := $(if $(VERSION),-X github.com/wesbragagt/syncthis/internal/cli.Version=$(VERSION)) \
           $(if $(COMMIT),-X github.com/wesbragagt/syncthis/internal/cli.Commit=$(COMMIT))

build:
	go build -ldflags "$(LDFLAGS)" -o bin/syncthis ./cmd/syncthis

# The end-to-end tests run last and on their own: two daemons over loopback
# compete with the rest of the tree for the machine, and a sync cycle that does
# not get the CPU it needs looks like a test that hangs. Set
# SYNCTHIS_E2E_TIMEOUT_SCALE to a whole number to give them more room still.
test:
	go test $$(go list ./... | grep -v '/internal/e2e$$')
	go test -p 1 -count=1 ./internal/e2e/

# The end-to-end tests run two daemons over loopback. They are kept out of the
# rest of the tree so they get the machine to themselves.
e2e:
	go test ./internal/e2e/ -race -count=1 -p 1

fmt:
	gofmt -l -w .

lint:
	@test -z "$$(gofmt -l .)" || (echo "gofmt: files need formatting"; gofmt -l .; exit 1)
	go vet ./...
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "warning: golangci-lint not found, skipping"; \
	fi

hooks:
	git config core.hooksPath .githooks
	@echo "git hooks installed from .githooks"
