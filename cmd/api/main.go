package main

import (
	"context"
	"log/slog"
	"net/url"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/go-chi/httprate"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/hridyesh/paperboxd-backend/internal/auth"
	"github.com/hridyesh/paperboxd-backend/internal/config"
	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/external"
	"github.com/hridyesh/paperboxd-backend/internal/handler"
	appMiddleware "github.com/hridyesh/paperboxd-backend/internal/middleware"
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
		Addr:     redisAddr,
		Password: redisPassword,
	}
	redisClient := redis.NewClient(redisOpts)
	defer redisClient.Close()

	if err := redisClient.Ping(ctx).Err(); err != nil {
		slog.Error("ping redis", "error", err)
		os.Exit(1)
	}
	slog.Info("connected to redis")

	// ── Queries ────────────────────────────────────────────────────────────────
	queries := db.New(dbPool)

	// ── Handlers ───────────────────────────────────────────────────────────────
	authHandler := auth.NewHandler(queries, cfg)
	healthHandler := auth.NewHealthHandler(dbPool, redisClient)

	isbndbClient := external.NewISBNdbClient(cfg.ISBNdbAPIKey)
	googleBooksClient := external.NewGoogleBooksClient(cfg.GoogleBooksAPIKey)

	bookHandler := handler.NewBookHandler(queries, cfg, isbndbClient, googleBooksClient)
	userHandler := &handler.UserHandler{
		Queries:     queries,
		Config:      cfg,
		ISBNdb:      isbndbClient,
		GoogleBooks: googleBooksClient,
	}

	// ── Router ─────────────────────────────────────────────────────────────────
	r := chi.NewRouter()

	// Global middleware
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Logger)
	r.Use(chimiddleware.Recoverer)
	r.Use(chimiddleware.Timeout(30 * time.Second))

	// CORS
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins: []string{
			"http://localhost:3000",
			"http://localhost:3001",
			"https://*.vercel.app",
		},
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	// Rate limiting: 100 requests per minute per IP
	r.Use(httprate.LimitByIP(100, time.Minute))

	// ── Routes ─────────────────────────────────────────────────────────────────

	// Health check (no auth)
	r.Get("/health", healthHandler.Health)

	// API v1
	r.Route("/api/v1", func(r chi.Router) {
		// Auth routes (no auth middleware)
		r.Route("/auth", func(r chi.Router) {
			r.Post("/register", authHandler.Register)
			r.Post("/login", authHandler.Login)
			r.Post("/refresh", authHandler.Refresh)
			r.Post("/logout", authHandler.Logout)
		})

		// Protected routes
		r.Group(func(r chi.Router) {
			r.Use(appMiddleware.Authenticate(cfg.JWTSecret))

			r.Get("/users/me", authHandler.Me)
		})

		// Books
		r.Route("/books", func(r chi.Router) {
			r.Get("/search", bookHandler.Search)

			r.Group(func(r chi.Router) {
				r.Use(appMiddleware.Authenticate(cfg.JWTSecret))
				r.Post("/", bookHandler.Create)
			})

			r.Route("/{id}", func(r chi.Router) {
				r.Get("/", bookHandler.GetByID)

				r.Group(func(r chi.Router) {
					r.Use(appMiddleware.Authenticate(cfg.JWTSecret))
					r.Post("/like", bookHandler.Like)
					r.Delete("/like", bookHandler.Unlike)
				})
			})
		})

		// Admin
		r.Route("/admin", func(r chi.Router) {
			r.Use(appMiddleware.Authenticate(cfg.JWTSecret))
			r.Delete("/cleanup-books", bookHandler.CleanupStaleBooks)
		})

		// Users
		r.Route("/users", func(r chi.Router) {
			r.Get("/search", userHandler.Search)

			r.Route("/{username}", func(r chi.Router) {
				// Public routes
				r.Get("/", userHandler.GetByUsername)
				r.Get("/bookshelf", userHandler.GetBookshelf)
				r.Get("/likes", userHandler.GetLikes)
				r.Get("/followers", userHandler.GetFollowers)
				r.Get("/following", userHandler.GetFollowing)

				// Auth-protected routes
				r.Group(func(r chi.Router) {
					r.Use(appMiddleware.Authenticate(cfg.JWTSecret))
					r.Put("/", userHandler.Update)
				r.Patch("/", userHandler.Update)
					r.Post("/bookshelf", userHandler.AddToBookshelf)
					r.Delete("/bookshelf/{bookId}", userHandler.RemoveFromBookshelf)
					r.Post("/follow", userHandler.Follow)
					r.Delete("/follow", userHandler.Unfollow)
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
