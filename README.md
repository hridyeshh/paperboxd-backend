# PaperBoxd Backend

> *One Go binary behind every PaperBoxd surface.*

The REST API server for [PaperBoxd](https://paperboxd.in) — a social book-tracking platform inspired by Letterboxd, but built exclusively for books. A single Go service backs the web app, the iOS app, and the Android app.

**Website:** [paperboxd.in](https://paperboxd.in) · **API:** [api.paperboxd.com](https://api.paperboxd.com) · **Contact:** paperboxd@gmail.com

---

## Table of Contents

- [Overview](#overview)
- [Why This Stack](#why-this-stack)
- [Requirements](#requirements)
- [Getting Started](#getting-started)
- [Configuration](#configuration)
- [Architecture](#architecture)
- [Project Layout](#project-layout)
- [Request Lifecycle](#request-lifecycle)
- [Authentication & Two Surfaces](#authentication--two-surfaces)
- [Data Layer (sqlc + migrations)](#data-layer-sqlc--migrations)
- [Caching & Graceful Degradation](#caching--graceful-degradation)
- [Recommendation Engine](#recommendation-engine)
- [Scan & Know](#scan--know)
- [External Services](#external-services)
- [Background Jobs](#background-jobs)
- [The MongoDB → PostgreSQL Migration](#the-mongodb--postgresql-migration)
- [Error Envelope](#error-envelope)
- [Makefile](#makefile)
- [Deployment](#deployment)
- [Conventions](#conventions)
- [Troubleshooting](#troubleshooting)
- [Related Repositories](#related-repositories)

---

## Overview

This is the only backend PaperBoxd has. Every client — Next.js web, SwiftUI iOS, Compose Android — talks to it over REST/JSON. It owns the database, the auth tokens, the recommendation engine, and every third-party integration; clients hold no business logic and no direct database access.

**At a glance:**

| | |
|---|---|
| Language | Go 1.25 |
| HTTP router | [chi v5](https://github.com/go-chi/chi) |
| Database | PostgreSQL 16 via [pgx/v5](https://github.com/jackc/pgx) — no ORM |
| SQL → Go | [sqlc](https://sqlc.dev) (compile-time-checked, `internal/db`) |
| Migrations | [golang-migrate](https://github.com/golang-migrate/migrate), embedded and auto-applied on boot |
| Cache | Redis 7 — soft dependency, degrades to DB-only |
| Auth | Stateless JWT (HS256), Bearer only, no cookies, no server session store |
| Vector search | [pgvector](https://github.com/pgvector/pgvector) for embedding-based recommendations |
| Logging | `log/slog` structured JSON |
| Deployment | Railway (Singapore), single binary |

**Design stance:** one process, one binary, one database. The service is stateless — all session state lives in the signed JWT — so it scales horizontally by running more copies behind the load balancer with nothing to synchronise. Redis and every third-party API are treated as *optional*: if they are down, the request path degrades to a slower-but-correct answer instead of a 500.

---

## Why This Stack

The backend was rewritten from a Node/MongoDB stack. Every choice below was made to remove a class of runtime bug or operational surprise that the previous stack shipped.

| Choice | Why | Rejected alternative |
|---|---|---|
| **Go** | One statically-linked binary, no runtime to provision, predictable memory, real concurrency for the parallel recommendation and scan fan-outs. Fast cold starts matter on Railway. | Node — the stack we migrated *off*; dynamic typing let malformed documents reach production. |
| **PostgreSQL** | Relational data (users, books, shelves, follows, lists) *is* relational. Foreign keys with `ON DELETE CASCADE` make account deletion one statement instead of an application-level sweep. `pgvector` means recommendations live in the same database as the books — no separate vector store to keep in sync. | MongoDB — the source of the data-integrity problems the migration fixed (orphaned refs, count drift, float/int rating mismatch). |
| **sqlc, not an ORM** | Queries are hand-written SQL in `queries/*.sql`; sqlc generates typed Go from them and the migration schema. A wrong column name or type is a **compile error**, not a 3am runtime panic. No hidden N+1s, no query builder to fight. | GORM / ent — runtime reflection, surprising SQL, and the exact stringly-typed footguns we were escaping. |
| **Stateless JWT** | No session table, no Redis session lookup on every request, no sticky sessions. Any instance can serve any request. | Server-side sessions — a shared-state dependency on the hot path. |
| **Redis as a soft dependency** | Cache is a speed optimization, never a correctness requirement. Every cache site nil/error-checks and falls back to Postgres, so a Redis blip degrades latency, not availability. | Redis as a hard dependency — turns a cache outage into a full outage. |
| **golang-migrate, embedded** | Migrations are compiled into the binary (`go:embed`) and applied on startup, so a deploy converges the schema atomically with the code that needs it. No separate migrate step to forget. | Manual migration runs — drift between "code deployed" and "schema migrated". |
| **chi** | Idiomatic `net/http`, no framework lock-in, composable middleware, sub-routers that mirror the URL tree. | A heavyweight framework — more magic, more to learn, no benefit at this size. |

---

## Requirements

| Tool | Version | Notes |
|---|---|---|
| Go | 1.25+ | See `go.mod` |
| PostgreSQL | 16 | With the `vector` extension for recommendations |
| Redis | 7 | Optional locally — the API boots in degraded mode without it |
| Docker | any recent | For `make docker-up` (Postgres + Redis) |
| [`migrate`](https://github.com/golang-migrate/migrate) CLI | optional | Only for manual migration; the server auto-migrates |
| [`sqlc`](https://sqlc.dev) | optional | Only when changing queries |

---

## Getting Started

```bash
git clone git@github.com:hridyesh/paperboxd-backend.git
cd paperboxd-backend
```

### 1. Start Postgres and Redis

```bash
make docker-up
```

Brings up `postgres:16-alpine` on `5432` and `redis:7-alpine` on `6379` from `docker-compose.yml`, each with a healthcheck.

### 2. Environment

```bash
cp .env.example .env
```

Fill in at minimum `DATABASE_URL` and a `JWT_SECRET` of **≥ 32 characters** — the server refuses to start otherwise (`config.Validate`). A typical local `DATABASE_URL` after `make docker-up`:

```
postgres://paperboxd:dev_password_change_in_prod@localhost:5432/paperboxd_dev?sslmode=disable
```

Everything else is optional and degrades gracefully — see [Configuration](#configuration).

### 3. Run the API

```bash
make dev
```

On boot the server: loads config → connects to Postgres → **applies pending migrations** → connects to Redis (warns and continues if unreachable) → wires handlers → starts the nightly cron → listens on `:8080`.

- Deep health check: `GET http://localhost:8080/health`
- Mobile reachability probe: `GET http://localhost:8080/api/health`
- API base paths: `/api/v1` (web) and `/api/mobile` (native) — see [`docs/API.md`](docs/API.md) and [`MOBILE_API.md`](MOBILE_API.md).

Migrations run automatically. Set `AUTO_MIGRATE=false` to skip that step (e.g. when a schema change is gated behind a manual release).

---

## Configuration

All configuration is environment variables, loaded in `internal/config/config.go` (with an optional `.env` via `godotenv`). Full list in [`.env.example`](.env.example).

**Required:**

| Variable | Description |
|---|---|
| `DATABASE_URL` | Postgres connection string |
| `JWT_SECRET` | HMAC signing secret, **≥ 32 chars** — validated at boot |

**Notable optional variables** (each has a documented degraded behavior — the service never crashes on a missing key):

| Variable | Default | Missing-value behavior |
|---|---|---|
| `PORT` | `8080` | — |
| `ENVIRONMENT` | `development` | Dev raises the default rate limit to 5000/min |
| `REDIS_URL` / `REDIS_PASSWORD` | `localhost:6379` | Unreachable → degraded DB-only mode, `/health` reports it |
| `TOKEN_EXPIRY_MOBILE` | `720h` (30 days) | Accepts a Go duration or a plain seconds integer |
| `GOOGLE_OAUTH_ALLOWED_AUDIENCES` | *(empty)* | **Empty = every mobile Google token rejected (fail-closed).** First thing to check if mobile Google sign-in 401s |
| `APPLE_ALLOWED_AUDIENCES` | `com.paperboxd.PaperBoxd` | Bundle-ID allowlist for Sign in with Apple |
| `ISBNDB_API_KEY` / `GOOGLE_BOOKS_API_KEY` | *(empty)* | Book search falls back across providers |
| `COHERE_API_KEY` | *(empty)* | Recommendation embeddings disabled (`NoopEmbedder`) |
| `ANTHROPIC_API_KEY` | *(empty)* | Scan scoring disabled |
| `HARDCOVER_API_TOKEN` | *(empty)* | Scan falls back to Open Library community counts |
| `RESEND_API_KEY` | *(empty)* | `NoopMailer` — OTP endpoints 200 but no email is sent |
| `CLOUDINARY_*` | *(empty)* | Avatar/banner upload endpoints return 503 |
| `INTERNAL_SECRET` | *(empty)* | Guards `/analytics/*` and the web→backend Google server call |
| `RATE_LIMIT_PER_MINUTE` | `100` (prod) | Per Bearer token, else per IP |
| `CORS_ALLOWED_ORIGINS` | localhost:3000/3001 | Browser allowlist; native clients (no `Origin`) always allowed |

The fail-closed defaults are deliberate for anything security-relevant (`GOOGLE_OAUTH_ALLOWED_AUDIENCES`), and fail-open for anything cosmetic (email, uploads, recommendations). The distinction is what keeps a missing third-party key from becoming an outage.

---

## Architecture

```
                         ┌───────────────────────────────────────────┐
   Web  ─┐               │              cmd/api/main.go              │
   iOS  ─┼── HTTPS ──►   │   config · db pool · redis · handlers     │
 Android─┘               └────────────────────┬──────────────────────┘
                                              │  chi.Router
                         ┌────────────────────▼──────────────────────┐
                         │  Global middleware                        │
                         │  RealIP · Logger · Recoverer · Timeout    │
                         │  CORS(AllowOriginFunc) · httprate         │
                         └────────────────────┬──────────────────────┘
                                              │  per-route
                         ┌────────────────────▼──────────────────────┐
                         │  Authenticate / OptionalAuthenticate /    │
                         │  RequireInternalSecret                    │
                         └────────────────────┬──────────────────────┘
                                              │
                         ┌────────────────────▼──────────────────────┐
                         │  Handlers  (internal/handler, auth)       │
                         │  HTTP in ⇄ JSON out, no business rules    │
                         └──────┬───────────────────────┬────────────┘
                                │                       │
                 ┌──────────────▼─────────┐   ┌─────────▼────────────────┐
                 │  Services              │   │  External clients        │
                 │  recommendations,      │   │  ISBNdb, Google Books,   │
                 │  scan, xp, events,     │   │  Hardcover, Cloudinary,  │
                 │  signals, mailer       │   │  Resend, Cohere, Claude  │
                 └──────┬─────────────────┘   └──────────────────────────┘
                        │
          ┌─────────────▼───────────┐        ┌──────────────────────────┐
          │  internal/db (sqlc)     │        │  internal/cache (Redis)  │
          │  typed queries          │        │  soft dependency         │
          └─────────────┬───────────┘        └──────────────────────────┘
                        │
                 ┌──────▼───────────┐
                 │  PostgreSQL 16   │
                 │  + pgvector      │
                 └──────────────────┘
```

**Layer rules:**

- **Handlers** parse the request, call one or more services or queries, and write the response. They contain no business logic worth unit-testing on their own.
- **Services** (`internal/service`) own the multi-step logic — the recommendation funnel, XP math, signal profiles, event recording. They depend on `db.Queries` and external clients, never on `net/http`.
- **`internal/db`** is 100% sqlc-generated. Never edit it by hand; edit `queries/*.sql` and regenerate.
- **`internal/token` is separate from `internal/auth`** on purpose — the middleware validates tokens without importing the auth package, avoiding an import cycle.

---

## Project Layout

```
cmd/
├── api/                     # The server. main.go wires everything.
├── backfill-embeddings/     # One-shot: embed all books for recommendations
├── embed-one/               # Debug: embed a single book
├── dbutil/                  # DB inspection helpers
└── migrate-mongo-to-pg/     # The MongoDB → Postgres migration tool

internal/
├── auth/                    # Register/login/refresh/OTP/Google/Apple, web + mobile handlers
├── cache/                   # Typed Redis wrapper (Get/Set/GetJSON, ErrMiss)
├── config/                  # Env → Config, with Validate()
├── cron/                    # Nightly jobs (profile recompute, soft-delete purge)
├── db/                      # sqlc-GENERATED queries + models — do not hand-edit
├── external/                # ISBNdb, Google Books, Hardcover, Cloudinary clients
├── handler/                 # HTTP handlers, one file per domain
├── middleware/              # Authenticate, OptionalAuthenticate, RequireInternalSecret, rate-limit key
├── reqctx/                  # Typed request-context helpers (user_id in/out)
├── service/                 # Recommendation engine, scan profile, XP, events, signals, mailer
├── token/                   # JWT generate/parse (HS256) — no import cycle with auth
├── types/                   # Shared request/response shapes + the error envelope
└── util/                    # pgvector NULL-safe codec, misc

migrations/                  # 35+ numbered up/down SQL pairs — the schema source of truth
queries/                     # sqlc query files (input to code generation)
docs/                        # API.md, MIGRATION_REPORT.md, LESSONS_LEARNED.md, privacy, terms
```

---

## Request Lifecycle

Every request passes the same global middleware chain (`cmd/api/main.go`), in order:

| Middleware | Role |
|---|---|
| `RealIP` | Trust the proxy's `X-Forwarded-For` so rate limiting and logs see the true client IP |
| `Logger` | One structured log line per request |
| `Recoverer` | A panic becomes a 500, not a crashed process |
| `Timeout(30s)` | Hard ceiling on any single request |
| `CORS` | See below |
| `httprate` | Rate limit, keyed per token-or-IP |

Then per-route auth middleware runs (`Authenticate`, `OptionalAuthenticate`, or `RequireInternalSecret`), the handler executes, and the response is written through the shared error/JSON helpers in `internal/types`.

**The CORS trick that supports web *and* native at once.** Browsers send an `Origin` header; native mobile clients do not. Rather than weaken the browser allowlist to accommodate mobile, the config uses `AllowOriginFunc`: a **missing** `Origin` is treated as a non-browser caller and allowed, while any request that *does* send `Origin` is checked against `CORS_ALLOWED_ORIGINS` unchanged. Web security is preserved; mobile just works.

**Rate limiting** is keyed by Bearer token when one is present, otherwise by IP (`middleware.KeyByAuthorizationOrIP`), so one user on a shared NAT doesn't rate-limit their whole office, and a 429 returns the same JSON error envelope every client already parses.

---

## Authentication & Two Surfaces

The backend serves two auth surfaces from one identity system, because a browser and a native app have different needs.

| | Web (`/api/v1/auth`) | Native (`/api/mobile/auth`) |
|---|---|---|
| Access token TTL | 1 hour | 30 days (`TOKEN_EXPIRY_MOBILE`) |
| Refresh | Short access + long refresh token (hashed, stored in Postgres) | `POST /refresh` re-mints the long-lived token on each app launch |
| Response shape | Web/NextAuth-compatible | Flat `{ token, user }` — what the mobile clients expect |
| Transport | `Authorization: Bearer <jwt>` | Same. **No cookies anywhere.** |

**Why stateless JWT.** The token is an HS256-signed claim carrying `user_id`. Validation is a signature check — no database or Redis lookup on the hot path — so any instance serves any request and the service scales by replication. The tradeoff (you can't instantly revoke a token) is acceptable for a book-tracking app and is bounded by the token TTL.

**Three auth postures**, chosen per route:

- `Authenticate` — 401s without a valid Bearer token. Writes, personal data.
- `OptionalAuthenticate` — parses the token if present, ignores it if missing/invalid, never 401s. Used where identity *changes* the response but isn't *required*: a book's diary entries surface the viewer's own private entries; recommendations personalize when logged in and fall back otherwise; list visibility depends on the requester.
- `RequireInternalSecret` — guards `/analytics/*` with a shared `X-Internal-Secret` header, not a user token. These are operator endpoints, not user endpoints.

**Social sign-in audience enforcement.** Google's `tokeninfo` endpoint validates a token's signature, expiry, and issuer — but **not** that the token was minted for *us*. So both the Google and Apple flows additionally check the token's `aud` claim against an allowlist (`GOOGLE_OAUTH_ALLOWED_AUDIENCES`, `APPLE_ALLOWED_AUDIENCES`). Without this, a valid Google token issued for any other app would authenticate here. The allowlist defaults empty and **fail-closed** for Google — a misconfiguration blocks logins loudly rather than accepting foreign tokens silently.

---

## Data Layer (sqlc + migrations)

**The schema lives in `migrations/`** as numbered `up`/`down` SQL pairs. That is the single source of truth — not a Go struct, not an ORM model.

**Queries live in `queries/*.sql`** as named, hand-written SQL. `sqlc generate` reads both and emits typed Go into `internal/db/`. The result: calling a query is a normal Go function call with typed arguments and typed rows, and a typo or type mismatch fails `go build` rather than a production request.

### sqlc workflow

```bash
# 1. Edit schema in migrations/ and/or a query in queries/
# 2. Regenerate typed Go:
make sqlc
# 3. Commit the updated internal/db/*.go
# 4. Confirm it compiles:
go build ./...
```

`sqlc.yaml` points the generator at `queries/` (queries) and `migrations/` (schema).

**pgvector NULL workaround.** `pgvector-go@v0.4.0`'s pgx scan plan panics on a NULL `vector` column (slice out-of-bounds inside `DecodeBinary`). Every new connection registers a local codec wrapper (`internal/util`, wired via `poolConfig.AfterConnect`) that short-circuits a NULL source to a zero-value vector. This is why books without embeddings scan cleanly instead of crashing the recommendation query.

---

## Caching & Graceful Degradation

Redis is a **cache, not a hard dependency**. This is a load-bearing design decision, stated in `main.go`:

> A boot-time blip should degrade to DB-only, not take the whole API down.

Concretely:

- At boot, an unreachable Redis logs a warning and the server **starts anyway**, in degraded mode.
- Every request-path cache site checks for `cache.ErrMiss` / connection errors and **falls back to Postgres**.
- `/health` reports `503-degraded` when Redis is down, so monitoring sees the degradation even though users don't.

Redis-backed caches include: the recommendation candidate pool (per user), signal profiles, activity-feed checks, leaderboards, author info, and the scan community-research results (24h TTL). Each is a speed optimization whose absence costs latency, never correctness.

---

## Recommendation Engine

`internal/service/recommendation_service.go` — the most involved part of the backend. `GET /api/v1/recommendations/home` returns up to 20 personalized books via a **two-path parallel funnel**:

```
                    ┌──────────────── Path A: Vector ────────────────┐
                    │  user taste vector  →  pgvector cosine search  │
   user  ──►        │  over book embeddings (Cohere)  → top 200      │
                    └────────────────────────┬───────────────────────┘
                                             │           run in parallel
                    ┌──────────────── Path B: Social ────────────────┐
                    │  books read/liked by followed users            │
                    │  +1 per friend who read, +2 per friend liked   │
                    └────────────────────────┬───────────────────────┘
                                             ▼
                    merge & dedupe pool (≤300, books in both keep both scores)
                                             ▼
                    rank (scoreV1 | scoreV2, feature-flagged)
                                             ▼
                    suppress seen  →  dedupe editions  →  MMR diversity
                                             ▼
                    exploration blend  →  top 20
```

**Why two paths.** Vector similarity captures "books like what you love"; the social graph captures "books your friends are into." Neither alone is enough — a new user has no taste vector, a loner has no social signal — so they run concurrently (goroutines + channels) and merge. If both come back empty, a `fallback` path returns popular books. The response is labeled with its source (`vector`, `social`, `vector+social`, `fallback`) for observability.

**Ranking** is behind a `ranking_v2` feature flag. `scoreV1` is the original four-signal formula; `scoreV2` adds temporal signals (reading velocity, diary activity, abandoned-book penalty) computed from the user's signal profile. **MMR (Maximal Marginal Relevance)** then trades a little relevance for diversity so the list isn't ten near-identical editions of one subgenre.

**Signal profiles** are cached per user (fresh < 24h) and recomputed lazily or by the nightly cron. **Embeddings** are produced by Cohere; `cmd/backfill-embeddings` embeds the whole catalog, and the recommendation cache is invalidated whenever a user's bookshelf changes.

---

## Scan & Know

`POST /api/v1/scan/analyze` (`internal/handler/scan.go`) powers the mobile "point your camera at a book, get a 0–100 compatibility score" feature. It is a real scoring pipeline, not a gimmick:

1. **Community research** — in parallel, gather how the world feels about this book: Hardcover community stats (readers/ratings counts), with Open Library as the fallback when `HARDCOVER_API_TOKEN` is unset, plus Brave sentiment queries. Results cache for 24h. *(A cached row with 0 readers **and** 0 ratings is treated as a miss and refetched — that pattern means the source was unreachable when cached, and we won't serve a poisoned zero for a full day.)*
2. **User reading profile** — build `UserReadingProfile` from the reader's shelf: genre distribution, favorite books, repeat authors, average rating, reading pace, whether followed users have read this book.
3. **Claude scoring** — send the book, the community summary, and the reading profile to the Anthropic API, which returns five per-dimension scores (genre fit, writing style, length/complexity, community love, personal fit), a verdict, and for/against reasons. The call retries once on a JSON parse failure.
4. **Quota** — the free-scan quota is decremented **only after a successful score**, so a failed scan never costs the user a scan. `scans_exhausted` returns a dedicated 403.

Two separate HTTP clients (30s for Claude, 10s for community lookups) isolate the slow scoring call from the faster research calls.

---

## External Services

Each third-party client lives in `internal/external` and is constructed once in `main.go`. All are optional; missing credentials log a warning and disable just that feature.

| Service | Used for | Fallback when absent |
|---|---|---|
| **ISBNdb** | Primary book metadata / search | Google Books |
| **Google Books** | Secondary search, cover/metadata fill | Local Postgres results only |
| **Hardcover** | Scan community reader/rating counts | Open Library counts |
| **Brave Search** | Scan sentiment research | Skipped |
| **Cohere** | Book embeddings for recommendations | `NoopEmbedder` — recs disabled |
| **Anthropic (Claude)** | Scan dimension scoring | Scan disabled |
| **Cloudinary** | Avatar / banner upload (server-signed) | Upload endpoints 503 |
| **Resend** | Transactional email (OTP) | `NoopMailer` — endpoints 200, no mail |

Book search is **local-first**: Postgres is queried before any external provider, so the common case is a fast DB hit and the external APIs are only touched on a miss.

---

## Background Jobs

`internal/cron/nightly.go` starts a goroutine that runs once at boot and every 24h thereafter (non-blocking — no external scheduler needed). Two jobs:

- **`recomputeStaleProfiles`** — refreshes recommendation signal profiles that are missing or older than 24h, up to 100 users per run, so recommendations stay warm without recomputing on the request path.
- **`purgeSoftDeletedUsers`** — hard-deletes accounts whose `deleted_at` is older than the 30-day retention window. This backs the privacy-policy commitment to erase data within 30 days of a deletion request. Because every user-owned table is `FK ... ON DELETE CASCADE`, a single `DELETE FROM users` removes the shelf, diary, reviews, lists, events, and tokens with it; the `account_deletions` audit row is intentionally *not* FK-linked and is retained for retention analysis.

---

## The MongoDB → PostgreSQL Migration

The backend was migrated from MongoDB to PostgreSQL with **zero data loss**: 39 users, 4,129 books, plus shelves, likes, lists, diary entries, follows, and activities. The migration tool is `cmd/migrate-mongo-to-pg` (run via `make migrate-mongo-to-pg`, needs `MONGO_URI` + `POSTGRES_URL`), with a `preflight` check, an `idmap` to translate Mongo ObjectIDs to Postgres UUIDs, and a `verify` pass that asserts row counts match.

Passwords were preserved so every user could log in immediately post-migration. Full details in [`docs/MIGRATION_REPORT.md`](docs/MIGRATION_REPORT.md) and the reflections in [`docs/LESSONS_LEARNED.md`](docs/LESSONS_LEARNED.md). Known, accepted limitations (float→int rating rounding, 314 ISBN-less books) are documented there.

---

## Error Envelope

Every error response is the same JSON shape, so all three clients parse failures uniformly:

```json
{ "error": "Human readable message", "code": "SNAKE_CASE_CODE" }
```

Codes and helpers live in `internal/types/errors.go` (`types.WriteError`). Representative codes: `UNAUTHORIZED`, `INVALID_TOKEN`, `EXPIRED_TOKEN`, `FORBIDDEN`, `NOT_FOUND`, `VALIDATION_ERROR`, `RATE_LIMITED`, `INTERNAL_ERROR`, plus feature-specific ones like `scans_exhausted`. The middleware and handlers all route through these helpers, so a 401 from auth and a 429 from the rate limiter look structurally identical to the client.

---

## Makefile

| Command | Description |
|---|---|
| `make dev` | `go run cmd/api/main.go` |
| `make build` | Build `bin/api` |
| `make docker-up` / `make docker-down` | Start / stop Postgres + Redis |
| `make migrate-up` / `make migrate-down` | Manual migration (server also auto-migrates on boot) |
| `make migrate-mongo-to-pg` | Run the MongoDB → Postgres migration |
| `make sqlc` | Regenerate `internal/db` from `queries/*.sql` |
| `make fmt` | `go fmt ./...` |
| `make tidy` | `go mod tidy` |

---

## Deployment

- **Host:** Railway — Go binary, managed PostgreSQL 16, managed Redis 7.
- **Region:** Singapore (lowest latency for the primary Indian user base).
- **Schema:** applied automatically on deploy via embedded migrations (`AUTO_MIGRATE` defaults on).
- **Secrets:** Railway environment variables; nothing sensitive is committed.
- **Lifecycle:** the server traps `SIGINT`/`SIGTERM` and shuts down gracefully with a 30s drain, so in-flight requests finish across a deploy.
- **Redis URL:** full `redis://user:pass@host:port` URLs are parsed automatically (Railway provides this form).

Set a strong `JWT_SECRET`, restrict `CORS_ALLOWED_ORIGINS` to real web origins, and populate `GOOGLE_OAUTH_ALLOWED_AUDIENCES` before mobile sign-in will work.

---

## Conventions

- **sqlc is generated — never hand-edit `internal/db`.** Change `queries/` or `migrations/`, then `make sqlc`.
- **Handlers stay thin.** Multi-step logic belongs in `internal/service`, which never imports `net/http`.
- **Every optional dependency degrades, loudly.** A missing key logs a warning at boot and disables exactly one feature — it never panics or 500s the unrelated request paths.
- **Errors go through `types.WriteError`.** No ad-hoc `http.Error` with a bespoke body; the envelope is uniform.
- **Comments explain *why*.** The codebase is dense with rationale comments on the non-obvious calls — the pgvector NULL codec, the CORS `AllowOriginFunc`, the fail-closed Google audience check, the scan quota decrement ordering, the soft-delete cascade. Keep them; they are why the next person doesn't re-break it.
- **Fail-closed on security, fail-open on cosmetics.** Auth audiences reject by default; email and uploads no-op by default.

---

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| Server won't start: `JWT_SECRET must be at least 32 characters` | Set a `JWT_SECRET` of ≥ 32 chars. |
| Server won't start: `DATABASE_URL is required` | Set `DATABASE_URL`. |
| Boot log: `redis unreachable at boot, starting in degraded (DB-only) mode` | Expected without Redis. Start it (`make docker-up`) to re-enable caching. |
| Mobile Google sign-in 401s | `GOOGLE_OAUTH_ALLOWED_AUDIENCES` is empty or missing the client's ID. It fails **closed** — check this first. |
| OTP endpoints 200 but no email arrives | `RESEND_API_KEY` unset → `NoopMailer`. Set it. |
| Avatar upload returns 503 | `CLOUDINARY_*` not configured. |
| Recommendations always `fallback` | `COHERE_API_KEY` unset (embeddings off) or the catalog isn't embedded — run `cmd/backfill-embeddings`. |
| Scan returns 403 `scans_exhausted` | The free quota is used up. Expected. |
| Panic on a NULL vector column | The pgvector NULL codec isn't registered — confirm `poolConfig.AfterConnect` wiring in `main.go`. |
| Schema out of date after deploy | `AUTO_MIGRATE=false` was set, or migrations errored — check boot logs. |

---

## Related Repositories

| Repository | Description | Stack |
|---|---|---|
| `paperboxd` | Web frontend | Next.js 15, React 19, TypeScript 5 |
| `paperboxd-ios` | Native iOS app | Swift 5, SwiftUI |
| `paperboxd-android` | Native Android app | Kotlin, Jetpack Compose |
| `Paperboxd design elements` | Design system & UI specs | CSS tokens, HTML prototypes |

---

## Contact

**Developer:** Hridyesh
**Email:** paperboxd@gmail.com
**Website:** [paperboxd.in](https://paperboxd.in)

---

*Powered by Go, PostgreSQL, pgvector, and a single binary.*
