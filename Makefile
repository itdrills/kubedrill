# KubeDrill – developer tasks. Run `make help` for an overview.
BINARY      ?= kubedrill
VERSION     ?= 0.1.0
LDFLAGS     := -s -w -X main.version=$(VERSION)
PLATFORMS   ?= darwin/arm64 darwin/amd64 linux/amd64 linux/arm64
K8S_VERSION ?= v1.37.2
GO          ?= go

.PHONY: help build test e2e lint fmt vet package snapshot brew-formula brew-local-install brew-uninstall smoke-test clean openapi

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-20s %s\n", $$1, $$2}'

build: ## Build bin/kubedrill for the current platform
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/kubedrill

test: ## Run unit and API tests (with the race detector)
	$(GO) test -race -count=1 ./...

e2e: ## Run end-to-end tests with the real kubectl binary
	$(GO) test -tags e2e -count=1 -v ./test/e2e/

fmt: ## Format all Go code
	gofmt -w cmd internal test

vet: ## go vet (including the e2e build tag)
	$(GO) vet ./...
	$(GO) vet -tags e2e ./test/e2e/

GOLANGCI_LINT_VERSION ?= v2.15.0

lint: vet ## gofmt check + go vet + golangci-lint (same pinned version as CI)
	@out=$$(gofmt -l cmd internal test); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run --build-tags e2e ./...

package: ## Cross-compile release tarballs + checksums into dist/
	./scripts/package.sh "$(VERSION)" "$(PLATFORMS)"

snapshot: ## GoReleaser dry run (archives, .deb, .rpm, cask) into dist/, nothing is published
	HOMEBREW_TAP_GITHUB_TOKEN=unused go run github.com/goreleaser/goreleaser/v2@v2.20.3 release --snapshot --clean

brew-formula: ## Render dist/kubedrill.rb for the local darwin tarball
	VERSION=$(VERSION) ./scripts/brew-formula.sh

brew-local-install: ## Package and install through a local Homebrew tap (macOS)
	./scripts/package.sh "$(VERSION)" "darwin/$$(uname -m | sed 's/x86_64/amd64/')"
	VERSION=$(VERSION) ./scripts/brew-local-install.sh

brew-uninstall: ## Uninstall the local formula and remove the local tap
	./scripts/brew-local-install.sh --uninstall

smoke-test: ## Smoke test kubedrill in an isolated sandbox (SMOKE_BINARY=path optional)
	./scripts/smoke-test.sh $(SMOKE_BINARY)

openapi: ## Refresh the embedded upstream OpenAPI v3 documents
	./scripts/update-openapi.sh "$(K8S_VERSION)"

clean: ## Remove build artifacts (bin/ and dist/)
	rm -rf ./bin ./dist
