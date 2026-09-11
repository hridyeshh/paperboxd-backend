package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"

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
	mode := r.URL.Query().Get("mode")
	res, err := h.svc.SurpriseMe(r.Context(), userID, mode)
	if err != nil {
		if mode != "" {
			types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "unknown mode")
			return
		}
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
