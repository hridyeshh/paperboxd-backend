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

// PersonalisedSearch handles POST /api/v1/search.
//
// Body: {"query": "...", "session_id": "...", "anon_id": "...", "limit": 10}
//
// session_id continues a conversation; omit it to start one. The response
// always carries the session id to send next time. anon_id lets a logged-out
// reader hold a session too — without it every refinement from a visitor would
// be a fresh search, which is most of the people who would be trying it.
func (h *BookHandler) PersonalisedSearch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string `json:"query"`
		SessionID string `json:"session_id"`
		AnonID    string `json:"anon_id"`
		Limit     int    `json:"limit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}
	if len(req.Query) == 0 {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "query is required")
		return
	}
	if len(req.Query) > 500 {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "query too long")
		return
	}
	if h.RecommendationService == nil {
		types.WriteError(w, http.StatusServiceUnavailable, "service_unavailable", "Search is not configured")
		return
	}

	userID := ""
	if idStr, ok := reqctx.GetUserID(r.Context()); ok {
		userID = idStr
	}
	// An anon id is only honoured when it parses; a made-up string would
	// otherwise become a session key.
	anonID := ""
	if req.AnonID != "" {
		if _, err := uuid.Parse(req.AnonID); err == nil {
			anonID = req.AnonID
		}
	}

	resp, err := h.RecommendationService.Search(r.Context(), h.Queries, service.SearchRequest{
		Query:     req.Query,
		SessionID: req.SessionID,
		UserID:    userID,
		AnonID:    anonID,
		Limit:     req.Limit,
	})
	if err != nil {
		slog.Error("search", "error", err, "query", req.Query)
		types.WriteError(w, http.StatusInternalServerError, types.ErrCodeInternalServer, "Search unavailable")
		return
	}

	types.WriteJSON(w, http.StatusOK, resp)
}
