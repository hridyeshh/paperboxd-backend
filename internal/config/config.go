package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	Port        string
	Environment string

	DatabaseURL string
	DBMaxConns  int32
	DBMinConns  int32

	RedisURL      string
	RedisPassword string

	JWTSecret                string
	AccessTokenExpiry        time.Duration
	AccessTokenExpiryMobile  time.Duration // longer expiry for tokens issued by /api/mobile/auth/*
	RefreshTokenExpiry       time.Duration

	GoogleBooksAPIKey string
	ISBNdbAPIKey      string
	CohereAPIKey      string

	RateLimitPerMinute int

	CORSAllowedOrigins []string

	InternalSecret string

	ResendAPIKey    string // POST https://api.resend.com/emails Bearer key. Empty → NoopMailer.
	ResendFromEmail string // sender, e.g. "PaperBoxd <onboarding@resend.dev>"

	CloudinaryCloudName string // Cloudinary cloud name. Empty → avatar upload disabled.
	CloudinaryAPIKey    string
	CloudinaryAPISecret string
}

func Load() (*Config, error) {
	// Load .env file (ignore error in production - Railway sets env vars)
	_ = godotenv.Load()

	env := getEnv("ENVIRONMENT", "development")
	defaultRateLimit := 100
	if env == "development" {
		defaultRateLimit = 5000
	}

	return &Config{
		Port:        getEnv("PORT", "8080"),
		Environment: env,

		DatabaseURL: getEnv("DATABASE_URL", ""),
		DBMaxConns:  getEnvAsInt32("DB_MAX_CONNS", 25),
		DBMinConns:  getEnvAsInt32("DB_MIN_CONNS", 5),

		RedisURL:      getEnv("REDIS_URL", "localhost:6379"),
		RedisPassword: getEnv("REDIS_PASSWORD", ""),

		JWTSecret:               getEnv("JWT_SECRET", ""),
		AccessTokenExpiry:       1 * time.Hour,
		AccessTokenExpiryMobile: getEnvAsDuration("TOKEN_EXPIRY_MOBILE", 30*24*time.Hour),
		RefreshTokenExpiry:      30 * 24 * time.Hour,

		GoogleBooksAPIKey: getEnv("GOOGLE_BOOKS_API_KEY", ""),
		ISBNdbAPIKey:      getEnv("ISBNDB_API_KEY", ""),
		CohereAPIKey:      getEnv("COHERE_API_KEY", ""),

		RateLimitPerMinute: getEnvAsInt("RATE_LIMIT_PER_MINUTE", defaultRateLimit),

		CORSAllowedOrigins: getEnvAsStringSlice("CORS_ALLOWED_ORIGINS",
			"http://localhost:3000,http://localhost:3001"),

		InternalSecret: getEnv("INTERNAL_SECRET", ""),

		ResendAPIKey:    getEnv("RESEND_API_KEY", ""),
		ResendFromEmail: getEnv("RESEND_FROM_EMAIL", "PaperBoxd <onboarding@resend.dev>"),

		CloudinaryCloudName: getEnv("CLOUDINARY_CLOUD_NAME", getEnv("NEXT_PUBLIC_CLOUDINARY_CLOUD_NAME", "")),
		CloudinaryAPIKey:    getEnv("CLOUDINARY_API_KEY", ""),
		CloudinaryAPISecret: getEnv("CLOUDINARY_API_SECRET", ""),
	}, nil
}

func (c *Config) Validate() error {
	if c.DatabaseURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	if c.JWTSecret == "" {
		return fmt.Errorf("JWT_SECRET is required")
	}
	if len(c.JWTSecret) < 32 {
		return fmt.Errorf("JWT_SECRET must be at least 32 characters")
	}
	return nil
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func getEnvAsInt32(key string, defaultVal int32) int32 {
	valStr := getEnv(key, "")
	if val, err := strconv.ParseInt(valStr, 10, 32); err == nil {
		return int32(val)
	}
	return defaultVal
}

func getEnvAsInt(key string, defaultVal int) int {
	valStr := getEnv(key, "")
	if val, err := strconv.Atoi(valStr); err == nil {
		return val
	}
	return defaultVal
}

// getEnvAsDuration parses either a Go duration string ("720h", "30m") or a plain
// integer number of seconds. Returns defaultVal when env is unset or unparseable.
func getEnvAsDuration(key string, defaultVal time.Duration) time.Duration {
	val := getEnv(key, "")
	if val == "" {
		return defaultVal
	}
	if d, err := time.ParseDuration(val); err == nil {
		return d
	}
	if secs, err := strconv.Atoi(val); err == nil {
		return time.Duration(secs) * time.Second
	}
	return defaultVal
}

func getEnvAsStringSlice(key, defaultVal string) []string {
	raw := getEnv(key, defaultVal)
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}
