package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/go-chi/httprate"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/hridyesh/paperboxd-backend/migrations"

	"github.com/hridyesh/paperboxd-backend/internal/auth"
	"github.com/hridyesh/paperboxd-backend/internal/cache"
	"github.com/hridyesh/paperboxd-backend/internal/config"
	"github.com/hridyesh/paperboxd-backend/internal/cron"
	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/external"
	"github.com/hridyesh/paperboxd-backend/internal/handler"
	appMiddleware "github.com/hridyesh/paperboxd-backend/internal/middleware"
	"github.com/hridyesh/paperboxd-backend/internal/service"
	"github.com/hridyesh/paperboxd-backend/internal/types"
)

func main() {
	// ── Structured logger ──────────────────────────────────────────────────────
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	// ── Configuration ──────────────────────────────────────────────────────────
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}
	if err := cfg.Validate(); err != nil {
		slog.Error("validate config", "error", err)
		os.Exit(1)
	}

	// ── Database ───────────────────────────────────────────────────────────────
	ctx := context.Background()

	poolConfig, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		slog.Error("parse database config", "error", err)
		os.Exit(1)
	}
	poolConfig.MaxConns = cfg.DBMaxConns
	poolConfig.MinConns = cfg.DBMinConns

	// Register pgvector type codecs on every new connection so columns of type
	// `vector` (incl. NULLs) scan cleanly. We use a local wrapper around the
	// upstream codec because pgvector-go@v0.4.0's pgx scan plan panics on NULL
	// vector columns (slice OOB inside DecodeBinary); the wrapper short-circuits
	// NULL src to a zero-value pgvector.Vector.
	poolConfig.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		// Pin the session to UTC. Every date computation in this codebase is
		// UTC-based (time.Now().UTC() in Go, `AT TIME ZONE 'UTC'` in SQL), but
		// the streak SQL compares against bare CURRENT_DATE — which follows the
		// server's TimeZone setting. Pinning it here keeps the day a streak is
		// written on and the day it is read on from ever disagreeing.
		if _, err := conn.Exec(ctx, "SET TIME ZONE 'UTC'"); err != nil {
			return err
		}
		return db.RegisterPgvectorTypes(ctx, conn)
	}

	dbPool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		slog.Error("create db pool", "error", err)
		os.Exit(1)
	}
	defer dbPool.Close()

	if err := dbPool.Ping(ctx); err != nil {
		slog.Error("ping postgres", "error", err)
		os.Exit(1)
	}
	slog.Info("connected to postgres")

	// ── Auto-migrate ───────────────────────────────────────────────────────────
	// Apply embedded SQL migrations on startup so deploys converge the schema
	// without a separate migrate step. Set AUTO_MIGRATE=false to skip (e.g. when
	// schema changes are gated behind a manual release).
	if !strings.EqualFold(os.Getenv("AUTO_MIGRATE"), "false") {
		if err := runMigrations(cfg.DatabaseURL); err != nil {
			slog.Error("run migrations", "error", err)
			os.Exit(1)
		}
		slog.Info("migrations up to date")
	}

	// ── Redis ──────────────────────────────────────────────────────────────────
	redisAddr := cfg.RedisURL
	redisPassword := cfg.RedisPassword

	// Parse full redis:// URLs (for providers like Railway).
	if strings.HasPrefix(cfg.RedisURL, "redis://") {
		parsedURL, err := url.Parse(cfg.RedisURL)
		if err != nil {
			slog.Error("parse redis url", "error", err)
			os.Exit(1)
		}

		redisAddr = parsedURL.Host

		if parsedURL.User != nil {
			if pass, ok := parsedURL.User.Password(); ok {
				redisPassword = pass
			}
		}
	}

	redisOpts := &redis.Options{
		Addr:         redisAddr,
		Password:     redisPassword,
		DialTimeout:  2 * time.Second,
		ReadTimeout:  1 * time.Second,
		WriteTimeout: 1 * time.Second,
	}
	redisClient := redis.NewClient(redisOpts)
	defer redisClient.Close()

	// Redis is a cache, not a hard dependency: every request-path cache site
	// nil/error-checks and falls back to Postgres. A boot-time blip should
	// degrade to DB-only, not take the whole API down. /health still reports
	// 503-degraded so monitoring sees it.
	if err := redisClient.Ping(ctx).Err(); err != nil {
		slog.Warn("redis unreachable at boot, starting in degraded (DB-only) mode", "error", err)
	} else {
		slog.Info("connected to redis")
	}

	// ── Queries ────────────────────────────────────────────────────────────────
	queries := db.New(dbPool)

	// ── Cache ──────────────────────────────────────────────────────────────────
	cacheClient := cache.New(redisClient)

	// ── Analytics event service ──────────────────────────────────────────────
	eventSvc := service.NewEventService(dbPool)

	// ── Handlers ───────────────────────────────────────────────────────────────
	// NewResendMailer returns NoopMailer when RESEND_API_KEY is empty, so dev
	// environments still run without an email provider — the OTP and password
	// reset paths will 200 but no mail goes out.
	mailer := service.NewResendMailer(cfg.ResendAPIKey, cfg.ResendFromEmail, cfg.AppBaseURL)
	if cfg.ResendAPIKey == "" {
		slog.Warn("RESEND_API_KEY not set; OTP and password-reset emails will not be delivered")
	}
	authHandler := auth.NewHandler(queries, cfg, mailer)
	mobileAuthHandler := auth.NewMobileHandler(authHandler, mailer)
	healthHandler := auth.NewHealthHandler(dbPool, redisClient)
	mobileHealthHandler := handler.NewMobileHealthHandler()

	isbndbClient := external.NewISBNdbClient(cfg.ISBNdbAPIKey)
	googleBooksClient := external.NewGoogleBooksClient(cfg.GoogleBooksAPIKey)
	hardcoverClient := external.NewHardcoverClient(cfg.HardcoverAPIToken)
	if cfg.HardcoverAPIToken == "" {
		slog.Warn("HARDCOVER_API_TOKEN not set; scan will fall back to Open Library counts")
	}
	cloudinaryClient := external.NewCloudinaryClient(cfg.CloudinaryCloudName, cfg.CloudinaryAPIKey, cfg.CloudinaryAPISecret)
	if cloudinaryClient == nil {
		slog.Warn("CLOUDINARY_* not set; avatar upload endpoint will return 503")
	}

	bookHandler := handler.NewBookHandler(queries, cfg, isbndbClient, googleBooksClient, eventSvc)
	bookHandler.Cache = cacheClient
	readLinksSvc := service.NewReadLinksService(queries, googleBooksClient, external.NewAppleBooksClient(), external.NewGutenbergClient())
	bookHandler.ReadLinks = readLinksSvc
	favoritesHandler := handler.NewFavoritesHandler(dbPool, queries, isbndbClient, googleBooksClient)
	listsHandler := handler.NewListsHandler(queries, isbndbClient, googleBooksClient, eventSvc)
	activitiesHandler := handler.NewActivitiesHandler(queries, cacheClient)
	communityHandler := handler.NewCommunityHandler(queries, cacheClient)
	leaderboardHandler := handler.NewLeaderboardHandler(queries, cacheClient)
	referralHandler := handler.NewReferralHandler(queries)
	wrappedHandler := handler.NewWrappedHandler(queries)
	deviceTokenHandler := handler.NewDeviceTokenHandler(queries)
	eventsHandler := handler.NewEventsHandler(dbPool, eventSvc)
	analyticsHandler := handler.NewAnalyticsHandler(dbPool, cacheClient)
	newsletterHandler := handler.NewNewsletterHandler(dbPool)
	authorInfoHandler := handler.NewAuthorInfoHandler(cacheClient)

	var embedder service.Embedder
	if cfg.CohereAPIKey != "" {
		embedder = service.NewCohereEmbedder(cfg.CohereAPIKey)
	} else {
		slog.Warn("COHERE_API_KEY not set; recommendation embeddings disabled")
		embedder = service.NoopEmbedder{}
	}
	recommendationSvc := service.NewRecommendationService(dbPool, embedder, redisClient, eventSvc, cfg.AnthropicAPIKey)
	recommendationHandler := handler.NewRecommendationHandler(recommendationSvc)
	fusionHandler := handler.NewFusionHandler(recommendationSvc)
	bookHandler.RecommendationService = recommendationSvc
	cron.StartNightlyCron(dbPool, recommendationSvc, readLinksSvc)

	thoughtHandler := handler.NewThoughtHandler(queries, isbndbClient, googleBooksClient, recommendationSvc, eventSvc)
	scanHandler := handler.NewScanHandler(dbPool, queries, cfg, isbndbClient, hardcoverClient)
	scanHandler.EventSvc = eventSvc

	userHandler := &handler.UserHandler{
		Queries:               queries,
		Config:                cfg,
		ISBNdb:                isbndbClient,
		GoogleBooks:           googleBooksClient,
		RecommendationService: recommendationSvc,
		Cloudinary:            cloudinaryClient,
		EventSvc:              eventSvc,
	}

	// ── Router ─────────────────────────────────────────────────────────────────
	r := chi.NewRouter()

	// Global middleware
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Logger)
	r.Use(chimiddleware.Recoverer)
	r.Use(chimiddleware.Timeout(30 * time.Second))

	// CORS — origins controlled by CORS_ALLOWED_ORIGINS env var.
	// Browsers send an Origin header; native mobile clients do not. To support both
	// without weakening the browser allowlist, we use AllowOriginFunc and treat a
	// missing Origin as a non-browser caller (allowed). The web allowlist is enforced
	// unchanged for any request that does send Origin.
	allowedOrigins := make(map[string]struct{}, len(cfg.CORSAllowedOrigins))
	for _, o := range cfg.CORSAllowedOrigins {
		allowedOrigins[o] = struct{}{}
	}
	r.Use(cors.Handler(cors.Options{
		AllowOriginFunc: func(_ *http.Request, origin string) bool {
			if origin == "" {
				// Native mobile / curl / server-to-server.
				return true
			}
			_, ok := allowedOrigins[origin]
			return ok
		},
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token", "X-Internal-Secret"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	// Rate limiting: per Bearer token when present, else per IP (see KeyByAuthorizationOrIP).
	// Returns the standard JSON error envelope so mobile + web clients parse 429 uniformly.
	rateLimitHandler := func(w http.ResponseWriter, _ *http.Request) {
		types.WriteError(w, http.StatusTooManyRequests, types.ErrCodeRateLimited, "Too many requests")
	}
	// tightLimit builds a stricter per-route limiter for the endpoints where a
	// single request is expensive or abusable. The global limit below still
	// applies on top of it.
	tightLimit := func(perMinute int) func(http.Handler) http.Handler {
		return httprate.Limit(perMinute, time.Minute,
			httprate.WithKeyFuncs(appMiddleware.KeyByAuthorizationOrIP),
			httprate.WithLimitHandler(rateLimitHandler),
		)
	}
	if cfg.RateLimitPerMinute > 0 {
		r.Use(httprate.Limit(cfg.RateLimitPerMinute, time.Minute,
			httprate.WithKeyFuncs(appMiddleware.KeyByAuthorizationOrIP),
			httprate.WithLimitHandler(rateLimitHandler),
		))
	}

	// ── Routes ─────────────────────────────────────────────────────────────────

	// Health check (no auth). /health is the original deep-check route used by
	// Railway. /api/health is the lightweight mobile reachability probe.
	r.Get("/health", healthHandler.Health)
	r.Get("/api/health", mobileHealthHandler.Get)

	// Native mobile auth (no cookies, long-lived tokens, flat {token,user} shape).
	r.Route("/api/mobile/auth", func(r chi.Router) {
		r.Use(tightLimit(10))
		r.Post("/login", mobileAuthHandler.MobileLogin)
		r.Post("/register", mobileAuthHandler.MobileRegister)
		r.Post("/otp/send", mobileAuthHandler.MobileSendOTP)
		r.Post("/otp/verify", mobileAuthHandler.MobileVerifyOTP)
		r.Post("/google", mobileAuthHandler.MobileGoogleAuth)
		r.Post("/apple", mobileAuthHandler.MobileAppleAuth)
		r.Post("/refresh", mobileAuthHandler.MobileRefresh)
	})

	// Authenticated mobile endpoints (Bearer token required).
	r.Group(func(r chi.Router) {
		r.Use(appMiddleware.Authenticate(cfg.JWTSecret))
		r.Patch("/api/mobile/users/me", mobileAuthHandler.MobileUpdateMe)

		// Push notification token registration. Clients call these on login/token
		// refresh and on logout respectively.
		r.Post("/api/mobile/users/me/device-token", deviceTokenHandler.Register)
		r.Delete("/api/mobile/users/me/device-token", deviceTokenHandler.Deregister)
	})

	// API v1
	r.Route("/api/v1", func(r chi.Router) {
		// Public routes (no auth)
		r.Post("/newsletter/subscribe", newsletterHandler.Subscribe)

		// Analytics ingest. OptionalAuthenticate, not Authenticate: the
		// acquisition events (landing_viewed, signup_started) fire before an
		// account exists. The handler enforces that an unauthenticated caller
		// supplies anon_id and only sends an event service.AllowsAnonymous
		// permits, so this is open to writes but not to arbitrary ones.
		r.With(appMiddleware.OptionalAuthenticate(cfg.JWTSecret)).Post("/events", eventsHandler.Track)

		// Auth routes (no auth middleware)
		r.Route("/auth", func(r chi.Router) {
			// Credential guessing and OTP email-bombing both live here, and a
			// human never needs 10 auth calls a minute.
			r.Use(tightLimit(10))
			r.Post("/register", authHandler.Register)
			r.Post("/login", authHandler.Login)
			r.Post("/refresh", authHandler.Refresh)
			r.Post("/logout", authHandler.Logout)
			r.Get("/check-username", authHandler.CheckUsername)
			r.Post("/forgot-password", authHandler.ForgotPassword)
			r.Post("/reset-password", authHandler.ResetPassword)
			r.Post("/otp/send", authHandler.SendOTP)
			r.Post("/otp/verify", authHandler.VerifyOTP)
			r.Post("/register/send-otp", authHandler.SendRegistrationOTP)
			r.Post("/register/verify-otp", authHandler.VerifyRegistrationOTP)
			r.Post("/google", authHandler.GoogleAuth)
		})

		// Protected routes
		r.Group(func(r chi.Router) {
			r.Use(appMiddleware.Authenticate(cfg.JWTSecret))

			r.Get("/users/me", authHandler.Me)
			r.Delete("/users/me", userHandler.DeleteMe)
			r.Post("/users/me/onboarding", userHandler.SaveOnboarding)
			r.Post("/users/me/daily-open", userHandler.RecordDailyOpen)
			r.Get("/users/me/leaderboard-stats", leaderboardHandler.GetMyLeaderboardStats)
			r.Get("/users/me/referral", referralHandler.GetMyReferralCode)
			r.Get("/users/me/referrals", referralHandler.GetMyReferrals)
			r.Get("/users/me/wrapped", wrappedHandler.Get)
			r.Get("/users/me/taste", recommendationHandler.GetTasteDashboard)
			r.Patch("/users/me/visibility", userHandler.UpdateVisibility)
			r.Get("/users/me/follow-requests", userHandler.ListFollowRequests)
			r.Post("/users/me/follow-requests/{username}", userHandler.AcceptFollowRequest)
			r.Delete("/users/me/follow-requests/{username}", userHandler.RejectFollowRequest)
			r.Patch("/users/me/avatar", userHandler.UpdateAvatar)
			r.Post("/users/me/avatar/upload", userHandler.UploadAvatar)
			r.Post("/users/me/banner/upload", userHandler.UploadBanner)
			r.Post("/reports", userHandler.CreateReport)
		})

		// Authors (public)
		r.Get("/authors/info", authorInfoHandler.Get)

		// Books
		r.Route("/books", func(r chi.Router) {
			r.Get("/search", bookHandler.Search)
			r.Get("/by-slug/{slug}", bookHandler.GetBySlug)
			r.Get("/latest", bookHandler.GetLatest)
			r.Get("/random", bookHandler.GetRandom)
			r.Get("/public", bookHandler.GetPublic)
			r.Get("/by-author", bookHandler.GetByAuthor)

			r.Group(func(r chi.Router) {
				r.Use(appMiddleware.Authenticate(cfg.JWTSecret))
				r.Post("/", bookHandler.Create)
			})

			r.Route("/{id}", func(r chi.Router) {
				r.Get("/", bookHandler.GetByID)
				// OptionalAuthenticate so the block filter sees the viewer.
				r.With(appMiddleware.OptionalAuthenticate(cfg.JWTSecret)).Get("/thoughts", thoughtHandler.GetBookThoughts)
				r.With(appMiddleware.OptionalAuthenticate(cfg.JWTSecret)).Get("/reviews", bookHandler.GetBookReviews)
				// Social proof: Paperboxd reader stats, friends, lists. Friends
				// only when the viewer is signed in.
				r.With(appMiddleware.OptionalAuthenticate(cfg.JWTSecret)).Get("/social", bookHandler.GetBookSocial)
				// "Why you'll like this" — the feed's reason engine on one book.
				r.With(appMiddleware.Authenticate(cfg.JWTSecret)).Get("/fit", bookHandler.GetBookFit)

				r.Group(func(r chi.Router) {
					r.Use(appMiddleware.Authenticate(cfg.JWTSecret))
					r.Post("/like", bookHandler.Like)
					r.Delete("/like", bookHandler.Unlike)
					r.Post("/share", bookHandler.ShareBook)
					r.Get("/friends-reading", bookHandler.GetFriendsReadingBook)
					r.Get("/reviews/friends", bookHandler.GetBookReviewsByFriends)
				})
			})
		})

		// Fusion: one-time invite links and the two-reader story. The preview is
		// open so the web join page can render for signed-out visitors.
		r.Route("/fusions", func(r chi.Router) {
			r.With(appMiddleware.OptionalAuthenticate(cfg.JWTSecret)).Get("/invites/{token}", fusionHandler.PreviewInvite)
			r.Group(func(r chi.Router) {
				r.Use(appMiddleware.Authenticate(cfg.JWTSecret))
				r.Get("/", fusionHandler.List)
				r.With(tightLimit(10)).Post("/invites", fusionHandler.CreateInvite)
				r.Delete("/invites/{token}", fusionHandler.CancelInvite)
				r.With(tightLimit(10)).Post("/invites/{token}/accept", fusionHandler.AcceptInvite)
				r.Get("/{id}", fusionHandler.Get)
				r.Delete("/{id}", fusionHandler.Delete)
			})
		})

		// Activity feed
		// Public community snapshot (trending, activity, lists, readers).
		r.Get("/community", communityHandler.Get)

		r.Route("/activities", func(r chi.Router) {
			r.Use(appMiddleware.Authenticate(cfg.JWTSecret))
			r.Get("/me", activitiesHandler.GetUserActivities)
			r.Get("/following", activitiesHandler.GetFollowingActivities)
			r.Get("/check-new", activitiesHandler.CheckNewActivities)
		})

		// Standalone list collaborators (frontend uses listId without knowing owner username)
		r.Group(func(r chi.Router) {
			r.Use(appMiddleware.Authenticate(cfg.JWTSecret))
			r.Post("/lists/{listId}/collaborators", listsHandler.AcceptCollaboration)
		})

		// Leaderboard (public read, auth required for friends)
		r.Route("/leaderboard", func(r chi.Router) {
			r.Get("/global", leaderboardHandler.GetGlobalLeaderboard)
			r.Get("/dimension/{dimension}", leaderboardHandler.GetLeaderboardByDimension)
			r.Group(func(r chi.Router) {
				r.Use(appMiddleware.Authenticate(cfg.JWTSecret))
				r.Get("/friends", leaderboardHandler.GetFriendsLeaderboard)
			})
		})

		// Recommendations — optional auth so expired/missing tokens still get fallback results
		r.Route("/recommendations", func(r chi.Router) {
			r.Use(appMiddleware.OptionalAuthenticate(cfg.JWTSecret))
			r.Get("/home", recommendationHandler.GetHomeRecommendations)
			r.Get("/similar/{bookId}", recommendationHandler.GetSimilarBooks)
			r.Post("/feedback", recommendationHandler.PostFeedback)
			r.Get("/feedback/options", recommendationHandler.GetFeedbackOptions)
			r.Get("/feed", recommendationHandler.GetFeed)
			r.Get("/twins", recommendationHandler.GetTasteTwins)
			r.Get("/surprise", recommendationHandler.SurpriseMe)
		})

		// Vibe / semantic search — no auth required, personalised when logged in
		r.With(tightLimit(20), appMiddleware.OptionalAuthenticate(cfg.JWTSecret)).Post("/search/vibe", bookHandler.VibeSearch)
		// Personalised, conversational search. Same limit as vibe: both embed
		// the query on every call.
		r.With(tightLimit(20), appMiddleware.OptionalAuthenticate(cfg.JWTSecret)).Post("/search", bookHandler.PersonalisedSearch)
		// Jazy concierge: search with a voice, and permission to ask one
		// question first. Tighter limit: every deck is a Claude completion.
		r.With(tightLimit(10), appMiddleware.OptionalAuthenticate(cfg.JWTSecret)).Post("/jazy", bookHandler.Concierge)
		// Context presets: a situation instead of a sentence. Same pipeline as
		// /search, so the preset constraints and taste ranking both apply.
		r.Get("/search/contexts", recommendationHandler.GetContextPresets)
		r.With(tightLimit(20), appMiddleware.OptionalAuthenticate(cfg.JWTSecret)).Post("/search/context", recommendationHandler.ContextDiscovery)

		// Scan & Know
		r.Group(func(r chi.Router) {
			r.Use(appMiddleware.Authenticate(cfg.JWTSecret))
			// Every scan is a paid Claude completion; the quota is the real
			// limit, this stops a loop burning it in seconds.
			r.With(tightLimit(10)).Post("/scan/analyze", scanHandler.Analyze)
		})

		// Analytics (admin-gated via X-Internal-Secret, not user-token gated)
		r.Route("/analytics", func(r chi.Router) {
			r.Use(appMiddleware.RequireInternalSecret(cfg.InternalSecret))
			r.Get("/overview", analyticsHandler.Overview)
			r.Get("/users", analyticsHandler.Users)
			r.Get("/features", analyticsHandler.Features)
			r.Get("/retention", analyticsHandler.Retention)
			r.Get("/discovery", analyticsHandler.Discovery)
		})

		// Admin — operator-only. Gated by X-Internal-Secret like /analytics, never
		// by a user token: there is no admin role on users, so a Bearer JWT proves
		// nothing about who is allowed to run destructive maintenance.
		r.Route("/admin", func(r chi.Router) {
			r.Use(appMiddleware.RequireInternalSecret(cfg.InternalSecret))
			r.Delete("/cleanup-books", bookHandler.CleanupStaleBooks)
			r.Post("/leaderboard/rebuild", leaderboardHandler.RebuildLeaderboard)
		})

		// Users
		r.Route("/users", func(r chi.Router) {
			r.Get("/search", userHandler.Search)
			r.With(appMiddleware.Authenticate(cfg.JWTSecret)).Get("/suggested", userHandler.Suggested)

			r.Route("/{username}", func(r chi.Router) {
				// Identify the viewer, then refuse every GET under a private
				// profile unless they are the owner or an approved follower.
				// Mounted on the subtree so routes added later inherit it; the
				// bare profile GET is allowed through and redacts itself.
				r.Use(appMiddleware.OptionalAuthenticate(cfg.JWTSecret))
				r.Use(appMiddleware.RequireProfileAccess(queries))

				// Public routes
				r.Get("/", userHandler.GetByUsername)
				r.Get("/likes", userHandler.GetLikes)
				r.Get("/followers", userHandler.GetFollowers)
				r.Get("/following", userHandler.GetFollowing)
				r.Get("/tbr", userHandler.GetUserTBR)
				r.Get("/dnf", userHandler.GetUserDNF)
				r.Get("/authors", userHandler.GetUserAuthors)
				r.Get("/reading", userHandler.GetCurrentlyReading)
				r.Get("/reading/today", userHandler.GetTodayProgress)
				r.Get("/reading/last", userHandler.GetLastLoggedBook)
				r.Get("/reading/activity", userHandler.GetReadingActivity)
				r.Get("/streak", userHandler.GetStreak)
				r.Get("/favorites", favoritesHandler.GetUserFavorites)

				// Favorites (auth-protected)
				r.Group(func(r chi.Router) {
					r.Use(appMiddleware.Authenticate(cfg.JWTSecret))
					r.Post("/favorites", favoritesHandler.AddToFavorites)
					r.Put("/favorites/reorder", favoritesHandler.ReorderFavorites)
					r.Delete("/favorites/{bookId}", favoritesHandler.RemoveFromFavorites)
				})

				// Auth-protected user routes
				r.Group(func(r chi.Router) {
					r.Use(appMiddleware.Authenticate(cfg.JWTSecret))
					r.Put("/", userHandler.Update)
					r.Patch("/", userHandler.Update)
					r.Post("/follow", userHandler.Follow)
					r.Delete("/follow", userHandler.Unfollow)
					r.Post("/block", userHandler.Block)
					r.Delete("/block", userHandler.Unblock)
				})

				// Thought routes. GETs use OptionalAuthenticate so the SQL viewer-id
				// clause sees the requester and surfaces their own private thoughts
				// (and 404s strangers correctly via the IsPrivate check in the
				// single-thought handler).
				r.With(appMiddleware.OptionalAuthenticate(cfg.JWTSecret)).Get("/thoughts", thoughtHandler.GetUserThoughts)
				r.Group(func(r chi.Router) {
					r.Use(appMiddleware.Authenticate(cfg.JWTSecret))
					r.Post("/thoughts", thoughtHandler.CreateThought)
				})
				r.Route("/thoughts/{thoughtId}", func(r chi.Router) {
					r.With(appMiddleware.OptionalAuthenticate(cfg.JWTSecret)).Get("/", thoughtHandler.GetThought)
					r.With(appMiddleware.OptionalAuthenticate(cfg.JWTSecret)).Get("/thread", thoughtHandler.GetThoughtThread)
					r.Group(func(r chi.Router) {
						r.Use(appMiddleware.Authenticate(cfg.JWTSecret))
						r.Put("/", thoughtHandler.UpdateThought)
						r.Delete("/", thoughtHandler.DeleteThought)
						r.Post("/like", thoughtHandler.LikeThought)
						r.Delete("/like", thoughtHandler.UnlikeThought)
						r.Post("/repost", thoughtHandler.RepostThought)
						r.Delete("/repost", thoughtHandler.UnrepostThought)
					})
				})

				// Lists routes (optional JWT: own private lists + saved lists when cookie is forwarded)
				r.With(appMiddleware.OptionalAuthenticate(cfg.JWTSecret)).Get("/lists", listsHandler.GetUserLists)
				r.Group(func(r chi.Router) {
					r.Use(appMiddleware.Authenticate(cfg.JWTSecret))
					r.Post("/lists", listsHandler.CreateList)
				})
				r.Route("/lists/{listId}", func(r chi.Router) {
					r.With(appMiddleware.OptionalAuthenticate(cfg.JWTSecret)).Get("/", listsHandler.GetListDetails)
					r.Group(func(r chi.Router) {
						r.Use(appMiddleware.Authenticate(cfg.JWTSecret))
						r.Put("/", listsHandler.UpdateList)
						r.Delete("/", listsHandler.DeleteList)
						r.Post("/books", listsHandler.AddBookToList)
						r.Delete("/books/{bookId}", listsHandler.RemoveBookFromList)
						r.Post("/share", listsHandler.ShareList)
						r.Post("/save", listsHandler.SaveList)
						r.Delete("/save", listsHandler.UnsaveList)
						r.Post("/access", listsHandler.GrantAccess)
						r.Delete("/access", listsHandler.RevokeAccess)
						r.Get("/access", listsHandler.GetAccessUsers)
					})
				})

				// Bookshelf routes
				r.Route("/bookshelf", func(r chi.Router) {
					r.Get("/", userHandler.GetBookshelf)
					r.Group(func(r chi.Router) {
						r.Use(appMiddleware.Authenticate(cfg.JWTSecret))
						r.Post("/", userHandler.AddToBookshelf)
					})
					r.Route("/{bookId}", func(r chi.Router) {
						r.Group(func(r chi.Router) {
							r.Use(appMiddleware.Authenticate(cfg.JWTSecret))
							r.Get("/status", userHandler.GetBookStatus)
							r.Get("/progress", userHandler.GetReadingProgress)
							r.Delete("/", userHandler.RemoveFromBookshelf)
							r.Patch("/", userHandler.UpdateBookshelfRating)
							r.Put("/tbr", userHandler.UpdateTBRNotes)
							r.Put("/progress", userHandler.UpdateReadingProgress)
							r.Post("/start", userHandler.MarkAsStarted)
							r.Post("/finish", userHandler.MarkAsFinished)
						})
					})
				})
			})
		})
	})

	// ── Server ─────────────────────────────────────────────────────────────────
	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start server in goroutine
	serverErr := make(chan error, 1)
	go func() {
		slog.Info("starting server", "port", cfg.Port, "env", cfg.Environment)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	// ── Graceful shutdown ──────────────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		slog.Error("server error", "error", err)
		os.Exit(1)
	case sig := <-quit:
		slog.Info("shutting down", "signal", sig.String())
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}

	slog.Info("server stopped")
}

// runMigrations applies all pending up migrations from the embedded SQL files.
// It is a no-op when the schema is already current.
func runMigrations(databaseURL string) error {
	src, err := iofs.New(migrations.Files, ".")
	if err != nil {
		return fmt.Errorf("open migration source: %w", err)
	}
	defer src.Close()

	// golang-migrate selects its database driver by URL scheme; the pgx/v5
	// driver registers under "pgx5". Rewrite postgres:// → pgx5:// so we reuse
	// the same DATABASE_URL the app already validated.
	migrateURL := databaseURL
	for _, prefix := range []string{"postgresql://", "postgres://"} {
		if strings.HasPrefix(migrateURL, prefix) {
			migrateURL = "pgx5://" + strings.TrimPrefix(migrateURL, prefix)
			break
		}
	}

	m, err := migrate.NewWithSourceInstance("iofs", src, migrateURL)
	if err != nil {
		return fmt.Errorf("init migrate: %w", err)
	}
	defer m.Close()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}
