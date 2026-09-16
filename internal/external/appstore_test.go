package external

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"math/big"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Apple's real root cannot sign test data, so the test mints its own
// root → intermediate → leaf chain with Apple's marker OIDs and checks the
// verifier accepts exactly that and rejects a chain missing the markers.
func TestAppleVerifier(t *testing.T) {
	root, rootKey := mintCert(t, "root", nil, nil, true, nil)
	inter, interKey := mintCert(t, "inter", root, rootKey, true, appleOIDWWDRIntermediate)
	leaf, leafKey := mintCert(t, "leaf", inter, interKey, false, appleOIDReceiptSigningLeaf)

	roots := x509.NewCertPool()
	roots.AddCert(root)
	v := NewAppleVerifierWithRoots(roots)

	payload := jwt.MapClaims{
		"originalTransactionId": "1000000",
		"productId":             "in.paperboxd.plus.monthly",
		"bundleId":              "com.paperboxd.PaperBoxd",
		"type":                  "Auto-Renewable Subscription",
		"expiresDate":           time.Now().Add(24 * time.Hour).UnixMilli(),
		"signedDate":            time.Now().UnixMilli(),
	}
	token := sign(t, payload, leafKey, leaf, inter, root)

	var tx AppleTransaction
	if err := v.Verify(token, &tx); err != nil {
		t.Fatalf("valid chain rejected: %v", err)
	}
	if tx.OriginalTransactionID != "1000000" || tx.ProductID != "in.paperboxd.plus.monthly" || !tx.ExpiresAt().After(time.Now()) {
		t.Fatalf("payload not decoded: %+v", tx)
	}

	// A leaf Apple issued for something else (no receipt-signing OID) must fail.
	plainLeaf, plainKey := mintCert(t, "dev", inter, interKey, false, nil)
	if err := v.Verify(sign(t, payload, plainKey, plainLeaf, inter, root), &tx); err == nil {
		t.Fatal("leaf without receipt-signing OID accepted")
	}

	// A chain under a different root must fail.
	otherRoot, otherKey := mintCert(t, "other", nil, nil, true, nil)
	otherInter, otherInterKey := mintCert(t, "oi", otherRoot, otherKey, true, appleOIDWWDRIntermediate)
	otherLeaf, otherLeafKey := mintCert(t, "ol", otherInter, otherInterKey, false, appleOIDReceiptSigningLeaf)
	if err := v.Verify(sign(t, payload, otherLeafKey, otherLeaf, otherInter, otherRoot), &tx); err == nil {
		t.Fatal("foreign root accepted")
	}

	// Revocation wins over expiry.
	rev := AppleTransaction{ExpiresDate: time.Now().Add(time.Hour).UnixMilli(), RevocationDate: time.Now().Add(-time.Hour).UnixMilli()}
	if rev.ExpiresAt().After(time.Now()) {
		t.Fatal("revoked transaction still active")
	}
}

func mintCert(t *testing.T, cn string, parent *x509.Certificate, parentKey *ecdsa.PrivateKey, isCA bool, marker asn1.ObjectIdentifier) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  isCA,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
	}
	if marker != nil {
		tmpl.ExtraExtensions = []pkix.Extension{{Id: marker, Value: []byte{0x05, 0x00}}}
	}
	if parent == nil {
		parent, parentKey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func sign(t *testing.T, claims jwt.MapClaims, key *ecdsa.PrivateKey, chain ...*x509.Certificate) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	x5c := make([]string, len(chain))
	for i, c := range chain {
		x5c[i] = base64.StdEncoding.EncodeToString(c.Raw)
	}
	tok.Header["x5c"] = x5c
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPlayNotificationPurchaseToken(t *testing.T) {
	data := base64.StdEncoding.EncodeToString([]byte(`{"version":"1.0","packageName":"in.paperboxd.app","subscriptionNotification":{"notificationType":2,"purchaseToken":"tok-123","subscriptionId":"plus"}}`))
	var n PlayNotification
	n.Message.Data = data
	tok, typ, err := n.PurchaseToken()
	if err != nil || tok != "tok-123" || typ != PlayNotifyRenewed {
		t.Fatalf("got %q %d, %v", tok, typ, err)
	}

	n.Message.Data = base64.StdEncoding.EncodeToString([]byte(`{"testNotification":{"version":"1.0"}}`))
	if tok, _, err := n.PurchaseToken(); err != nil || tok != "" {
		t.Fatalf("test notification: got %q, %v", tok, err)
	}
}
