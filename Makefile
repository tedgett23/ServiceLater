.PHONY: e2e lint check apps-validate sync-apps dev-db dev-backend dev-web build test sqlc openapi pack-apps image release-check

# The source revision stamped into the binary (servicelater version).
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null)

dev-db:
	docker compose -f docker-compose.dev.yml up -d db

# Dev-only secret and admin; never reuse these outside a laptop.
dev-backend:
	cd backend && SL_APPS_LOCAL_DIR=$(CURDIR)/apps-local SL_ATTACHMENTS_DIR=$(CURDIR)/data/attachments SL_JWT_SECRET=dev-only-secret-dev-only-secret-dev-only SL_BOOTSTRAP_ADMIN_EMAIL=admin@localhost SL_BOOTSTRAP_ADMIN_PASSWORD=admin-password go run ./cmd/servicelater

dev-web:
	cd web && npm run dev -- --host

# Frontend first: it is embedded into the binary.
build:
	cd web && npm run build
	cd backend && go build -ldflags "-X main.commit=$(COMMIT)" -o bin/servicelater ./cmd/servicelater

test:
	cd backend && SL_TEST_DATABASE_URL=$${SL_TEST_DATABASE_URL:-postgres://servicelater:servicelater@localhost:5432/servicelater_test?sslmode=disable} go test -p 1 ./...
	cd web && npm run test:unit && npm run check

# Browser tests against the real binary and a scratch database (see web/e2e). Needs Playwright's Chromium:
#   cd web && npx playwright install chromium
e2e: build
	cd backend && go build -o bin/fakeidp ./cmd/fakeidp
	cd web && npm run test:e2e

# First-party apps live in apps/ and are embedded in the binary; the server installs them at startup.
FIRST_PARTY_APPS := core-itsm incident-management change-management cmdb knowledge-base service-catalog problem-management

apps-validate:
	cd backend && go run ./cmd/servicelater app validate $(addprefix ../apps/,$(FIRST_PARTY_APPS))

# Apply the bundled apps and any files in apps-local/ to the dev database now, without restarting the backend.
sync-apps:
	cd backend && SL_APPS_LOCAL_DIR=$(CURDIR)/apps-local go run ./cmd/servicelater app sync

lint:
	cd backend && PATH="$$PATH:$$(go env GOPATH)/bin" golangci-lint run

# Everything CI should run.
check: lint test

sqlc:
	cd backend && sqlc generate

# The API's OpenAPI document, generated from the handlers' source (internal/openapi). Run after changing a route, a
# handler's doc comment, or a request or response type; TestOpenAPISpecIsCurrent fails until you do.
openapi:
	cd backend && go run ./cmd/openapi

pack-apps:
	for a in apps/*/; do backend/bin/servicelater app pack $$a; done

# The release image, tagged with the commit and as latest. CONTAINER=podman works too (where builds cannot reach the
# network on their own, add BUILD_FLAGS=--network=host).
CONTAINER ?= docker
COMPOSE ?= docker compose
image:
	$(CONTAINER) build $(BUILD_FLAGS) --build-arg COMMIT=$(COMMIT) -t servicelater:$(COMMIT) -t servicelater:latest .

# Before tagging a release: build the image, run it in a fresh Compose stack and smoke-test it (deploy/release-check.sh).
# COMPOSE=podman-compose with CONTAINER=podman.
release-check: image
	COMPOSE="$(COMPOSE)" ./deploy/release-check.sh
