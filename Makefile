# -------------------------------------------------------------------------------
# Oracle Watchdog - Build, Test, Package, and Push
#
# Author: Alex Freidah
#
# Go-based Oracle Cloud instance watchdog. Builds multi-arch container images
# and Debian packages for deployment to the munchbox infrastructure.
# -------------------------------------------------------------------------------

REGISTRY   ?= registry.munchbox.cc
IMAGE      := oracle-watchdog
VERSION    ?= $(shell cat .version)

FULL_TAG   := $(REGISTRY)/$(IMAGE):$(VERSION)
CACHE_TAG  := $(REGISTRY)/$(IMAGE):cache
PLATFORMS  := linux/amd64,linux/arm64

# --- Go build flags ---
GO_LDFLAGS := -s -w -X github.com/afreidah/oracle-watchdog/internal/tracing.Version=$(VERSION)


# -------------------------------------------------------------------------
# DEFAULT TARGET
# -------------------------------------------------------------------------

help: ## Display available Make targets
	@echo ""
	@echo "Available targets:"
	@echo ""
	@grep -E '^[a-zA-Z0-9_-]+:.*?## ' Makefile | \
		awk 'BEGIN {FS = ":.*?## "} {printf "  %-20s %s\n", $$1, $$2}'
	@echo ""

# -------------------------------------------------------------------------
# BUILDX SETUP
# -------------------------------------------------------------------------

builder: ## Ensure the Buildx builder exists
	@docker buildx inspect watchdog-builder >/dev/null 2>&1 || \
		docker buildx create --name watchdog-builder --driver-opt network=host --use
	@docker buildx inspect --bootstrap

# -------------------------------------------------------------------------
# BUILD
# -------------------------------------------------------------------------

build: ## Build the Go binary for the local platform
	CGO_ENABLED=0 go build -ldflags="$(GO_LDFLAGS)" -o oracle-watchdog ./cmd/watchdog

# -------------------------------------------------------------------------
# DOCKER
# -------------------------------------------------------------------------

docker: ## Build Docker image for local architecture
	@echo "Building $(FULL_TAG) for local architecture"
	docker build --pull --build-arg VERSION=$(VERSION) -t $(FULL_TAG) .

# -------------------------------------------------------------------------
# BUILD AND PUSH (MULTI-ARCH)
# -------------------------------------------------------------------------

push: builder ## Build and push multi-arch images to registry
	@echo "Building and pushing $(FULL_TAG) for $(PLATFORMS)"
	docker buildx build \
	  --pull \
	  --platform $(PLATFORMS) \
	  --build-arg VERSION=$(VERSION) \
	  -t $(FULL_TAG) \
	  --cache-from type=registry,ref=$(CACHE_TAG) \
	  --cache-to type=registry,ref=$(CACHE_TAG),mode=max \
	  --output type=image,push=true \
	  .

# -------------------------------------------------------------------------
# DEVELOPMENT
# -------------------------------------------------------------------------

test: ## Run Go tests with coverage
	go test -race -cover ./...

vet: ## Run Go vet static analysis
	go vet ./...

lint: ## Run Go linter
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.10.1 run ./...

govulncheck: ## Run Go vulnerability scanner
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

# -------------------------------------------------------------------------
# DEBIAN PACKAGING
# -------------------------------------------------------------------------

build-linux: ## Build for Linux amd64
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="$(GO_LDFLAGS)" -o bin/oracle-watchdog-linux-amd64 ./cmd/watchdog

build-linux-arm64: ## Build for Linux arm64
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="$(GO_LDFLAGS)" -o bin/oracle-watchdog-linux-arm64 ./cmd/watchdog

prep-changelog: ## Prepare changelog for Debian packaging
	@gzip -9 -n -c packaging/changelog > packaging/changelog.gz

deb: build-linux prep-changelog ## Build Debian package for amd64
	@mkdir -p dist
	@cp bin/oracle-watchdog-linux-amd64 bin/oracle-watchdog-linux
	VERSION=$(VERSION) GOARCH=amd64 nfpm package --packager deb --target dist/
	@rm -f bin/oracle-watchdog-linux

deb-arm64: build-linux-arm64 prep-changelog ## Build Debian package for arm64
	@mkdir -p dist
	@cp bin/oracle-watchdog-linux-arm64 bin/oracle-watchdog-linux
	VERSION=$(VERSION) GOARCH=arm64 nfpm package --packager deb --target dist/
	@rm -f bin/oracle-watchdog-linux

# -------------------------------------------------------------------------
# CLEANUP
# -------------------------------------------------------------------------

clean: ## Remove build artifacts
	go clean
	rm -f oracle-watchdog
	rm -rf bin/ dist/
	rm -f packaging/changelog.gz
	docker rmi $(FULL_TAG) 2>/dev/null || true

.PHONY: help builder build docker push test vet lint govulncheck build-linux build-linux-arm64 prep-changelog deb deb-arm64 clean
.DEFAULT_GOAL := help
