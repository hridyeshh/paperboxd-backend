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

// Concierge handles POST /api/v1/jazy.
//
// Body: {"query": "...", "session_id": "...", "anon_id": "...", "answer": "...", "limit": 5}
//
// Returns either kind "jazy#deck" with books, or kind "jazy#question" with one
// clarifying question and its options. A client that gets a question sends
// the same query back with the chosen option as "answer".
//
// Plus-only: the route requires auth and the caller must hold an active
// subscription, else 402 SUBSCRIPTION_REQUIRED. Every deck is a Claude
// completion, so the check runs before any work.
func (h *BookHandler) Concierge(w http.ResponseWriter, r *http.Request) {
	callerID, ok := authenticatedUserID(w, r)
	if !ok {
		return
	}
	if !requirePlus(w, r, h.Queries, callerID, "Ask Jazy") {
		return
	}

	var req struct {
		Query     string `json:"query"`
		SessionID string `json:"session_id"`
		AnonID    string `json:"anon_id"`
		Answer    string `json:"answer"`
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
	if len(req.Query) > 500 || len(req.Answer) > 200 {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "input too long")
		return
	}
	if h.RecommendationService == nil {
		types.WriteError(w, http.StatusServiceUnavailable, "service_unavailable", "Jazy is not configured")
		return
	}

	userID := ""
	if idStr, ok := reqctx.GetUserID(r.Context()); ok {
		userID = idStr
	}
	anonID := ""
	if req.AnonID != "" {
		if _, err := uuid.Parse(req.AnonID); err == nil {
			anonID = req.AnonID
		}
	}

	resp, err := h.RecommendationService.Concierge(r.Context(), h.Queries, service.ConciergeRequest{
		Query:     req.Query,
		SessionID: req.SessionID,
		UserID:    userID,
		AnonID:    anonID,
		Answer:    req.Answer,
		Limit:     req.Limit,
	})
	if err != nil {
		slog.Error("concierge", "error", err, "query", req.Query)
		types.WriteError(w, http.StatusInternalServerError, types.ErrCodeInternalServer, "Jazy unavailable")
		return
	}

	types.WriteJSON(w, http.StatusOK, resp)
}
