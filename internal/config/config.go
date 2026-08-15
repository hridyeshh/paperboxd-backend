package config

import (
	"encoding/base64"
	"encoding/json"
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

	// AllowedGoogleAudiences is the allowlist of Google OAuth client IDs that a
	// mobile id_token's `aud` claim must match. Google's tokeninfo endpoint
	// validates signature/expiry/issuer but NOT that the token was minted for
	// us, so we enforce audience here. One entry per native client (iOS, then
	// the Android Web-type serverClientId).
	AllowedGoogleAudiences []string

	// AllowedAppleAudiences is the allowlist for Sign in with Apple identity
	// tokens' `aud` claim (the app's bundle ID). Same rationale as Google above.
	AllowedAppleAudiences []string

	ResendAPIKey    string // POST https://api.resend.com/emails Bearer key. Empty → NoopMailer.
	ResendFromEmail string // sender, e.g. "PaperBoxd <onboarding@resend.dev>"

	// AppBaseURL is the public web origin used to build links inside emails the
	// backend sends itself (currently the password-reset link, which lands on
	// the Next.js /auth/reset-password page). No trailing slash.
	AppBaseURL string

	CloudinaryCloudName string // Cloudinary cloud name. Empty → avatar upload disabled.
	CloudinaryAPIKey    string
	CloudinaryAPISecret string

	AnthropicAPIKey string

	BraveAPIKey string

	HardcoverAPIToken string

	// PushEnabled gates outbound push notifications. Off by default so a deploy
	// missing provider credentials starts cleanly with pushes disabled instead
	// of erroring per-send. When on, Validate() requires every field below.
	PushEnabled bool

	// FCMServiceAccountJSON is the Firebase service account key for Android
	// sends — either the raw JSON blob or a path to it on disk.
	FCMServiceAccountJSON string

	// APNs credentials for iOS. Direct APNs (token-based, .p8 auth key) rather
	// than routing iOS through Firebase, so the app target keeps zero
	// third-party runtime dependencies. APNsTopic is the app's bundle ID.
	APNsKeyPath string
	APNsKeyID   string
	APNsTeamID  string
	APNsTopic   string
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

		AllowedGoogleAudiences: getEnvAsStringSlice("GOOGLE_OAUTH_ALLOWED_AUDIENCES", ""),
		AllowedAppleAudiences:  getEnvAsStringSlice("APPLE_ALLOWED_AUDIENCES", "com.paperboxd.PaperBoxd"),

		ResendAPIKey:    getEnv("RESEND_API_KEY", ""),
		ResendFromEmail: getEnv("RESEND_FROM_EMAIL", "PaperBoxd <onboarding@resend.dev>"),

		AppBaseURL: strings.TrimRight(getEnv("APP_BASE_URL", "https://paperboxd.in"), "/"),

		CloudinaryCloudName: getEnv("CLOUDINARY_CLOUD_NAME", getEnv("NEXT_PUBLIC_CLOUDINARY_CLOUD_NAME", "")),
		CloudinaryAPIKey:    getEnv("CLOUDINARY_API_KEY", ""),
		CloudinaryAPISecret: getEnv("CLOUDINARY_API_SECRET", ""),

		AnthropicAPIKey: getEnv("ANTHROPIC_API_KEY", ""),

		BraveAPIKey: getEnv("BRAVE_API_KEY", ""),

		HardcoverAPIToken: getEnv("HARDCOVER_API_TOKEN", ""),

		PushEnabled:           getEnvAsBool("PUSH_ENABLED", false),
		FCMServiceAccountJSON: getEnv("FCM_SERVICE_ACCOUNT_JSON", ""),
		APNsKeyPath:           getEnv("APNS_KEY_PATH", ""),
		APNsKeyID:             getEnv("APNS_KEY_ID", ""),
		APNsTeamID:            getEnv("APNS_TEAM_ID", ""),
		APNsTopic:             getEnv("APNS_TOPIC", "com.paperboxd.PaperBoxd"),
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
	// Fail closed: push turned on with incomplete credentials would silently
	// drop every notification for one platform. Name all the gaps at once so a
	// misconfigured deploy needs one fix, not five restarts.
	if c.PushEnabled {
		var missing []string
		for _, f := range []struct {
			name, value string
		}{
			{"FCM_SERVICE_ACCOUNT_JSON", c.FCMServiceAccountJSON},
			{"APNS_KEY_PATH", c.APNsKeyPath},
			{"APNS_KEY_ID", c.APNsKeyID},
			{"APNS_TEAM_ID", c.APNsTeamID},
			{"APNS_TOPIC", c.APNsTopic},
		} {
			if f.value == "" {
				missing = append(missing, f.name)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("PUSH_ENABLED is set but %s missing", strings.Join(missing, ", "))
		}
		if _, err := c.FCMCredentials(); err != nil {
			return fmt.Errorf("FCM_SERVICE_ACCOUNT_JSON: %w", err)
		}
	}
	return nil
}

// FCMCredentials returns the decoded Firebase service account key.
//
// FCM_SERVICE_ACCOUNT_JSON may hold either the raw JSON or a base64 encoding of
// it, and both are accepted because the two survive different transports: the
// key's `private_key` field embeds \n escapes that Railway's UI, docker-compose
// and shell `export` each mangle differently, while base64 is inert everywhere.
// Base64 is the safer paste; raw JSON is the easier one to eyeball.
//
// Callers get a parse error at startup via Validate() rather than a failed send
// hours later.
func (c *Config) FCMCredentials() ([]byte, error) {
	raw := strings.TrimSpace(c.FCMServiceAccountJSON)
	if raw == "" {
		return nil, fmt.Errorf("empty")
	}

	// A JSON object always starts with '{'; anything else is treated as base64.
	if !strings.HasPrefix(raw, "{") {
		// Strip interior whitespace first: macOS `base64` wraps at 76 columns by
		// default and Railway's textarea can reflow a long value, either of which
		// would otherwise fail decoding for a perfectly good key.
		compact := strings.Map(func(r rune) rune {
			switch r {
			case ' ', '\t', '\n', '\r':
				return -1
			}
			return r
		}, raw)
		decoded, err := base64.StdEncoding.DecodeString(compact)
		if err != nil {
			return nil, fmt.Errorf("not valid JSON or base64: %w", err)
		}
		raw = strings.TrimSpace(string(decoded))
	}

	// Verify it is a service account key, not some other JSON that happens to
	// parse — a wrong-but-valid blob would otherwise fail at first send.
	var key struct {
		Type        string `json:"type"`
		ProjectID   string `json:"project_id"`
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
	}
	if err := json.Unmarshal([]byte(raw), &key); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if key.Type != "service_account" {
		return nil, fmt.Errorf(`expected "type":"service_account", got %q`, key.Type)
	}
	for _, f := range []struct{ name, value string }{
		{"project_id", key.ProjectID},
		{"client_email", key.ClientEmail},
		{"private_key", key.PrivateKey},
	} {
		if f.value == "" {
			return nil, fmt.Errorf("service account key missing %s", f.name)
		}
	}
	return []byte(raw), nil
}

func getEnvAsBool(key string, defaultVal bool) bool {
	valStr := getEnv(key, "")
	if val, err := strconv.ParseBool(valStr); err == nil {
		return val
	}
	return defaultVal
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
