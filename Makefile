# Murmur. Run `make` to list the targets; docs/development.md says how they fit together.

GO         ?= go
GO_VERSION := $(shell sed -n 's/^go //p' go.mod)
MODULE     := github.com/EledoneAi/murmur
# "dev" builds never update themselves, so a test laptop stays on the binary you just built.
VERSION    ?= dev
# MURMUR_RELEASE_PUBKEY builds a release key into the binaries: laptops then install only what it signs.
LDFLAGS    := -s -w -X $(MODULE)/internal/release.Version=$(VERSION) -X $(MODULE)/internal/release.Key=$(MURMUR_RELEASE_PUBKEY)
BUILD      := CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)'
PLATFORMS  := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64

# Everything a developer's hub and test laptop write stays in .dev/, never in ~/.claude or ~/.murmur.
DEV        := $(CURDIR)/.dev
PORT       ?= 8080
HUB_URL    ?= http://localhost:$(PORT)
IMAGE      ?= murmur-hub:dev

# Keep in step with the golangci-lint version in .github/workflows/ci.yml.
GOLANGCI_LINT_VERSION ?= v2.15.0
GOLANGCI_LINT ?= $(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
GOVULNCHECK   ?= $(GO) run golang.org/x/vuln/cmd/govulncheck@latest
GORELEASER    ?= $(shell command -v goreleaser 2>/dev/null || echo '$(GO) run github.com/goreleaser/goreleaser/v2@v2.19.0-1015c53f-nightly')
SHA256        := $(shell command -v sha256sum >/dev/null 2>&1 && echo sha256sum || echo 'shasum -a 256')

# The gated store and bus tests run against these (see make services).
TEST_ENV := STORE_TEST_POSTGRES='postgres://postgres:postgres@localhost:15432/postgres?sslmode=disable' \
	BUS_TEST_POSTGRES='postgres://postgres:postgres@localhost:15432/postgres?sslmode=disable' \
	BUS_TEST_REDIS='redis://localhost:16379' \
	FIRESTORE_EMULATOR_HOST='localhost:18086'

# The test laptop's murmur, and the folder of each coding tool murmur connects. A development build connects a
# tool only when its variable is set (see harness.Guard). OpenCode loads OPENCODE_CONFIG_DIR besides its own folder.
LAPTOP_ENV := MURMUR_HOME='$(DEV)/home' MURMUR_HUB='$(HUB_URL)' CLAUDE_CONFIG_DIR='$(DEV)/claude' CODEX_HOME='$(DEV)/codex' \
	GEMINI_CLI_HOME='$(DEV)/gemini' COPILOT_HOME='$(DEV)/copilot' CURSOR_CONFIG_DIR='$(DEV)/cursor' OPENCODE_CONFIG_DIR='$(DEV)/opencode'

COMPOSE_DEV := MURMUR_IMAGE=$(IMAGE) PUBLIC_URL=http://localhost DEV_LOGIN=1 \
	docker compose -f deploy/compose/docker-compose.yml

.DEFAULT_GOAL := help

.PHONY: help build test race lint vuln fmt dev dev-dist laptop web web-dev demo compat image snapshot \
	compose-up compose-down services services-down integration clean

help: ## List the targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-z-]+:.*## / {printf "  %-14s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build bin/murmur and bin/murmur-hub for this machine
	$(BUILD) -o bin/murmur ./cmd/murmur
	$(BUILD) -o bin/murmur-hub ./cmd/murmur-hub

test: ## Run the tests
	$(GO) test ./...

race: ## Run the tests with the race detector, as CI does
	$(GO) test -race ./...

lint: ## Run golangci-lint (go vet, staticcheck, errcheck, formatting and more; see .golangci.yml)
	$(GOLANGCI_LINT) run ./...

vuln: ## Check dependencies and the standard library for known vulnerabilities
	$(GOVULNCHECK) ./...

fmt: ## Format the Go code
	gofmt -s -w $$($(GO) list -f '{{.Dir}}' ./...)

dev: dev-dist ## Run a hub on localhost:8080 with dev sign-in, serving freshly built laptop binaries
	$(BUILD) -o bin/murmur-hub ./cmd/murmur-hub
	@echo "hub on $(HUB_URL): sign in with any email. In another terminal: make laptop"
	DEV_LOGIN=1 PORT=$(PORT) PUBLIC_URL=$(HUB_URL) STORE=sqlite://$(DEV)/hub.db DIST_DIR=$(DEV)/dist LOG_FORMAT=text \
		./bin/murmur-hub

dev-dist: ## Cross-build the four laptop binaries and checksums.txt into .dev/dist
	@mkdir -p $(DEV)/dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "building .dev/dist/murmur-$$os-$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags '$(LDFLAGS)' \
			-o $(DEV)/dist/murmur-$$os-$$arch ./cmd/murmur || exit 1; \
	done
	@cd $(DEV)/dist && $(SHA256) murmur-darwin-* murmur-linux-* > checksums.txt

laptop: ## Build bin/murmur and open a shell whose murmur and coding tools keep their settings in .dev/
	@$(BUILD) -o bin/murmur ./cmd/murmur
	@mkdir -p $(DEV)/home $(DEV)/claude $(DEV)/codex $(DEV)/gemini $(DEV)/copilot $(DEV)/cursor $(DEV)/opencode
	@if [ -t 1 ]; then \
		echo "Test laptop shell: MURMUR_HOME=.dev/home, each coding tool's settings in .dev/, hub $(HUB_URL)."; \
		echo "Run murmur join, then claude or another coding tool, and murmur for the console. Type exit to leave."; \
		$(LAPTOP_ENV) PATH="$(CURDIR)/bin:$$PATH" $${SHELL:-/bin/sh} -i; \
		echo "Left the test laptop shell."; \
	else \
		echo "export $(LAPTOP_ENV) PATH=\"$(CURDIR)/bin:\$$PATH\""; \
	fi

web: ## Build the hub's web pages (web/) into internal/hub/webdist, which the hub embeds
	cd web && pnpm install --frozen-lockfile && pnpm build

web-dev: ## Serve the web pages with hot reload on http://localhost:4321, against the hub of make dev
	cd web && pnpm install --frozen-lockfile && HUB=$(HUB_URL) pnpm dev

demo: ## Open the console with made-up teammates and agents, no hub needed
	$(GO) run ./cmd/murmur demo

compat: ## Check murmur against the Claude Code installed on this machine
	MURMUR_COMPAT=1 $(GO) test ./internal/harness/claude/ -run Compat -count=1 -v

image: ## Build the hub image from source, tagged IMAGE (default murmur-hub:dev)
	docker build -f deploy/docker/Dockerfile --build-arg GO_VERSION=$(GO_VERSION) --build-arg VERSION=$(VERSION) \
		-t $(IMAGE) .

snapshot: ## Build a whole release into dist/ with GoReleaser, without publishing anything
	@[ -n "$$MURMUR_RELEASE_KEY" ] || echo "snapshot: signing with a throwaway key; set MURMUR_RELEASE_KEY to use yours"
	MURMUR_RELEASE_KEY="$${MURMUR_RELEASE_KEY:-$$($(GO) run ./deploy/sign -keygen | sed -n 's/^private key[^:]*: //p')}" \
		$(GORELEASER) release --snapshot --clean

compose-up: image ## Run deploy/compose locally on http://localhost (port 80) with dev sign-in
	$(COMPOSE_DEV) up -d
	@echo "hub on http://localhost: sign in with any email"

compose-down: ## Stop the local Compose stack (its data volume stays)
	$(COMPOSE_DEV) down

services: ## Start Postgres, Redis and the Firestore emulator in Docker for make integration
	docker run -d --name murmur-test-postgres -e POSTGRES_PASSWORD=postgres -p 15432:5432 postgres:18
	docker run -d --name murmur-test-redis -p 16379:6379 redis:8
	docker run -d --name murmur-test-firestore -p 18086:8086 gcr.io/google.com/cloudsdktool/google-cloud-cli:emulators \
		gcloud emulators firestore start --host-port=0.0.0.0:8086
	@echo "waiting for Postgres and the Firestore emulator"
	@until docker exec murmur-test-postgres pg_isready -q -U postgres; do sleep 1; done
	@until curl -fs -o /dev/null http://localhost:18086/; do sleep 1; done

integration: ## Run the store and bus tests that need Postgres, Redis and Firestore (start them with make services)
	$(TEST_ENV) $(GO) test -count=1 ./internal/store/... ./internal/bus/...

services-down: ## Remove the containers make services started
	docker rm -f murmur-test-postgres murmur-test-redis murmur-test-firestore

clean: ## Remove bin/, dist/ and .dev/ (the dev hub's data and the test laptop's sign-in go with it)
	rm -rf bin dist .dev
