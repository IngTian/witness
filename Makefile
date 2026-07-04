# claude-witness — build & install. Pure-Go, CGO disabled (single static binary).
BIN := bin/witness-$(shell go env GOOS)-$(shell go env GOARCH)
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILDTIME  ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -X github.com/IngTian/claude-witness/cmd/commands.version=$(VERSION) -X github.com/IngTian/claude-witness/cmd/commands.commit=$(COMMIT) -X github.com/IngTian/claude-witness/cmd/commands.buildTime=$(BUILDTIME)

.PHONY: build build-all fetch-model install install-opencode uninstall uninstall-opencode doctor test vet fmt clean

## build: compile the binary for this OS/arch into bin/
build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/witness

## build-all: cross-compile for mac+linux, amd64+arm64
build-all:
	@for os in darwin linux; do for arch in amd64 arm64; do \
	  echo "building $$os/$$arch"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" -o bin/witness-$$os-$$arch ./cmd/witness; \
	done; done

## fetch-model: download the embedding model (~448MB, once; idempotent)
fetch-model:
	./scripts/fetch-model.sh

## install: build + fetch model + wire hooks/MCP into Claude Code (idempotent)
install: build fetch-model
	$(BIN) install claude

## install-opencode: build + fetch model + wire OpenCode plugin/MCP (idempotent)
install-opencode: build fetch-model
	$(BIN) install opencode

## uninstall: remove the hooks + MCP server
uninstall: build
	$(BIN) uninstall claude

## uninstall-opencode: remove the OpenCode plugin + MCP server
uninstall-opencode: build
	$(BIN) uninstall opencode

## doctor: verify the embedder + model + config
doctor: build
	GOMLX_BACKEND=go $(BIN) doctor

## test / vet / fmt
test:
	CGO_ENABLED=0 go test ./...
vet:
	CGO_ENABLED=0 go vet ./...
fmt:
	gofmt -w internal cmd

## clean: remove built binaries and the fetched model
clean:
	rm -rf bin assets/e5-small
