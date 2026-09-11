package handler

import (
	"testing"

	"github.com/hridyesh/paperboxd-backend/internal/config"
)

// The scan allowlist is the seam the Plus entitlement will replace, and it
// gates money (each scan is a paid Claude call). Matching must be exact apart
// from case and surrounding whitespace — never a prefix or substring.
func TestIsScanUnlimited(t *testing.T) {
	h := &ScanHandler{Config: &config.Config{
		ScanUnlimitedEmails: []string{" Reader@Example.com ", "second@example.com"},
	}}

	for _, email := range []string{"reader@example.com", "READER@EXAMPLE.COM", " reader@example.com "} {
		if !h.isScanUnlimited(email) {
			t.Fatalf("%q should be unlimited", email)
		}
	}
	for _, email := range []string{"", "other@example.com", "reader@example.com.evil", "eader@example.com"} {
		if h.isScanUnlimited(email) {
			t.Fatalf("%q must not be unlimited", email)
		}
	}

	// No allowlist configured (the production default) means nobody is exempt.
	empty := &ScanHandler{Config: &config.Config{}}
	if empty.isScanUnlimited("reader@example.com") {
		t.Fatal("empty allowlist must exempt nobody")
	}
	if (&ScanHandler{}).isScanUnlimited("reader@example.com") {
		t.Fatal("nil config must exempt nobody")
	}
}
