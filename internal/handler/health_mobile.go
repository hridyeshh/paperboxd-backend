package handler

import (
	"net/http"
	"os"
	"time"

	"github.com/hridyesh/paperboxd-backend/internal/types"
)

// MobileHealthHandler serves GET /api/health for native mobile connectivity checks.
// Distinct from /health (which inspects Postgres + Redis) so a mobile reachability
// probe stays cheap and never reports degraded due to a transient Redis blip.
type MobileHealthHandler struct {
	version string
}

// NewMobileHealthHandler returns a handler that reports the build version from
// the APP_VERSION env var (defaulting to "dev").
func NewMobileHealthHandler() *MobileHealthHandler {
	v := os.Getenv("APP_VERSION")
	if v == "" {
		v = "dev"
	}
	return &MobileHealthHandler{version: v}
}

type mobileHealthResponse struct {
	Status    string `json:"status"`
	Version   string `json:"version"`
	Timestamp string `json:"timestamp"`
}

// Get handles GET /api/health.
func (h *MobileHealthHandler) Get(w http.ResponseWriter, _ *http.Request) {
	types.WriteJSON(w, http.StatusOK, mobileHealthResponse{
		Status:    "ok",
		Version:   h.version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
}
