package config

import (
	"fmt"
	"os"
	"strconv"
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

	JWTSecret          string
	AccessTokenExpiry  time.Duration
	RefreshTokenExpiry time.Duration

	GoogleBooksAPIKey string
	ISBNdbAPIKey      string
}

func Load() (*Config, error) {
	// Load .env file (ignore error in production - Railway sets env vars)
	_ = godotenv.Load()

	return &Config{
		Port:        getEnv("PORT", "8080"),
		Environment: getEnv("ENVIRONMENT", "development"),

		DatabaseURL: getEnv("DATABASE_URL", ""),
		DBMaxConns:  getEnvAsInt32("DB_MAX_CONNS", 25),
		DBMinConns:  getEnvAsInt32("DB_MIN_CONNS", 5),

		RedisURL:      getEnv("REDIS_URL", "localhost:6379"),
		RedisPassword: getEnv("REDIS_PASSWORD", ""),

		JWTSecret:          getEnv("JWT_SECRET", ""),
		AccessTokenExpiry:  15 * time.Minute,
		RefreshTokenExpiry: 30 * 24 * time.Hour,

		GoogleBooksAPIKey: getEnv("GOOGLE_BOOKS_API_KEY", ""),
		ISBNdbAPIKey:      getEnv("ISBNDB_API_KEY", ""),
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
