# PaperBoxd API Documentation

> Complete REST API reference for the PaperBoxd Go backend (`cmd/api`).

**Base URL:** `https://paperboxd-backend-production-d9e0.up.railway.app`

**Version:** 1.0  
**Last Updated:** March 2026

---

## Conventions

- All API routes below are rooted at **`/api/v1`** unless noted (e.g. `GET /health`).
- JSON request and response bodies use **`application/json`**.
- UUIDs in path segments (books, lists, diary entries) must be valid UUID strings.
- **Pagination:** `page` (default `1`) and `page_size` (default `20`, max `100`) as query parameters where supported.
- **Rate limiting:** `100` requests per minute per IP (see `cmd/api/main.go`).

---

## Table of Contents

1. [Health](#health)
2. [Authentication](#authentication)
3. [Users](#users)
4. [Books](#books)
5. [Bookshelf](#bookshelf)
6. [Favorites](#favorites)
7. [Reading Lists](#reading-lists)
8. [Diary Entries](#diary-entries)
9. [Social Features](#social-features)
10. [Activities](#activities)
11. [Admin](#admin)
12. [Error Codes](#error-codes)
13. [Activity Types](#activity-types)

---

## Health

### Health check

**Endpoint:** `GET /health`

**Authentication:** Not required.

**Response:** `200 OK` — JSON body from `Health` handler (database and Redis status).

---

## Authentication

Authenticated routes expect a Bearer JWT from login/register:

```http
Authorization: Bearer <access_token>
```

Access token lifetime is **`15m`**; refresh token lifetime is **`30d`** (`internal/config/config.go`). Middleware returns `401` with `EXPIRED_TOKEN` or `INVALID_TOKEN` when applicable (`internal/middleware/auth.go`).

### Register

**Endpoint:** `POST /api/v1/auth/register`

**Request body:**

| Field | Rules |
|--------|--------|
| `username` | Required; **3–50** chars; lowercase letters, digits, `_`, `-` |
| `email` | Required; valid email format |
| `password` | Required; **≥ 8** chars with **uppercase**, **lowercase**, and **digit** |
| `name` | Optional string (stored if non-empty) |

**Response:** `201 Created`

```json
{
  "access_token": "string",
  "refresh_token": "string",
  "expires_in": 900,
  "user": { "...": "see User object below" }
}
```

**Errors:**

- `400` — `VALIDATION_ERROR` / `INVALID_REQUEST` (validation or JSON)
- `409` — `CONFLICT` (username or email taken)

---

### Login

**Endpoint:** `POST /api/v1/auth/login`

**Request body:**

```json
{
  "email": "string",
  "password": "string"
}
```

**Response:** `200 OK` — same shape as register (`types.AuthResponse`).

**Errors:**

- `400` — missing email/password
- `401` — `UNAUTHORIZED` — invalid credentials

---

### Refresh token

**Endpoint:** `POST /api/v1/auth/refresh`

**Request body:**

```json
{
  "refresh_token": "string"
}
```

**Response:** `200 OK`

```json
{
  "access_token": "string",
  "refresh_token": "string",
  "expires_in": 900
}
```

The previous refresh token is **revoked** and a **new** refresh token is issued (`internal/auth/auth.go`).

**Errors:**

- `400` — missing `refresh_token`
- `401` — `INVALID_TOKEN` — invalid or unknown refresh token

---

### Logout

**Endpoint:** `POST /api/v1/auth/logout`

**Authentication:** **Not required** (no JWT; only refresh token in body).

**Request body:**

```json
{
  "refresh_token": "string"
}
```

**Response:** `200 OK`

```json
{
  "message": "Logged out successfully"
}
```

Revocation failures are logged but still return `200` (token may already be gone).

---

### Get current user

**Endpoint:** `GET /api/v1/users/me`

**Authentication:** Required.

**Response:** `200 OK` — `UserResponse` (`internal/types/responses.go`), same fields as **Get user by username** (including `email`).

---

## Users

### User object (`UserResponse`)

Common fields returned for users:

```json
{
  "id": "uuid",
  "_id": "uuid",
  "username": "string",
  "email": "string",
  "name": "string",
  "avatar_url": "string | null",
  "bio": "string | null",
  "pronouns": ["string"],
  "birthday": "YYYY-MM-DD | null",
  "gender": "string | null",
  "links": ["string"],
  "is_public": true,
  "books_read_count": 0,
  "total_pages_read": 0,
  "followers_count": 0,
  "following_count": 0,
  "favorites_count": 0,
  "lists_count": 0,
  "diary_entries_count": 0,
  "created_at": "RFC3339 string"
}
```

`email` may be omitted on some public views when empty in the struct tag sense; the handler sets it from the DB for profile endpoints.

---

### Get user by username

**Endpoint:** `GET /api/v1/users/{username}`

**Authentication:** Not required.

**Response:** `200 OK` — `UserResponse`.

**Errors:**

- `404` — user not found

---

### Update profile

**Endpoint:** `PUT /api/v1/users/{username}` or `PATCH /api/v1/users/{username}`

**Authentication:** Required. Path `username` must match the authenticated user’s profile.

**Request body** (`types.UpdateUserRequest` — all optional):

```json
{
  "name": "string",
  "bio": "string",
  "pronouns": ["string"],
  "avatar_url": "string",
  "birthday": "YYYY-MM-DD",
  "gender": "string",
  "links": ["string"]
}
```

**Response:** `200 OK` — updated `UserResponse`.

**Errors:**

- `401` / `403` — not owner
- `400` — invalid JSON or invalid `birthday` format

---

### Search users

**Endpoint:** `GET /api/v1/users/search?query=...&page=&page_size=`

**Query parameters:**

- `query` — **required**
- `page`, `page_size` — optional (defaults above)

**Response:** `200 OK`

```json
{
  "users": [ { "...": "UserResponse subset per row" } ],
  "total_count": 0,
  "page": 1,
  "page_size": 20
}
```

**Note:** `total_count` is the **number of users in the current response slice**, not the total matching rows in the database (`internal/handler/users.go`).

---

## Books

### Book volume shape (`BookResponse`)

Books follow a Google Books–like envelope (`types.BookResponse`):

- `id`, `_id`, `slug`, `volumeInfo` (title, authors, `imageLinks`, `industryIdentifiers`, etc.)
- `paperboxdStats` (`totalReads`, `totalLikes`, `totalTBR`, optional rating fields)
- `apiSource`: `"db"` | `"isbndb"` | etc.
- `fromCache`, `googleBooksId`, `isbndbId`, `openLibraryId` as applicable

---

### Search books

**Endpoint:** `GET /api/v1/books/search?query=...&page=&page_size=`

**Behavior:** DB cache first, then ISBNdb, then Google Books (`internal/handler/books.go`).

**Response:** `200 OK` — `BookListResponse`:

```json
{
  "kind": "books#volumes",
  "totalItems": 0,
  "items": [],
  "page": 1,
  "pageSize": 20,
  "source": "db | isbndb | google | none"
}
```

`totalItems` is the **count of items in this response**, not an external API total.

---

### Create book (cache by Google Books ID)

**Endpoint:** `POST /api/v1/books`

**Authentication:** Required.

**Request body:**

```json
{
  "google_books_id": "string (required)"
}
```

**Response:** `200 OK` — existing `BookResponse`, or `201 Created` after insert.

**Errors:** `400` if missing ID or book not found in Google Books; `401` if not authenticated.

---

### Get book by ID

**Endpoint:** `GET /api/v1/books/{id}`

Path `id` must be a **UUID** of a row in `books`.

**Response:** `200 OK` — `BookResponse`.

**Errors:**

- `400` — invalid UUID
- `404` — not found

---

### Like book

**Endpoint:** `POST /api/v1/books/{id}/like`

**Authentication:** Required. `id` = book UUID.

**Response:** `200 OK`

```json
{ "message": "Book liked" }
```

Duplicate likes do not return `409`; the handler ignores the insert conflict case.

---

### Unlike book

**Endpoint:** `DELETE /api/v1/books/{id}/like`

**Authentication:** Required.

**Response:** `200 OK`

```json
{ "message": "Book unliked" }
```

---

## Bookshelf

Allowed `status` values everywhere: **`read`**, **`reading`**, **`to-read`**.

### Get user bookshelf

**Endpoint:** `GET /api/v1/users/{username}/bookshelf?status=&page=&page_size=`

**Query parameters:**

- `status` — optional; default **`read`**
- `page`, `page_size` — optional

**Response:** `200 OK` — `BookshelfResponse`:

```json
{
  "books": [
    {
      "...BookResponse fields (flattened)...": "",
      "status": "read | reading | to-read",
      "rating": 1,
      "finished_at": "RFC3339 | null",
      "added_at": "RFC3339"
    }
  ],
  "total_count": 0,
  "page": 1,
  "page_size": 20
}
```

Each element embeds `BookResponse` at the top level (plus `status`, `rating`, `finished_at`, `added_at`). TBR-only fields are **not** included on this endpoint; use **`/tbr`** or raw bookshelf rows from mutating endpoints.

---

### Add book to bookshelf

**Endpoint:** `POST /api/v1/users/{username}/bookshelf`

**Authentication:** Required; must be profile owner.

**Request body** (`types.AddToBookshelfRequest`):

- One of: `book_id` (UUID), `isbn`, `google_books_id`
- `status` — required (`read` | `reading` | `to-read`)
- Optional: `rating` (1–5), `started_at`, `finished_at` (**RFC3339**)

**Response:** `200 OK` — `db.Bookshelf` row as JSON (UUIDs and `pgtype` fields as emitted by encoding/json).

**Errors:** `400` validation; `404` book not found; `403` not owner.

---

### Remove book from bookshelf

**Endpoint:** `DELETE /api/v1/users/{username}/bookshelf/{bookId}`

**Authentication:** Required; owner.

**Response:** `200 OK`

```json
{ "message": "Removed from bookshelf" }
```

---

### Update TBR notes

**Endpoint:** `PUT /api/v1/users/{username}/bookshelf/{bookId}/tbr`

**Authentication:** Required; owner.

**Request body:**

```json
{
  "notes": "string | null",
  "priority": "high | medium | low"
}
```

**Response:** `200 OK` — updated `db.Bookshelf` row.

---

### Get TBR list

**Endpoint:** `GET /api/v1/users/{username}/tbr`

**Response:** `200 OK` — JSON array of `TBRResponse` (`book`, `tbr_notes`, `tbr_priority`, `tbr_added_at`, etc.).

---

### Get currently reading

**Endpoint:** `GET /api/v1/users/{username}/reading`

**Response:** `200 OK` — array of `CurrentlyReadingResponse` (`progress_percentage`, `pages_remaining`, `estimated_finish_date`, `current_page`, embedded `book`, …).

---

### Update reading progress

**Endpoint:** `PUT /api/v1/users/{username}/bookshelf/{bookId}/progress`

**Authentication:** Required; owner.

**Request body:**

```json
{
  "current_page": 200
}
```

**Response:** `200 OK` — updated `db.Bookshelf` row (may include recalculated `estimated_finish_date` using a simple pages-per-day heuristic in the handler).

---

### Mark as started / finished

- **POST** `/api/v1/users/{username}/bookshelf/{bookId}/start` — `200 OK`, `db.Bookshelf` row  
- **POST** `/api/v1/users/{username}/bookshelf/{bookId}/finish` — `200 OK`, `db.Bookshelf` row  

**Authentication:** Required; owner.

---

## Favorites

Maximum **4** favorites per user (enforced on add).

### Get favorites

**Endpoint:** `GET /api/v1/users/{username}/favorites`

**Response:** `200 OK` — JSON **array** of `FavoriteResponse` (`id`, `book_id`, `display_order`, `note`, `book`, `created_at`).

---

### Add to favorites

**Endpoint:** `POST /api/v1/users/{username}/favorites`

**Authentication:** Required; owner.

**Request body:** `book_id` and/or `isbn` / `google_books_id`; **`display_order`** required (1–4); optional `note`.

**Response:** `201 Created` — `db.Favorite` row as JSON.

**Errors:** `400` (order, max 4, missing book identifier); `409` already favorited; `403` not owner.

---

### Remove from favorites

**Endpoint:** `DELETE /api/v1/users/{username}/favorites/{bookId}`

**Authentication:** Required; owner.

**Response:** `200 OK`

```json
{ "message": "Removed from favorites" }
```

---

### Reorder favorites

**Endpoint:** `PUT /api/v1/users/{username}/favorites/reorder`

**Authentication:** Required; owner.

**Request body:**

```json
{
  "favorites": [
    { "book_id": "uuid", "display_order": 1 }
  ]
}
```

**Response:** `200 OK`

```json
{ "message": "Favorites reordered" }
```

Invalid `book_id` entries are skipped (warned in logs).

---

## Reading Lists

Optional **Bearer** token on read routes: when present, responses include viewer-specific flags (`is_saved`, `can_edit`, private list visibility).

### Get user lists

**Endpoint:** `GET /api/v1/users/{username}/lists?page=&page_size=`

**Response:** `200 OK`

```json
{
  "own_lists": [ { "...": "ListResponse" } ],
  "saved_lists": [ { "...": "ListResponse" } ]
}
```

`ListResponse` includes `id`, `user_id`, `username`, `title`, `description`, `is_private`, `book_count`, `save_count`, `is_saved`, `can_edit`, `can_view`, `created_at`, `updated_at`.

**Pagination note:** `page` / `page_size` apply to **owned** lists from the database. **Saved** lists use the same `limit` with **offset 0** (first page of saves only) — see `GetUserLists` in `internal/handler/lists.go`.

---

### Create list

**Endpoint:** `POST /api/v1/users/{username}/lists`

**Authentication:** Required; owner.

**Request body:** `title` (1–50 chars), optional `description` (max 200), `is_private`.

**Response:** `201 Created` — `ListResponse`.

---

### Get list details

**Endpoint:** `GET /api/v1/users/{username}/lists/{listId}`

**Response:** `200 OK` — `ListWithBooksResponse` (`ListResponse` + `books` array).

**Errors:** `404` if list missing or viewer cannot access (private and no access).

---

### Update list

**Endpoint:** `PUT /api/v1/users/{username}/lists/{listId}`

**Authentication:** Required; list owner.

**Response:** `200 OK` — `ListResponse`.

---

### Delete list

**Endpoint:** `DELETE /api/v1/users/{username}/lists/{listId}`

**Authentication:** Required; owner.

**Response:** `200 OK`

```json
{ "message": "List deleted" }
```

---

### Add book to list

**Endpoint:** `POST /api/v1/users/{username}/lists/{listId}/books`

**Authentication:** Required; owner.

**Request body:** one of `book_id`, `isbn`, `google_books_id`; optional `display_order`.

**Response:** `201 Created` — list book row (`list_books`).

**Errors:** `409` — book already in list.

---

### Remove book from list

**Endpoint:** `DELETE /api/v1/users/{username}/lists/{listId}/books/{bookId}`

**Authentication:** Required; owner.

**Response:** `200 OK`

```json
{ "message": "Book removed from list" }
```

---

### Share list

**Endpoint:** `POST /api/v1/users/{username}/lists/{listId}/share`

**Authentication:** Required; owner.

**Request body:**

```json
{
  "usernames": ["user1", "user2"]
}
```

**Response:** `200 OK`

```json
{
  "message": "List shared",
  "granted": 0
}
```

`granted` is the number of users for whom access was newly granted (skips invalid usernames / self).

---

### Save / unsave list

- **POST** `/api/v1/users/{username}/lists/{listId}/save` — `201 Created` — `{ "message": "List saved" }`  
- **DELETE** `/api/v1/users/{username}/lists/{listId}/save` — `200 OK` — `{ "message": "List unsaved" }`  

**Authentication:** Required (viewer, not owner for save).

**Errors:** `400` cannot save own list; `409` already saved; `404` list not visible.

---

### Grant access (private lists)

**Endpoint:** `POST /api/v1/users/{username}/lists/{listId}/access`

**Authentication:** Required; list owner.

**Request body:**

```json
{
  "username": "string"
}
```

**Response:** `201 Created` — `{ "message": "Access granted" }`

**Errors:** `409` user already has access.

---

### Revoke access

**Endpoint:** `DELETE /api/v1/users/{username}/lists/{listId}/access?username=target`

**Authentication:** Required; owner.

**Query parameter:** `username` — **required** (not a JSON body).

**Response:** `200 OK` — `{ "message": "Access revoked" }`

---

### Get access users

**Endpoint:** `GET /api/v1/users/{username}/lists/{listId}/access`

**Authentication:** Required; list owner.

**Response:** `200 OK` — JSON array of `ListAccessResponse` (`id`, `username`, `name`, `avatar_url`, `granted_at`).

---

## Diary Entries

### Get user diary entries

**Endpoint:** `GET /api/v1/users/{username}/diary?page=&page_size=`

**Authentication:** Optional. Private entries are filtered unless the viewer is the owner.

**Response:** `200 OK` — `DiaryEntriesResponse` (`entries`, `total_count`, `page`, `page_size`).

**Note:** List construction in the handler does not populate per-entry `likes_count` / `is_liked`; those fields use **zero values** in the list. Use **Get diary entry** for accurate like state when needed.

---

### Create diary entry

**Endpoint:** `POST /api/v1/users/{username}/diary`

**Authentication:** Required; owner.

**Request body:** optional book via `book_id` / `isbn` / `google_books_id`; `content` **required**; optional `title` (max 100), `is_private`, `rating` (1–5).

**Response:** `201 Created` — `DiaryEntryResponse`.

---

### Get diary entry

**Endpoint:** `GET /api/v1/users/{username}/diary/{entryId}`

**Authentication:** Optional. Private entries return `404` for non-owners.

**Response:** `200 OK` — `DiaryEntryResponse` (includes `likes_count`, `is_liked` for eligible viewers).

---

### Update diary entry

**Endpoint:** `PUT /api/v1/users/{username}/diary/{entryId}`

**Authentication:** Required; owner.

**Response:** `200 OK` — `DiaryEntryResponse`.

---

### Delete diary entry

**Endpoint:** `DELETE /api/v1/users/{username}/diary/{entryId}`

**Authentication:** Required; owner.

**Response:** `200 OK`

```json
{ "message": "Entry deleted" }
```

---

### Like / unlike diary entry

- **POST** `.../diary/{entryId}/like` — `201 Created` — `{ "message": "Entry liked", "likes_count": 0 }`  
- **DELETE** `.../diary/{entryId}/like` — `200 OK` — `{ "message": "Entry unliked", "likes_count": 0 }`  

**Authentication:** Required.

**Errors:** `400` cannot like own entry; `409` already liked.

---

### Get book diary entries

**Endpoint:** `GET /api/v1/books/{id}/diary?page=&page_size=`

Path `id` = book **UUID**.

**Response:** `200 OK`

```json
{
  "entries": [],
  "page": 1,
  "page_size": 20
}
```

Public entries only; **no** `total_count` in this payload.

---

## Social Features

### Follow / unfollow

- **POST** `/api/v1/users/{username}/follow` — `200 OK` — `FollowResponse` (`message`, `isFollowing`, `followersCount`, `followingCount`)  
- **DELETE** `/api/v1/users/{username}/follow` — same shape, `isFollowing: false`  

**Authentication:** Required.

**Errors:** `400` cannot follow yourself.

---

### Get followers / following

- **GET** `/api/v1/users/{username}/followers?page=&page_size=`  
- **GET** `/api/v1/users/{username}/following?page=&page_size=`  

**Response:** `200 OK` — `UserListResponse`:

```json
{
  "users": [],
  "total_count": 0,
  "page": 1,
  "page_size": 20
}
```

---

### Get user likes

**Endpoint:** `GET /api/v1/users/{username}/likes?page=&page_size=`

**Response:** `200 OK` — `LikesResponse` (`books` includes `liked_at` per `BookWithLikedAt`).

**Note:** `total_count` is the **count of books returned on this page**, not the full total (`internal/handler/bookshelf.go`).

---

## Activities

All routes require authentication.

### Get my activities

**Endpoint:** `GET /api/v1/activities/me?page=&page_size=`

**Response:** `200 OK`

```json
{
  "activities": [],
  "page": 1,
  "page_size": 20
}
```

---

### Get following activities

**Endpoint:** `GET /api/v1/activities/following?page=&page_size=`

**Response:** Same shape as `/me`. SQL includes activities from followed users and certain types where the current user is the target (`queries/activities.sql`).

---

### Check new activities

**Endpoint:** `GET /api/v1/activities/check-new?since=`

**Query parameter:** `since` — optional RFC3339 timestamp. If omitted or invalid, the server uses **now − 24h** (`internal/handler/activities.go`).

**Response:** `200 OK`

```json
{
  "has_new": true
}
```

---

## Admin

### Cleanup stale books

**Endpoint:** `DELETE /api/v1/admin/cleanup-books`

**Authentication:** **Any valid JWT** (no separate admin role in code).

**Response:** `200 OK`

```json
{
  "deleted": 0
}
```

---

## Error Codes

All errors use (`internal/types/errors.go`):

```json
{
  "error": {
    "code": "ERROR_CODE",
    "message": "Human-readable message"
  }
}
```

| Code | Typical HTTP | Description |
|------|----------------|-------------|
| `INVALID_REQUEST` | 400 | Malformed JSON or bad parameters |
| `VALIDATION_ERROR` | 400 | Validation failed |
| `UNAUTHORIZED` | 401 | Missing/invalid auth |
| `INVALID_TOKEN` | 401 | Bad JWT |
| `EXPIRED_TOKEN` | 401 | Expired JWT |
| `FORBIDDEN` | 403 | Not allowed for this resource |
| `NOT_FOUND` | 404 | Resource missing or hidden |
| `CONFLICT` | 409 | Duplicate or conflicting state |
| `INTERNAL_SERVER_ERROR` | 500 | Unhandled server error |

---

## Activity Types

Types **written by handlers** today include:

| `activity_type` | When |
|-----------------|------|
| `added_book` | Bookshelf add |
| `created_list` | List created |
| `shared_list` | List share granted to another user |
| `created_diary_entry` | Public diary entry created |
| `liked_diary_entry` | Someone likes a diary entry |

The **following feed** query also considers `shared_book` and `granted_access` if those rows exist in the database (`queries/activities.sql`).

---

## Changelog

### v1.0 (March 2026)

- Initial documented surface aligned with `cmd/api/main.go` and handlers.

---

## Support

**Issues:** GitHub Issues for the backend repository  
**Email:** paperboxd@gmail.com
