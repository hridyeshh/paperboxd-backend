package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/hridyesh/paperboxd-backend/internal/reqctx"
	"github.com/hridyesh/paperboxd-backend/internal/types"
)

// VibeSearch handles POST /api/v1/search/vibe.
// Auth is optional — works logged out, personalised when logged in.
func (h *BookHandler) VibeSearch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}
	if len(req.Query) == 0 {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "query is required")
		return
	}
	if req.Limit <= 0 {
		req.Limit = 10
	}
	if req.Limit > 20 {
		req.Limit = 20
	}

	if h.RecommendationService == nil {
		types.WriteError(w, http.StatusServiceUnavailable, "service_unavailable", "Vibe search is not configured")
		return
	}

	// Read user ID if authenticated (optional)
	userID := ""
	if idStr, ok := reqctx.GetUserID(r.Context()); ok {
		userID = idStr
	}

	resp, err := h.RecommendationService.VibeSearch(r.Context(), h.Queries, req.Query, req.Limit, userID)
	if err != nil {
		slog.Error("vibe search", "error", err, "query", req.Query)
		types.WriteError(w, http.StatusInternalServerError, types.ErrCodeInternalServer, "Vibe search unavailable")
		return
	}

	types.WriteJSON(w, http.StatusOK, resp)
}
