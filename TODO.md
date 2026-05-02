# TODO - PaperBoxd Backend Support for Frontend Integration

**Last Updated:** May 1, 2026  
**Current Phase:** Frontend Phase 5B -> 5C Support (Auth Bug Blocking)

---

## URGENT - Critical Blocker

### Auth Flow Bug (MUST FIX FIRST)

**Problem:** Login creates new user "hridyesh2309" instead of authenticating existing user "hridyesh"

**Status:** BLOCKING

**Tasks:**
- [ ] Query database for all `hridyesh`-related users and emails
- [ ] Validate `/api/v1/auth/login` behavior directly via curl
- [ ] Validate `/api/v1/auth/register` behavior directly via curl
- [ ] Inspect `internal/handler/auth.go` login handler
- [ ] Inspect `internal/handler/auth.go` register handler
- [ ] Inspect auth service flow in `internal/auth`
- [ ] Verify no fallback path can create users on login failure
- [ ] Check username generation/collision logic
- [ ] Confirm login and register response contracts used by frontend
- [ ] Implement targeted backend fix (if root cause is backend)
- [ ] Remove test user `hridyesh2309`
- [ ] Re-test auth end-to-end against frontend
- [ ] Confirm no new users created during login attempts
- [ ] Document fix in backend changelog/release notes

---

## Backend Auth Stabilization

- [ ] Add/expand auth handler tests for login vs register separation
- [ ] Add regression test for "existing user login never creates user"
- [ ] Add clear logs around auth action path (login/register)
- [ ] Validate password-reset migration and queries
- [ ] Verify sqlc-generated auth queries align with expected behavior

---

## Backend Support for Frontend Phase 5C

### API Contract Hardening
- [ ] Verify response shapes for profile/bookshelf/lists/diary endpoints
- [ ] Confirm flat bookshelf structure is consistently returned
- [ ] Document any endpoint contract changes
- [ ] Ensure pagination metadata consistency across endpoints

### Reliability
- [ ] Add request validation coverage on user/book/activity routes
- [ ] Add error response consistency checks (status + message format)
- [ ] Verify CORS/cookie behavior for Vercel frontend domain

---

## Phase 5D/5E Backend Readiness

- [ ] Run smoke tests for critical endpoints
- [ ] Check Railway health and DB pool behavior under load
- [ ] Ensure migrations are up to date in production
- [ ] Validate production logging/monitoring for auth and profile routes

---

## Daily Priorities

### Today
1. Fix auth bug root cause
2. Verify login flow no longer creates users
3. Unblock frontend endpoint migration

### This Week
- Finalize auth stability
- Support frontend API migration with contract guarantees
- Prep backend for production validation window

---

## Notes

- Frontend migration is blocked until auth behavior is fixed
- Backend and frontend must share a stable auth contract
- Keep scope tight: fix blocker first, then support broader migration
