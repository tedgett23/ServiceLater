.PHONY: e2e lint check dev-db dev-backend dev-web build test sqlc pack-apps

dev-db:
	docker compose -f docker-compose.dev.yml up -d db

# Dev-only secret and admin; never reuse these outside a laptop.
dev-backend:
	cd backend && SL_JWT_SECRET=dev-only-secret-dev-only-secret-dev-only SL_BOOTSTRAP_ADMIN_EMAIL=admin@localhost SL_BOOTSTRAP_ADMIN_PASSWORD=admin-password go run ./cmd/servicelater

dev-web:
	cd web && npm run dev

# Frontend first: it is embedded into the binary.
build:
	cd web && npm run build
	cd backend && go build -o bin/servicelater ./cmd/servicelater

test:
	cd backend && SL_TEST_DATABASE_URL=$${SL_TEST_DATABASE_URL:-postgres://servicelater:servicelater@localhost:5432/servicelater_test?sslmode=disable} go test -p 1 ./...
	cd web && npm run check

# Browser tests against the real binary and a scratch database (see web/e2e). Needs Playwright's Chromium:
#   cd web && npx playwright install chromium
e2e: build
	cd web && npm run test:e2e

lint:
	cd backend && PATH="$$PATH:$$(go env GOPATH)/bin" golangci-lint run

# Everything CI should run.
check: lint test

sqlc:
	cd backend && sqlc generate

pack-apps:
	for a in apps/*/; do backend/bin/servicelater app pack $$a; done
