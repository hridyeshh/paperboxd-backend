# PLAN - Backend Execution Strategy for Frontend Integration

**Project:** PaperBoxd Backend (`paperboxd-backend`)  
**Goal:** Keep auth and API contracts stable while frontend migrates fully to Go/PostgreSQL backend  
**Timeline:** Immediate blocker fix + 1-2 weeks of integration support  
**Status:** Auth bug investigation is highest priority

---

## Overview

The backend is production-deployed and feature-complete for parity. Current objective is to remove auth regression risk and provide contract reliability so frontend endpoint migration can complete quickly and safely.

---

## Execution Phases

### Phase A: Auth Bug Investigation & Fix (CRITICAL)

**Target Duration:** 2-4 hours  
**Status:** BLOCKING

1. Reproduce with direct API calls (`/auth/login`, `/auth/register`)
2. Compare DB state before/after each call
3. Trace handler path in `internal/handler/auth.go`
4. Validate service/repository behavior in `internal/auth` and db queries
5. Patch root cause
6. Add regression tests
7. Verify with frontend login flow

**Success Criteria:**
- Login of existing user never creates a new user
- Register is the only code path that creates users
- Frontend receives expected login payload

---

### Phase B: Contract Verification for Frontend Migration

**Target Duration:** 1-2 days

- Validate response shape consistency for all high-traffic endpoints
- Ensure bookshelf/list payloads are stable and documented
- Standardize error payloads across user/book/activity handlers
- Confirm authentication headers/cookies work in frontend environments

---

### Phase C: Reliability & Deployment Readiness

**Target Duration:** 1-2 days

- Expand automated tests for auth, profile, bookshelf, and list flows
- Validate migrations and sqlc query generation health
- Check production observability (logs/errors/latency)
- Confirm CORS and env configuration for production domain(s)

---

## Risks & Mitigation

- **Auth bug persists:** isolate with API-level tests and DB diffing
- **Contract drift during frontend migration:** add a written endpoint checklist and regression tests
- **Production-only errors:** run targeted smoke checks against Railway before frontend rollout

---

## Next Actions

1. Investigate and fix auth bug immediately
2. Add regression tests for login/register separation
3. Publish/confirm endpoint contracts for frontend consumption
4. Support frontend validation pass for Phase 5C/5D
