.PHONY: help dev build docker-up docker-down migrate-up migrate-down sqlc fmt tidy

help:
	@echo "Available commands:"
	@echo "  make dev          - Run development server"
	@echo "  make build        - Build binary"
	@echo "  make docker-up    - Start Docker containers"
	@echo "  make docker-down  - Stop Docker containers"
	@echo "  make migrate-up   - Run migrations"
	@echo "  make migrate-down - Rollback migrations"
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

sqlc:
	sqlc generate

fmt:
	go fmt ./...

tidy:
	go mod tidy
