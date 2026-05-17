package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/hridyesh/paperboxd-backend/internal/reqctx"
	"github.com/hridyesh/paperboxd-backend/internal/service"
	"github.com/hridyesh/paperboxd-backend/internal/types"
)

// RecommendationHandler serves personalised book recommendations.
type RecommendationHandler struct {
	svc *service.RecommendationService
}

func NewRecommendationHandler(svc *service.RecommendationService) *RecommendationHandler {
	return &RecommendationHandler{svc: svc}
}

// GetHomeRecommendations handles GET /api/v1/recommendations/home
func (h *RecommendationHandler) GetHomeRecommendations(w http.ResponseWriter, r *http.Request) {
	userID, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteJSON(w, http.StatusOK, map[string]any{
			"recommendations": []any{},
			"source":          "fallback",
		})
		return
	}

	recs, source, err := h.svc.GetHomeRecommendations(r.Context(), userID)
	if err != nil {
		slog.Error("get home recommendations", "error", err, "user_id", userID)
		types.WriteJSON(w, http.StatusOK, map[string]any{
			"recommendations": []any{},
			"source":          "fallback",
		})
		return
	}

	if recs == nil {
		recs = []service.BookCandidate{}
	}

	types.WriteJSON(w, http.StatusOK, map[string]any{
		"recommendations": recs,
		"source":          source,
	})
}

// GetSimilarBooks handles GET /api/v1/recommendations/similar/{bookId}
func (h *RecommendationHandler) GetSimilarBooks(w http.ResponseWriter, r *http.Request) {
	bookID := chi.URLParam(r, "bookId")
	if bookID == "" {
		types.WriteJSON(w, http.StatusOK, map[string]any{"similar": []any{}})
		return
	}

	userID, _ := reqctx.GetUserID(r.Context())

	similar, err := h.svc.GetSimilarBooks(r.Context(), bookID, userID)
	if err != nil {
		slog.Error("get similar books", "error", err, "book_id", bookID)
		types.WriteJSON(w, http.StatusOK, map[string]any{"similar": []any{}})
		return
	}

	if similar == nil {
		similar = []service.BookCandidate{}
	}

	types.WriteJSON(w, http.StatusOK, map[string]any{"similar": similar})
}

// PostFeedback handles POST /api/v1/recommendations/feedback
// Records that the user interacted with (or dismissed) a recommendation.
func (h *RecommendationHandler) PostFeedback(w http.ResponseWriter, r *http.Request) {
	userID, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteJSON(w, http.StatusOK, map[string]bool{"ok": false})
		return
	}

	var body struct {
		BookID string `json:"book_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.BookID == "" {
		types.WriteJSON(w, http.StatusOK, map[string]bool{"ok": false})
		return
	}

	if err := h.svc.UpdateImpressions(r.Context(), userID, body.BookID); err != nil {
		slog.Warn("update impressions", "error", err)
	}

	types.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
