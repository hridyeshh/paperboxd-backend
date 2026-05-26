package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/hridyesh/paperboxd-backend/internal/config"
	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/external"
	"github.com/hridyesh/paperboxd-backend/internal/reqctx"
	"github.com/hridyesh/paperboxd-backend/internal/service"
	"github.com/hridyesh/paperboxd-backend/internal/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var usernameRegex = regexp.MustCompile(`^[a-zA-Z0-9_]{3,30}$`)

// UserHandler holds dependencies for user endpoints.
type UserHandler struct {
	Queries               *db.Queries
	Config                *config.Config
	ISBNdb                *external.ISBNdbClient
	GoogleBooks           *external.GoogleBooksClient
	RecommendationService *service.RecommendationService
	Enricher              *service.Enricher
}

// embedCallback returns a fire-and-forget func for newly cached books, or nil if embedding is disabled.
func (h *UserHandler) embedCallback() func(db.Book) {
	if h.RecommendationService == nil {
		return nil
	}
	svc := h.RecommendationService
	enricher := h.Enricher
	return func(book db.Book) {
		svc.EmbedBookAsync(bookToEnrichable(book), enricher)
	}
}

// GetByUsername handles GET /api/v1/users/:username
func (h *UserHandler) GetByUsername(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	if username == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Username is required")
		return
	}

	user, err := h.Queries.GetUserByUsername(r.Context(), strings.ToLower(username))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "User not found")
			return
		}
		slog.Error("get user by username", "error", err, "username", username)
		types.WriteInternalError(w)
		return
	}

	resp := userToResponse(user)

	// Live follower / following counts — the cached counters on `users` can drift
	// because Follow/Unfollow inserts into `follows` without updating them.
	if followers, err := h.Queries.CountFollowers(r.Context(), user.ID); err == nil {
		resp.FollowersCount = int32(followers)
	}
	if following, err := h.Queries.CountFollowing(r.Context(), user.ID); err == nil {
		resp.FollowingCount = int32(following)
	}

	types.WriteJSON(w, http.StatusOK, resp)
}

// Update handles PUT/PATCH /api/v1/users/:username (owner only)
func (h *UserHandler) Update(w http.ResponseWriter, r *http.Request) {
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

	// Verify the authenticated user owns this profile
	username := chi.URLParam(r, "username")
	target, err := h.Queries.GetUserByUsername(r.Context(), strings.ToLower(username))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "User not found")
			return
		}
		slog.Error("get user by username for update", "error", err)
		types.WriteInternalError(w)
		return
	}

	if target.ID != userID {
		types.WriteError(w, http.StatusForbidden, types.ErrCodeForbidden, "You can only update your own profile")
		return
	}

	var req types.UpdateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}

	// Handle username change separately (uniqueness check + dedicated query)
	if req.Username != nil {
		newUsername := strings.ToLower(strings.TrimSpace(*req.Username))
		if !usernameRegex.MatchString(newUsername) {
			types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "username must be 3-30 characters: letters, numbers, underscores only")
			return
		}
		_, taken := h.Queries.GetUserByUsername(r.Context(), newUsername)
		if taken == nil {
			types.WriteError(w, http.StatusConflict, types.ErrCodeConflict, "username is already taken")
			return
		}
		if _, err := h.Queries.UpdateUsername(r.Context(), db.UpdateUsernameParams{
			ID:       userID,
			Username: newUsername,
		}); err != nil {
			slog.Error("update username", "error", err, "user_id", userID)
			types.WriteInternalError(w)
			return
		}
		// If only username is changing, return early
		if req.Name == nil && req.Bio == nil && req.Pronouns == nil && req.AvatarURL == nil &&
			req.Birthday == nil && req.Gender == nil && req.Links == nil {
			refreshed, err := h.Queries.GetUserByID(r.Context(), userID)
			if err != nil {
				types.WriteInternalError(w)
				return
			}
			types.WriteJSON(w, http.StatusOK, userToResponse(refreshed))
			return
		}
	}

	params := db.UpdateUserParams{ID: userID}
	if req.Name != nil {
		params.Name = pgtype.Text{String: *req.Name, Valid: true}
	}
	if req.Bio != nil {
		params.Bio = pgtype.Text{String: *req.Bio, Valid: true}
	}
	if req.Pronouns != nil {
		params.Pronouns = *req.Pronouns
	}
	if req.AvatarURL != nil {
		params.AvatarUrl = pgtype.Text{String: *req.AvatarURL, Valid: true}
	}
	if req.Birthday != nil {
		t, err := time.Parse("2006-01-02", *req.Birthday)
		if err != nil {
			types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "birthday must be in YYYY-MM-DD format")
			return
		}
		params.Birthday = pgtype.Date{Time: t, Valid: true}
	}
	if req.Gender != nil {
		params.Gender = pgtype.Text{String: *req.Gender, Valid: true}
	}
	if req.Links != nil {
		params.Links = *req.Links
	}

	updated, err := h.Queries.UpdateUser(r.Context(), params)
	if err != nil {
		slog.Error("update user", "error", err, "user_id", userID)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusOK, userToResponse(updated))
}

// SaveOnboarding handles POST /api/v1/users/me/onboarding
// Saves preferred genres to users.favorite_genres and upserts author interest rows.
func (h *UserHandler) SaveOnboarding(w http.ResponseWriter, r *http.Request) {
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

	var req struct {
		Genres  []string `json:"genres"`
		Authors []string `json:"authors"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}
	if len(req.Genres) == 0 {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "genres is required")
		return
	}

	if _, err := h.Queries.UpdateUserGenres(r.Context(), db.UpdateUserGenresParams{
		ID:             userID,
		FavoriteGenres: req.Genres,
	}); err != nil {
		slog.Error("update user genres", "error", err)
		types.WriteInternalError(w)
		return
	}

	for _, author := range req.Authors {
		if author == "" {
			continue
		}
		if _, err := h.Queries.UpsertAuthorRead(r.Context(), db.UpsertAuthorReadParams{
			UserID:     userID,
			AuthorName: author,
		}); err != nil {
			slog.Warn("upsert author read", "error", err, "author", author)
		}
	}

	types.WriteJSON(w, http.StatusOK, map[string]any{
		"success":  true,
		"message":  "Onboarding saved successfully",
		"genres":   req.Genres,
		"authors":  req.Authors,
	})
}

// DeleteMe handles DELETE /api/v1/users/me — records exit reasons, soft-deletes
// the authenticated user, and revokes all their refresh tokens. The optional
// JSON body {reasons: string[]} is persisted to account_deletions for retention
// analysis; if missing or unparseable we still allow the deletion.
func (h *UserHandler) DeleteMe(w http.ResponseWriter, r *http.Request) {
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

	var req types.DeleteAccountRequest
	_ = json.NewDecoder(r.Body).Decode(&req)

	user, err := h.Queries.GetUserByID(r.Context(), userID)
	if err != nil {
		slog.Error("delete me: lookup user", "error", err, "user_id", userID)
		types.WriteInternalError(w)
		return
	}

	if err := h.Queries.RecordAccountDeletion(r.Context(), db.RecordAccountDeletionParams{
		UserID:   pgtype.UUID{Bytes: userID, Valid: true},
		Email:    user.Email,
		Username: pgtype.Text{String: user.Username, Valid: user.Username != ""},
		Reasons:  req.Reasons,
	}); err != nil {
		// Non-fatal: don't block deletion just because the audit write failed.
		slog.Warn("record account deletion", "error", err, "user_id", userID)
	}

	if err := h.Queries.SoftDeleteUser(r.Context(), userID); err != nil {
		slog.Error("soft delete user", "error", err, "user_id", userID)
		types.WriteInternalError(w)
		return
	}

	if err := h.Queries.RevokeAllUserTokens(r.Context(), userID); err != nil {
		// Non-fatal: account is already soft-deleted, so auth queries will fail anyway.
		slog.Warn("revoke tokens on delete", "error", err, "user_id", userID)
	}

	types.WriteJSON(w, http.StatusOK, types.SuccessResponse{Message: "Account deleted"})
}

// Search handles GET /api/v1/users/search?query=... or ?q=...
func (h *UserHandler) Search(w http.ResponseWriter, r *http.Request) {
	query := searchQueryString(r)
	if query == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "query parameter is required")
		return
	}

	page, pageSize := parsePagination(r)
	offset := int32((page - 1) * pageSize)
	limit := int32(pageSize)

	users, err := h.Queries.SearchUsers(r.Context(), db.SearchUsersParams{
		Column1: pgtype.Text{String: query, Valid: true},
		Limit:   limit,
		Offset:  offset,
	})
	if err != nil {
		slog.Error("search users", "error", err)
		types.WriteInternalError(w)
		return
	}

	resp := make([]types.UserResponse, len(users))
	for i, u := range users {
		resp[i] = userToResponse(u)
	}

	types.WriteJSON(w, http.StatusOK, types.UserListResponse{
		Users:      resp,
		TotalCount: int64(len(resp)),
		Page:       page,
		PageSize:   pageSize,
	}.WithPagination())
}

// RecordDailyOpen tracks daily app usage and awards XP once per day.
// POST /api/v1/users/me/daily-open
func (h *UserHandler) RecordDailyOpen(w http.ResponseWriter, r *http.Request) {
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

	xpSvc := service.NewXPService(h.Queries)
	info, err := xpSvc.GetUserXPInfo(r.Context(), userID)
	if err != nil {
		slog.Error("record daily open: get xp info", "error", err)
		types.WriteInternalError(w)
		return
	}

	today := time.Now().UTC().Format("2006-01-02")
	alreadyOpened := info.LastActivityDate.Valid && info.LastActivityDate.Time.UTC().Format("2006-01-02") == today

	if !alreadyOpened {
		if err := xpSvc.AwardXP(r.Context(), userID, "daily_open", service.XPDailyOpen, nil); err != nil {
			slog.Error("record daily open: award xp", "error", err)
		}
		// Reload after update
		info, err = xpSvc.GetUserXPInfo(r.Context(), userID)
		if err != nil {
			slog.Error("record daily open: reload xp info", "error", err)
			types.WriteInternalError(w)
			return
		}
		// Check streak milestone bonus
		go func() {
			_ = xpSvc.CheckAndAwardStreakBonus(r.Context(), userID, int(info.CurrentStreak.Int32))
		}()
	}

	types.WriteJSON(w, http.StatusOK, map[string]any{
		"daily_open_awarded": !alreadyOpened,
		"current_streak":     info.CurrentStreak.Int32,
		"total_xp":           info.TotalXp.Int32,
		"level":              info.Level.Int32,
		"level_name":         service.GetLevelName(int(info.Level.Int32)),
	})
}

// UpdateAvatar handles PATCH /api/v1/users/me/avatar
// Updates only the avatar_url for the authenticated user.
// Body: { avatar_url: string } — empty string clears the avatar.
func (h *UserHandler) UpdateAvatar(w http.ResponseWriter, r *http.Request) {
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

	var body struct {
		AvatarURL string `json:"avatar_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}

	// Valid:true with empty string clears the column; non-empty sets it.
	// Other UpdateUserParams fields default to pgtype.Text{Valid:false} (NULL),
	// which the COALESCE SQL leaves unchanged.
	updated, err := h.Queries.UpdateUser(r.Context(), db.UpdateUserParams{
		ID:        userID,
		AvatarUrl: pgtype.Text{String: body.AvatarURL, Valid: true},
	})
	if err != nil {
		slog.Error("update avatar", "error", err, "user_id", userID)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusOK, userToResponse(updated))
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func userToResponse(u db.User) types.UserResponse {
	resp := types.UserResponse{
		ID:             u.ID.String(),
		MongoID:        u.ID.String(),
		Username:       u.Username,
		Email:          u.Email,
		IsPublic:       u.IsPublic.Bool,
		BooksReadCount: u.BooksReadCount.Int32,
		TotalPagesRead: u.TotalPagesRead.Int32,
		FavoritesCount:    u.FavoritesCount,
		ListsCount:        u.ListsCount,
		DiaryEntriesCount: u.DiaryEntriesCount,
		FollowersCount:    u.FollowersCount.Int32,
		FollowingCount:    u.FollowingCount.Int32,
		FavoriteGenres:    u.FavoriteGenres,
		CreatedAt:         u.CreatedAt.Time.Format(time.RFC3339),
		Pronouns:          u.Pronouns,
		Links:             u.Links,
	}
	if resp.Pronouns == nil {
		resp.Pronouns = []string{}
	}
	if resp.Links == nil {
		resp.Links = []string{}
	}
	if resp.FavoriteGenres == nil {
		resp.FavoriteGenres = []string{}
	}
	if u.Name.Valid {
		resp.Name = u.Name.String
	}
	if u.AvatarUrl.Valid {
		resp.AvatarURL = &u.AvatarUrl.String
	}
	if u.Bio.Valid {
		resp.Bio = &u.Bio.String
	}
	if u.Birthday.Valid {
		s := u.Birthday.Time.Format("2006-01-02")
		resp.Birthday = &s
	}
	if u.Gender.Valid {
		resp.Gender = &u.Gender.String
	}
	return resp
}
