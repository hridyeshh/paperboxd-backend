# PaperBoxd Backend

> *One Go binary behind every PaperBoxd surface.*

The REST API server for [PaperBoxd](https://paperboxd.in) — a social book-tracking platform inspired by Letterboxd, but built exclusively for books. A single Go service backs the web app, the iOS app, and the Android app.

**Website:** [paperboxd.in](https://paperboxd.in) · **API:** [api.paperboxd.com](https://api.paperboxd.com) · **Contact:** contact@paperboxd.in

---

## Table of Contents

- [Overview](#overview)
- [Why This Stack](#why-this-stack)
- [Architecture](#architecture)
- [Project Layout](#project-layout)
- [Request Lifecycle](#request-lifecycle)
- [Authentication & Two Surfaces](#authentication--two-surfaces)
- [Data Layer (sqlc + migrations)](#data-layer-sqlc--migrations)
- [Caching & Graceful Degradation](#caching--graceful-degradation)
- [Discovery Engine](#discovery-engine)
- [Search & Jazy](#search--jazy)
- [Feed, Taste Twins & Intelligence](#feed-taste-twins--intelligence)
- [Feature Flags & Rollout](#feature-flags--rollout)
- [Scan & Know](#scan--know)
- [Wrapped](#wrapped)
- [Fusion](#fusion)
- [Privacy, Moderation & Push](#privacy-moderation--push)
- [Analytics](#analytics)
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
| Migrations | [golang-migrate](https://github.com/golang-migrate/migrate), embedded and auto-applied on boot (48 so far) |
| Cache | Redis 7 — soft dependency, degrades to DB-only |
| Auth | Stateless JWT (HS256), Bearer only, no cookies, no server session store |
| Vector search | [pgvector](https://github.com/pgvector/pgvector) for embedding-based recommendations, vibe search and Jazy |
| AI | Anthropic Claude — Sonnet 4.6 for Scan & Know and Jazy's voice, Haiku 4.5 for book-trait extraction · Cohere `embed-v3` (1024-d) |
| Feature flags | Rows in the `feature_flags` table, 60 s in-process cache — flip without a deploy |
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
                         │  RequireInternalSecret /                  │
                         │  RequireProfileAccess                     │
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
                 │  search, jazy, feed,   │   │  Hardcover, Brave,       │
                 │  traits, feedback,     │   │  Cloudinary, Resend,     │
                 │  taste overlap, xp,    │   │  Cohere, Claude          │
                 │  events, mailer        │   │                          │
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
- **Services** (`internal/service`) own the multi-step logic — the discovery engine (candidates, ranking, reasons, traits, feedback), search and Jazy, the feed, XP math, event recording. They depend on `db.Queries` and external clients, never on `net/http`.
- **`internal/db`** is 100% sqlc-generated. Never edit it by hand; edit `queries/*.sql` and regenerate.
- **`internal/token` is separate from `internal/auth`** on purpose — the middleware validates tokens without importing the auth package, avoiding an import cycle.

---

## Project Layout

```
cmd/
├── api/                     # The server. main.go wires everything.
├── backfill-embeddings/     # One-shot: embed all books for recommendations
├── backfill-traits/         # One-shot: Claude-extract book traits, rebuild reader trait profiles
├── embed-one/               # Debug: embed a single book
├── dbutil/                  # DB inspection helpers
└── migrate-mongo-to-pg/     # The MongoDB → Postgres migration tool

internal/
├── auth/                    # Register/login/refresh/OTP/Google/Apple, web + mobile handlers
├── cache/                   # Typed Redis wrapper (Get/Set/GetJSON, ErrMiss)
├── config/                  # Env → Config, with Validate()
├── cron/                    # Nightly jobs (signal, diary & trait profiles, taste overlaps, soft-delete purge)
├── db/                      # sqlc-GENERATED queries + models — do not hand-edit
├── external/                # ISBNdb, Google Books, Hardcover, Cloudinary clients
├── handler/                 # HTTP handlers, one file per domain (incl. wrapped, community, device tokens)
├── middleware/              # Authenticate, OptionalAuthenticate, RequireInternalSecret, RequireProfileAccess, rate-limit key
├── reqctx/                  # Typed request-context helpers (user_id in/out)
├── service/                 # Discovery engine: candidates, ranking, reason_engine, traits, feedback,
│                            #   taste_overlap, feed, search + search_intent + search_session,
│                            #   concierge (Jazy), intelligence, book_fit, fusion + fusion_view —
│                            #   plus XP, events, mailer
├── token/                   # JWT generate/parse (HS256) — no import cycle with auth
├── types/                   # Shared request/response shapes + the error envelope
└── util/                    # pgvector NULL-safe codec, misc

migrations/                  # 48 numbered up/down SQL pairs — the schema source of truth
queries/                     # sqlc query files (input to code generation)
docs/                        # API.md, MIGRATION_REPORT.md, LESSONS_LEARNED.md, PRIVACY_AUDIT.md, privacy, terms
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

**Per-route limits** stack on top of the global one wherever a single request is expensive or abusable:

| Route | Limit | Why |
|---|---|---|
| `/api/v1/auth/*`, `/api/mobile/auth/*` | 10/min | Credential guessing and OTP email-bombing; a human never needs more |
| `POST /search`, `/search/vibe`, `/search/context` | 20/min | Every call embeds the query |
| `POST /jazy`, `/scan/analyze` | 10/min | Every call is a paid Claude completion |
| `POST /fusions/invites`, `…/accept` | 10/min | Link minting and the row-locked accept |

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

**Four access postures**, chosen per route:

- `Authenticate` — 401s without a valid Bearer token. Writes, personal data.
- `OptionalAuthenticate` — parses the token if present, ignores it if missing/invalid, never 401s. Used where identity *changes* the response but isn't *required*: a book's diary entries surface the viewer's own private entries; recommendations personalize when logged in and fall back otherwise; list visibility depends on the requester.
- `RequireInternalSecret` — guards `/analytics/*` and `/admin/*` with a shared `X-Internal-Secret` header, not a user token. There is no admin role on users, so a Bearer JWT proves nothing about who may run destructive maintenance.
- `RequireProfileAccess` — mounted on the whole `/users/{username}` subtree. On a private profile it refuses every GET unless the viewer is the owner or an approved follower; the bare profile GET is let through and redacts itself. Mounting it on the subtree means routes added later inherit it.

**Sign in with Apple** (`POST /api/mobile/auth/apple`) mirrors Google: the identity token's `aud` must be in `APPLE_ALLOWED_AUDIENCES` (defaults to the iOS bundle ID), and the Apple `sub` is stored on `users.apple_user_id` so a relay email never splits one reader into two accounts.

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

**Every connection is pinned to UTC.** The same `AfterConnect` hook runs `SET TIME ZONE 'UTC'`. Go and most SQL compute dates in UTC explicitly, but the streak SQL compares against bare `CURRENT_DATE`, which follows the session time zone — pinning it keeps the day a streak is written on and the day it is read on from ever disagreeing.

---

## Caching & Graceful Degradation

Redis is a **cache, not a hard dependency**. This is a load-bearing design decision, stated in `main.go`:

> A boot-time blip should degrade to DB-only, not take the whole API down.

Concretely:

- At boot, an unreachable Redis logs a warning and the server **starts anyway**, in degraded mode.
- Every request-path cache site checks for `cache.ErrMiss` / connection errors and **falls back to Postgres**.
- `/health` reports `503-degraded` when Redis is down, so monitoring sees the degradation even though users don't.

Redis-backed caches include: the recommendation candidate pool (per user), signal profiles, activity-feed checks, leaderboards, author info, the public community snapshot (5 min), book social proof, the scan community-research results (24h TTL), and search/Jazy conversation sessions (30 min). Each is a speed optimization whose absence costs latency, never correctness.

---

## Discovery Engine

`internal/service/` — the most involved part of the backend, rebuilt in seven releases (R0–R7, September 2026). The full build log, with every gap pass, lives in the web repo at `docs/DISCOVERY_ENGINE.md`. `GET /api/v1/recommendations/home` returns up to 20 personalized books, each with a reason sentence and a confidence tier:

```
   ┌─ Path A: Vector ──────────┐ ┌─ Path B: Social ─────────┐ ┌─ Path C: Candidates ───────────────┐
   │ taste vector → pgvector   │ │ books followed readers   │ │ author · TBR neighbours · trending │
   │ cosine over Cohere        │ │ read / loved             │ │ · taste twins                      │
   │ embeddings                │ │                          │ │                                    │
   └─────────────┬─────────────┘ └────────────┬─────────────┘ └──────────────────┬─────────────────┘
                 └────────────────────────────┼──────────────────────────────────┘
                                              │
                   run in parallel (+ hidden-gem retrieval), then merged —
                a book found by several sources keeps every source's evidence
                                              ▼
                     suppress seen & dismissed  →  dedupe editions  →  hydrate community stats
                                              ▼
              scoreV2: embedding · genre · author · velocity · diary · popularity · quality
                       + trait fit − trait clash + recent taste + twins     (each term flag-gated)
                                              ▼
                  diversify: caps on genre, author, length, popularity, familiar authors
                                              ▼
                  exploration interleaved at 20%  →  reason + confidence  →  top 20
```

### Book traits (R1)

Genres are too coarse to explain taste — two "literary fiction" books can be a quiet character study and a propulsive thriller. `cmd/backfill-traits` asks Claude Haiku to score every book on **nine interpretable axes** — character-driven, emotional intensity, plot intensity, pacing, prose density, narrative complexity, darkness, romance centrality, worldbuilding — into `book_traits`, confidence-aware and index-aligned in batches. `trait_service.go` folds a reader's ratings into `trait_prefs` / `trait_dislikes`, weighted so evidence ranks: a rating always outweighs a finished-unrated book (0.3) or a TBR save (0.2) — ten saved doorstops cannot outvote three loved novellas. A 90-day `trait_recent` profile catches drift ("you've been drawn to darker books lately").

### Negative taste (R2)

`POST /recommendations/feedback` takes one of **5 verdicts** (`loved`, `maybe`, `not_for_me`, `not_now`, `already_read`) and optional **reason codes** from `GET /feedback/options`. Rules that keep it honest:

- A reason code moves **only its own axis** — "too slow" teaches pace, not genre.
- `not_now` dismisses for 90 days; `not_for_me` forever.
- `already_read` never trains dislike.
- Abandoning a book before 25% counts as an implicit rejection.
- A `loved` / `not_for_me` verdict recomputes the reader's negative signals asynchronously, so the next request already reflects it — not the next nightly run.

### Ranking 2.0 (R3)

- **Confidence** is evidence-based and capped on a trait clash, then mapped to one of four human tiers. The client never sees a number.
- **`diversify` replaced MMR.** The old MMR measured `1 − |scoreA − scoreB|` — a statement about two numbers that never diversified content. The replacement enforces per-page caps on genre, author, length bucket (≤10 per short/mid/long), popularity (≤8 books with ≥10 shelves) and familiar authors (≤8 already on the reader's shelf).
- **Hidden gems** need a rating floor *and* `ratings_count ≤ 200` globally — Paperboxd's own shelf counts can't tell a gem from a bestseller at this community size.
- **Exploration** is interleaved, not appended, and names what it is stretching away from: *"You don't usually read Historical Fiction, but you love quiet, character-driven stories."*

### Reasons

`reason_engine.go` picks one sentence per book, in priority order: vibe → social → twins → recent → trait → anchor → velocity → diary → author → genre → trending → popular → picked. Each line is gated on its own evidence — "maya loved this" only when every named friend actually rated it 4★+, "because you loved *X*" only when the book is within `anchorMinSim = 0.72` cosine of a shelf anchor. The same engine answers `GET /books/{id}/fit` ("Why you'll like this" on a book page) by ranking a pool of one; it returns **204** when nothing personal applies rather than inventing a line.

---

## Search & Jazy

### Search 2.0

`POST /api/v1/search` is retrieve → hard filters → taste rank → explain.

- **`search_intent.go`** is a deterministic parser, not a model call: 7 intents, page limits ("under 300 pages"), `SimilarTo` anchoring ("like Murakami"), exclusions ("no romance"), axis adjectives and context words. It reads **adjectives only, never topic nouns**, so the embedder still owns meaning.
- **`search_session.go`** keeps a Redis session (30 min TTL, ownership-checked) so one-word follow-ups refine instead of restarting: *"shorter"*, *"less weird"*, *"darker"*. The response carries `understood` / `refined` so clients can quote the accumulated ask back.
- Page bounds are pushed **into** the ANN query (`max_pages` / `min_pages`), not applied after it — otherwise "under 250 pages" over 120 long-book neighbours leaves a handful.

`POST /search/vibe` is the original semantic search; `GET /search/contexts` + `POST /search/context` offer six situation presets (*long flight*, *reading slump*, *just finished something devastating*, *book club*, *stretch*, *comfort*) through the same pipeline.

### Jazy

`POST /api/v1/jazy` — the concierge — is **search with a voice**, not a second engine:

1. **Maybe ask one question.** If the request is too open to answer well, return `jazy#question` with tappable options (comfort-vs-stretch for known readers, pace for strangers). Never a second question — that's an interrogation.
2. **Retrieve and rank** through the search pipeline with the session it already advanced, so constraints and negative taste apply. (It used to advance the session twice per turn, applying "shorter" twice.)
3. **Voice.** Claude writes the intro and per-book reasons from a `ReaderContext` that knows what the reader has read, rated, loved, disliked, abandoned, saved, what their follows loved, their favourite authors, pace, recent drift, previous asks, and the last three **non-private** diary lines. Private diary entries never leave the app.

The answers do real work: `comfort` doubles the taste terms, `surprise` zeroes them and rewards leaving the reader's genres. The voice call is best-effort — on any failure the deck ships with the engine's own reasons. *"A librarian who is briefly hoarse still hands you the books."*

---

## Feed, Taste Twins & Intelligence

### Feed

`GET /recommendations/feed?tz=<IANA zone>` returns a greeting and up to twelve **server-titled modules**, each omitted when empty: *Pick up where you left off* · *Your next read* · *Because you loved X* · *Picked for you* · *Your people are reading* · *Readers with your taste loved these* · taste twin · *You might be ready for* · *Trending among readers like you* · *From your to-read pile* · *You probably haven't discovered this* · *Not your usual thing*. **Your next read** fires only within five days of a finish with nothing in progress. The greeting uses the client's zone — on Railway, server time would say "Good morning" at 17:30 IST.

### Taste twins

`taste_overlap.go` materialises reader pairs nightly — shelf Jaccard + containment, rating agreement, and trait distance, weighted by evidence. `GET /recommendations/twins` returns them with the overlap %, shared authors, and up to three titles you both loved. The pairwise pass is O(n²); the code carries a `ponytail:` note to block by genre cluster past ~5k readers.

### Intelligence

| Endpoint | What |
|---|---|
| `GET /recommendations/surprise` | One book, five modes — `safe`, `unexpected`, `gem`, `obsession`, `wild` — weighted random when no mode is given, always with a because-line |
| `GET /users/me/taste` | Taste dashboard — trait bars, recent shifts, mood, dislikes, and six insights each gated on enough evidence |
| `GET /books/{id}/fit` | "Why you'll like this" for a single book (see [Reasons](#reasons)) |

---

## Feature Flags & Rollout

Flags are rows in `feature_flags`, read through a 60 s in-memory cache (`service/feature_flags.go`), so a flip propagates without a deploy. **Every discovery flag defaults off.**

| Flag | Gates |
|---|---|
| `ranking_v2` | `scoreV2` instead of the original four-signal formula |
| `trait_ranking` | Trait fit / clash terms |
| `negative_taste` | The clash penalty — in home ranking *and* search, so the kill switch means one thing everywhere. Verdict collection runs regardless |
| `recent_taste` | The 90-day drift term |
| `taste_twins` | Twin candidates, twin reasons, twin feed modules |

**Rollout order:** migrations auto-apply on boot → `go run ./cmd/backfill-traits --limit 24 --sample 8` (eyeball known titles before spending on the corpus) → `go run ./cmd/backfill-traits --profiles` → flip flags in the table order above → watch `/analytics/discovery` love-rate by `reason_type` for a week before touching weights.

> Until the trait backfill runs, `book_traits` is empty: trait reasons never fire and the taste dashboard's bars stay blank. That's expected, not a bug.

---

## Scan & Know

`POST /api/v1/scan/analyze` (`internal/handler/scan.go`) powers the mobile "point your camera at a book, get a 0–100 compatibility score" feature. It is a real scoring pipeline, not a gimmick:

1. **Community research** — in parallel, gather how the world feels about this book: Hardcover community stats (readers/ratings counts), with Open Library as the fallback when `HARDCOVER_API_TOKEN` is unset, plus Brave sentiment queries. Results cache for 24h. *(A cached row with 0 readers **and** 0 ratings is treated as a miss and refetched — that pattern means the source was unreachable when cached, and we won't serve a poisoned zero for a full day.)*
2. **User reading profile** — build `UserReadingProfile` from the reader's shelf: genre distribution, favorite books, repeat authors, average rating, reading pace, whether followed users have read this book.
3. **Claude scoring** — send the book, the community summary, and the reading profile to the Anthropic API, which returns five per-dimension scores (genre fit, writing style, length/complexity, community love, personal fit), a verdict, and for/against reasons. The call retries once on a JSON parse failure.
4. **Quota** — a scan is **reserved before** the paid call with an atomic `UPDATE … WHERE scan_uses_remaining > 0 RETURNING`, and every early return refunds it via `defer`. Two concurrent scans with one left can no longer both bill a Claude completion, and a failed scan still never costs the reader a scan. Losing the race returns `scans_exhausted` (a dedicated 403) without calling Claude. Accounts in `SCAN_UNLIMITED_EMAILS` skip the quota — matched exactly, case-insensitive, never as a substring.

Outcomes are recorded server-side as `scan_succeeded` / `scan_failed` (`stage: lookup | scoring`), where the result is actually known; the apps emit `scan_started`.

Two separate HTTP clients (30s for Claude, 10s for community lookups) isolate the slow scoring call from the faster research calls.

---

## Wrapped

`GET /api/v1/users/me/wrapped?month=YYYY-MM&tz=<zone>` builds a monthly reading story from shelf, progress log and diary: books finished, pages, an **estimated** reading time (logged pages at 40 pages/hour — the app records no session durations, and the JSON field names say "estimated"), authors, genres, reading rhythm, streak, the top-rated book, the book that stalled (untouched for 7+ days before month end), a community rank, a reader archetype, and a dare for next month. `has_data: false` means nothing was logged, and every client shows an empty state instead of a story about nothing. Queries live in `queries/wrapped.sql`.

---

## Fusion

Two readers' shelves side by side, made from a one-time link — the Spotify Blend shape. **The link is the consent:** nothing is computed and nobody's ratings are shown to anyone until the invitee taps Fuse. Build log in the web repo at `docs/FUSION.md`; contract in [`MOBILE_API.md`](MOBILE_API.md) §3.12.

```
POST /fusions/invites              mint (or return) your live link — 12 chars, 7-day expiry, works once
GET  /fusions/invites/{token}      preview: valid · expired · used · own · joined · unavailable  (optional auth)
POST /fusions/invites/{token}/accept   the Fuse tap — row-locked, first to tap wins
GET  /fusions · /fusions/{id}      your Fusions · the ten-page story from your side
DELETE /fusions/{id}               removes it for both readers
```

- **Schema** (migration 000048): `fusion_invites` keyed by token, with `consumed_at` as the spent marker — `consumed_by` goes NULL if that reader deletes their account, but the link stays spent. `fusions` stores one row per pair (`user_a < user_b`, like `taste_overlap`) with a `snapshot` jsonb holding the story from *each* side, because the sentences are perspective-dependent.
- **Story assembly** (`fusion_view.go`) is pure and testable. The score is `ComputeOverlap` from taste twins. *Agree* and *Split* pages use trait axes when both profiles have signal, else fall back to shared genres, median page count and genre share. Picks come from both readers' home recommendations filtered to books neither has shelved; a book on both lists is the "Strong Fusion pick". The same rule as recommendation reasons applies — every sentence is gated on the evidence that makes it true, and a page with no honest content ships as an empty array for the client to skip. `low_data` flags a shelf under 3 books.
- **Freshness:** `GET /fusions/{id}` rebuilds the snapshot when either `bookshelf.updated_at` is newer or after 24 h, so the first open after a shelf change may take a few seconds.
- **Blocked pairs** read as `unavailable`. Accepting writes a `fusion_joined` activity addressed to the inviter only — `queries/activities.sql` keeps it out of profile and follower feeds.

Tests in `fusion_test.go`: story from both sides, picks unread by both, wildcard genre gate, splits without traits, low data, token validity.

---

## Privacy, Moderation & Push

| Feature | Endpoints | Notes |
|---|---|---|
| **Private profiles** | `PATCH /users/me/visibility` | Enforced by `RequireProfileAccess` on the whole `/users/{username}` subtree |
| **Follow requests** | `GET /users/me/follow-requests`, `POST\|DELETE …/{username}` | Following a private profile creates a request instead of a follow |
| **Blocking** | `POST\|DELETE /users/{username}/block` | Works both ways: neither reader sees the other's diary entries, reviews or social proof (those GETs are `OptionalAuthenticate` so the filter sees the viewer) |
| **Reports** | `POST /reports` | App Store 1.2 / Play UGC compliance — content type, content id, reason |
| **Push tokens** | `POST\|DELETE /api/mobile/users/me/device-token` | Stores APNs / FCM tokens (`device_tokens`, migration 000036). **Nothing sends yet** — `PUSH_ENABLED` and the APNs/FCM config are read, but no sender ships and neither app registers a token |

Privacy migrations worth knowing: **000039** cleared ratings nobody earned (rating now needs 20 pages read or a finish — the apps used to shelve a book at 0 pages on a star tap), **000040** nulls embeddings on private diary entries (private text never feeds recommendations), **000041** stores deletion-audit emails hashed. The full review is in [`docs/PRIVACY_AUDIT.md`](docs/PRIVACY_AUDIT.md).

---

## Analytics

`POST /api/v1/events` is `OptionalAuthenticate`: acquisition events (`landing_viewed`, `signup_started`) fire before an account exists, so an unauthenticated caller may send an `anon_id` — but only for event types `service.AllowsAnonymous` permits. Names are canonical `snake_case` (`service/event_types.go`); `NormalizeEventType` maps every alias a shipped client still sends.

Operator reads, all behind `X-Internal-Secret`, power the separate `analytics-paperboxd` dashboard:

| Endpoint | Shows |
|---|---|
| `/analytics/overview`, `/users`, `/features` | Totals, growth, feature usage |
| `/analytics/retention` | D1/7/14/30 cohorts, activation, stickiness |
| `/analytics/discovery` | Funnel per `reason_type`: impression → open → save → start → finish → 4★ → 5★ → diary → share. North star is `love_rate` — recommended books that end at 4★+ |

---

## External Services

Each third-party client lives in `internal/external` and is constructed once in `main.go`. All are optional; missing credentials log a warning and disable just that feature.

| Service | Used for | Fallback when absent |
|---|---|---|
| **ISBNdb** | Primary book metadata / search | Google Books |
| **Google Books** | Secondary search, cover/metadata fill | Local Postgres results only |
| **Hardcover** | Scan community reader/rating counts | Open Library counts |
| **Brave Search** | Scan sentiment research | Skipped |
| **Cohere** | Book, diary and query embeddings — recommendations, vibe search, Jazy | `NoopEmbedder` — vector paths off, recs fall back to social / popular |
| **Anthropic (Claude)** | Scan scoring and Jazy's voice (Sonnet 4.6), vibe match reasons, book-trait extraction (Haiku 4.5) | Scan disabled; Jazy and vibe search return decks with the engine's own reasons; traits not extracted |
| **Cloudinary** | Avatar / banner upload (server-signed) | Upload endpoints 503 |
| **Resend** | Transactional email (OTP) | `NoopMailer` — endpoints 200, no mail |

Book search is **local-first**: Postgres is queried before any external provider, so the common case is a fast DB hit and the external APIs are only touched on a miss.

---

## Background Jobs

`internal/cron/nightly.go` starts a goroutine that runs once at boot and every 24h thereafter (non-blocking — no external scheduler needed). Five jobs, in order:

- **`recomputeStaleProfiles`** — refreshes recommendation signal profiles (genre, author, velocity) that are missing or older than 24h, up to 100 users per run, so recommendations stay warm without recomputing on the request path.
- **`recomputeStaleDiaryCentroids`** — rebuilds each diarist's diary embedding centroid. Enumerates from `diary_entries`, not `bookshelf`, so shelf-less diarists are included and a reader who made every entry private gets their centroid nulled.
- **`recomputeStaleTraitProfiles`** — rebuilds trait preferences *and* negative signals. It calls the superset (`ComputeAndSaveNegativeSignals`) on purpose — the narrower trait-only call would overwrite verdicts and abandonments every night.
- **`recomputeTasteOverlaps`** — rebuilds the reader-pair table. Runs after trait profiles so the trait-distance term sees tonight's numbers.
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

There's no `make test`; run `go test ./...` directly. The 24 test files — ranking, reasons, search intent, concierge, feedback, traits, taste overlap, fusion, wrapped, streak, handler contracts — need no database. One-shot tools run with `go run`: `./cmd/backfill-embeddings`, `./cmd/backfill-traits [--dry-run] [--limit N] [--sample N] [--profiles]`.

---

## Deployment

- **Host:** Railway — Go binary, managed PostgreSQL 16, managed Redis 7.
- **Region:** Singapore (lowest latency for the primary Indian user base).
- **Schema:** applied automatically on deploy via embedded migrations (`AUTO_MIGRATE` defaults on).
- **Secrets:** Railway environment variables; nothing sensitive is committed.
- **Lifecycle:** the server traps `SIGINT`/`SIGTERM` and shuts down gracefully with a 30s drain, so in-flight requests finish across a deploy.
- **Redis URL:** full `redis://user:pass@host:port` URLs are parsed automatically (Railway provides this form).

Set a strong `JWT_SECRET`, restrict `CORS_ALLOWED_ORIGINS` to real web origins, and populate `GOOGLE_OAUTH_ALLOWED_AUDIENCES` before mobile sign-in will work. Set `INTERNAL_SECRET` for the analytics dashboard and `/admin/*`; `APPLE_ALLOWED_AUDIENCES` defaults to the iOS bundle ID.

---

## Conventions

- **sqlc is generated — never hand-edit `internal/db`.** Change `queries/` or `migrations/`, then `make sqlc`.
- **Handlers stay thin.** Multi-step logic belongs in `internal/service`, which never imports `net/http`.
- **Every optional dependency degrades, loudly.** A missing key logs a warning at boot and disables exactly one feature — it never panics or 500s the unrelated request paths.
- **Errors go through `types.WriteError`.** No ad-hoc `http.Error` with a bespoke body; the envelope is uniform.
- **Comments explain *why*.** The codebase is dense with rationale comments on the non-obvious calls — the pgvector NULL codec, the CORS `AllowOriginFunc`, the fail-closed Google audience check, the scan quota decrement ordering, the soft-delete cascade. Keep them; they are why the next person doesn't re-break it.
- **Fail-closed on security, fail-open on cosmetics.** Auth audiences reject by default; email and uploads no-op by default.
- **New ranking behaviour ships behind a flag, default off.** Signal *collection* always runs; the flag only gates the ranking *effect*, so turning it on later has history to work with.
- **Deliberate shortcuts say so.** A `ponytail:` comment names the ceiling and the upgrade path (the O(n²) taste overlap, the absolute gem/popularity thresholds sized for a ~100-reader community).

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
| Taste dashboard bars empty, no trait reasons | `cmd/backfill-traits` hasn't run — `book_traits` is empty. See [Rollout](#feature-flags--rollout). |
| Verdicts collected but ranking unchanged | `negative_taste` (and the other discovery flags) default off. Flip the row in `feature_flags`; it takes effect within 60 s. |
| Jazy / scan returns 429 | Per-route 10/min limit — each call is a paid Claude completion. |
| Fusion accept returns 409 | The link is `used`, `expired`, `own`, or already `joined` — the body's `status` says which. |
| Fusion Agree/Split pages show genres, not taste axes | Trait backfill hasn't run — the genre and page-count fallbacks are in use. |
| `GET /books/{id}/fit` returns 204 | Nothing personal applies to that book for that reader. By design — no invented reason. |
| Feed greeting says the wrong time of day | Client didn't send `?tz=`; unknown zones fall back to UTC. |
| Private profile returns 403 for a follower | The follow is still a pending request — check `GET /users/me/follow-requests` on the owner. |
| Device token registered, no push arrives | Expected — no push sender ships yet. |

---

## Related Repositories

| Repository | Description | Stack |
|---|---|---|
| `paperboxd` | Web frontend | Next.js 15, React 19, TypeScript 5 |
| `paperboxd-ios` | Native iOS app | Swift 5, SwiftUI |
| `paperboxd-android` | Native Android app | Kotlin, Jetpack Compose |
| `analytics-paperboxd` | Internal analytics dashboard | Next.js, Tailwind |
| `Paperboxd design elements` | Design system & UI specs | CSS tokens, HTML prototypes |

---

## Contact

**Email:** contact@paperboxd.in  
**Developer:** Hridyesh · hridyesh@paperboxd.in  
**Website:** [paperboxd.in](https://paperboxd.in)

---

*Powered by Go, PostgreSQL, pgvector, and a single binary.*
