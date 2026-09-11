package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

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

// PostFeedback handles POST /api/v1/recommendations/feedback.
//
// Two shapes go through one endpoint:
//
//	{"book_id":"…","event_type":"impression|click|dismiss","reason_type":"…"}
//	{"book_id":"…","verdict":"not_for_me","reason_codes":["too_slow"]}
//
// The first is passive telemetry that every client already sends; the second is
// the reader deliberately answering. They are kept on one route because a
// verdict is also an interaction worth counting, and splitting them would mean
// every client learns two endpoints to say one thing.
func (h *RecommendationHandler) PostFeedback(w http.ResponseWriter, r *http.Request) {
	userID, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteJSON(w, http.StatusOK, map[string]bool{"ok": false})
		return
	}

	var body struct {
		BookID      string   `json:"book_id"`
		EventType   string   `json:"event_type"`
		ReasonType  string   `json:"reason_type"`
		Verdict     string   `json:"verdict"`
		ReasonCodes []string `json:"reason_codes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.BookID == "" {
		types.WriteJSON(w, http.StatusOK, map[string]bool{"ok": false})
		return
	}

	// Explicit verdict path.
	if body.Verdict != "" {
		if !service.ValidVerdict(body.Verdict) {
			types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "unknown verdict")
			return
		}
		if err := h.svc.RecordFeedback(r.Context(), userID, body.BookID,
			body.Verdict, body.ReasonCodes, body.ReasonType); err != nil {
			slog.Error("record feedback", "error", err, "verdict", body.Verdict)
			types.WriteInternalError(w)
			return
		}

		// A verdict changes what should be recommended right now, so the
		// cached candidate pool is stale the moment it is given. Without this
		// the reader keeps seeing the book they just rejected until the pool
		// expires, which reads as the button having done nothing.
		h.svc.InvalidateUserPool(r.Context(), userID)

		// Recompute the trait profile now rather than at the nightly job. A
		// reader who says "not for me — too slow" and then reloads should see
		// the pacing axis move today; the roadmap's "every recommendation makes
		// it smarter" cannot lag a day. One reader, a few hundred rows: cheap.
		if body.Verdict == service.VerdictLoved || body.Verdict == service.VerdictNotForMe {
			go func() {
				if err := h.svc.ComputeAndSaveNegativeSignals(context.Background(), userID); err != nil {
					slog.Warn("refresh profile after verdict", "error", err, "user_id", userID)
				}
			}()
		}

		go func() {
			meta := map[string]any{"verdict": body.Verdict}
			if body.ReasonType != "" {
				meta["reason_type"] = body.ReasonType
			}
			if codes := service.SanitizeReasonCodes(body.ReasonCodes); len(codes) > 0 {
				meta["reason_codes"] = codes
			}
			if err := h.svc.TrackEvent(context.Background(), userID, body.BookID,
				verdictEvent(body.Verdict), meta); err != nil {
				slog.Warn("track verdict event", "error", err)
			}
		}()

		types.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}

	// Passive telemetry path. Shipped clients send the bare verbs
	// ("impression", "click", "dismiss") and iOS sends "book_impression";
	// NormalizeEventType maps all of them onto the canonical rec_* names. An
	// unrecognised value is treated as a plain impression rather than rejected
	// — feedback is fire-and-forget on every client and a 400 here would be
	// silently swallowed anyway.
	eventType, ok := service.NormalizeEventType(body.EventType)
	if !ok {
		eventType = service.EventRecImpression
	}

	if err := h.svc.UpdateImpressions(r.Context(), userID, body.BookID); err != nil {
		slog.Warn("update impressions", "error", err)
	}

	go func() {
		meta := map[string]any{}
		if body.ReasonType != "" {
			meta["reason_type"] = body.ReasonType
		}
		if err := h.svc.TrackEvent(context.Background(), userID, body.BookID, eventType, meta); err != nil {
			slog.Warn("track event", "error", err, "event_type", eventType)
		}
	}()

	types.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// verdictEvent maps a verdict onto its analytics event name.
func verdictEvent(verdict string) string {
	switch verdict {
	case service.VerdictLoved:
		return service.EventRecLoved
	case service.VerdictMaybe:
		return service.EventRecMaybe
	case service.VerdictNotForMe:
		return service.EventRecNotForMe
	case service.VerdictAlreadyRead:
		return service.EventRecAlreadyRead
	case service.VerdictNotNow:
		return service.EventRecNotNow
	default:
		return service.EventRecDismiss
	}
}

// GetFeedbackOptions handles GET /api/v1/recommendations/feedback/options.
//
// Clients render the negative-reason chips from this rather than hardcoding
// them, so adding a reason code is a backend change and the three clients do
// not drift into three different vocabularies — which is exactly how the event
// table ended up with three naming conventions.
func (h *RecommendationHandler) GetFeedbackOptions(w http.ResponseWriter, r *http.Request) {
	types.WriteJSON(w, http.StatusOK, map[string]any{
		"verdicts":     []string{service.VerdictLoved, service.VerdictMaybe, service.VerdictNotForMe, service.VerdictAlreadyRead, service.VerdictNotNow},
		"reason_codes": service.FeedbackReasonCodes(),
	})
}

// GetFeed handles GET /api/v1/recommendations/feed.
//
// The modular home page. Anonymous callers get an empty module list rather
// than an error: the client renders its logged-out home in that case.
func (h *RecommendationHandler) GetFeed(w http.ResponseWriter, r *http.Request) {
	userID, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteJSON(w, http.StatusOK, service.FeedResponse{Modules: []service.FeedModule{}, Source: "anonymous"})
		return
	}
	feed, err := h.svc.GetFeed(r.Context(), userID)
	if err != nil {
		slog.Error("get feed", "error", err, "user_id", userID)
		types.WriteJSON(w, http.StatusOK, service.FeedResponse{Modules: []service.FeedModule{}, Source: "fallback"})
		return
	}
	if feed.Modules == nil {
		feed.Modules = []service.FeedModule{}
	}
	types.WriteJSON(w, http.StatusOK, feed)
}

// GetTasteTwins handles GET /api/v1/recommendations/twins?limit=10.
func (h *RecommendationHandler) GetTasteTwins(w http.ResponseWriter, r *http.Request) {
	userID, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}
	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	twins, err := h.svc.GetTasteTwins(r.Context(), userID, limit)
	if err != nil {
		slog.Error("taste twins", "error", err, "user_id", userID)
		types.WriteJSON(w, http.StatusOK, map[string]any{"twins": []any{}})
		return
	}
	if twins == nil {
		twins = []service.TasteTwin{}
	}
	types.WriteJSON(w, http.StatusOK, map[string]any{"twins": twins})
}
