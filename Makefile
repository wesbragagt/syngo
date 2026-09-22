.PHONY: build release test e2e lint fmt hooks e2e-docker-image e2e-docker e2e-docker-soak e2e-docker-clean

# Build stamp. Both are compiled into the binary and reported by
# `syncthis version`; a build outside a git checkout falls back to the module
# version the go tool records.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null)
LDFLAGS := $(if $(VERSION),-X github.com/wesbragagt/syncthis/internal/cli.Version=$(VERSION)) \
           $(if $(COMMIT),-X github.com/wesbragagt/syncthis/internal/cli.Commit=$(COMMIT))

build:
	go build -ldflags "$(LDFLAGS)" -o bin/syncthis ./cmd/syncthis

# A binary that runs on a machine other than the one that built it. A
# dynamically linked build made on NixOS names its interpreter by an absolute
# /nix/store path that does not exist on any other machine, so a copy of it
# dies with exit 127 before it prints anything. CGO_ENABLED=0 removes the
# interpreter; every dependency is pure Go, so nothing is lost. The
# two-machine benchmark scenarios build the binary they copy to the peer with
# exactly these flags. GOOS and GOARCH may be overridden to cross-compile.
release:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/syncthis ./cmd/syncthis

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

# The container suite gives every device its own container, its own network
# address and its own kernel view of the filesystem, so a test can kill a
# daemon, partition a pair or slow a link. It is opt-in: `make test` compiles
# it and skips it.
E2E_IMAGE ?= syncthis-e2e:dev
MINUTES   ?= 30

e2e-docker-image:
	docker build -f internal/e2edocker/Dockerfile \
	  --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(E2E_IMAGE) .

e2e-docker: e2e-docker-image
	SYNCTHIS_E2E_DOCKER=1 SYNCTHIS_E2E_IMAGE=$(E2E_IMAGE) \
	  go test ./internal/e2edocker/ -count=1 -p 1 -timeout 90m -skip TestSoak -v

e2e-docker-soak: e2e-docker-image
	SYNCTHIS_E2E_DOCKER=1 SYNCTHIS_E2E_IMAGE=$(E2E_IMAGE) SYNCTHIS_E2E_SOAK_MINUTES=$(MINUTES) \
	  go test ./internal/e2edocker/ -count=1 -run TestSoak -timeout $$(( $(MINUTES) + 30 ))m -v

# Containers and networks of a run killed before its cleanups ran.
e2e-docker-clean:
	-docker ps -aq --filter label=syncthis.e2e | xargs -r docker rm -f
	-docker network ls -q --filter label=syncthis.e2e | xargs -r docker network rm

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

# Release artifacts for every supported platform. Each binary is built with
# CGO_ENABLED=0 for the same reason `release` is: a dynamically linked build
# made on NixOS names its interpreter by an absolute /nix/store path, so a copy
# of it cannot start on any other machine. -trimpath keeps the build machine's
# paths out of the binary, and the version and commit stamp are the same ones
# `syncthis version` reports.
#
# linux/arm is GOARM=7, the instruction set of the NAS and single-board boxes
# that are not arm64.
DIST_DIR     := dist
DIST_VERSION := $(if $(VERSION),$(VERSION),dev)
# One entry per artifact: GOOS:GOARCH:GOARM, with an empty GOARM where it does
# not apply.
DIST_TARGETS := linux:amd64: linux:arm64: linux:arm:7 darwin:amd64: darwin:arm64: windows:amd64:

.PHONY: dist dist-verify dist-clean

dist: dist-clean
	@mkdir -p $(DIST_DIR)/build
	@for t in $(DIST_TARGETS); do \
	  os=$${t%%:*}; rest=$${t#*:}; arch=$${rest%%:*}; arm=$${rest##*:}; \
	  name=syncthis_$(DIST_VERSION)_$$os-$$arch$${arm:+v$$arm}; \
	  bin=syncthis; [ "$$os" = windows ] && bin=syncthis.exe; \
	  echo "building $$name"; \
	  mkdir -p $(DIST_DIR)/build/$$name; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch GOARM=$$arm \
	    go build -trimpath -ldflags "$(LDFLAGS)" \
	      -o $(DIST_DIR)/build/$$name/$$bin ./cmd/syncthis || exit 1; \
	  if [ "$$os" = windows ]; then \
	    go run _scripts/dist.go zip $(DIST_DIR)/$$name.zip $(DIST_DIR)/build/$$name/$$bin || exit 1; \
	  else \
	    tar -czf $(DIST_DIR)/$$name.tar.gz -C $(DIST_DIR)/build/$$name $$bin || exit 1; \
	  fi; \
	done
	@go run _scripts/dist.go sums $(DIST_DIR)
	@rm -rf $(DIST_DIR)/build
	@ls -l $(DIST_DIR)

# Checks every artifact `dist` produced: the digests match, each archive
# extracts, and the binary inside is statically linked and built for the
# platform its name claims. The binary for this machine's own platform is run,
# so the stamped version is checked against what the build asked for; a binary
# for another platform is only inspected, because this machine cannot run it.
dist-verify:
	@go run _scripts/dist.go check $(DIST_DIR)
	@rm -rf $(DIST_DIR)/verify
	@host_os=$$(go env GOOS); host_arch=$$(go env GOARCH); \
	for t in $(DIST_TARGETS); do \
	  os=$${t%%:*}; rest=$${t#*:}; arch=$${rest%%:*}; arm=$${rest##*:}; \
	  name=syncthis_$(DIST_VERSION)_$$os-$$arch$${arm:+v$$arm}; \
	  bin=syncthis; [ "$$os" = windows ] && bin=syncthis.exe; \
	  dir=$(DIST_DIR)/verify/$$name; mkdir -p $$dir; \
	  if [ "$$os" = windows ]; then \
	    unzip -q -o $(DIST_DIR)/$$name.zip -d $$dir || exit 1; \
	  else \
	    tar -xzf $(DIST_DIR)/$$name.tar.gz -C $$dir || exit 1; \
	  fi; \
	  echo "extract ok  $$name"; \
	  go run _scripts/dist.go static $$dir/$$bin $$os $$arch || exit 1; \
	  if [ "$$os" = "$$host_os" ] && [ "$$arch" = "$$host_arch" ]; then \
	    got=$$($$dir/$$bin version) || exit 1; \
	    case "$$got" in \
	      *"$(DIST_VERSION)"*) echo "version ok  $$name: $$got";; \
	      *) echo "version bad $$name: $$got, want $(DIST_VERSION)"; exit 1;; \
	    esac; \
	  fi; \
	done
	@rm -rf $(DIST_DIR)/verify
	@echo "dist-verify: all artifacts ok"

dist-clean:
	rm -rf $(DIST_DIR)

# The release image, one daemon per container. It is not the end-to-end image:
# see the comment at the top of packaging/container/Dockerfile.
IMAGE ?= syncthis:$(DIST_VERSION)

.PHONY: container

container:
	docker build -f packaging/container/Dockerfile \
	  --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE) .
