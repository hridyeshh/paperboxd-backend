package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/hridyesh/paperboxd-backend/internal/config"
)

// stubAppleJWKS generates an RSA keypair, serves it as Apple's JWKS at a stubbed
// appleJWKSURL, and returns a MobileHandler carrying the given audience
// allowlist plus the private key + kid used to sign test identity tokens.
func stubAppleJWKS(t *testing.T, allowed []string) (*MobileHandler, *rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	const kid = "test-kid"

	n := base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes())
	body, _ := json.Marshal(map[string]any{
		"keys": []map[string]string{{"kty": "RSA", "kid": kid, "n": n, "e": e}},
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	prev := appleJWKSURL
	appleJWKSURL = srv.URL
	t.Cleanup(func() { appleJWKSURL = prev })

	m := &MobileHandler{Handler: &Handler{cfg: &config.Config{AllowedAppleAudiences: allowed}}}
	return m, key, kid
}

func signAppleToken(t *testing.T, key *rsa.PrivateKey, kid, aud, sub, email string) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss":   appleIssuer,
		"aud":   aud,
		"sub":   sub,
		"email": email,
		"exp":   time.Now().Add(time.Hour).Unix(),
		"iat":   time.Now().Unix(),
	})
	tok.Header["kid"] = kid
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return s
}

func TestVerifyAppleIdentityToken_Valid(t *testing.T) {
	const aud = "com.paperboxd.PaperBoxd"
	m, key, kid := stubAppleJWKS(t, []string{aud})
	tok := signAppleToken(t, key, kid, aud, "apple-sub-1", "a@b.com")

	claims, err := m.verifyAppleIdentityToken(context.Background(), tok)
	if err != nil {
		t.Fatalf("expected success for allowlisted audience, got: %v", err)
	}
	if claims.Subject != "apple-sub-1" || claims.Email != "a@b.com" {
		t.Fatalf("claims not parsed: %+v", claims)
	}
}

func TestVerifyAppleIdentityToken_AudienceMismatch(t *testing.T) {
	m, key, kid := stubAppleJWKS(t, []string{"com.paperboxd.PaperBoxd"})
	tok := signAppleToken(t, key, kid, "com.someone.else", "apple-sub-1", "a@b.com")

	_, err := m.verifyAppleIdentityToken(context.Background(), tok)
	if err == nil || !strings.Contains(err.Error(), "audience mismatch") {
		t.Fatalf("expected audience-mismatch error, got: %v", err)
	}
}

func TestVerifyAppleIdentityToken_EmptyAllowlistFailsClosed(t *testing.T) {
	m, key, kid := stubAppleJWKS(t, nil) // unset APPLE_ALLOWED_AUDIENCES → reject everything
	tok := signAppleToken(t, key, kid, "com.paperboxd.PaperBoxd", "apple-sub-1", "a@b.com")

	if _, err := m.verifyAppleIdentityToken(context.Background(), tok); err == nil {
		t.Fatal("expected empty allowlist to reject all tokens, got nil error")
	}
}
