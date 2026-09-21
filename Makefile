.PHONY: build test lint fmt hooks

build:
	go build -o bin/syncthis ./cmd/syncthis

test:
	go test ./...

fmt:
	gofmt -l -w .

lint:
	@test -z "$$(gofmt -l .)" || (echo "gofmt: files need formatting"; gofmt -l .; exit 1)
	go vet ./...
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run ./... || true

hooks:
	git config core.hooksPath .githooks
	@echo "git hooks installed from .githooks"
