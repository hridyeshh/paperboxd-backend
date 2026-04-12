.PHONY: help dev build docker-up docker-down migrate-up migrate-down migrate-mongo-to-pg sqlc fmt tidy

help:
	@echo "Available commands:"
	@echo "  make dev          - Run development server"
	@echo "  make build        - Build binary"
	@echo "  make docker-up    - Start Docker containers"
	@echo "  make docker-down  - Stop Docker containers"
	@echo "  make migrate-up   - Run migrations"
	@echo "  make migrate-down - Rollback migrations"
	@echo "  make migrate-mongo-to-pg - MongoDB → Postgres (needs MONGO_URI, POSTGRES_URL)"
	@echo "  make sqlc         - Generate sqlc code"
	@echo "  make fmt          - Format code"
	@echo "  make tidy         - Tidy dependencies"

dev:
	go run cmd/api/main.go

build:
	go build -o bin/api cmd/api/main.go

docker-up:
	docker-compose up -d

docker-down:
	docker-compose down

migrate-up:
	migrate -database "$(DATABASE_URL)" -path migrations up

migrate-down:
	migrate -database "$(DATABASE_URL)" -path migrations down

# Requires MONGO_URI and POSTGRES_URL or DATABASE_URL. Put a space between each VAR='...'
# assignment — pasting .../railway'MONGO_URI=... merges URLs and breaks Postgres.
migrate-mongo-to-pg:
	MONGO_URI="$(MONGO_URI)" MONGO_DB="$(MONGO_DB)" POSTGRES_URL="$${POSTGRES_URL:-$${DATABASE_URL}}" go run ./cmd/migrate-mongo-to-pg

sqlc:
	sqlc generate

fmt:
	go fmt ./...

tidy:
	go mod tidy
