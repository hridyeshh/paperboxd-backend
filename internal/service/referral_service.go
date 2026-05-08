package service

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

type ReferralService struct {
	queries   *db.Queries
	xpService *XPService
}

func NewReferralService(queries *db.Queries, xpService *XPService) *ReferralService {
	return &ReferralService{queries: queries, xpService: xpService}
}

// GenerateReferralCode returns an 8-character uppercase alphanumeric code.
func GenerateReferralCode() string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return strings.ToUpper(string(b))
}

// GetReferralLink builds the shareable signup link.
func GetReferralLink(baseURL, referralCode string) string {
	return fmt.Sprintf("%s/signup?ref=%s", baseURL, referralCode)
}

// ProcessReferralSignup awards XP when a referred user signs up.
// Referrer gets 50 XP (exempt from cap); new user gets 25 XP welcome bonus.
func (s *ReferralService) ProcessReferralSignup(ctx context.Context, referrerID, newUserID uuid.UUID) error {
	if err := s.xpService.AwardXP(ctx, referrerID, "referral_signup", XPReferralSignup, &newUserID); err != nil {
		return err
	}
	const welcomeXP = 25
	if err := s.xpService.AwardXP(ctx, newUserID, "referral_signup", welcomeXP, &referrerID); err != nil {
		return err
	}
	if err := s.queries.IncrementReferralCount(ctx, referrerID); err != nil {
		return err
	}
	_, _ = s.queries.RebuildUserLeaderboardStats(ctx, referrerID)
	return nil
}

// CheckAndAwardReferralMilestone awards the referrer a bonus if the referred
// user hits a milestone (first_book, 30_day_streak).
func (s *ReferralService) CheckAndAwardReferralMilestone(ctx context.Context, userID uuid.UUID, milestone string) error {
	user, err := s.queries.GetUserByID(ctx, userID)
	if err != nil || !user.ReferredBy.Valid {
		return nil
	}
	referrerID := uuid.UUID(user.ReferredBy.Bytes)

	rewardKey := fmt.Sprintf("%s_%s", userID.String(), milestone)
	claimed, err := s.queries.HasClaimedReferralReward(ctx, db.HasClaimedReferralRewardParams{
		ID:      referrerID,
		Column2: rewardKey,
	})
	if err != nil || claimed {
		return nil
	}

	var xpAmount int
	var actionType string
	switch milestone {
	case "first_book":
		xpAmount, actionType = XPReferralBook, "referral_book"
	case "30_day_streak":
		xpAmount, actionType = XPReferral30Day, "referral_30day"
	default:
		return nil
	}

	if err := s.xpService.AwardXP(ctx, referrerID, actionType, xpAmount, &userID); err != nil {
		return err
	}
	return s.queries.MarkReferralRewardClaimed(ctx, db.MarkReferralRewardClaimedParams{
		ID:      referrerID,
		Column2: rewardKey,
	})
}

// pgUUID converts a uuid.UUID to pgtype.UUID.
func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}
