# PaperBoxd — Data Flow & Privacy Audit (Phase 1)

**Status:** Read-only audit. No product code was modified.
**Date:** 2026-07-18
**Scope:** `paperboxd-backend` (Go), `paperboxd` (Next.js web), `paperboxd-ios` (SwiftUI), `paperboxd-android` (Kotlin).
**Method:** Every claim below cites the file (and line where useful) it was verified against. Where the source-of-truth is an environment variable or a hosting-provider setting, that is called out as *not verifiable from the repo* rather than assumed.

> **Three assumptions in the original brief did NOT match the code — read these first:**
> 1. **Scan & Know does not send any image anywhere.** The barcode is decoded to an ISBN *on-device* (iOS `AVCaptureMetadataOutput`, Android ML Kit). Only the ISBN *string* reaches the backend; the image/camera frames are never stored or transmitted. Claude receives book metadata + the user's reading profile — **never a photo**.
> 2. **There is no FCM / push-notification system.** No Firebase, no APNs registration, no device tokens anywhere in any repo. The "Notifications" screens are in-app feeds served from Postgres.
> 3. **There is no "Reads" AI content-card feature and no RevenueCat billing.** Claude (Anthropic) is called from exactly one place — Scan & Know. No payment processor is wired; the scan "paywall" is a plain integer counter.

---

## 1. Personal data collected & where it enters the system

### Auth / registration fields
| Field | Entry point | Evidence |
|---|---|---|
| Email | Web credentials, mobile register/login, OTP, Google | `internal/auth/mobile.go:140` (register), `:92` (login), `paperboxd/lib/auth.ts:87` |
| Password (hashed) | Web + mobile register | bcrypt cost 12 `internal/auth/password.go:9`; web `bcryptjs` `paperboxd/lib/auth.ts:148` |
| Username | Auto-generated from email, editable | `internal/auth/mobile.go:165` (`generateUniqueUsername`), `:454` (`MobileUpdateMe`) |
| OTP code (hashed) | Email OTP login/register | `otp_codes` table `migrations/000011`; hashed, 5-attempt cap, 10-min expiry `internal/auth/mobile.go:245`, `:34` |

Password reset uses a one-time OTP session token on web (`paperboxd/lib/auth.ts:109-140`) and `migrations/000009_password_reset_tokens`.

### Google OAuth data captured
- **Scopes requested are identical across all three clients: `openid email profile`** — iOS `PaperBoxd/Network/GoogleOAuth.swift:20`, Android `GetSignInWithGoogleOption` (default email/profile) `auth/google/GoogleSignInHelper.kt:44`, web NextAuth GoogleProvider `paperboxd/lib/auth.ts:189`.
- Fields the backend reads from Google's token: `aud, sub, email, email_verified, name, picture` — `internal/auth/mobile.go:543-550`.
- **Fields actually persisted: `email` and `name` only.** The Google profile `picture` is read but never written to the DB (`CreateUser` sets only username/email/name — `internal/auth/mobile.go:375`). Web explicitly nulls the Google image out of the session/JWT (`paperboxd/lib/auth.ts:233`, `:278`).
- Mobile verifies the id_token against Google's `tokeninfo` endpoint and enforces the `aud` allowlist (see §5). Web trusts NextAuth's own Google verification, then syncs to the Go backend server-to-server via `X-Internal-Secret` (`internal/auth/auth.go:382-390`).

### Profile fields (all optional, user-supplied)
`name`, `avatar_url`, `bio`, `pronouns` — `migrations/000001_initial_schema.up.sql:12-15`; `birthday DATE` — `migrations/000003_frontend_compatibility.up.sql:3`; `links TEXT[]`, `banner_url` — `migrations/000003`, `migrations/000029_add_banner_url`; `favorite_genres`, `is_public`, `settings JSONB` — `migrations/000001:17-19`.

> **`birthday` (DATE) exists on the users table** but is not requested at signup and is not used for any age check — it is an optional profile field. See §6.

### User-generated content (can contain sensitive free-text disclosure)
- Diary entries — `migrations/000006_diary_and_activities`, `internal/handler/diary.go`
- Reviews — stored on the bookshelf row, `migrations/000021_bookshelf_review`, `migrations/000022_bookshelf_review_edited`
- Lists (names/descriptions) — `migrations/000005_reading_lists`, `internal/handler/lists.go`
- Bookshelf status, ratings, page progress, favorites — `internal/handler/bookshelf.go`, `migrations/000004`, `migrations/000020_reading_log`

### Device / technical data
- **IP address + User-Agent are captured only on web login**, stored in `refresh_tokens.device_info` (JSONB) — `internal/auth/auth.go:562-570` (`extractDeviceInfo`), table `migrations/000001:48`. Mobile issues a long-lived JWT with **no refresh-token row**, so it captures neither (`internal/auth/mobile.go:85-89`).
- IP is also used transiently for rate-limit bucketing (in-memory, not persisted) — `internal/middleware/ratelimit_key.go`.
- First-party analytics events (`user_id, event_type, book_id, metadata, session_id, source, path, created_at`) — `migrations/000018_temporal_signals`, `migrations/000028_analytics_event_columns`, `internal/handler/events.go`. **No IP, no User-Agent stored in the events table.**

### Camera / photo — Scan & Know
- iOS: live `AVCaptureSession` restricted to barcode metadata (`.ean13/.ean8/.upce`); the delegate emits only `obj.stringValue` (the ISBN) and never captures a still — `paperboxd-ios/.../Scan/BarcodeScannerView.swift:90`, `:120-132`.
- Android: CameraX + ML Kit on-device barcode scanner; each `ImageProxy` is `.close()`d immediately after decode, only `barcodes.firstOrNull()?.rawValue` is emitted — `paperboxd-android/.../scan/CameraScanner.kt:83-99`. Dependency is the **bundled, fully on-device** `com.google.mlkit:barcode-scanning:17.3.0`.
- Backend `POST /api/v1/scan/analyze` accepts `{ "isbn": string }` — **not an image** — `internal/handler/scan.go:130-141`.
- **Net: no image data leaves the device.** The only thing sent onward from a scan is the ISBN string.

### Photo library
- iOS declares `NSPhotoLibraryAddUsageDescription` (saving the share card to Photos) and `NSCameraUsageDescription` — `paperboxd-ios/PaperBoxd/Info.plist`. Avatar images are user-selected and uploaded (see Cloudinary, §2).

### Location data
- **None collected on any platform.** No `CLLocation` on iOS, no location permission in the Android manifest (only `CAMERA` + `INTERNET`), no location fields in any table or analytics event.

---

## 2. Third-party services that receive data

| Service | What is sent | PII? | Why | Evidence |
|---|---|---|---|---|
| **Google** (OAuth / Identity) | Standard OAuth handshake; we *receive* email/name | Auth of the user's own Google identity | Sign-in | `GoogleOAuth.swift:38`, `GoogleSignInHelper.kt:44`, `lib/auth.ts:189` |
| **Anthropic (Claude)** | Book metadata + the reader's reading profile: books read, genre distribution, ratings, favorite book **titles**, reading pace, and the **usernames of followed users who own the book** — *no name/email/photo* | Reading behaviour + follower usernames = personal data | Scan & Know scoring | `internal/handler/scan.go:440` (`api.anthropic.com`), model `claude-sonnet-4-6` `:430`, prompt build `:570-637`, follower usernames `:555-558` |
| **Brave Search** | `"<title> <author> reddit/goodreads/amazon review"` | No user PII | Community sentiment for scan | `internal/handler/scan.go:926-942` |
| **Hardcover** | ISBN13 / title | No user PII | Community rating counts | `internal/external/hardcover.go`, `scan.go:854` |
| **Open Library** | ISBN | No user PII | Reader/rating counts | `internal/handler/scan.go:882` |
| **ISBNdb** | ISBN | No user PII | Book metadata | `internal/external/isbndb.go`, `scan.go:170` |
| **Google Books** | Search query text | Query text only | Book search/metadata | `internal/external/google_books.go` |
| **Cloudinary** | **User-uploaded avatar/banner images** (≤5 MB), signed server-side (secret never on client) | Yes — profile imagery | Image hosting | `internal/handler/users.go:452-475` (`UploadAvatar`), `internal/external/cloudinary.go` |
| **Resend** | Recipient **email address** + OTP/reset code | Yes — email | Transactional email (OTP, reset) | `internal/service/mailer_resend.go`; web SMTP `paperboxd/.env.example` |
| **Wikipedia/Wikimedia** | Author name lookups (outbound) | No user PII | Author bios | `internal/handler/author_info.go:169` |
| **Railway** | Hosts Postgres + Redis (**all data at rest**) | Yes — everything | Infrastructure | `GO_DATABASE_URL` → `*.proxy.rlwy.net` |
| **Vercel** | Hosts web app (processes all web traffic) | Yes — in transit | Infrastructure | Next.js/`@vercel/og` in `paperboxd/package.json` |
| **MongoDB Atlas** | Legacy user store: email, password hash, name, avatar, password-reset token | Yes | Legacy web credentials auth | `paperboxd/lib/auth.ts:6`, `:100` |

**Analytics processors: none.** No PostHog, Mixpanel, Amplitude, Segment, Sentry, GA, or Firebase Analytics in any repo (grep of all four `package.json`/gradle/SwiftPM + source). All product analytics are first-party in the `events` Postgres table.

**Note on data residency:** Railway/MongoDB Atlas/Cloudinary regions are configured outside the repo and are **not verifiable from source**. The brief states Railway = Singapore, Anthropic/Cohere/Cloudinary = US — confirm before the policy asserts specific regions.

**Note:** `COHERE_API_KEY`/`CohereAPIKey` and `GOOGLE_BOOKS_API_KEY` are wired in config (`internal/config/config.go:31`, `:29`) for embeddings/search; Cohere is used by the embedding/recommendation path (`internal/service/embedding_service.go`) on book text, not user PII.

---

## 3. Cookies, local storage, mobile persistent storage

| Surface | What's stored | Sensitive? | Evidence |
|---|---|---|---|
| Web cookies | NextAuth JWT session (`next-auth.session-token` / `__Secure-next-auth.session-token`), CSRF + callback cookies; 30-day maxAge; httpOnly; `__Secure`/`__Host` prefixes in prod | Session token | `paperboxd/lib/auth.ts:300-303`, cookie names `app/api/users/delete-account/route.ts:25-29` |
| Web (Go session) | Go-backend JWT held in a server-set cookie for the BFF proxy | Session token | `paperboxd/lib/auth/jwt-session.ts` (referenced `delete-account/route.ts:4`) |
| iOS Keychain | `auth.token` (JWT) + `auth.user` (cached user JSON), `kSecAttrAccessibleAfterFirstUnlock` | Yes — encrypted by Keychain | `paperboxd-ios/PaperBoxd/Keychain/KeychainManager.swift:53-67` |
| iOS UserDefaults | Only `pb_scans_remaining` (scan quota) | No | grep of `PaperBoxd` |
| Android | **EncryptedSharedPreferences** (AES256-GCM master key, AES256-SIV keys / AES256-GCM values) holding `auth.token` + `auth.user` | Yes — encrypted at rest | `paperboxd-android/.../data/local/SecurePrefs.kt:24-59` |

**Confirmed: no auth token or PII in plain UserDefaults / plain SharedPreferences on either mobile platform.**

---

## 4. Data retention & deletion

- **Account deletion is a soft-delete, not a cascade hard-delete.** `DELETE /api/v1/users/me` → `RecordAccountDeletion` (audit row) → `SoftDeleteUser` → `RevokeAllUserTokens` — `internal/handler/users.go:279-323`.
- `SoftDeleteUser` sets `deleted_at = NOW()` and **anonymizes the identifiers** (`email → 'd_<uuid>@deleted.local'`, `username → 'd_<uuid>'`) to free them for re-registration — `queries/users.sql:64-76`. All other rows (diary entries, reviews, lists, bookshelf, events, embeddings) **remain in Postgres**, linked to the still-present user UUID.
- Exit reasons + the original email/username are copied to the `account_deletions` audit table (retained) — `migrations/000010_account_deletions`, `migrations/000007` (table def), `users.go:302`.
- **No hard-delete / purge job exists.** The only scheduled job is `recomputeStaleProfiles` — `internal/cron/nightly.go:26-33`. There is **no documented retention window** after which soft-deleted rows are erased.
- **No self-serve data export exists** anywhere in any repo.
- Web deletion path proxies to the same backend endpoint and clears NextAuth cookies — `paperboxd/app/api/users/delete-account/route.ts`.

---

## 5. Security posture actually in place

| Control | Status | Evidence |
|---|---|---|
| Password hashing | bcrypt cost 12 (Go); `bcryptjs` (web legacy) | `internal/auth/password.go:9`; `paperboxd/lib/auth.ts:148` |
| JWT expiry | Web access 1h, refresh 30d; **mobile access 30d, no refresh row**; web NextAuth session 30d | `internal/config/config.go:82-84`; `internal/auth/mobile.go:5,85`; `lib/auth.ts:302` |
| JWT storage | iOS Keychain / Android EncryptedSharedPreferences / web httpOnly cookie | §3 |
| JWT secret validation | Required, min 32 chars, fails startup otherwise | `internal/config/config.go:114-125` |
| Google OAuth audience check | **Enforced, fail-closed** — empty allowlist rejects every token | `internal/auth/mobile.go:607-610`; test `mobile_google_test.go:68` (`EmptyAllowlistFailsClosed`) |
| OTP hardening | Hashed at rest, 5-attempt cap, 10-min expiry, generic "if an account exists" response | `internal/auth/mobile.go:206-263` |
| Rate limiting | `httprate`, keyed by JWT-hash else IP; in-memory (per-instance) | `internal/middleware/ratelimit_key.go`, `internal/config/config.go:90` |
| Server-to-server auth | Web→backend Google sync gated by `X-Internal-Secret` | `internal/auth/auth.go:387` |
| Cloudinary upload | Signed **server-side**; secret never reaches client; 5 MB cap | `internal/handler/users.go:452-475` |
| HTTPS/TLS | **Not enforced in app code** — terminated at Railway/Vercel edge. No HSTS middleware. | (absence; no TLS/HSTS code found) |

> **`GOOGLE_OAUTH_ALLOWED_AUDIENCES` deployment status:** the *mechanism* is live and fail-closed in code. The `.env.example` ships it **empty**, and the actual production value is a Railway env var **not verifiable from the repo**. Per project memory the iOS client ID is configured (Handoff A done) and the Android Web client ID was pending. **Confirm the prod env var is populated before the policy claims Google login is protected against token-substitution.**

---

## 6. Children / minors

- **No age gate exists.** No repo requests date-of-birth at signup, and nothing checks a minimum age. Registration is email/password or Google only (`internal/auth/mobile.go:139`, `paperboxd/lib/auth.ts:91`).
- The `birthday DATE` column (`migrations/000003`) is an optional profile field, never populated by any signup flow and never used for verification.
- **This is a gap.** The Privacy Policy will need either an age-gate implementation or an explicit "not intended for users under 13/16, no age verification performed" clause (which carries its own COPPA/DPDP-minor risk).

---

## 7. Payments

- **RevenueCat is NOT wired. No payment processor of any kind exists** (no RevenueCat/Stripe/Play Billing/StoreKit in any repo).
- The Scan & Know "paywall" is a server-side integer counter: `users.scan_uses_remaining` defaults to 7 (`migrations/000030_add_scan_feature`), decremented after a successful scan (`internal/handler/scan.go:369`), with a hard-coded `scanUnlimited=false` flag and an internal email allowlist (`scan.go:102-108`). No money changes hands.
- **Draft no billing/subscription terms as active.** Mark any subscription section "future / placeholder only."

---

## 8. AI-generated content disclosure points

- **The only AI feature is Scan & Know.** Claude (`claude-sonnet-4-6`) returns a 0–100 fit score, per-dimension scores, a verdict, "for you / against you" reasons, and a one-line take — `internal/handler/scan.go:82-89`, `:413-490`. Shown to the user on the reveal screen (iOS `Features/Scan/RevealScreen.swift`, Android `ui/screens/scan/RevealScreen.kt`).
- **No "Reads" content-card AI feature exists** (grep for it returns nothing; Claude is called from `scan.go` only).
- **AI labeling in the UI is not confirmed.** No explicit "AI-generated" disclosure string was located in the scan reveal screens during this pass. **Flag for a decision:** add an AI-generated label, and confirm whether scan output is presented as opinion vs. fact.

---

## Consolidated data inventory

| Data Type | Collection Point | Purpose | Third Parties | Where Stored | Retention |
|---|---|---|---|---|---|
| Email | Register / login / OTP / Google | Identity, login, transactional email | Resend (send), Google (verify) | Postgres `users`; MongoDB (legacy) | Until deletion → anonymized on soft-delete, audit email kept |
| Password hash | Register (email/pw only) | Auth | — | Postgres `users`, MongoDB | Same |
| Username / name | Auto + edit; Google `name` | Identity, display | Claude (followed usernames, scan only) | Postgres | Anonymized on soft-delete |
| Profile: bio, pronouns, avatar, banner, links, birthday, genres | Edit Profile (optional) | Profile display | Cloudinary (images) | Postgres; images on Cloudinary | Retained until deletion (rows persist) |
| Reviews, diary entries, lists | In-app authoring | Core product | — | Postgres | **Persist after soft-delete** |
| Bookshelf / ratings / page progress | In-app + Goodreads CSV import (parsed on-device) | Reading tracking | Claude (aggregated, scan only) | Postgres | Persist after soft-delete |
| ISBN (from barcode) | Scan camera (decoded on-device) | Book lookup + scoring | ISBNdb, Hardcover, Open Library, Brave, Claude | Not stored per-user; scan cache keyed by ISBN | Community scan cache 24h TTL (`migrations/000030`) |
| OTP / reset codes | OTP/reset flows | Passwordless auth | Resend | Postgres (hashed) | 10-min expiry |
| IP + User-Agent | **Web login only** | Device tracking on session | — | Postgres `refresh_tokens.device_info` | Until token revoked |
| Analytics events | Web + mobile event calls | First-party product analytics | **None** | Postgres `events` | No documented purge |
| Session/JWT | All clients | Session | — | Keychain / EncryptedSharedPreferences / httpOnly cookie | 30-day token life |
| Newsletter email | Newsletter signup | Marketing email | — | Postgres `newsletter_subscriptions` (`migrations/000019`) | No documented purge |

---

## Open questions / gaps — product decisions needed before the policy can be finalized

1. **Soft-delete leaves user content in Postgres indefinitely.** Deletion anonymizes email/username but does **not** remove diary entries, reviews, lists, bookshelf, or analytics events, and there is **no purge job or documented retention window**. Decide: true cascade delete, a scheduled hard-delete after N days, or a policy that honestly states content is retained.
2. **No self-serve data export.** DPDP/GDPR/CCPA portability will have to be described as a **manual email request** unless an export endpoint is built.
3. **No age gate.** Decide between implementing one or publishing a "not intended for under-13/16, no verification" clause.
4. **Grievance Officer (DPDP Act 2023) not designated.** Needs a name + email + response-time commitment before publishing.
5. **`GOOGLE_OAUTH_ALLOWED_AUDIENCES` prod value unverifiable from repo.** Confirm it is populated on Railway (and includes the Android Web client ID) before claiming Google-login protection.
6. **AI output is not labeled as AI in the UI (unconfirmed).** Decide on an "AI-generated, may be inaccurate" label for Scan & Know.
7. **Data residency (Railway / Mongo Atlas / Cloudinary regions) is not in the repo.** Confirm actual regions before the international-transfer clause names them.
8. **Followed users' usernames are sent to Anthropic during a scan** (`scan.go:555-558`). Minor, but the policy's Anthropic disclosure should be accurate about this.
9. **Legacy MongoDB Atlas still holds user PII** (email, password hash, name, avatar) for web credentials auth, in parallel with Postgres. Deleting a Postgres account does **not** clearly delete the Mongo record — confirm and reconcile, or the "we delete your data" claim is incomplete.
10. **🟠 SECURITY — real secrets in a local file, but NOT in git (verified 2026-07-18):** `paperboxd/.env.example` contains **real production secrets** — a live MongoDB Atlas URI with password, the NextAuth secret, the Google client secret, a live Resend SMTP password, the Cloudinary API secret, and a direct Railway Postgres connection string with password (`GO_DATABASE_URL`). **However:** `git ls-files` shows it untracked, `git check-ignore` shows it matched by `.env*` (`.gitignore:34`), and `git log --all --full-history` returns nothing — **it was never committed to any ref.** Risk is therefore local-disk only, not repo exposure. Residual action (low priority): the file is misleadingly *named* `.env.example` while the ignore rule is `.env*` — one ignore-rule change from an accidental commit. Rename to `.env` (or move secrets there) so the safe example name is free again. Secret rotation is optional, not urgent.

---

*End of Phase 1. Per the brief, stopping here for review before drafting the Privacy Policy and Terms of Service in Phase 2.*
