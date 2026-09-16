package external

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// App Store Server: StoreKit 2 transactions and Server Notifications V2 are
// both JWS objects whose x5c header carries a certificate chain rooted at
// Apple Root CA - G3. Verifying that chain locally is the whole of the trust
// model — no App Store Connect key, no network call.
//
// Root cert from https://www.apple.com/certificateauthority/AppleRootCA-G3.cer
// SHA-256 63343abfb89a6a03ebb57e9b3f5fa7be7c4f5c756f3017b3a8c488c3653e9179.
const appleRootCAG3 = `-----BEGIN CERTIFICATE-----
MIICQzCCAcmgAwIBAgIILcX8iNLFS5UwCgYIKoZIzj0EAwMwZzEbMBkGA1UEAwwS
QXBwbGUgUm9vdCBDQSAtIEczMSYwJAYDVQQLDB1BcHBsZSBDZXJ0aWZpY2F0aW9u
IEF1dGhvcml0eTETMBEGA1UECgwKQXBwbGUgSW5jLjELMAkGA1UEBhMCVVMwHhcN
MTQwNDMwMTgxOTA2WhcNMzkwNDMwMTgxOTA2WjBnMRswGQYDVQQDDBJBcHBsZSBS
b290IENBIC0gRzMxJjAkBgNVBAsMHUFwcGxlIENlcnRpZmljYXRpb24gQXV0aG9y
aXR5MRMwEQYDVQQKDApBcHBsZSBJbmMuMQswCQYDVQQGEwJVUzB2MBAGByqGSM49
AgEGBSuBBAAiA2IABJjpLz1AcqTtkyJygRMc3RCV8cWjTnHcFBbZDuWmBSp3ZHtf
TjjTuxxEtX/1H7YyYl3J6YRbTzBPEVoA/VhYDKX1DyxNB0cTddqXl5dvMVztK517
IDvYuVTZXpmkOlEKMaNCMEAwHQYDVR0OBBYEFLuw3qFYM4iapIqZ3r6966/ayySr
MA8GA1UdEwEB/wQFMAMBAf8wDgYDVR0PAQH/BAQDAgEGMAoGCCqGSM49BAMDA2gA
MGUCMQCD6cHEFl4aXTQY2e3v9GwOAEZLuN+yRhHFD/3meoyhpmvOwgPUnPWTxnS4
at+qIxUCMG1mihDK1A3UT82NQz60imOlM27jbdoXt2QfyFMm+YhidDkLF1vLUagM
6BgD56KyKA==
-----END CERTIFICATE-----`

// Apple marks the certificates it uses for receipt signing with these
// extensions. Without the check, any certificate Apple ever issued under
// this root (a developer's code-signing cert, say) could sign a "receipt".
var (
	appleOIDReceiptSigningLeaf = asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 6, 11, 1}
	appleOIDWWDRIntermediate   = asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 6, 2, 1}
)

// AppleTransaction is the subset of a JWSTransactionDecodedPayload we act on.
type AppleTransaction struct {
	OriginalTransactionID string `json:"originalTransactionId"`
	TransactionID         string `json:"transactionId"`
	ProductID             string `json:"productId"`
	BundleID              string `json:"bundleId"`
	Type                  string `json:"type"`
	Environment           string `json:"environment"`
	ExpiresDate           int64  `json:"expiresDate"`    // ms since epoch, 0 for non-subscriptions
	RevocationDate        int64  `json:"revocationDate"` // ms since epoch, 0 unless refunded/revoked
	SignedDate            int64  `json:"signedDate"`
	OfferDiscountType     string `json:"offerDiscountType"` // "FREE_TRIAL" during an introductory trial
}

// IsTrial reports whether this period is a free trial.
func (t AppleTransaction) IsTrial() bool { return t.OfferDiscountType == "FREE_TRIAL" }

// ExpiresAt is when the entitlement ends: revocation wins over expiry.
func (t AppleTransaction) ExpiresAt() time.Time {
	if t.RevocationDate > 0 {
		return time.UnixMilli(t.RevocationDate)
	}
	return time.UnixMilli(t.ExpiresDate)
}

// AppleRenewalInfo is the subset of JWSRenewalInfoDecodedPayload we act on.
type AppleRenewalInfo struct {
	AutoRenewStatus int `json:"autoRenewStatus"` // 1 on, 0 off
}

// AppleNotification is a decoded App Store Server Notification V2 payload.
type AppleNotification struct {
	NotificationType string `json:"notificationType"`
	Subtype          string `json:"subtype"`
	Data             struct {
		BundleID              string `json:"bundleId"`
		Environment           string `json:"environment"`
		SignedTransactionInfo string `json:"signedTransactionInfo"`
		SignedRenewalInfo     string `json:"signedRenewalInfo"`
	} `json:"data"`
}

// AppleVerifier checks App Store JWS signatures against a root pool.
type AppleVerifier struct {
	roots *x509.CertPool
}

// NewAppleVerifier trusts Apple Root CA - G3.
func NewAppleVerifier() *AppleVerifier {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(appleRootCAG3))
	return &AppleVerifier{roots: pool}
}

// NewAppleVerifierWithRoots is for tests, which cannot mint under Apple's root.
func NewAppleVerifierWithRoots(roots *x509.CertPool) *AppleVerifier {
	return &AppleVerifier{roots: roots}
}

// Verify checks the x5c chain and ES256 signature, then decodes the payload
// into out. It is the one trust boundary for everything Apple sends us.
func (v *AppleVerifier) Verify(token string, out any) error {
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(token, claims, v.keyFunc, jwt.WithValidMethods([]string{"ES256"}))
	if err != nil {
		return err
	}
	// Round-trip through JSON so the caller's struct tags do the mapping.
	raw, err := json.Marshal(claims)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func (v *AppleVerifier) keyFunc(t *jwt.Token) (any, error) {
	x5c, _ := t.Header["x5c"].([]any)
	if len(x5c) < 3 {
		return nil, errors.New("apple jws: x5c chain must have 3 certificates")
	}
	certs := make([]*x509.Certificate, 0, len(x5c))
	for _, entry := range x5c {
		s, _ := entry.(string)
		der, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("apple jws: x5c decode: %w", err)
		}
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, fmt.Errorf("apple jws: x5c parse: %w", err)
		}
		certs = append(certs, c)
	}
	leaf, intermediate := certs[0], certs[1]
	if !hasExtension(leaf, appleOIDReceiptSigningLeaf) || !hasExtension(intermediate, appleOIDWWDRIntermediate) {
		return nil, errors.New("apple jws: certificate is not an App Store receipt signer")
	}

	// Verify at the time Apple signed, not now: a chain that was valid when
	// the receipt was issued stays valid evidence after its certs expire.
	at := time.Now()
	if claims, ok := t.Claims.(jwt.MapClaims); ok {
		if signed, ok := claims["signedDate"].(float64); ok && signed > 0 {
			at = time.UnixMilli(int64(signed))
		}
	}
	inter := x509.NewCertPool()
	inter.AddCert(intermediate)
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:         v.roots,
		Intermediates: inter,
		CurrentTime:   at,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}); err != nil {
		return nil, fmt.Errorf("apple jws: chain: %w", err)
	}
	key, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("apple jws: leaf key is not ECDSA")
	}
	return key, nil
}

func hasExtension(c *x509.Certificate, oid asn1.ObjectIdentifier) bool {
	for _, ext := range c.Extensions {
		if ext.Id.Equal(oid) {
			return true
		}
	}
	return false
}
