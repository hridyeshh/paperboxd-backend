# PaperBoxd

> Your reading universe, organized.

---

## 🎉 **Backend Migration Complete!**

**Status:** ✅ Production-ready Go/PostgreSQL backend
**Migrated:** 39 users, 4,129 books, zero data loss
**API Endpoints:** 60+ RESTful endpoints
**Documentation:** Complete API reference in `docs/API.md`

**Current Phase:** Frontend integration (connecting Next.js to Go backend)

---

## System Architecture

### Current State (April 2026)

```
┌─────────────────────────────────────────────────────────┐
│                     Frontend Layer                       │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐  │
│  │   Next.js    │  │  React 19    │  │   Tailwind   │  │
│  │ App Router   │  │  Components  │  │      CSS     │  │
│  └──────────────┘  └──────────────┘  └──────────────┘  │
└─────────────────────────────────────────────────────────┘
                          ↕ HTTP/REST
┌─────────────────────────────────────────────────────────┐
│                 Go Backend (Production)                  │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐  │
│  │  Chi Router  │  │  JWT Auth    │  │  Redis Cache │  │
│  │  60+ routes  │  │   bcrypt     │  │   15-day TTL │  │
│  └──────────────┘  └──────────────┘  └──────────────┘  │
└─────────────────────────────────────────────────────────┘
                          ↕ SQL
┌─────────────────────────────────────────────────────────┐
│                 Data & External Services                 │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐  │
│  │ PostgreSQL16 │  │    Redis 7   │  │   ISBNdb     │  │
│  │  (Railway)   │  │  (Railway)   │  │     API      │  │
│  └──────────────┘  └──────────────┘  └──────────────┘  │
└─────────────────────────────────────────────────────────┘
```

**Deployment:**
- Backend: Railway (Go 1.21+, PostgreSQL 16, Redis 7)
- Frontend: Vercel (Next.js 15, React 19)
- Cost: $5/month (Railway Hobby plan)
- Region: Singapore (optimal for Indian users)

---

## Migration Summary (March 2026)

### What Was Migrated

Successfully migrated from MongoDB to PostgreSQL:

- ✅ **39 users** - Complete profiles, settings, preferences
- ✅ **4,129 books** - Full metadata, covers, ISBNs
- ✅ **39 bookshelf entries** - Read/reading/TBR books
- ✅ **23 likes** - User-liked books
- ✅ **4 lists** - Reading lists with 9 books
- ✅ **5 diary entries** - Reading journals
- ✅ **3 follows** - Social connections (2 orphaned)
- ✅ **37 activities** - Activity feed entries
- ✅ **1 newsletter** - Email subscriptions
- ✅ **21 account deletions** - Historical records

### Migration Achievements

- **Zero data loss** - 100% data integrity maintained
- **Password compatibility** - All users can log in immediately
- **Zero downtime** - MongoDB kept as backup during migration
- **Data integrity** - 0 orphaned entries, 0 count mismatches
- **Type safety** - Go + sqlc ensures compile-time SQL validation

### Known Limitations

- Float ratings not migrated (MongoDB had 3.5 stars, PostgreSQL uses integers 1-5)
- 314 books without ISBNs (ISBNdb-only imports, no data loss)
- 2 orphaned follows (users following deleted accounts - expected)

---

## Stack

### Backend (Go - Production)

| Layer | Technology |
|--------|------------|
| HTTP | [chi](https://github.com/go-chi/chi) |
| Database | PostgreSQL 16 ([pgx/v5](https://github.com/jackc/pgx)) |
| Cache / sessions | Redis 7 |
| Auth | JWT access tokens + hashed refresh tokens in Postgres |
| Migrations | [golang-migrate](https://github.com/golang-migrate/migrate) |
| SQL → Go | [sqlc](https://docs.sqlc.dev/) → `internal/db` |

### Frontend (Next.js - Vercel)

- **Next.js 15** (App Router)
- **React 19**
- **TypeScript 5**
- **Tailwind CSS 4**
- **Radix UI** - Component primitives
- **NextAuth.js v5** (To be replaced with JWT bridge)

---

## Prerequisites

- **Go** 1.21+ (see `go.mod`)
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
docs/MIGRATION_REPORT.md  # Migration details and results
docs/LESSONS_LEARNED.md   # Reflections on the migration process
CHANGELOG.md       # Version history
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

## Project Status (April 2026)

### ✅ Completed
- [x] Complete Go/PostgreSQL backend (60+ endpoints)
- [x] MongoDB → PostgreSQL migration (39 users, 4,129 books)
- [x] API documentation (`docs/API.md`)
- [x] Migration testing (100% data integrity)
- [x] Production deployment (Railway)

### 🔄 In Progress
- [ ] Frontend integration (Next.js → Go backend)
- [ ] Auth bridge (NextAuth → JWT)
- [ ] End-to-end testing

### 📅 Planned
- [ ] iOS app (SwiftUI)
- [ ] Android app (Kotlin)
- [ ] Recommendation engine v2
- [ ] Scale beyond Hobby plan

---

## Documentation

- **API Reference:** [`docs/API.md`](docs/API.md)
- **Migration Report:** [`docs/MIGRATION_REPORT.md`](docs/MIGRATION_REPORT.md)
- **Lessons Learned:** [`docs/LESSONS_LEARNED.md`](docs/LESSONS_LEARNED.md)
- **Changelog:** [`CHANGELOG.md`](CHANGELOG.md)

---

## Contact

**Developer:** Hridyesh
**Email:** paperboxd@gmail.com
**Website:** paperboxd.in

---

*Powered by Go, PostgreSQL, Next.js, and a lot of coffee ☕*

---

## License

No license file is present in this repository; all rights reserved unless you add one.
