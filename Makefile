.PHONY: build test lint release clean

BIN        := bellerophon
PKG        := github.com/stefandsl/bellerophon-go/cmd/bellerophon
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-alpha)
COMMIT     := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -s -w \
              -X main.version=$(VERSION) \
              -X main.commit=$(COMMIT) \
              -X main.buildDate=$(BUILD_DATE)

build:
	@mkdir -p bin
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BIN) ./cmd/bellerophon

test:
	go test -race -count=1 ./...

lint:
	@command -v golangci-lint >/dev/null || { echo "golangci-lint not installed: https://golangci-lint.run/usage/install/"; exit 1; }
	golangci-lint run ./...

# Cross-compile release artifacts for supported platforms.
release: clean
	@mkdir -p dist
	@set -e; for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do \
	  os=$${target%/*}; arch=$${target#*/}; \
	  out=dist/$(BIN)-$$os-$$arch; \
	  echo "→ $$out"; \
	  GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
	    go build -trimpath -ldflags '$(LDFLAGS)' -o $$out ./cmd/bellerophon; \
	done

clean:
	rm -rf bin dist
