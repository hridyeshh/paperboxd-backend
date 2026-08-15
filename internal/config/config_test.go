package config

import (
	"encoding/base64"
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

// complete fills in a full set of working push credentials, so each case below
// can knock out exactly one field and assert on that.
func complete(c *Config) *Config {
	c.PushEnabled = true
	c.FCMServiceAccountJSON = fakeKey
	c.APNsKeyPath = "/run/secrets/apns.p8"
	c.APNsKeyID = "ABC123"
	c.APNsTeamID = "TEAM456"
	c.APNsTopic = "com.paperboxd.PaperBoxd"
	return c
}

func TestValidatePushGating(t *testing.T) {
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

// fakeKey is a structurally valid service account key. The private_key value is
// nonsense on purpose — FCMCredentials checks shape, not cryptographic validity,
// and no real key belongs in a test file.
const fakeKey = `{"type":"service_account","project_id":"paperboxd-test",` +
	`"client_email":"sa@paperboxd-test.iam.gserviceaccount.com",` +
	`"private_key":"-----BEGIN PRIVATE KEY-----\nnotarealkey\n-----END PRIVATE KEY-----\n"}`

func TestFCMCredentials(t *testing.T) {
	t.Run("accepts raw json", func(t *testing.T) {
		c := &Config{FCMServiceAccountJSON: fakeKey}
		got, err := c.FCMCredentials()
		if err != nil {
			t.Fatalf("raw JSON rejected: %v", err)
		}
		if !strings.Contains(string(got), "paperboxd-test") {
			t.Error("decoded credentials should round-trip the project id")
		}
	})

	t.Run("accepts base64", func(t *testing.T) {
		c := &Config{FCMServiceAccountJSON: base64.StdEncoding.EncodeToString([]byte(fakeKey))}
		got, err := c.FCMCredentials()
		if err != nil {
			t.Fatalf("base64 rejected: %v", err)
		}
		if !strings.Contains(string(got), "paperboxd-test") {
			t.Error("base64 branch should decode to the same JSON")
		}
	})

	t.Run("accepts line-wrapped base64", func(t *testing.T) {
		// macOS `base64` wraps at 76 columns by default; a naive decode rejects it.
		enc := base64.StdEncoding.EncodeToString([]byte(fakeKey))
		var wrapped strings.Builder
		for i := 0; i < len(enc); i += 76 {
			end := i + 76
			if end > len(enc) {
				end = len(enc)
			}
			wrapped.WriteString(enc[i:end] + "\n")
		}
		c := &Config{FCMServiceAccountJSON: wrapped.String()}
		if _, err := c.FCMCredentials(); err != nil {
			t.Fatalf("wrapped base64 rejected: %v", err)
		}
	})

	t.Run("tolerates surrounding whitespace", func(t *testing.T) {
		// Copy-paste out of a terminal or the Railway UI often carries a newline.
		c := &Config{FCMServiceAccountJSON: "  \n" + fakeKey + "\n  "}
		if _, err := c.FCMCredentials(); err != nil {
			t.Fatalf("whitespace-padded key rejected: %v", err)
		}
	})

	for _, tc := range []struct {
		name, value, wantErr string
	}{
		{"empty", "", "empty"},
		{"garbage", "not json and not base64!!!", "base64"},
		{"valid json wrong shape", `{"hello":"world"}`, "service_account"},
		{"oauth client not service account", `{"type":"authorized_user"}`, "service_account"},
		{"missing private key", `{"type":"service_account","project_id":"p","client_email":"e"}`, "private_key"},
	} {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			c := &Config{FCMServiceAccountJSON: tc.value}
			_, err := c.FCMCredentials()
			if err == nil {
				t.Fatalf("expected error mentioning %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q should mention %q", err, tc.wantErr)
			}
		})
	}

	t.Run("validate rejects unparseable key when push enabled", func(t *testing.T) {
		c := complete(base())
		c.FCMServiceAccountJSON = `{"type":"authorized_user"}`
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "FCM_SERVICE_ACCOUNT_JSON") {
			t.Fatalf("expected startup failure naming the var, got %v", err)
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
