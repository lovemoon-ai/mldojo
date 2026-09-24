# MLDojo build. On hosts without direct proxy.golang.org access:
#   make GOPROXY=https://goproxy.cn,direct GO=/path/to/go
# Site-specific settings (gitignored, KEY=value without quotes): copy
# deploy/site.env.example to deploy/site.env.
-include deploy/site.env
GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
MLDOJO_DEFAULT_SERVER := $(or $(MLDOJO_DEFAULT_SERVER),http://localhost:8765)
LDFLAGS := -s -w -X github.com/lovemoon-ai/mldojo/internal/version.Version=$(VERSION) \
	-X github.com/lovemoon-ai/mldojo/cli/internal/commands.defaultServer=$(MLDOJO_DEFAULT_SERVER)
export GOPROXY
export CGO_ENABLED=0

.PHONY: all build dist web test test-sidecar vet fmt clean install-native

all: build dist

build: ## native binaries in bin/
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/mldojo-api ./api/cmd/mldojo-api
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/mldojo ./cli/cmd/mldojo
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/mldojo-agent ./agent/cmd/mldojo-agent

dist: ## agent binaries for every node arch (uploaded by `mldojo node add`) + CLI (served at /dl/)
	@mkdir -p dist
	GOOS=linux  GOARCH=amd64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/mldojo-agent-linux-amd64 ./agent/cmd/mldojo-agent
	GOOS=linux  GOARCH=arm64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/mldojo-agent-linux-arm64 ./agent/cmd/mldojo-agent
	GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/mldojo-agent-darwin-arm64 ./agent/cmd/mldojo-agent
	for p in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do \
	  GOOS=$${p%/*} GOARCH=$${p#*/} $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/mldojo-$${p%/*}-$${p#*/} ./cli/cmd/mldojo || exit 1; done

web: ## static web app in web/out
	cd web && npm ci --no-audit --no-fund && npm run build

test: ## unit tests (+ integration tests when MLDOJO_TEST_DATABASE_URL is set)
	$(GO) test ./...

test-sidecar:
	python3 adapters/queue_sidecar/mock/test_server.py

vet:
	$(GO) vet ./...

fmt:
	gofmt -w agent api cli internal proto recipes adapters sdk

install-native: build dist ## install as systemd --user services (see deploy/native)
	deploy/native/install.sh

clean:
	rm -rf bin dist web/out web/.next
