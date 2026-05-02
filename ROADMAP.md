# ROADMAP - PaperBoxd Backend Platform

**Project:** PaperBoxd Go/PostgreSQL Backend  
**Current Stage:** Stabilization and frontend integration support  
**Last Updated:** May 1, 2026

---

## Vision

Provide a reliable, fast, type-safe backend platform that supports web and upcoming mobile clients with shared auth, consistent API contracts, and scalable data architecture.

---

## Timeline Overview

```text
2026 Q1: Go/PostgreSQL Migration COMPLETE
2026 Q2: Frontend Integration Support (Current)
2026 Q3: Mobile Client Enablement
2026 Q4: Optimization & Scale
2027+: Advanced platform capabilities
```

---

## Completed Milestone: Backend Migration

Highlights:
- Go backend with Chi routing
- PostgreSQL with sqlc and migration system
- Redis caching
- JWT + bcrypt auth
- MongoDB -> PostgreSQL data migration with zero loss
- Production deployment on Railway

Outcome:
- Stable production API baseline for frontend and future mobile apps

---

## Current Milestone: Integration Stabilization (Q2 2026)

### Priority 1: Auth Regression Fix
- Resolve login path creating unintended user records
- Add regression coverage to prevent reoccurrence
- Validate with real frontend flows

### Priority 2: API Contract Stability
- Ensure endpoint response shapes remain stable
- Improve error consistency
- Support frontend migration sequencing

### Priority 3: Readiness for Production Scale
- Improve observability and performance baselines
- Verify migration and release procedures
- Harden high-traffic endpoints

---

## Next Milestone: Mobile Enablement (Q3 2026)

- Confirm API ergonomics for Swift/Kotlin clients
- Document auth/session flows for native apps
- Add any missing endpoints required for parity
- Establish versioning policy for mobile-safe contract evolution

---

## Future Milestones (Q4 2026+)

- Recommendation engine enhancements
- Additional caching layers for profile/feed hot paths
- Rate limiting and abuse protection
- Platform-level analytics and operational tooling

---

## Success Metrics

- Auth correctness: zero unintended user creations on login
- API reliability: high success rates on critical routes
- Performance: low-latency responses for cached and common endpoints
- Contract stability: no breaking changes during frontend/mobile rollouts

---

## Immediate Next Steps

1. Close auth blocker
2. Validate endpoint contracts with frontend team
3. Run targeted production smoke tests
4. Track integration issues and resolve quickly
