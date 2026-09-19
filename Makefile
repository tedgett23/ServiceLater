.PHONY: dev-db dev-backend dev-web build test sqlc pack-apps

dev-db:
	docker compose -f docker-compose.dev.yml up -d db

dev-backend:
	cd backend && go run ./cmd/servicelater

dev-web:
	cd web && npm run dev

# Frontend first: it is embedded into the binary.
build:
	cd web && npm run build
	cd backend && go build -o bin/servicelater ./cmd/servicelater

test:
	cd backend && go test ./...
	cd web && npm run check

sqlc:
	cd backend && sqlc generate

pack-apps:
	for a in apps/*/; do backend/bin/servicelater app pack $$a; done
