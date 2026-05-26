# PaperBoxd Mobile API Contract

This document is the contract the iOS (Swift/SwiftUI) and Android (Jetpack Compose) clients work against. It is generated from the Go backend on the `mobile-api` branch and supersedes ad-hoc API notes.

## Conventions

- **Base URL**: `https://api.paperboxd.com` (Railway-hosted). For local dev, `http://localhost:8080`.
- **Transport**: HTTPS only in production. JSON in both directions (`Content-Type: application/json`).
- **Auth**: `Authorization: Bearer <jwt>`. Mobile tokens are issued by `/api/mobile/auth/*` and live for **30 days** by default (configurable via `TOKEN_EXPIRY_MOBILE`). No cookies are read or written for mobile clients.
- **CORS**: mobile clients do not send an `Origin` header and are always allowed. The browser allowlist (`CORS_ALLOWED_ORIGINS`) is unchanged.
- **Errors**: every error response has the same flat shape:
  ```json
  { "error": "Human readable message", "code": "SNAKE_CASE_CODE" }
  ```
  Standard codes:

  | Code | HTTP | Meaning |
  |------|------|---------|
  | `VALIDATION_ERROR` | 400 | Body or query params failed validation; message contains specifics |
  | `UNAUTHORIZED` | 401 | Missing/invalid/expired token, or wrong credentials |
  | `INVALID_TOKEN` | 401 | Token could not be parsed or signature mismatch |
  | `EXPIRED_TOKEN` | 401 | Token signature valid, but past `exp` |
  | `FORBIDDEN` | 403 | Authenticated, but not allowed |
  | `NOT_FOUND` | 404 | Resource does not exist or caller has no read access |
  | `CONFLICT` | 409 | Uniqueness violation (e.g. email/username taken) |
  | `RATE_LIMITED` | 429 | Per-token (or per-IP) rate limit tripped; retry after a minute |
  | `INTERNAL_ERROR` | 500 | Unhandled server error; safe to retry once |

- **Pagination**: all paginated endpoints return both the legacy fields used by the web frontend AND a new `pagination` block for mobile:
  ```json
  {
    "data_field_name": [...],
    "total_count": 123,
    "page": 1,
    "page_size": 20,
    "pagination": { "page": 1, "per_page": 20, "total": 123, "total_pages": 7 }
  }
  ```
  Mobile MUST read from `pagination`. The data array key varies per endpoint (`books`, `users`, `entries`, `items`).

- **Timestamps**: ISO-8601 UTC (`time.RFC3339`).
- **IDs**: UUIDv4 strings.

---

## 1. Connectivity

### `GET /api/health` — public

Lightweight reachability probe. No database round-trip. Use for splash-screen connectivity check.

**Request headers**: none required.

**Response 200**:
```json
{
  "status": "ok",
  "version": "dev",
  "timestamp": "2026-05-26T12:34:56Z"
}
```

> The deeper `GET /health` route also exists (Railway uses it) and reports Postgres + Redis status. Mobile should not hit it.

---

## 2. Authentication — `/api/mobile/auth/*`

All six endpoints are public (no Bearer required) except `refresh`, which requires a current valid token in the header.

### `POST /api/mobile/auth/login` — public

**Request**:
```json
{ "email": "user@example.com", "password": "Secret123" }
```

**Response 200**:
```json
{
  "token": "eyJhbGciOiJIUzI1NiIs...",
  "user": {
    "id": "uuid",
    "username": "username",
    "email": "user@example.com",
    "avatar_url": "https://...",
    "level": 3,
    "xp": 420
  }
}
```

**Errors**: `VALIDATION_ERROR` (400), `UNAUTHORIZED` (401 wrong credentials), `INTERNAL_ERROR` (500).

---

### `POST /api/mobile/auth/register` — public

Single-step registration. Username is auto-generated from the email local-part and de-duplicated.

**Request**:
```json
{ "email": "user@example.com", "password": "Secret123" }
```

Password rule: minimum 8 chars, must contain uppercase, lowercase, and a digit.

**Response 201**:
```json
{
  "token": "eyJhbGciOiJIUzI1NiIs...",
  "user": {
    "id": "uuid",
    "username": "user",
    "email": "user@example.com"
  }
}
```

**Errors**: `VALIDATION_ERROR` (400 bad email/password), `CONFLICT` (409 email taken), `INTERNAL_ERROR` (500).

---

### `POST /api/mobile/auth/otp/send` — public

Generates a 6-digit code, stores its SHA-256 hash, and hands it to the configured `service.Mailer` for delivery. Response is always 200 to avoid leaking whether an account exists.

> **Operational note**: the default backend ships with `service.NoopMailer`. To actually deliver OTPs on mobile flows in production, wire a real `service.Mailer` in `cmd/api/main.go` (the Resend-backed implementation used by the web flow lives in the Next.js layer — Go has no email integration yet).

**Request**:
```json
{ "email": "user@example.com" }
```

**Response 200**:
```json
{
  "message": "If an account exists for that email, a code has been sent.",
  "expires_in_seconds": 600
}
```

**Errors**: `VALIDATION_ERROR` (400 malformed email), `INTERNAL_ERROR` (500).

---

### `POST /api/mobile/auth/otp/verify` — public

**Request**:
```json
{ "email": "user@example.com", "code": "123456" }
```

**Response 200**:
```json
{
  "token": "eyJhbGciOiJIUzI1NiIs...",
  "user": {
    "id": "uuid",
    "username": "username",
    "email": "user@example.com",
    "avatar_url": null,
    "level": 1,
    "xp": 0
  }
}
```

**Errors**: `VALIDATION_ERROR` (400), `UNAUTHORIZED` (401 wrong/expired code — message shows attempts remaining), `RATE_LIMITED` (429 too many failed attempts), `INTERNAL_ERROR` (500).

---

### `POST /api/mobile/auth/google` — public

Verifies the Google ID token server-side against `https://oauth2.googleapis.com/tokeninfo` (no extra Go deps required). Either logs in the existing user with the matching email or auto-creates an account.

**Request**:
```json
{ "id_token": "<google-issued-id-token>" }
```

The mobile client SHOULD obtain `id_token` via Google's official SDKs (Sign in with Google for iOS, Credential Manager for Android).

**Response 200**:
```json
{
  "token": "eyJhbGciOiJIUzI1NiIs...",
  "user": {
    "id": "uuid",
    "username": "user",
    "email": "user@example.com",
    "avatar_url": null
  },
  "is_new_user": false
}
```

**Errors**: `VALIDATION_ERROR` (400 missing id_token), `UNAUTHORIZED` (401 id_token invalid or unverified email), `INVALID_TOKEN` (401 verification failed), `INTERNAL_ERROR` (500).

---

### `POST /api/mobile/auth/refresh` — requires valid Bearer

Re-mints a new 30-day access token using the current one. Mobile clients should call this proactively before expiry; once a token is past `exp`, the user must log in again.

**Request headers**: `Authorization: Bearer <current-token>`. No body.

**Response 200**:
```json
{ "token": "eyJhbGciOiJIUzI1NiIs..." }
```

**Errors**: `UNAUTHORIZED` (401 missing header), `EXPIRED_TOKEN` (401 token past exp — re-login required), `INVALID_TOKEN` (401 malformed/wrong signature), `INTERNAL_ERROR` (500).

---

## 3. Read endpoints mobile will call

All endpoints below live under `/api/v1`. Auth requirement is per-route. Responses are unchanged from the web frontend's existing contract (we did not alter any 2xx shape) — see notes on pagination above.

### 3.1 Current user

| Method | Path | Auth | Notes |
|--------|------|------|-------|
| `GET` | `/api/v1/users/me` | Required | Returns the full `UserResponse` for the bearer's user |
| `DELETE` | `/api/v1/users/me` | Required | Soft-delete current user |
| `POST` | `/api/v1/users/me/onboarding` | Required | Save onboarding answers |
| `POST` | `/api/v1/users/me/daily-open` | Required | Daily open tracker (XP) |
| `GET` | `/api/v1/users/me/leaderboard-stats` | Required | Mine on the leaderboard |
| `GET` | `/api/v1/users/me/referral` | Required | Get my referral code |
| `GET` | `/api/v1/users/me/referrals` | Required | List my referrals |
| `PATCH` | `/api/v1/users/me/avatar` | Required | Update avatar URL |

### 3.2 Users (public profile)

| Method | Path | Auth | Notes |
|--------|------|------|-------|
| `GET` | `/api/v1/users/search?q=...&page=&page_size=` | Public | Paginated, `UserListResponse` |
| `GET` | `/api/v1/users/{username}` | Public | Public profile |
| `GET` | `/api/v1/users/{username}/followers` | Public | Paginated |
| `GET` | `/api/v1/users/{username}/following` | Public | Paginated |
| `GET` | `/api/v1/users/{username}/likes` | Public | Paginated `LikesResponse` |
| `GET` | `/api/v1/users/{username}/tbr` | Public | TBR list |
| `GET` | `/api/v1/users/{username}/reading` | Public | Currently reading |
| `GET` | `/api/v1/users/{username}/reading/today` | Public | Today's progress |
| `GET` | `/api/v1/users/{username}/favorites` | Public | Favorites |
| `GET` | `/api/v1/users/{username}/bookshelf` | Public | Paginated `BookshelfResponse` |
| `POST` | `/api/v1/users/{username}/follow` | Required | Follow user |
| `DELETE` | `/api/v1/users/{username}/follow` | Required | Unfollow user |
| `PUT/PATCH` | `/api/v1/users/{username}` | Required | Update own profile |

### 3.3 Books

| Method | Path | Auth | Notes |
|--------|------|------|-------|
| `GET` | `/api/v1/books/search?q=...&page=&page_size=` | Public | Paginated `BookListResponse` |
| `GET` | `/api/v1/books/by-slug/{slug}` | Public | Single book |
| `GET` | `/api/v1/books/latest` | Public | Paginated |
| `GET` | `/api/v1/books/public` | Public | New + popular carousels |
| `GET` | `/api/v1/books/by-author?author=...` | Public | Paginated |
| `GET` | `/api/v1/books/{id}` | Public | Single book |
| `GET` | `/api/v1/books/{id}/diary` | Public | Diary entries for a book |
| `GET` | `/api/v1/books/{id}/reviews` | Public | Reviews |
| `POST` | `/api/v1/books` | Required | Create a book record |
| `POST` | `/api/v1/books/{id}/like` | Required | Like book |
| `DELETE` | `/api/v1/books/{id}/like` | Required | Unlike |
| `POST` | `/api/v1/books/{id}/share` | Required | Share event |

### 3.4 Bookshelf

| Method | Path | Auth | Notes |
|--------|------|------|-------|
| `POST` | `/api/v1/users/{username}/bookshelf` | Required | Add book |
| `DELETE` | `/api/v1/users/{username}/bookshelf/{bookId}` | Required | Remove |
| `PATCH` | `/api/v1/users/{username}/bookshelf/{bookId}` | Required | Update rating |
| `PUT` | `/api/v1/users/{username}/bookshelf/{bookId}/tbr` | Required | Update TBR notes |
| `PUT` | `/api/v1/users/{username}/bookshelf/{bookId}/progress` | Required | Update reading progress |
| `POST` | `/api/v1/users/{username}/bookshelf/{bookId}/start` | Required | Mark started |
| `POST` | `/api/v1/users/{username}/bookshelf/{bookId}/finish` | Required | Mark finished |

### 3.5 Diary

| Method | Path | Auth | Notes |
|--------|------|------|-------|
| `GET` | `/api/v1/users/{username}/diary?page=&page_size=` | Public | Paginated `DiaryEntriesResponse` |
| `POST` | `/api/v1/users/{username}/diary` | Required | Create entry |
| `GET` | `/api/v1/users/{username}/diary/{entryId}` | Public | Single entry |
| `PUT` | `/api/v1/users/{username}/diary/{entryId}` | Required | Update entry |
| `DELETE` | `/api/v1/users/{username}/diary/{entryId}` | Required | Delete |
| `POST` | `/api/v1/users/{username}/diary/{entryId}/like` | Required | Like entry |
| `DELETE` | `/api/v1/users/{username}/diary/{entryId}/like` | Required | Unlike |

### 3.6 Lists

| Method | Path | Auth | Notes |
|--------|------|------|-------|
| `GET` | `/api/v1/users/{username}/lists` | Optional | Own + saved lists |
| `POST` | `/api/v1/users/{username}/lists` | Required | Create list |
| `GET` | `/api/v1/users/{username}/lists/{listId}` | Optional | List details |
| `PUT` | `/api/v1/users/{username}/lists/{listId}` | Required | Update |
| `DELETE` | `/api/v1/users/{username}/lists/{listId}` | Required | Delete |
| `POST` | `/api/v1/users/{username}/lists/{listId}/books` | Required | Add book |
| `DELETE` | `/api/v1/users/{username}/lists/{listId}/books/{bookId}` | Required | Remove book |
| `POST` | `/api/v1/users/{username}/lists/{listId}/save` | Required | Save list |
| `DELETE` | `/api/v1/users/{username}/lists/{listId}/save` | Required | Unsave |
| `POST` | `/api/v1/users/{username}/lists/{listId}/share` | Required | Share |
| `POST/DELETE/GET` | `/api/v1/users/{username}/lists/{listId}/access` | Required | Manage collaborator access |
| `POST` | `/api/v1/lists/{listId}/collaborators` | Required | Accept collaboration invite |

### 3.7 Favorites

| Method | Path | Auth | Notes |
|--------|------|------|-------|
| `GET` | `/api/v1/users/{username}/favorites` | Public | Already in §3.2 |
| `POST` | `/api/v1/users/{username}/favorites` | Required | Add to favorites |
| `PUT` | `/api/v1/users/{username}/favorites/reorder` | Required | Reorder |
| `DELETE` | `/api/v1/users/{username}/favorites/{bookId}` | Required | Remove |

### 3.8 Activity + social

| Method | Path | Auth | Notes |
|--------|------|------|-------|
| `GET` | `/api/v1/activities/me` | Required | My activities |
| `GET` | `/api/v1/activities/following` | Required | Following feed |
| `GET` | `/api/v1/activities/check-new` | Required | Poll for unseen items |
| `POST` | `/api/v1/events` | Required | Track client event |

### 3.9 Leaderboard

| Method | Path | Auth | Notes |
|--------|------|------|-------|
| `GET` | `/api/v1/leaderboard/global` | Public | Global ranking |
| `GET` | `/api/v1/leaderboard/dimension/{dimension}` | Public | One of `books`, `pages`, `diary`, `genres`, `xp`, `streak` |
| `GET` | `/api/v1/leaderboard/friends` | Required | Friends-only |

### 3.10 Recommendations + search

| Method | Path | Auth | Notes |
|--------|------|------|-------|
| `GET` | `/api/v1/recommendations/home` | Optional | Falls back to popular if not authed |
| `GET` | `/api/v1/recommendations/similar/{bookId}` | Optional | Similar books |
| `POST` | `/api/v1/recommendations/feedback` | Optional | Train signals |
| `POST` | `/api/v1/search/vibe` | Optional | Semantic search; personalised when authed |

### 3.11 Authors + newsletter

| Method | Path | Auth | Notes |
|--------|------|------|-------|
| `GET` | `/api/v1/authors/info?name=...` | Public | Author bio + photo |
| `POST` | `/api/v1/newsletter/subscribe` | Public | Subscribe |

---

## 4. Things mobile MUST NOT call

- `POST /api/v1/auth/google` — requires `X-Internal-Secret` (Next.js server-to-server only). Use `/api/mobile/auth/google` instead.
- `POST /api/v1/auth/otp/send`, `/api/v1/auth/register/send-otp`, `/api/v1/auth/forgot-password` — return the **plaintext** OTP/reset token in the response body for the Next.js proxy to email via Resend. Mobile should use the `/api/mobile/auth/otp/*` flow instead.
- `/api/v1/admin/*` — restricted admin endpoints.
- `/api/v1/test/*` — temporary test routes scheduled for removal.

---

## 5. Rate limiting

- Per Bearer token when present, else per client IP.
- Limit defaults to `RATE_LIMIT_PER_MINUTE` env (production default 100/min, dev 5000/min).
- Triggered: 429 with body `{ "error": "Too many requests", "code": "RATE_LIMITED" }`.
- Mobile should back off ≥60s before retrying when this is seen.

## 6. Client expectations

- Persist the JWT in the platform's secure store (Keychain on iOS, EncryptedSharedPreferences on Android).
- Refresh proactively: any time a request is being made and the token has <7 days left, fire `/api/mobile/auth/refresh` in the background.
- On 401 with `EXPIRED_TOKEN`: drop the token, route to login.
- On 401 with `INVALID_TOKEN`: drop the token, log the user out, route to login.
- On 5xx: surface a generic "Something went wrong" and offer retry once with backoff.
