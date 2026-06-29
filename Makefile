# -------------------------------------------------------------------------------
# Oracle Watchdog - Build, Test, Package, and Push
#
# Author: Alex Freidah
#
# Go-based Oracle Cloud instance watchdog. Builds multi-arch container images
# and Debian packages.
# -------------------------------------------------------------------------------

# --- Container registry: DOCKER_REGISTRY env if set, else GHCR under the repo
#     owner (derived from the git remote, so forks build to their own
#     namespace). Override REGISTRY directly to publish elsewhere. ---
REPO_OWNER ?= $(shell git config --get remote.origin.url 2>/dev/null | sed -E 's#(\.git)?$$##; s#.*[/:]([^/]+)/[^/]+$$#\1#')
REGISTRY   ?= $(or $(DOCKER_REGISTRY),ghcr.io/$(REPO_OWNER))
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

integration-test: ## Run integration tests (real Consul via testcontainers — needs Docker)
	go test -race -v -tags integration -count=1 -timeout=600s ./internal/integration/

vet: ## Run Go vet static analysis
	go vet ./...

lint: ## Run Go linter
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.10.1 run ./...

govulncheck: ## Run Go vulnerability scanner
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

# -------------------------------------------------------------------------
# CHANGELOG
# -------------------------------------------------------------------------

changelog: ## Generate CHANGELOG.md from git history
	git cliff -o CHANGELOG.md

# -------------------------------------------------------------------------
# RELEASE
# -------------------------------------------------------------------------

release: ## Tag and push to trigger a GitHub Release (reads .version)
	git tag $(VERSION)
	git push origin $(VERSION)

release-local: prep-changelog ## Dry-run GoReleaser locally (no publish)
	goreleaser release --snapshot --clean

# -------------------------------------------------------------------------
# DEBIAN PACKAGING
# -------------------------------------------------------------------------

prep-changelog: ## Compress changelog for Debian packaging
	@gzip -9 -n -c packaging/changelog > packaging/changelog.gz

deb: prep-changelog ## Build .deb packages via GoReleaser snapshot
	goreleaser release --snapshot --clean --skip=publish

# -------------------------------------------------------------------------
# APTLY PUBLISHING
# -------------------------------------------------------------------------

# --- Endpoint/repo/prefix come from the APTLY_* env vars, with fallbacks. ---
APTLY_URL            ?= $(or $(APTLY_ENDPOINT),https://apt.example.com)
APTLY_REPO           ?= $(or $(APTLY_REPOSITORY),stable)
APTLY_USER           ?= admin
APTLY_PUBLISH_PREFIX ?= $(or $(APTLY_PREFIX),.)
APTLY_DISTRIBUTION   ?= stable
APTLY_ARCHITECTURES  ?= amd64,arm64
DEB_DIR              ?= dist
SNAPSHOT_NAME        ?= $(IMAGE)-$(shell date +%Y%m%d-%H%M%S)

publish-deb: ## Publish .deb packages to Aptly repository
	@if [ -z "$(APTLY_PASS)" ]; then echo "Error: APTLY_PASS not set"; exit 1; fi
	@echo "Publishing packages to $(APTLY_URL)..."
	@for deb in $(DEB_DIR)/*.deb; do \
		echo "Uploading $$(basename $$deb)..."; \
		curl -fsS -u "$(APTLY_USER):$(APTLY_PASS)" \
			-X POST -F "file=@$$deb" \
			"$(APTLY_URL)/api/files/$(IMAGE)" || exit 1; \
	done
	@echo "Adding packages to repo $(APTLY_REPO)..."
	@curl -fsS -u "$(APTLY_USER):$(APTLY_PASS)" \
		-X POST "$(APTLY_URL)/api/repos/$(APTLY_REPO)/file/$(IMAGE)?forceReplace=1" || exit 1
	@echo "Creating snapshot $(SNAPSHOT_NAME)..."
	@curl -fsS -u "$(APTLY_USER):$(APTLY_PASS)" \
		-X POST -H 'Content-Type: application/json' \
		-d '{"Name":"$(SNAPSHOT_NAME)"}' \
		"$(APTLY_URL)/api/repos/$(APTLY_REPO)/snapshots" || exit 1
	@echo "Updating published repo at $(APTLY_PUBLISH_PREFIX) ($(APTLY_DISTRIBUTION))..."
	@set -e; \
	status=$$(curl -sS -u "$(APTLY_USER):$(APTLY_PASS)" -o /dev/null -w '%{http_code}' \
		-X PUT -H 'Content-Type: application/json' \
		-d '{"Snapshots":[{"Component":"main","Name":"$(SNAPSHOT_NAME)"}],"ForceOverwrite":true}' \
		'$(APTLY_URL)/api/publish/$(APTLY_PUBLISH_PREFIX)/$(APTLY_DISTRIBUTION)'); \
	if [ "$$status" = "404" ]; then \
		echo "No publication at $(APTLY_PUBLISH_PREFIX)/$(APTLY_DISTRIBUTION); bootstrapping..."; \
		archs=$$(echo '$(APTLY_ARCHITECTURES)' | sed 's/,/","/g'); \
		curl -fsS -u "$(APTLY_USER):$(APTLY_PASS)" \
			-X POST -H 'Content-Type: application/json' \
			-d "{\"SourceKind\":\"snapshot\",\"Sources\":[{\"Component\":\"main\",\"Name\":\"$(SNAPSHOT_NAME)\"}],\"Architectures\":[\"$$archs\"],\"Distribution\":\"$(APTLY_DISTRIBUTION)\"}" \
			'$(APTLY_URL)/api/publish/$(APTLY_PUBLISH_PREFIX)' || exit 1; \
	elif [ "$$status" != "200" ]; then \
		echo "Publish switch failed with HTTP $$status"; exit 1; \
	fi
	@echo "Cleaning up uploaded files..."
	@curl -fsS -u "$(APTLY_USER):$(APTLY_PASS)" \
		-X DELETE "$(APTLY_URL)/api/files/$(IMAGE)" || true
	@echo "Published successfully!"

# -------------------------------------------------------------------------
# WEBSITE
# -------------------------------------------------------------------------

WEB_IMAGE  := $(REGISTRY)/oracle-watchdog-web
WEB_TAG    ?= $(VERSION)

web-godoc: ## Generate Go API reference markdown for the website (packages discovered dynamically)
	@mkdir -p web/content/godoc
	@# Drop previously generated package pages so removed packages disappear; keep the section index.
	@find web/content/godoc -maxdepth 1 -name '*.md' ! -name '_index.md' -delete 2>/dev/null || true
	@for dir in $$(go list -f '{{.Dir}}' ./internal/...); do \
		pkg=$$(basename "$$dir"); \
		desc=$$(go list -f '{{.Doc}}' ./internal/$$pkg | sed 's/"/\\"/g'); \
		echo "  godoc: internal/$$pkg"; \
		printf -- '---\ntitle: "%s"\ndescription: "%s"\n---\n\n' "$$pkg" "$$desc" > web/content/godoc/$$pkg.md; \
		gomarkdoc ./internal/$$pkg >> web/content/godoc/$$pkg.md; \
		sed -i '/^# '"$$pkg"'$$/d' web/content/godoc/$$pkg.md; \
	done

web-serve: web-godoc ## Serve the project website locally
	cd web && hugo serve

web-build: web-godoc ## Build the project website
	cd web && hugo --minify

web-docker: ## Build website Docker image for local architecture
	docker build --pull -f web/Dockerfile -t $(WEB_IMAGE):$(WEB_TAG) .

web-push: builder ## Build and push multi-arch website image to registry
	docker buildx build \
	  --pull \
	  --platform $(PLATFORMS) \
	  -f web/Dockerfile \
	  -t $(WEB_IMAGE):$(WEB_TAG) \
	  --output type=image,push=true \
	  .

# -------------------------------------------------------------------------
# CLEANUP
# -------------------------------------------------------------------------

clean: ## Remove build artifacts
	go clean
	rm -f oracle-watchdog
	rm -rf bin/ dist/
	rm -f packaging/changelog.gz
	docker rmi $(FULL_TAG) 2>/dev/null || true

.PHONY: help builder build docker push test integration-test vet lint govulncheck changelog release release-local prep-changelog deb publish-deb web-godoc web-serve web-build web-docker web-push clean
.DEFAULT_GOAL := help
