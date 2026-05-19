package service

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

// XP Constants - points awarded for each action
const (
	MaxDailyXP = int32(150)

	XPDailyOpen     = 3
	XPDailyStreak   = 5
	XPBookRead      = 25
	XPAddToTBR      = 2
	XPReadProgress  = 5
	XPDiaryEntry    = 15
	XPBookDiary     = 20
	XPCreateList    = 15
	XPCreateListMore = 5
	XPAddToList     = 1

	XPFollowGained = 5  // awarded to the person being followed
	XPDiaryLiked   = 5  // awarded to the diary entry author

	XPStreak7   = 50
	XPStreak30  = 200
	XPStreak100 = 1000
	XPStreak365 = 5000

	XPReferralSignup = 50
	XPReferralBook   = 50
	XPReferral30Day  = 100

	XPNewGenre      = 5
	XPGenreDiversity = 25
)

var exemptFromDailyCap = map[string]bool{
	"streak_7":       true,
	"streak_30":      true,
	"streak_100":     true,
	"streak_365":     true,
	"referral_signup": true,
	"referral_book":  true,
	"referral_30day": true,
	"goal_milestone": true,
	"goal_completed": true,
}

type XPService struct {
	queries *db.Queries
}

func NewXPService(queries *db.Queries) *XPService {
	return &XPService{queries: queries}
}

// AwardXP gives XP to a user with daily cap enforcement.
func (s *XPService) AwardXP(ctx context.Context, userID uuid.UUID, actionType string, xpAmount int, referenceID *uuid.UUID) error {
	if !exemptFromDailyCap[actionType] {
		todayXP, err := s.queries.GetUserXPToday(ctx, userID)
		if err != nil {
			return err
		}
		if todayXP >= MaxDailyXP {
			xpAmount = 0
		} else if int32(xpAmount)+todayXP > MaxDailyXP {
			xpAmount = int(MaxDailyXP - todayXP)
		}
	}

	if xpAmount > 0 {
		if err := s.queries.AddXP(ctx, db.AddXPParams{
			ID:      userID,
			TotalXp: pgtype.Int4{Int32: int32(xpAmount), Valid: true},
		}); err != nil {
			return err
		}
	}

	var refID pgtype.UUID
	if referenceID != nil {
		refID = pgtype.UUID{Bytes: *referenceID, Valid: true}
	}

	metadata, _ := json.Marshal(map[string]any{
		"xp_awarded":        xpAmount,
		"daily_cap_reached": !exemptFromDailyCap[actionType] && xpAmount == 0,
		"exempt_from_cap":   exemptFromDailyCap[actionType],
	})

	if err := s.queries.LogXPTransaction(ctx, db.LogXPTransactionParams{
		UserID:      userID,
		ActionType:  actionType,
		XpAmount:    int32(xpAmount),
		ReferenceID: refID,
		Metadata:    metadata,
	}); err != nil {
		return err
	}

	return s.queries.UpdateUserStreak(ctx, userID)
}

// GetXPForAction returns the base XP for a given action type.
func (s *XPService) GetXPForAction(actionType string, metadata map[string]any) int {
	switch actionType {
	case "daily_open":
		return XPDailyOpen
	case "daily_streak":
		return XPDailyStreak
	case "book_read":
		return XPBookRead
	case "add_to_tbr":
		return XPAddToTBR
	case "read_progress":
		return XPReadProgress
	case "diary_entry":
		if metadata != nil && metadata["book_specific"] == true {
			return XPBookDiary
		}
		return XPDiaryEntry
	case "create_list":
		if metadata != nil && metadata["is_first_list"] == true {
			return XPCreateList
		}
		return XPCreateListMore
	case "add_to_list":
		return XPAddToList
	case "streak_7":
		return XPStreak7
	case "streak_30":
		return XPStreak30
	case "streak_100":
		return XPStreak100
	case "streak_365":
		return XPStreak365
	case "referral_signup":
		return XPReferralSignup
	case "referral_book":
		return XPReferralBook
	case "referral_30day":
		return XPReferral30Day
	case "follow_gained":
		return XPFollowGained
	case "diary_liked":
		return XPDiaryLiked
	case "new_genre":
		return XPNewGenre
	case "genre_diversity":
		return XPGenreDiversity
	default:
		return 0
	}
}

// CheckAndAwardStreakBonus awards a bonus if the user just hit a streak milestone.
func (s *XPService) CheckAndAwardStreakBonus(ctx context.Context, userID uuid.UUID, currentStreak int) error {
	var bonusXP int
	var actionType string
	switch currentStreak {
	case 7:
		bonusXP, actionType = XPStreak7, "streak_7"
	case 30:
		bonusXP, actionType = XPStreak30, "streak_30"
		defer func() {
			refSvc := &ReferralService{queries: s.queries, xpService: s}
			_ = refSvc.CheckAndAwardReferralMilestone(ctx, userID, "30_day_streak")
		}()
	case 100:
		bonusXP, actionType = XPStreak100, "streak_100"
	case 365:
		bonusXP, actionType = XPStreak365, "streak_365"
	default:
		return nil
	}
	return s.AwardXP(ctx, userID, actionType, bonusXP, nil)
}

func (s *XPService) GetUserXPInfo(ctx context.Context, userID uuid.UUID) (db.GetUserXPRow, error) {
	return s.queries.GetUserXP(ctx, userID)
}

func (s *XPService) GetUserXPToday(ctx context.Context, userID uuid.UUID) (int32, error) {
	return s.queries.GetUserXPToday(ctx, userID)
}

func (s *XPService) GetUserXPHistory(ctx context.Context, userID uuid.UUID, limit int32) ([]db.GetUserXPHistoryRow, error) {
	return s.queries.GetUserXPHistory(ctx, db.GetUserXPHistoryParams{
		UserID: userID,
		Limit:  limit,
	})
}

// CalculateLevel mirrors the SQL level formula.
func CalculateLevel(totalXP int) int {
	switch {
	case totalXP >= 12000:
		return 31 + ((totalXP - 12000) / 500)
	case totalXP >= 8000:
		return 26 + ((totalXP - 8000) / 800)
	case totalXP >= 5000:
		return 21 + ((totalXP - 5000) / 600)
	case totalXP >= 3000:
		return 16 + ((totalXP - 3000) / 400)
	case totalXP >= 1500:
		return 11 + ((totalXP - 1500) / 300)
	case totalXP >= 500:
		return 6 + ((totalXP - 500) / 200)
	default:
		return 1 + (totalXP / 100)
	}
}

func GetLevelName(level int) string {
	switch {
	case level >= 31:
		return "Living Library"
	case level >= 26:
		return "Legendary Curator"
	case level >= 21:
		return "Grand Archivist"
	case level >= 16:
		return "Literary Scholar"
	case level >= 11:
		return "Bibliophile"
	case level >= 6:
		return "Avid Reader"
	default:
		return "Beginner Reader"
	}
}

func GetLevelBadge(level int) string {
	switch {
	case level >= 30:
		return "🌟"
	case level >= 25:
		return "👑"
	case level >= 20:
		return "💎"
	case level >= 15:
		return "🥇"
	case level >= 10:
		return "🥈"
	case level >= 5:
		return "🥉"
	default:
		return "📖"
	}
}

// GetXPNeededForNextLevel returns XP remaining to reach the next level.
func GetXPNeededForNextLevel(currentXP, currentLevel int) int {
	var nextLevelXP int
	switch {
	case currentLevel >= 31:
		nextLevelXP = 12000 + ((currentLevel - 30) * 500)
	case currentLevel >= 26:
		nextLevelXP = 8000 + ((currentLevel - 25) * 800)
	case currentLevel >= 21:
		nextLevelXP = 5000 + ((currentLevel - 20) * 600)
	case currentLevel >= 16:
		nextLevelXP = 3000 + ((currentLevel - 15) * 400)
	case currentLevel >= 11:
		nextLevelXP = 1500 + ((currentLevel - 10) * 300)
	case currentLevel >= 6:
		nextLevelXP = 500 + ((currentLevel - 5) * 200)
	default:
		nextLevelXP = currentLevel * 100
	}
	return nextLevelXP - currentXP
}
