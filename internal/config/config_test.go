package config

import (
	"strings"
	"testing"
)

// base returns a Config that already satisfies the pre-existing DATABASE_URL /
// JWT_SECRET rules, so each case below only exercises the push gating.
func base() *Config {
	return &Config{
		DatabaseURL: "postgres://localhost/paperboxd",
		JWTSecret:   strings.Repeat("x", 32),
	}
}

func TestValidatePushGating(t *testing.T) {
	complete := func(c *Config) *Config {
		c.PushEnabled = true
		c.FCMServiceAccountJSON = "{}"
		c.APNsKeyPath = "/run/secrets/apns.p8"
		c.APNsKeyID = "ABC123"
		c.APNsTeamID = "TEAM456"
		c.APNsTopic = "com.paperboxd.PaperBoxd"
		return c
	}

	t.Run("disabled ignores empty credentials", func(t *testing.T) {
		if err := base().Validate(); err != nil {
			t.Fatalf("push off should not require credentials, got %v", err)
		}
	})

	t.Run("enabled with full credentials passes", func(t *testing.T) {
		if err := complete(base()).Validate(); err != nil {
			t.Fatalf("complete push config rejected: %v", err)
		}
	})

	// Each credential must be individually required — a single missing key
	// would otherwise silently disable one platform at send time.
	for _, tc := range []struct {
		name  string
		clear func(*Config)
		want  string
	}{
		{"missing fcm", func(c *Config) { c.FCMServiceAccountJSON = "" }, "FCM_SERVICE_ACCOUNT_JSON"},
		{"missing apns key path", func(c *Config) { c.APNsKeyPath = "" }, "APNS_KEY_PATH"},
		{"missing apns key id", func(c *Config) { c.APNsKeyID = "" }, "APNS_KEY_ID"},
		{"missing apns team id", func(c *Config) { c.APNsTeamID = "" }, "APNS_TEAM_ID"},
		{"missing apns topic", func(c *Config) { c.APNsTopic = "" }, "APNS_TOPIC"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := complete(base())
			tc.clear(c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("expected error naming %s, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q should name %s", err, tc.want)
			}
		})
	}

	t.Run("reports every gap at once", func(t *testing.T) {
		c := complete(base())
		c.APNsKeyID = ""
		c.APNsTeamID = ""
		err := c.Validate()
		if err == nil {
			t.Fatal("expected error")
		}
		for _, want := range []string{"APNS_KEY_ID", "APNS_TEAM_ID"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q should name %s", err, want)
			}
		}
	})
}

func TestGetEnvAsBool(t *testing.T) {
	t.Setenv("PB_TEST_BOOL", "true")
	if !getEnvAsBool("PB_TEST_BOOL", false) {
		t.Error(`"true" should parse as true`)
	}
	t.Setenv("PB_TEST_BOOL", "nonsense")
	if !getEnvAsBool("PB_TEST_BOOL", true) {
		t.Error("unparseable value should fall back to the default")
	}
	if getEnvAsBool("PB_TEST_BOOL_UNSET", false) {
		t.Error("unset value should fall back to the default")
	}
}
