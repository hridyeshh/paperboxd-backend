package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
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
	Cloudinary            *external.CloudinaryClient
	EventSvc              *service.EventService
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
	// Live diary count — the cached `users.diary_entries_count` column only gets
	// incremented on the CreateDiaryEntry path, so it drifts from other diary
	// mutation paths (imports, backfills) and shows a stale number on profile.
	if diaryCount, err := h.Queries.CountUserDiaryEntries(r.Context(), user.ID); err == nil {
		resp.DiaryEntriesCount = int32(diaryCount)
	}
	// Live books-read + pages-read — the cached `users.books_read_count` and
	// `total_pages_read` columns drift (imports/backfills don't touch them), so
	// the profile summary was showing stale counts and 0 pages.
	if stats, err := h.Queries.GetUserReadStats(r.Context(), user.ID); err == nil {
		resp.BooksReadCount = stats.BooksRead
		resp.TotalPagesRead = stats.PagesRead
	}

	// Attach is_following when an authenticated viewer requests another user's profile.
	isSelf := false
	isFollowing := false
	hasRequested := false
	if viewerIDStr, ok := reqctx.GetUserID(r.Context()); ok {
		if viewerID, err := uuid.Parse(viewerIDStr); err == nil {
			if viewerID == user.ID {
				isSelf = true
			} else {
				if following, err := h.Queries.CheckFollowing(r.Context(), db.CheckFollowingParams{
					FollowerID:  viewerID,
					FollowingID: user.ID,
				}); err == nil {
					isFollowing = following
					resp.IsFollowing = &isFollowing
				}
				// Social signal: someone looked at someone else. Self-views and
				// logged-out views are not emitted (events.user_id is NOT NULL).
				if h.EventSvc != nil {
					profileID := user.ID
					go h.EventSvc.Emit(context.Background(), service.EmitParams{
						UserID:    viewerID,
						EventType: "profile_viewed",
						Source:    "server",
						Metadata: map[string]any{
							"profile_user_id": profileID.String(),
							"is_public":       user.IsPublic,
							"is_following":    isFollowing,
						},
					})
				}
				if !user.IsPublic && !isFollowing {
					if requested, err := h.Queries.CheckFollowRequest(r.Context(), db.CheckFollowRequestParams{
						RequesterID: viewerID,
						TargetID:    user.ID,
					}); err == nil {
						hasRequested = requested
					}
				}
			}
		}
	}

	// A private profile shows a stranger only enough to decide whether to ask:
	// who this is, and how to request. Everything they have read is stripped
	// here, and the middleware blocks the routes that would serve it directly.
	canView := user.IsPublic || isSelf || isFollowing
	if !canView {
		resp = types.UserResponse{
			ID:             resp.ID,
			MongoID:        resp.MongoID,
			Username:       resp.Username,
			Name:           resp.Name,
			AvatarURL:      resp.AvatarURL,
			Bio:            resp.Bio,
			Pronouns:       resp.Pronouns,
			IsPublic:       false,
			FollowersCount: resp.FollowersCount,
			FollowingCount: resp.FollowingCount,
			CreatedAt:      resp.CreatedAt,
			IsFollowing:    &isFollowing,
			HasRequested:   &hasRequested,
		}
	}
	resp.CanView = &canView

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
		"success": true,
		"message": "Onboarding saved successfully",
		"genres":  req.Genres,
		"authors": req.Authors,
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

	// Hash, not the address. This audit row is intentionally not FK-linked to
	// users, so it survives the 30-day hard purge; a cleartext email here would
	// outlive the deleted account indefinitely. The hash still supports the
	// retention questions this table exists to answer (how many, why, repeat
	// signups) without holding a readable identifier.
	emailSum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(user.Email))))
	if err := h.Queries.RecordAccountDeletion(r.Context(), db.RecordAccountDeletionParams{
		UserID:    pgtype.UUID{Bytes: userID, Valid: true},
		EmailHash: pgtype.Text{String: hex.EncodeToString(emailSum[:]), Valid: true},
		Reasons:   req.Reasons,
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

// Suggested handles GET /api/v1/users/suggested?limit=8
//
// Readers to follow, for onboarding and empty-feed states. Ranked by
// favourite-genre overlap with the caller; the reason string is built here so
// web, iOS and Android render the same explanation.
func (h *UserHandler) Suggested(w http.ResponseWriter, r *http.Request) {
	viewerIDStr, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}
	viewerID, err := uuid.Parse(viewerIDStr)
	if err != nil {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 {
		limit = 8
	}
	if limit > 20 {
		limit = 20
	}

	viewer, err := h.Queries.GetUserByID(r.Context(), viewerID)
	if err != nil {
		slog.Error("suggested users: get viewer", "error", err, "user_id", viewerID)
		types.WriteInternalError(w)
		return
	}
	genres := viewer.FavoriteGenres
	if genres == nil {
		genres = []string{}
	}

	// Over-fetch: the genre query is only the first pass. The signals worth
	// leading with — books you have both read, and people you follow who follow
	// them — are joined on below and change the order.
	candidateLimit := limit * 4
	if candidateLimit > 60 {
		candidateLimit = 60
	}
	rows, err := h.Queries.SuggestedUsers(r.Context(), db.SuggestedUsersParams{
		ViewerGenres: genres,
		ViewerID:     viewerID,
		RowLimit:     int32(candidateLimit),
	})
	if err != nil {
		slog.Error("suggested users", "error", err, "user_id", viewerID)
		types.WriteInternalError(w)
		return
	}

	candidateIDs := make([]uuid.UUID, len(rows))
	for i, u := range rows {
		candidateIDs[i] = u.ID
	}
	shared, mutuals := h.suggestionSignals(r.Context(), viewerID, candidateIDs)

	out := make([]types.SuggestedUserResponse, 0, len(rows))
	for _, u := range rows {
		sh := shared[u.ID]
		mu := mutuals[u.ID]
		out = append(out, types.SuggestedUserResponse{
			ID:             u.ID.String(),
			Username:       u.Username,
			Name:           u.Name.String,
			AvatarURL:      u.AvatarUrl.String,
			Bio:            u.Bio.String,
			BooksReadCount: u.BooksReadCount,
			FollowersCount: u.FollowersCount.Int32,
			SharedGenres:   u.SharedGenres,
			SharedBooks:    sh.count,
			MutualFollows:  mu,
			Reason:         suggestedUserReason(sh.count, sh.title, mu, u.SharedGenres, u.BooksReadCount),
		})
	}

	// Strongest signal first; the query already ordered by genre overlap, and
	// sort.SliceStable keeps that as the tie-break.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].SharedBooks != out[j].SharedBooks {
			return out[i].SharedBooks > out[j].SharedBooks
		}
		return out[i].MutualFollows > out[j].MutualFollows
	})
	if len(out) > limit {
		out = out[:limit]
	}

	types.WriteJSON(w, http.StatusOK, map[string]any{"users": out})
}

type sharedReadSignal struct {
	count int32
	title string
}

// suggestionSignals fetches the two cross-reader signals for a candidate set in
// two set-based queries rather than per candidate. Failures degrade to empty
// maps: a weaker reason is better than no suggestions.
func (h *UserHandler) suggestionSignals(ctx context.Context, viewerID uuid.UUID, candidateIDs []uuid.UUID) (map[uuid.UUID]sharedReadSignal, map[uuid.UUID]int32) {
	shared := map[uuid.UUID]sharedReadSignal{}
	mutuals := map[uuid.UUID]int32{}
	if len(candidateIDs) == 0 {
		return shared, mutuals
	}

	if bookIDs, err := h.Queries.UserReadBookIDs(ctx, viewerID); err != nil {
		slog.Warn("suggested users: viewer read books", "error", err)
	} else if len(bookIDs) > 0 {
		rows, err := h.Queries.SharedReadCounts(ctx, db.SharedReadCountsParams{
			CandidateIds: candidateIDs,
			BookIds:      bookIDs,
		})
		if err != nil {
			slog.Warn("suggested users: shared reads", "error", err)
		}
		for _, r := range rows {
			shared[r.UserID] = sharedReadSignal{count: r.Shared, title: r.SampleTitle}
		}
	}

	if followingIDs, err := h.Queries.FollowingIDs(ctx, viewerID); err != nil {
		slog.Warn("suggested users: following ids", "error", err)
	} else if len(followingIDs) > 0 {
		rows, err := h.Queries.MutualFollowCounts(ctx, db.MutualFollowCountsParams{
			CandidateIds: candidateIDs,
			FollowerIds:  followingIDs,
		})
		if err != nil {
			slog.Warn("suggested users: mutual follows", "error", err)
		}
		for _, r := range rows {
			mutuals[r.UserID] = r.Mutuals
		}
	}
	return shared, mutuals
}

// suggestedUserReason turns the ranking signals into one human line, strongest
// first. Only claims what the row supports — a shared book is named, a shared
// genre is not invented, and volume is the last resort.
func suggestedUserReason(sharedBooks int32, sharedTitle string, mutualFollows int32, sharedGenres []string, booksRead int32) string {
	if sharedBooks > 0 && sharedTitle != "" {
		if sharedBooks == 1 {
			return fmt.Sprintf("You both read %s", sharedTitle)
		}
		return fmt.Sprintf("You both read %s and %d more", sharedTitle, sharedBooks-1)
	}
	if mutualFollows > 0 {
		if mutualFollows == 1 {
			return "Followed by someone you follow"
		}
		return fmt.Sprintf("Followed by %d people you follow", mutualFollows)
	}
	labels := make([]string, 0, 2)
	for _, g := range sharedGenres {
		if len(labels) == 2 {
			break
		}
		labels = append(labels, strings.ReplaceAll(g, "-", " "))
	}
	switch len(labels) {
	case 2:
		return fmt.Sprintf("Also into %s and %s", labels[0], labels[1])
	case 1:
		return fmt.Sprintf("Also into %s", labels[0])
	}
	if booksRead >= 20 {
		return fmt.Sprintf("Has read %d books", booksRead)
	}
	return "Active reader"
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

// UploadAvatar handles POST /api/v1/users/me/avatar/upload
// Accepts multipart/form-data with a `file` field (the raw image), uploads it to
// Cloudinary (signed, server-side — the secret never reaches the client), then
// persists the returned secure URL. Used by the iOS app, which has no Next.js
// BFF to do the Cloudinary round-trip the web client relies on.
func (h *UserHandler) UploadAvatar(w http.ResponseWriter, r *http.Request) {
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

	if h.Cloudinary == nil {
		types.WriteError(w, http.StatusServiceUnavailable, types.ErrCodeInternalServer, "Image upload is not configured")
		return
	}

	const maxUpload = 5 << 20 // 5 MB, matches the web upload limit.
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+1024)
	if err := r.ParseMultipartForm(maxUpload + 1024); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Image too large (max 5MB) or invalid form data")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Missing file field")
		return
	}
	defer file.Close()

	if header.Size > maxUpload {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Image too large (max 5MB)")
		return
	}

	data, err := io.ReadAll(io.LimitReader(file, maxUpload+1))
	if err != nil {
		slog.Error("upload avatar: read file", "error", err)
		types.WriteInternalError(w)
		return
	}
	if len(data) == 0 {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Empty image")
		return
	}
	if !strings.HasPrefix(http.DetectContentType(data), "image/") {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "File must be an image")
		return
	}

	secureURL, err := h.Cloudinary.UploadAvatar(r.Context(), userID.String(), data, header.Header.Get("Content-Type"))
	if err != nil {
		slog.Error("upload avatar: cloudinary", "error", err, "user_id", userID)
		types.WriteError(w, http.StatusBadGateway, types.ErrCodeInternalServer, "Failed to upload image")
		return
	}

	updated, err := h.Queries.UpdateUser(r.Context(), db.UpdateUserParams{
		ID:        userID,
		AvatarUrl: pgtype.Text{String: secureURL, Valid: true},
	})
	if err != nil {
		slog.Error("upload avatar: persist url", "error", err, "user_id", userID)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusOK, userToResponse(updated))
}

// UploadBanner handles POST /api/v1/users/me/banner/upload
// Accepts multipart/form-data with a `file` field, uploads to Cloudinary, and
// persists the returned secure URL as the user's banner.
func (h *UserHandler) UploadBanner(w http.ResponseWriter, r *http.Request) {
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

	if h.Cloudinary == nil {
		types.WriteError(w, http.StatusServiceUnavailable, types.ErrCodeInternalServer, "Image upload is not configured")
		return
	}

	const maxUpload = 8 << 20 // 8 MB — banners are larger than avatars.
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+1024)
	if err := r.ParseMultipartForm(maxUpload + 1024); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Image too large (max 8MB) or invalid form data")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Missing file field")
		return
	}
	defer file.Close()

	if header.Size > maxUpload {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Image too large (max 8MB)")
		return
	}

	data, err := io.ReadAll(io.LimitReader(file, maxUpload+1))
	if err != nil {
		slog.Error("upload banner: read file", "error", err)
		types.WriteInternalError(w)
		return
	}
	if len(data) == 0 {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Empty image")
		return
	}
	if !strings.HasPrefix(http.DetectContentType(data), "image/") {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "File must be an image")
		return
	}

	secureURL, err := h.Cloudinary.UploadBanner(r.Context(), userID.String(), data, header.Header.Get("Content-Type"))
	if err != nil {
		slog.Error("upload banner: cloudinary", "error", err, "user_id", userID)
		types.WriteError(w, http.StatusBadGateway, types.ErrCodeInternalServer, "Failed to upload image")
		return
	}

	updated, err := h.Queries.UpdateUser(r.Context(), db.UpdateUserParams{
		ID:        userID,
		BannerUrl: pgtype.Text{String: secureURL, Valid: true},
	})
	if err != nil {
		slog.Error("upload banner: persist url", "error", err, "user_id", userID)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusOK, userToResponse(updated))
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func userToResponse(u db.User) types.UserResponse {
	resp := types.UserResponse{
		ID:                u.ID.String(),
		MongoID:           u.ID.String(),
		Username:          u.Username,
		Email:             u.Email,
		IsPublic:          u.IsPublic,
		BooksReadCount:    u.BooksReadCount.Int32,
		TotalPagesRead:    u.TotalPagesRead.Int32,
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
	if u.BannerUrl.Valid {
		resp.BannerURL = &u.BannerUrl.String
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
