# MongoDB → PostgreSQL Migration Analysis

## Collections Overview

| Collection | Type | Priority | PG Equivalent |
|---|---|---|---|
| `users` | Core | Required | `users` + multiple tables |
| `books` | Core | Required | `books` |
| `events` | Analytics | Skip (ephemeral) | — |
| `accountdeletions` | Audit | Needed | New table |
| `newsletters` | Marketing | Needed | New table |
| `recommendationlogs` | Analytics | Skip (computed) | — |
| `recommendationcaches` | Cache | Skip (computed) | — |
| `otps` | Auth | Needed | New table |
| `userpreferences` | ML | Needed | New table |

---

## Collection: `users`

MongoDB stores ALL user data in one heavily-denormalized document.
Every sub-entity below is an **embedded array** that maps to a separate PG table.

### Top-level fields

| Mongo field | PG column | Notes |
|---|---|---|
| `_id` | `users.id` (UUID) | ObjectId → deterministic UUID v5 |
| `email` | `users.email` | Direct |
| `password` | `users.password_hash` | Direct (already bcrypt) |
| `username` | `users.username` | Optional in Mongo, sparse unique |
| `name` | `users.name` | Direct |
| `avatar` | `users.avatar_url` | May be base64 or URL; strip base64 prefix |
| `bio` | `users.bio` | Direct (max 500 chars) |
| `birthday` | `users.birthday` | Direct |
| `gender` | `users.gender` | Direct |
| `pronouns` | `users.pronouns` | `string[]` → `TEXT[]` |
| `links` | `users.links` | `string[]` → `TEXT[]` |
| `isPublic` | `users.is_public` | Direct (inverted naming) |
| `totalBooksRead` | `users.books_read_count` | Direct |
| `totalPagesRead` | `users.total_pages_read` | Direct |
| `lastActive` | `users.last_active` | ✅ PG has this column |
| `createdAt` | `users.created_at` | Direct |
| `updatedAt` | `users.updated_at` | Direct |
| `passwordReset.token` | — | **MISSING in PG** (use OTP table instead) |
| `readingGoal` | — | **MISSING in PG** — needs new migration |
| `authorsRead` | — | **MISSING in PG** — needs new migration |

### Embedded: `bookshelf[]` (finished books)

**Maps to:** `bookshelf` table with `status = 'read'`

| Mongo field | PG column | Notes |
|---|---|---|
| `bookId` | `bookshelf.book_id` | ObjectId → UUID mapping |
| `finishedOn` | `bookshelf.finished_at` | Direct |
| `rating` | `bookshelf.rating` | 1-5, direct |
| `thoughts` | — | **MISSING in PG** — bookshelf has no review text column |
| `format` | — | **MISSING in PG** — Print/Digital/Audio not stored |
| `mood` | — | **MISSING in PG** |
| (inferred) | `bookshelf.status` | Set to `'read'` |

### Embedded: `currentlyReading[]`

**Maps to:** `bookshelf` table with `status = 'reading'`

| Mongo field | PG column | Notes |
|---|---|---|
| `bookId` | `bookshelf.book_id` | ObjectId → UUID |
| (none) | `bookshelf.status` | Set to `'reading'` |

**Note:** Conflict if same book appears in both `bookshelf` (read) and `currentlyReading` — skip duplicate, prefer `'read'`.

### Embedded: `tbrBooks[]`

**Maps to:** `bookshelf` table with `status = 'to-read'`

| Mongo field | PG column | Notes |
|---|---|---|
| `bookId` | `bookshelf.book_id` | ObjectId → UUID |
| `addedOn` | `bookshelf.tbr_added_at` | Direct |
| `urgency` | `bookshelf.tbr_priority` | Mapping needed: "Soon"→'high', "This weekend"→'high', "Eventually"→'low' |
| `whyNow` | `bookshelf.tbr_notes` | Direct (partial) |
| (none) | `bookshelf.status` | Set to `'to-read'` |

### Embedded: `likedBooks[]`

**Maps to:** `likes` table

| Mongo field | PG column | Notes |
|---|---|---|
| `bookId` | `likes.book_id` | ObjectId → UUID |
| `likedOn` | `likes.created_at` | Direct |

### Embedded: `readingProgress[]`

**Maps to:** `bookshelf.current_page` (update existing row)

| Mongo field | PG column | Notes |
|---|---|---|
| `bookId` | identifies which bookshelf row | |
| `pagesRead` | `bookshelf.current_page` | Update the 'reading' status row |

### Embedded: `topBooks[]` (4-6 books)

**Maps to:** `favorites` table

| Mongo field | PG column | Notes |
|---|---|---|
| `bookId` | `favorites.book_id` | ObjectId → UUID |
| array index | `favorites.display_order` | 1-based, max 4 |

**Note:** PG `favorites` is max 4 (`UNIQUE(user_id, display_order)` with orders 1-4). Mongo `topBooks` can have up to 6. Take first 4.

### Embedded: `favoriteBooks[]` (up to 12)

**MISSING in PG** — no equivalent table. Options:
1. Skip (low priority)
2. Add new `user_favorite_books` table

For MVP migration: **skip** (topBooks → favorites covers the Letterboxd-style display use case).

### Embedded: `readingLists[]`

**Maps to:** `lists` + `list_books` + `list_access`

| Mongo field | PG column | Notes |
|---|---|---|
| `_id` | `lists.id` | Subdoc _id → UUID mapping (critical for activity references) |
| `title` | `lists.title` | Direct |
| `description` | `lists.description` | Direct |
| `isPublic` | `lists.is_private` | **INVERTED**: `is_private = !isPublic` |
| `books[]` | `list_books.book_id` | Array of ObjectIds → rows |
| `allowedUsers[]` | `list_access` | Username → look up user_id |
| `collaborators[]` | — | **MISSING** — PG has no collaborator/edit-access concept |
| `createdAt` | `lists.created_at` | Direct |
| `updatedAt` | `lists.updated_at` | Direct |
| (user context) | `lists.user_id` | Set from parent user |

### Embedded: `diaryEntries[]`

**Maps to:** `diary_entries` + `diary_entry_likes`

| Mongo field | PG column | Notes |
|---|---|---|
| `_id` | `diary_entries.id` | Subdoc _id → UUID |
| `bookId` | `diary_entries.book_id` | ObjectId → UUID (nullable) |
| `subject` | `diary_entries.title` | Field name change |
| `content` | `diary_entries.content` | Direct (HTML) |
| `likes[]` | `diary_entry_likes.user_id` | Array of User ObjectIds → rows |
| `createdAt` | `diary_entries.created_at` | Direct |
| `updatedAt` | `diary_entries.updated_at` | Direct |
| (user context) | `diary_entries.user_id` | Set from parent user |
| `bookTitle` | — | Denormalized, skip (PG uses FK) |
| `bookAuthor` | — | Denormalized, skip |
| `bookCover` | — | Denormalized, skip |

**Note:** Mongo diary entries have no `is_private` field — set all to `false` on migration.
**Note:** Mongo diary entries have no `rating` field — leave null in PG.

### Embedded: `activities[]`

**Maps to:** `activities` table

| Mongo field | PG column | Notes |
|---|---|---|
| `type` | `activities.activity_type` | See type mapping below |
| `bookId` | `activities.book_id` | ObjectId → UUID (nullable) |
| `listId` | `activities.list_id` | **String** → look up list UUID by old _id |
| `diaryEntryId` | `activities.entry_id` | ObjectId or String → look up entry UUID |
| `timestamp` | `activities.created_at` | Direct |
| `sharedBy` | `activities.user_id` | The actor (sharedBy = who triggered) |
| (user context) | target or originator | Depends on activity type |

**Activity type mapping:**

| Mongo type | PG activity_type | Notes |
|---|---|---|
| `read` | `added_book` | |
| `rated` | `added_book` | Merge with read |
| `liked` | — | Tracked via likes table, skip activity |
| `added_to_list` | — | Skip (no direct equivalent) |
| `started_reading` | `added_book` | |
| `reviewed` | `created_diary_entry` | |
| `shared_list` | `shared_list` | Direct |
| `shared_book` | `shared_book` | Direct |
| `collaboration_request` | — | **MISSING in PG**, skip |
| `granted_access` | `granted_access` | Direct |
| `liked_diary_entry` | `liked_diary_entry` | Direct |

### Embedded: `followers[]` / `following[]`

**Maps to:** `follows` table

| Mongo field | PG table | Notes |
|---|---|---|
| `followers[i]` | `follows.follower_id = followers[i], following_id = this_user` | |
| `following[i]` | `follows.follower_id = this_user, following_id = following[i]` | |

**Deduplication:** Process from both sides, use INSERT ON CONFLICT DO NOTHING.

### Fields with NO PG equivalent (data loss unless migrated)

| Field | Impact | Recommendation |
|---|---|---|
| `readingGoal` | Feature gap | Add `user_reading_goals` table |
| `authorsRead[]` | Feature gap | Add `user_author_stats` table |
| `favoriteBooks[]` | Feature gap | Skip for MVP |
| `bookshelf.thoughts` | Data loss | Add `notes` column to bookshelf |
| `bookshelf.format` | Data loss | Add `format` column to bookshelf |
| `passwordReset` | Auth | PG uses OTP table |

---

## Collection: `books`

### Field mapping

| Mongo field | PG column | Notes |
|---|---|---|
| `_id` | `books.id` | ObjectId → UUID |
| `isbndbId` | `books.isbndb_id` | Direct |
| `isbn` | — | ISBN-10, not stored in PG |
| `isbn13` | `books.isbn_13` | Direct |
| `googleBooksId` | `books.google_books_id` | Direct |
| `openLibraryId` | `books.open_library_id` | Direct |
| `openLibraryKey` | — | **MISSING** (e.g., "/works/OL45804W") |
| `volumeInfo.title` | `books.title` | Direct |
| `volumeInfo.subtitle` | `books.subtitle` | Direct |
| `volumeInfo.authors[]` | `books.authors` | `string[]` → `TEXT[]` |
| `volumeInfo.publisher` | `books.publisher` | Direct |
| `volumeInfo.publishedDate` | `books.published_date` | String → DATE (parse) |
| `volumeInfo.description` | `books.description` | Direct |
| `volumeInfo.pageCount` | `books.page_count` | Direct |
| `volumeInfo.categories[]` | `books.categories` | `string[]` → `TEXT[]` |
| `volumeInfo.language` | `books.language` | Direct |
| `volumeInfo.imageLinks.thumbnail` | `books.cover_url` | Use thumbnail URL |
| `volumeInfo.previewLink` | `books.preview_link` | Direct |
| `volumeInfo.averageRating` | `books.average_rating` | Direct |
| `volumeInfo.ratingsCount` | `books.ratings_count` | Direct |
| `paperboxdRating` | `books.average_rating` | Overwrite with this if set |
| `paperboxdRatingsCount` | `books.ratings_count` | Overwrite if set |
| `totalReads` | `books.total_reads_count` | Direct |
| `totalTBR` | `books.total_tbr_count` | Direct |
| `createdAt` | `books.created_at` | Direct |
| `volumeInfo.infoLink` | — | **MISSING in PG** |
| `volumeInfo.canonicalVolumeLink` | — | **MISSING in PG** |
| `saleInfo` | — | **MISSING** — not tracked in PG |
| `apiSource` | — | **MISSING** — not tracked in PG |
| `usageCount` | — | **MISSING** |
| `cachedAt` / `lastUpdated` | — | **MISSING** |
| `totalLikes` | — | Derivable from `likes` table |

**Books that exist in PG but not Mongo:** The Go backend has been caching books independently. Deduplication strategy: match on `isbn_13`, `google_books_id`, `isbndb_id` (in that priority order). If matched, update missing fields; otherwise insert.

**Slug generation:** PG requires a `slug` field; Mongo has no slug. Generate during migration: `slugify(title + "-" + primary_author)`.

---

## Collection: `otps`

**No PG equivalent** — needs new table.

```sql
CREATE TABLE otps (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code TEXT NOT NULL,              -- Hashed 6-digit code
    type VARCHAR(20) NOT NULL,       -- 'login' | 'password_reset'
    attempts INTEGER NOT NULL DEFAULT 0,
    used BOOLEAN NOT NULL DEFAULT false,
    expires_at TIMESTAMP NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_otps_user_id ON otps(user_id);
CREATE INDEX idx_otps_expires ON otps(expires_at);
```

**No data to migrate** — OTPs are ephemeral and expired entries are worthless.

---

## Collection: `newsletters`

**No PG equivalent** — needs new table.

```sql
CREATE TABLE newsletters (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email VARCHAR(255) NOT NULL UNIQUE,
    is_active BOOLEAN NOT NULL DEFAULT true,
    source VARCHAR(50) DEFAULT 'footer',
    subscribed_at TIMESTAMP NOT NULL DEFAULT NOW(),
    unsubscribed_at TIMESTAMP,
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_newsletters_email ON newsletters(email);
CREATE INDEX idx_newsletters_active ON newsletters(is_active);
```

**Migrate all documents** — email addresses are valuable.

---

## Collection: `accountdeletions`

**No PG equivalent** — needs new table.

```sql
CREATE TABLE account_deletions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email VARCHAR(255) NOT NULL,
    username VARCHAR(30),
    reasons TEXT[] NOT NULL,
    deleted_at TIMESTAMP NOT NULL
);
CREATE INDEX idx_account_deletions_email ON account_deletions(email);
CREATE INDEX idx_account_deletions_deleted_at ON account_deletions(deleted_at DESC);
```

**Migrate all documents** — audit trail should be preserved.

---

## Collection: `userpreferences`

**No PG equivalent** — needed for recommendation system.

```sql
CREATE TABLE user_preferences (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
    onboarding_genres JSONB,         -- [{genre, weight, lastUpdated}]
    onboarding_authors TEXT[],
    onboarding_completed_at TIMESTAMP,
    genre_weights JSONB DEFAULT '{}', -- Map<string, number>
    author_weights JSONB DEFAULT '{}',
    avg_page_length FLOAT8 DEFAULT 0,
    diversity_score FLOAT8 DEFAULT 0,
    reading_velocity FLOAT8 DEFAULT 0,
    preferences_computed_at TIMESTAMP,
    last_recommendation_generated TIMESTAMP,
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP NOT NULL DEFAULT NOW()
);
-- Interaction history is ephemeral; views/searches can be dropped
```

---

## Collections to Skip

| Collection | Reason |
|---|---|
| `events` | Analytics only; 90-day TTL; not needed post-migration |
| `recommendationlogs` | Analytics; 180-day TTL; skip |
| `recommendationcaches` | Computed; can be regenerated; skip |

---

## Additional PG Migrations Needed

Before running the migration script, apply:

```sql
-- Migration 000007: Missing user fields
ALTER TABLE users
    ADD COLUMN last_active TIMESTAMP;   -- already exists from 000001

-- Actually last_active exists. Add what's truly missing:
ALTER TABLE bookshelf
    ADD COLUMN notes TEXT,              -- thoughts/review for finished books
    ADD COLUMN format VARCHAR(20)       -- Print | Digital | Audio
        CHECK (format IN ('Print', 'Digital', 'Audio'));

ALTER TABLE users
    ADD COLUMN reading_goal_year INTEGER,
    ADD COLUMN reading_goal_target INTEGER,
    ADD COLUMN reading_goal_current INTEGER DEFAULT 0;

-- New collections
CREATE TABLE otps ( ... );
CREATE TABLE newsletters ( ... );
CREATE TABLE account_deletions ( ... );
CREATE TABLE user_preferences ( ... );
```

---

## ObjectId → UUID Mapping Strategy

Use **UUID v5** (deterministic, SHA-1 based) with a fixed namespace:

```go
var migrationNamespace = uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8")

func objectIDToUUID(oid primitive.ObjectID) uuid.UUID {
    return uuid.NewSHA1(migrationNamespace, []byte(oid.Hex()))
}
```

This guarantees:
- Same ObjectId always → same UUID
- Works without a lookup table
- Cross-references (e.g., diary entry liked by user X) resolve correctly as long as user X is migrated first

---

## Migration Order (dependency-safe)

```
1. books            — no dependencies
2. users            — no dependencies (skip embedded)
3. follows          — depends on users
4. likes            — depends on users + books
5. bookshelf        — depends on users + books (bookshelf + tbrBooks + currentlyReading merged)
6. favorites        — depends on users + books
7. lists            — depends on users + books (creates lists + list_books + list_access)
8. diary_entries    — depends on users + books
9. diary_likes      — depends on users + diary_entries
10. activities      — depends on all above
11. newsletters     — no dependencies
12. account_deletions — no dependencies
13. user_preferences — depends on users
14. recount stats   — UPDATE users SET books_read_count = ..., lists_count = ..., etc.
```

---

## Data Volume Estimates

Actual counts require running against production MongoDB. Expected ranges for an early-stage app:

| Collection | Estimated documents |
|---|---|
| users | 100–5,000 |
| books | 500–10,000 |
| events | 10,000–500,000 (90-day TTL) |
| otps | 0–50 (ephemeral) |
| newsletters | 10–500 |
| accountdeletions | 0–100 |
| userpreferences | ~= users |
| recommendationcaches | ~= users |
| recommendationlogs | 1,000–50,000 |

---

## Known Data Issues

1. **`listId` in activities is a string** (the subdocument's `_id.toString()`), not an ObjectId. Must look up by the embedded doc's `_id` after lists are migrated.

2. **`diaryEntryId` in activities is `ObjectId | string`** — normalize both to the UUID after diary entries are migrated.

3. **`avatar` may be base64 data** — detect `data:image/` prefix; if base64, either upload to storage or set to null.

4. **`username` is optional in MongoDB** — users without a username cannot be migrated to PG until they set one (PG users table requires username for auth). Options: generate a temp username from email, or skip and migrate when they next log in.

5. **`publishedDate` is a string in Mongo** (e.g., "2023-10-15", "2023", "2023-10") — need flexible date parsing.

6. **`topBooks` can have 4-6 entries** but PG `favorites` is hard-coded to max 4 via `display_order` check. Truncate to 4 on migration.

7. **Duplicate bookshelf entries**: a book could appear in `bookshelf` (read), `currentlyReading`, and `tbrBooks`. PG has `UNIQUE(user_id, book_id)`. Merge with priority: `read` > `reading` > `to-read`.
