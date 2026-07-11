package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hridyesh/paperboxd-backend/internal/config"
)

// stubTokenInfo spins up a fake Google tokeninfo endpoint returning the given
// JSON body, points googleTokenInfoURL at it for the duration of the test, and
// returns a MobileHandler whose config carries the supplied audience allowlist.
func stubTokenInfo(t *testing.T, body string, allowed []string) *MobileHandler {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	prev := googleTokenInfoURL
	googleTokenInfoURL = srv.URL
	t.Cleanup(func() { googleTokenInfoURL = prev })

	return &MobileHandler{
		Handler: &Handler{cfg: &config.Config{AllowedGoogleAudiences: allowed}},
	}
}

func TestVerifyGoogleIDToken_AudienceMismatch(t *testing.T) {
	m := stubTokenInfo(t,
		`{"aud":"some-other-app.apps.googleusercontent.com","sub":"123","email":"a@b.com","email_verified":"true"}`,
		[]string{"893085484645-ourclient.apps.googleusercontent.com"},
	)

	_, err := m.verifyGoogleIDToken(context.Background(), "irrelevant")
	if err == nil {
		t.Fatal("expected audience-mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "audience mismatch") {
		t.Fatalf("expected 'audience mismatch' error, got: %v", err)
	}
}

func TestVerifyGoogleIDToken_AudienceMatch(t *testing.T) {
	const aud = "893085484645-ourclient.apps.googleusercontent.com"
	m := stubTokenInfo(t,
		`{"aud":"`+aud+`","sub":"123","email":"a@b.com","email_verified":"true"}`,
		[]string{aud},
	)

	claims, err := m.verifyGoogleIDToken(context.Background(), "irrelevant")
	if err != nil {
		t.Fatalf("expected success for allowlisted audience, got: %v", err)
	}
	if claims.Audience != aud {
		t.Fatalf("expected audience %q, got %q", aud, claims.Audience)
	}
	if claims.Email != "a@b.com" || !claims.EmailVerified {
		t.Fatalf("claims not parsed: %+v", claims)
	}
}

func TestVerifyGoogleIDToken_EmptyAllowlistFailsClosed(t *testing.T) {
	m := stubTokenInfo(t,
		`{"aud":"anything.apps.googleusercontent.com","sub":"123","email":"a@b.com","email_verified":"true"}`,
		nil, // unset GOOGLE_OAUTH_ALLOWED_AUDIENCES → reject everything
	)

	if _, err := m.verifyGoogleIDToken(context.Background(), "irrelevant"); err == nil {
		t.Fatal("expected empty allowlist to reject all tokens, got nil error")
	}
}
