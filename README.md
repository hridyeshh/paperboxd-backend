# PaperBoxd Backend

REST API for PaperBoxd: user profiles, books (ISBNdb + Google Books), bookshelf, favorites, reading lists, diary entries, follows, and an activity feed. Written in Go with PostgreSQL, Redis, and [sqlc](https://sqlc.dev/) for type-safe queries.

---

## Stack

| Layer | Technology |
|--------|------------|
| HTTP | [chi](https://github.com/go-chi/chi) |
| Database | PostgreSQL 16 ([pgx/v5](https://github.com/jackc/pgx)) |
| Cache / sessions | Redis |
| Auth | JWT access tokens + hashed refresh tokens in Postgres |
| Migrations | [golang-migrate](https://github.com/golang-migrate/migrate) |
| SQL → Go | [sqlc](https://docs.sqlc.dev/) → `internal/db` |

---

## Prerequisites

- **Go** 1.25+ (see `go.mod`)
- **PostgreSQL** and **Redis** (local or Docker)
- Optional CLI tools: [`migrate`](https://github.com/golang-migrate/migrate), [`sqlc`](https://docs.sqlc.dev/overview/install.html)

---

## Quick start

### 1. Start Postgres and Redis

```bash
make docker-up
```

This uses `docker-compose.yml` (Postgres on `5432`, Redis on `6379`).

### 2. Environment

Create a `.env` in the project root (or export variables). Minimum:

| Variable | Description |
|----------|-------------|
| `DATABASE_URL` | Postgres connection string (**required**) |
| `JWT_SECRET` | Secret for signing JWTs; **≥ 32 characters** (**required**) |

Typical local URL after `make docker-up`:

```bash
export DATABASE_URL="postgres://paperboxd:dev_password_change_in_prod@localhost:5432/paperboxd_dev?sslmode=disable"
export JWT_SECRET="your-development-secret-at-least-32-chars-long"
export REDIS_URL="localhost:6379"
```

Optional:

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `8080` | HTTP listen port |
| `ENVIRONMENT` | `development` | Environment label |
| `REDIS_PASSWORD` | _(empty)_ | Redis password if configured |
| `DB_MAX_CONNS` / `DB_MIN_CONNS` | `25` / `5` | Pool size |
| `GOOGLE_BOOKS_API_KEY` | _(empty)_ | Book search / cache fallback |
| `ISBNDB_API_KEY` | _(empty)_ | Primary external book search |

Configuration is loaded in `internal/config/config.go` (with optional `.env` via `godotenv`).

### 3. Migrations

```bash
export DATABASE_URL="postgres://..."
make migrate-up
```

### 4. Run the API

```bash
make dev
```

Health check: `GET http://localhost:8080/health`  
API base path: `/api/v1` (see [API documentation](docs/API.md)).

---

## Makefile

| Command | Description |
|---------|-------------|
| `make dev` | `go run cmd/api/main.go` |
| `make build` | Build `bin/api` from `cmd/api/main.go` |
| `make docker-up` / `make docker-down` | Start / stop Compose services |
| `make migrate-up` / `make migrate-down` | Apply / roll back migrations (`DATABASE_URL` required) |
| `make sqlc` | Regenerate `internal/db` from `queries/*.sql` |
| `make fmt` | `go fmt ./...` |
| `make tidy` | `go mod tidy` |

---

## Project layout

```
cmd/api/           # HTTP server entrypoint
internal/
  auth/            # Register, login, refresh, logout, /users/me
  config/          # Environment configuration
  db/              # sqlc-generated queries and models (do not edit by hand)
  handler/         # Route handlers (users, books, lists, diary, …)
  middleware/      # JWT authentication
  types/           # Shared request/response and error helpers
  external/        # ISBNdb, Google Books clients
migrations/        # SQL migrations (source of truth for schema)
queries/           # sqlc query files
docs/API.md        # REST API reference
```

---

## sqlc workflow

1. Edit SQL in `migrations/` (schema) and/or `queries/` (named queries).
2. Run:

   ```bash
   make sqlc
   ```

3. Commit updated files under `internal/db/`.
4. Run `go build ./...` to confirm everything compiles.

`sqlc.yaml` points queries at `queries/` and schema at `migrations/`.

---

## API documentation

Full endpoint reference, status codes, and request/response shapes: **[docs/API.md](docs/API.md)**.

---

## Production notes

- Set strong `JWT_SECRET`, restrict CORS origins in `cmd/api/main.go` if needed, and use managed Postgres/Redis (e.g. Railway) with `DATABASE_URL` / `REDIS_URL` as provided by the host.
- The server applies **100 requests per minute per IP** (`httprate` in `main.go`).

---

## License

No license file is present in this repository; all rights reserved unless you add one.
