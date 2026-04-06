# Changelog

All notable changes to PaperBoxd backend will be documented in this file.

## [2.0.0] - 2026-04-03 - **MAJOR: MongoDB → PostgreSQL Migration**

### Added - Backend Rewrite
- Complete Go backend with 60+ RESTful endpoints
- PostgreSQL 16 database with 7 migration files
- Redis 7 caching layer
- JWT authentication system (bcrypt password hashing)
- Auto-caching book system (15-day TTL)
- Type-safe SQL queries via sqlc
- Comprehensive API documentation (`docs/API.md`)

### Added - New Features
- **Currently Reading** - Track reading progress with page numbers
- **TBR Notes** - Add notes and priority to to-be-read books
- **Top 4 Favorites** - Curate favorite books (max 4, display order)
- **Reading Lists** - Unlimited lists with privacy controls
- **Private Lists** - Username-based access management
- **Diary Entries** - Rich text reading journal
- **Activity Feed** - Social timeline (friends + personal)
- **Newsletter System** - Email subscriptions
- **Account Deletion Tracking** - Historical records

### Changed - Architecture
- Database: MongoDB → PostgreSQL 16
- Backend: Next.js API routes → Go Chi router
- Authentication: NextAuth v5 → JWT tokens
- Caching: None → Redis
- Deployment: Vercel → Railway

### Changed - Data Format
- User IDs: MongoDB ObjectId → UUID v4
- Book IDs: MongoDB ObjectId → UUID v4
- Ratings: Float (0.5 increments) → Integer (1-5)
- Embedded bookshelf → Separate table
- Embedded follows → Separate table

### Migrated
- 39 production users with complete data
- 4,129 books with metadata
- All social connections, lists, diary entries
- Password hashes (bcrypt compatible)

### Performance Improvements
- Book search: 10-50ms (PostgreSQL) vs 200-500ms (MongoDB + API)
- Auto-caching reduces external API calls by 70-80%
- Type-safe queries prevent runtime SQL errors
- Connection pooling for better concurrency

### Migration Timeline
- **Phase 1** (Week 1): Core infrastructure (auth, health)
- **Phase 2** (Week 1): Books, bookshelf, social features
- **Phase 3A** (Week 1): Frontend compatibility layer
- **Phase 3B** (Week 2): Auto-cache system
- **Phase 4A** (Week 2): TBR, currently reading, favorites
- **Phase 4B** (Week 2): Reading lists
- **Phase 4C** (Week 2): Diary entries & activity feed
- **Phase 4D** (Week 3): Testing & documentation
- **Phase 5** (Week 3-4): Frontend integration (in progress)
- **Phase 6** (Week 3): Data migration (completed)

### Deployment
- Railway Hobby plan: $5/month
- PostgreSQL: 5GB storage, 25 connections
- Redis: 256MB memory
- API: https://paperboxd-backend-production-d9e0.up.railway.app
- Region: Singapore (optimal for Indian users)

---

## [1.0.0] - 2025-12-01 - Initial Release

### Added
- Next.js 15 full-stack application
- MongoDB Atlas database
- User authentication (NextAuth v5)
- Book search (Google Books API)
- Bookshelf management
- Social features (follow, like)
- Reading lists
- User profiles
- Dark mode support
