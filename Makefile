# witness — build & install. Pure-Go, CGO disabled (single static binary).
BIN := bin/witness-$(shell go env GOOS)-$(shell go env GOARCH)
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILDTIME  ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -X github.com/IngTian/witness/cmd/commands.version=$(VERSION) -X github.com/IngTian/witness/cmd/commands.commit=$(COMMIT) -X github.com/IngTian/witness/cmd/commands.buildTime=$(BUILDTIME)

.PHONY: build build-all package-windows npm-opencode-package fetch-model install install-opencode uninstall uninstall-opencode doctor test vet fmt clean

## build: compile the binary for this OS/arch into bin/
build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/witness

## build-all: cross-compile for mac+linux+windows, amd64+arm64
build-all:
	@for os in darwin linux windows; do for arch in amd64 arm64; do \
	  ext=; [ "$$os" = "windows" ] && ext=.exe; \
	  echo "building $$os/$$arch"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" -o bin/witness-$$os-$$arch$$ext ./cmd/witness; \
	done; done

## package-windows: build self-contained Windows zips (exe + prompts + model) for
## each arch. Needs the model present (make fetch-model). Each zip unpacks to a
## witness/ folder holding witness.exe + prompts/ + assets/e5-small, exactly the
## layout `witness install claude` expects — so the user unzips, runs witness.exe
## install, done. The binary finds prompts/ and the model relative to itself.
package-windows: build-all fetch-model
	@command -v zip >/dev/null || { echo "zip not found; install it (apt/brew install zip)"; exit 1; }
	@test -f assets/e5-small/model.onnx || { echo "model missing; run: make fetch-model"; exit 1; }
	@for arch in amd64 arm64; do \
	  stage="bin/pkg/witness"; \
	  echo "packaging windows/$$arch"; \
	  rm -rf bin/pkg && mkdir -p "$$stage/assets/e5-small"; \
	  cp "bin/witness-windows-$$arch.exe" "$$stage/witness.exe"; \
	  cp -R prompts "$$stage/prompts"; \
	  cp assets/e5-small/model.onnx assets/e5-small/tokenizer.json "$$stage/assets/e5-small/"; \
	  cp README.md "$$stage/"; \
	  ( cd bin/pkg && zip -qr "../witness-windows-$$arch.zip" witness ); \
	  echo "  wrote bin/witness-windows-$$arch.zip"; \
	done; \
	rm -rf bin/pkg

## npm-opencode-package: stage prebuilt binaries/prompts and verify the npm package
npm-opencode-package:
	./scripts/stage-npm-opencode.sh --build
	cd npm/opencode && npm pack --dry-run

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
