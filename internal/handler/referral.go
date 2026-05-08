package handler

import (
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/reqctx"
	"github.com/hridyesh/paperboxd-backend/internal/service"
	"github.com/hridyesh/paperboxd-backend/internal/types"
	"github.com/jackc/pgx/v5/pgtype"
)

const referralBaseURL = "https://paperboxd.in"

// ReferralHandler holds dependencies for referral endpoints.
type ReferralHandler struct {
	Queries *db.Queries
}

func NewReferralHandler(queries *db.Queries) *ReferralHandler {
	return &ReferralHandler{Queries: queries}
}

// GetMyReferralCode returns the caller's referral code and share link.
// GET /api/v1/users/me/referral
func (h *ReferralHandler) GetMyReferralCode(w http.ResponseWriter, r *http.Request) {
	userIDStr, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}

	row, err := h.Queries.GetUserReferralCode(r.Context(), userID)
	if err != nil {
		slog.Error("get user referral code", "error", err)
		types.WriteInternalError(w)
		return
	}

	code := row.ReferralCode.String
	types.WriteJSON(w, http.StatusOK, map[string]any{
		"referral_code":  code,
		"referral_link":  service.GetReferralLink(referralBaseURL, code),
		"referral_count": row.ReferralCount.Int32,
	})
}

// GetMyReferrals returns the list of users the caller referred plus summary stats.
// GET /api/v1/users/me/referrals
func (h *ReferralHandler) GetMyReferrals(w http.ResponseWriter, r *http.Request) {
	userIDStr, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}

	pgUID := pgtype.UUID{Bytes: userID, Valid: true}

	referrals, err := h.Queries.GetUserReferrals(r.Context(), db.GetUserReferralsParams{
		ReferredBy: pgUID,
		Limit:      100,
	})
	if err != nil {
		slog.Error("get user referrals", "error", err)
		types.WriteInternalError(w)
		return
	}

	stats, err := h.Queries.GetReferralStats(r.Context(), pgUID)
	if err != nil {
		slog.Error("get referral stats", "error", err)
		types.WriteInternalError(w)
		return
	}

	results := make([]map[string]any, len(referrals))
	for i, ref := range referrals {
		results[i] = map[string]any{
			"username":       ref.Username,
			"joined_at":      ref.CreatedAt,
			"total_xp":       ref.TotalXp.Int32,
			"level":          ref.Level.Int32,
			"current_streak": ref.CurrentStreak.Int32,
			"level_name":     service.GetLevelName(int(ref.Level.Int32)),
		}
	}

	types.WriteJSON(w, http.StatusOK, map[string]any{
		"referrals":           results,
		"total_referrals":     stats.TotalReferrals,
		"active_referrals":    stats.ActiveReferrals,
		"long_term_referrals": stats.LongTermReferrals,
	})
}
