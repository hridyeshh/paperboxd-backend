package handler

import (
	"encoding/json"
	"github.com/go-chi/chi/v5"
	"log/slog"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/hridyesh/paperboxd-backend/internal/reqctx"
	"github.com/hridyesh/paperboxd-backend/internal/service"
	"github.com/hridyesh/paperboxd-backend/internal/types"
)

// SurpriseMe handles GET /api/v1/recommendations/surprise?mode=safe|unexpected|wild|gem|obsession
func (h *RecommendationHandler) SurpriseMe(w http.ResponseWriter, r *http.Request) {
	userID, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}
	// Validate the mode up front. Inferring "unknown mode" from any error
	// returned with mode set reported a DB failure as a client mistake, which
	// hid every real breakage behind a 400.
	mode := r.URL.Query().Get("mode")
	if mode != "" && !slices.Contains(service.SurpriseModes(), mode) {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "unknown mode")
		return
	}
	res, err := h.svc.SurpriseMe(r.Context(), userID, mode)
	if err != nil {
		slog.Error("surprise me", "error", err, "user_id", userID)
		types.WriteInternalError(w)
		return
	}
	if res == nil {
		types.WriteJSON(w, http.StatusOK, map[string]any{"surprise": nil, "modes": service.SurpriseModes()})
		return
	}
	types.WriteJSON(w, http.StatusOK, map[string]any{"surprise": res, "modes": service.SurpriseModes()})
}

// GetTasteDashboard handles GET /api/v1/users/me/taste
func (h *RecommendationHandler) GetTasteDashboard(w http.ResponseWriter, r *http.Request) {
	userID, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}
	d, err := h.svc.GetTasteDashboard(r.Context(), userID)
	if err != nil {
		slog.Error("taste dashboard", "error", err, "user_id", userID)
		types.WriteInternalError(w)
		return
	}
	// Free tier sees the summary; Plus sees the whole mirror.
	d.Plus = false
	if uid, err := uuid.Parse(userID); err == nil {
		d.Plus = subscriptionActive(r.Context(), h.Queries, uid)
	}
	if !d.Plus {
		d = d.FreeTier()
	}
	types.WriteJSON(w, http.StatusOK, d)
}

// GetContextPresets handles GET /api/v1/search/contexts
func (h *RecommendationHandler) GetContextPresets(w http.ResponseWriter, r *http.Request) {
	types.WriteJSON(w, http.StatusOK, map[string]any{"contexts": service.ContextPresets()})
}

// ContextDiscovery handles POST /api/v1/search/context
// Body: {"context": "flight", "anon_id": "...", "limit": 10}
func (h *RecommendationHandler) ContextDiscovery(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Context string `json:"context"`
		AnonID  string `json:"anon_id"`
		Limit   int    `json:"limit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Context == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "context is required")
		return
	}
	userID, _ := reqctx.GetUserID(r.Context())
	anonID := ""
	if req.AnonID != "" {
		if _, err := uuid.Parse(req.AnonID); err == nil {
			anonID = req.AnonID
		}
	}
	resp, err := h.svc.ContextDiscovery(r.Context(), userID, anonID, req.Context, req.Limit)
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, err.Error())
		return
	}
	types.WriteJSON(w, http.StatusOK, resp)
}

// GetSmartTBR handles GET /api/v1/users/me/tbr/smart — Plus-only.
func (h *RecommendationHandler) GetSmartTBR(w http.ResponseWriter, r *http.Request) {
	userID, ok := authenticatedUserID(w, r)
	if !ok {
		return
	}
	if !requirePlus(w, r, h.Queries, userID, "Smart TBR") {
		return
	}
	out, err := h.svc.SmartTBR(r.Context(), userID.String())
	if err != nil {
		slog.Error("smart tbr", "error", err, "user_id", userID)
		types.WriteInternalError(w)
		return
	}
	types.WriteJSON(w, http.StatusOK, out)
}

// KeepTBR handles POST /api/v1/users/me/tbr/smart/{bookId}/keep — the
// "still want it" answer on a stale book.
func (h *RecommendationHandler) KeepTBR(w http.ResponseWriter, r *http.Request) {
	userID, ok := authenticatedUserID(w, r)
	if !ok {
		return
	}
	if err := h.svc.TouchTBR(r.Context(), userID.String(), chi.URLParam(r, "bookId")); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "invalid book id")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
