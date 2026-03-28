package handler

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/reqctx"
	"github.com/hridyesh/paperboxd-backend/internal/types"
	"github.com/jackc/pgx/v5/pgtype"
)

// ActivitiesHandler holds dependencies for activity feed endpoints.
type ActivitiesHandler struct {
	Queries *db.Queries
}

// NewActivitiesHandler creates an ActivitiesHandler.
func NewActivitiesHandler(queries *db.Queries) *ActivitiesHandler {
	return &ActivitiesHandler{Queries: queries}
}

// GetUserActivities handles GET /api/v1/activities/me.
func (h *ActivitiesHandler) GetUserActivities(w http.ResponseWriter, r *http.Request) {
	userIDStr, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}
	userID, _ := uuid.Parse(userIDStr)

	page, pageSize := parsePagination(r)

	rows, err := h.Queries.GetUserActivities(r.Context(), db.GetUserActivitiesParams{
		UserID: userID,
		Limit:  int32(pageSize),
		Offset: int32((page - 1) * pageSize),
	})
	if err != nil {
		slog.Error("get user activities", "error", err)
		types.WriteInternalError(w)
		return
	}

	activities := make([]types.ActivityResponse, len(rows))
	for i, row := range rows {
		activities[i] = activityRowToResponse(activityRowFields{
			id: row.ID, userID: row.UserID, activityType: row.ActivityType,
			bookID: row.BookID, listID: row.ListID, entryID: row.EntryID, targetUserID: row.TargetUserID,
			createdAt: row.CreatedAt, username: row.Username, name: row.Name, avatarUrl: row.AvatarUrl,
			bookTitle: row.BookTitle, bookSlug: row.BookSlug,
			listTitle: row.ListTitle, entryTitle: row.EntryTitle, targetUsername: row.TargetUsername,
		})
	}

	types.WriteJSON(w, http.StatusOK, map[string]any{
		"activities": activities,
		"page":       page,
		"page_size":  pageSize,
	})
}

// GetFollowingActivities handles GET /api/v1/activities/following.
func (h *ActivitiesHandler) GetFollowingActivities(w http.ResponseWriter, r *http.Request) {
	userIDStr, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}
	userID, _ := uuid.Parse(userIDStr)

	page, pageSize := parsePagination(r)

	rows, err := h.Queries.GetFollowingActivities(r.Context(), db.GetFollowingActivitiesParams{
		FollowerID: userID,
		Limit:      int32(pageSize),
		Offset:     int32((page - 1) * pageSize),
	})
	if err != nil {
		slog.Error("get following activities", "error", err)
		types.WriteInternalError(w)
		return
	}

	activities := make([]types.ActivityResponse, len(rows))
	for i, row := range rows {
		activities[i] = activityRowToResponse(activityRowFields{
			id: row.ID, userID: row.UserID, activityType: row.ActivityType,
			bookID: row.BookID, listID: row.ListID, entryID: row.EntryID, targetUserID: row.TargetUserID,
			createdAt: row.CreatedAt, username: row.Username, name: row.Name, avatarUrl: row.AvatarUrl,
			bookTitle: row.BookTitle, bookSlug: row.BookSlug,
			listTitle: row.ListTitle, entryTitle: row.EntryTitle, targetUsername: row.TargetUsername,
		})
	}

	types.WriteJSON(w, http.StatusOK, map[string]any{
		"activities": activities,
		"page":       page,
		"page_size":  pageSize,
	})
}

// CheckNewActivities handles GET /api/v1/activities/check-new.
func (h *ActivitiesHandler) CheckNewActivities(w http.ResponseWriter, r *http.Request) {
	userIDStr, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}
	userID, _ := uuid.Parse(userIDStr)

	since := time.Now().Add(-24 * time.Hour)
	if sinceStr := r.URL.Query().Get("since"); sinceStr != "" {
		if t, err := time.Parse(time.RFC3339, sinceStr); err == nil {
			since = t
		}
	}

	hasNew, err := h.Queries.CheckNewActivities(r.Context(), db.CheckNewActivitiesParams{
		FollowerID: userID,
		CreatedAt:  pgtype.Timestamp{Time: since, Valid: true},
	})
	if err != nil {
		slog.Error("check new activities", "error", err)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusOK, map[string]bool{"has_new": hasNew})
}

// ── Row converter ─────────────────────────────────────────────────────────────

// activityRowFields is a normalised view of either GetUserActivitiesRow or GetFollowingActivitiesRow.
type activityRowFields struct {
	id           uuid.UUID
	userID       uuid.UUID
	activityType string
	bookID       pgtype.UUID
	listID       pgtype.UUID
	entryID      pgtype.UUID
	targetUserID pgtype.UUID
	createdAt    pgtype.Timestamp
	username     string
	name         pgtype.Text
	avatarUrl    pgtype.Text
	bookTitle    pgtype.Text
	bookSlug     pgtype.Text
	listTitle    pgtype.Text
	entryTitle   pgtype.Text
	targetUsername pgtype.Text
}

func activityRowToResponse(f activityRowFields) types.ActivityResponse {
	resp := types.ActivityResponse{
		ID:           f.id.String(),
		UserID:       f.userID.String(),
		Username:     f.username,
		ActivityType: f.activityType,
		CreatedAt:    f.createdAt.Time,
	}
	if f.name.Valid {
		resp.Name = f.name.String
	}
	if f.avatarUrl.Valid {
		resp.AvatarURL = &f.avatarUrl.String
	}
	if f.bookID.Valid {
		idStr := uuid.UUID(f.bookID.Bytes).String()
		resp.BookID = &idStr
	}
	if f.bookTitle.Valid {
		resp.BookTitle = &f.bookTitle.String
	}
	if f.bookSlug.Valid {
		resp.BookSlug = &f.bookSlug.String
	}
	if f.listID.Valid {
		idStr := uuid.UUID(f.listID.Bytes).String()
		resp.ListID = &idStr
	}
	if f.listTitle.Valid {
		resp.ListTitle = &f.listTitle.String
	}
	if f.entryID.Valid {
		idStr := uuid.UUID(f.entryID.Bytes).String()
		resp.EntryID = &idStr
	}
	if f.entryTitle.Valid {
		resp.EntryTitle = &f.entryTitle.String
	}
	if f.targetUserID.Valid {
		idStr := uuid.UUID(f.targetUserID.Bytes).String()
		resp.TargetUserID = &idStr
	}
	if f.targetUsername.Valid {
		resp.TargetUsername = &f.targetUsername.String
	}
	return resp
}
