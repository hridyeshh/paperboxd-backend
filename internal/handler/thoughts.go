package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/external"
	"github.com/hridyesh/paperboxd-backend/internal/reqctx"
	"github.com/hridyesh/paperboxd-backend/internal/service"
	"github.com/hridyesh/paperboxd-backend/internal/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ThoughtHandler holds dependencies for thought endpoints.
type ThoughtHandler struct {
	Queries               *db.Queries
	ISBNdb                *external.ISBNdbClient
	GoogleBooks           *external.GoogleBooksClient
	RecommendationService *service.RecommendationService
	EventSvc              *service.EventService
}

// NewThoughtHandler creates a ThoughtHandler.
func NewThoughtHandler(queries *db.Queries, isbndb *external.ISBNdbClient, google *external.GoogleBooksClient, rec *service.RecommendationService, evtSvc *service.EventService) *ThoughtHandler {
	return &ThoughtHandler{
		Queries:               queries,
		ISBNdb:                isbndb,
		GoogleBooks:           google,
		RecommendationService: rec,
		EventSvc:              evtSvc,
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// uuidToPgtype converts a uuid.UUID to pgtype.UUID.
func uuidToPgtype(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

// resolveBookForThought resolves a book from the request (book_id / isbn / google_books_id).
// Returns pgtype.UUID{} (null) if no book fields are set.
func (h *ThoughtHandler) resolveBookForThought(ctx context.Context, bookID, isbn, googleBooksID *string) (pgtype.UUID, error) {
	switch {
	case bookID != nil:
		id, err := uuid.Parse(*bookID)
		if err != nil {
			return pgtype.UUID{}, errors.New("invalid book_id")
		}
		if _, err := h.Queries.GetBookByID(ctx, id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return pgtype.UUID{}, errors.New("book not found")
			}
			return pgtype.UUID{}, err
		}
		return uuidToPgtype(id), nil

	case googleBooksID != nil:
		book, err := h.Queries.GetBookByGoogleID(ctx, pgtype.Text{String: *googleBooksID, Valid: true})
		if errors.Is(err, pgx.ErrNoRows) {
			book, err = cacheBookFromGoogleBooks(ctx, h.Queries, h.GoogleBooks, *googleBooksID, nil)
		}
		if err != nil {
			return pgtype.UUID{}, err
		}
		return uuidToPgtype(book.ID), nil

	case isbn != nil:
		book, err := h.Queries.GetBookByISBN(ctx, pgtype.Text{String: *isbn, Valid: true})
		if errors.Is(err, pgx.ErrNoRows) {
			book, err = cacheBookFromISBNdb(ctx, h.Queries, h.ISBNdb, *isbn, nil)
		}
		if err != nil {
			return pgtype.UUID{}, err
		}
		return uuidToPgtype(book.ID), nil
	}

	return pgtype.UUID{}, nil // no book
}

// shouldEmbedThought reports whether a thought may be sent to the embedding
// provider. Private thoughts never qualify: the text would leave this system for
// Cohere and persist in cleartext in thoughts.embedding_text, which
// contradicts what the is_private toggle promises the reader. Thoughts with no
// book are skipped because the embedding is keyed to book context.
func shouldEmbedThought(hasBook, isPrivate bool) bool {
	return hasBook && !isPrivate
}

// threadRootOf is the thought that started t's thread — t itself when t is not
// a follow-up. Threads are flat, so one hop always reaches the root.
func threadRootOf(t db.Thought) uuid.UUID {
	if t.ThreadRootID.Valid {
		return uuid.UUID(t.ThreadRootID.Bytes)
	}
	return t.ID
}

// repostDenial returns the HTTP status and message refusing a repost, or 0 when
// the repost is allowed. Private thoughts and blocks answer 404 so a repost
// attempt cannot confirm the thought exists. A private account's thoughts are
// not repostable at all: a repost would show them to readers the author never
// approved.
func repostDenial(isOwnThought, thoughtPrivate, authorPublic, blocked bool) (int, string) {
	switch {
	case thoughtPrivate || blocked:
		return http.StatusNotFound, "Thought not found"
	case isOwnThought:
		return http.StatusBadRequest, "You can't repost your own thought"
	case !authorPublic:
		return http.StatusForbidden, "Thoughts from private accounts can't be reposted"
	}
	return 0, ""
}

// thoughtFields is the column set every thought query shares, so the several
// sqlc row types convert through one function.
type thoughtFields struct {
	id, userID          uuid.UUID
	bookID              pgtype.UUID
	title               pgtype.Text
	content             string
	isPrivate           bool
	rating              pgtype.Int4
	threadRootID        pgtype.UUID
	createdAt           pgtype.Timestamp
	updatedAt           pgtype.Timestamp
	username            string
	name, avatarURL     pgtype.Text
	bookTitle           pgtype.Text
	bookSlug            pgtype.Text
	bookCoverURL        pgtype.Text
	bookAuthors         []string
	likesCount          int64
	repostsCount        int64
	threadCount         int64
	isLiked, isReposted bool
}

func thoughtFieldsToResponse(f thoughtFields, viewerID *uuid.UUID) types.ThoughtResponse {
	resp := types.ThoughtResponse{
		ID:           f.id.String(),
		UserID:       f.userID.String(),
		Username:     f.username,
		Name:         f.name.String,
		Content:      f.content,
		IsPrivate:    f.isPrivate,
		LikesCount:   f.likesCount,
		IsLiked:      f.isLiked,
		RepostsCount: f.repostsCount,
		IsReposted:   f.isReposted,
		ThreadCount:  f.threadCount,
		CanEdit:      viewerID != nil && *viewerID == f.userID,
		CreatedAt:    f.createdAt.Time,
		UpdatedAt:    f.updatedAt.Time,
	}
	if f.avatarURL.Valid {
		resp.AvatarURL = &f.avatarURL.String
	}
	if f.title.Valid {
		resp.Title = &f.title.String
	}
	if f.rating.Valid {
		v := int(f.rating.Int32)
		resp.Rating = &v
	}
	if f.threadRootID.Valid {
		s := uuid.UUID(f.threadRootID.Bytes).String()
		resp.ThreadRootID = &s
	}
	if f.bookID.Valid {
		idStr := uuid.UUID(f.bookID.Bytes).String()
		resp.BookID = &idStr
		if f.bookTitle.Valid {
			resp.Book = &types.BookResponse{
				ID:      idStr,
				MongoID: idStr,
				Slug:    f.bookSlug.String,
				VolumeInfo: types.VolumeInfo{
					Title:   f.bookTitle.String,
					Authors: f.bookAuthors,
				},
			}
			if f.bookCoverURL.Valid {
				resp.Book.VolumeInfo.ImageLinks = types.ImageLinks{Thumbnail: f.bookCoverURL.String}
			}
		}
	}
	return resp
}

// singleThoughtResponse builds the response for one thought outside a list
// query, fetching the counts the list queries compute inline.
func (h *ThoughtHandler) singleThoughtResponse(ctx context.Context, t db.Thought, author db.User, viewerID *uuid.UUID) types.ThoughtResponse {
	f := thoughtFields{
		id: t.ID, userID: t.UserID, bookID: t.BookID, title: t.Title, content: t.Content,
		isPrivate: t.IsPrivate, rating: t.Rating, threadRootID: t.ThreadRootID,
		createdAt: t.CreatedAt, updatedAt: t.UpdatedAt,
		username: author.Username, name: author.Name, avatarURL: author.AvatarUrl,
	}
	f.likesCount, _ = h.Queries.CountThoughtLikes(ctx, t.ID)
	f.repostsCount, _ = h.Queries.CountThoughtReposts(ctx, t.ID)
	if !t.ThreadRootID.Valid {
		f.threadCount, _ = h.Queries.CountThreadFollowUps(ctx, uuidToPgtype(t.ID))
	}
	if viewerID != nil {
		f.isLiked, _ = h.Queries.CheckThoughtLiked(ctx, db.CheckThoughtLikedParams{UserID: *viewerID, ThoughtID: t.ID})
		f.isReposted, _ = h.Queries.CheckThoughtReposted(ctx, db.CheckThoughtRepostedParams{UserID: *viewerID, ThoughtID: t.ID})
	}
	return thoughtFieldsToResponse(f, viewerID)
}

// requireUserID returns the authenticated caller, writing 401 when there is none.
func requireUserID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	userIDStr, ok := reqctx.GetUserID(r.Context())
	if ok {
		if id, err := uuid.Parse(userIDStr); err == nil {
			return id, true
		}
	}
	types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
	return uuid.Nil, false
}

// thoughtFromURL loads the thought named by the {thoughtId} route param,
// writing 400/404/500 itself when it cannot.
func (h *ThoughtHandler) thoughtFromURL(w http.ResponseWriter, r *http.Request) (db.Thought, bool) {
	thoughtID, err := uuid.Parse(chi.URLParam(r, "thoughtId"))
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid thought ID")
		return db.Thought{}, false
	}
	t, err := h.Queries.GetThoughtByID(r.Context(), thoughtID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "Thought not found")
			return db.Thought{}, false
		}
		slog.Error("get thought", "error", err)
		types.WriteInternalError(w)
		return db.Thought{}, false
	}
	return t, true
}

// ── Handlers ──────────────────────────────────────────────────────────────────

// CreateThought handles POST /api/v1/users/:username/thoughts.
//
// With thread_parent_id the thought continues one of the author's own threads:
// it takes the thread's book and privacy, and earns no XP, activity or count of
// its own — the thread is one thought as far as the profile and leaderboard go.
// Title and rating belong to the first thought and are ignored on follow-ups.
func (h *ThoughtHandler) CreateThought(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}

	username := chi.URLParam(r, "username")
	target, err := h.Queries.GetUserByUsername(r.Context(), strings.ToLower(username))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "User not found")
			return
		}
		slog.Error("get user for thought", "error", err)
		types.WriteInternalError(w)
		return
	}
	if target.ID != userID {
		types.WriteError(w, http.StatusForbidden, types.ErrCodeForbidden, "Forbidden")
		return
	}

	var req types.CreateThoughtRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}

	if len(req.Content) == 0 {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "content is required")
		return
	}
	if req.Title != nil && len(*req.Title) > 100 {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "title must be max 100 characters")
		return
	}
	if req.Rating != nil && (*req.Rating < 1 || *req.Rating > 5) {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "rating must be between 1 and 5")
		return
	}

	params := db.CreateThoughtParams{
		UserID:    userID,
		Content:   req.Content,
		IsPrivate: req.IsPrivate,
	}

	if req.ThreadParentID != nil {
		parentID, err := uuid.Parse(*req.ThreadParentID)
		if err != nil {
			types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "invalid thread_parent_id")
			return
		}
		parent, err := h.Queries.GetThoughtByID(r.Context(), parentID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "Thought not found")
				return
			}
			slog.Error("get thread parent", "error", err)
			types.WriteInternalError(w)
			return
		}
		if parent.UserID != userID {
			types.WriteError(w, http.StatusForbidden, types.ErrCodeForbidden, "You can only continue your own thoughts")
			return
		}
		root := parent
		if parent.ThreadRootID.Valid {
			if root, err = h.Queries.GetThoughtByID(r.Context(), threadRootOf(parent)); err != nil {
				slog.Error("get thread root", "error", err)
				types.WriteInternalError(w)
				return
			}
		}
		params.ThreadRootID = uuidToPgtype(root.ID)
		params.BookID = root.BookID
		params.IsPrivate = root.IsPrivate
	} else {
		bookPgID, err := h.resolveBookForThought(r.Context(), req.BookID, req.ISBN, req.GoogleBooksID)
		if err != nil {
			slog.Error("resolve book for thought", "error", err)
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "Book not found")
			return
		}
		params.BookID = bookPgID
		if req.Title != nil {
			params.Title = pgtype.Text{String: *req.Title, Valid: true}
		}
		if req.Rating != nil {
			params.Rating = pgtype.Int4{Int32: int32(*req.Rating), Valid: true}
		}
	}

	thought, err := h.Queries.CreateThought(r.Context(), params)
	if err != nil {
		slog.Error("create thought", "error", err)
		types.WriteInternalError(w)
		return
	}

	thoughtID := thought.ID
	isFollowUp := thought.ThreadRootID.Valid
	if !isFollowUp {
		isBookSpecific := thought.BookID.Valid
		go func() {
			_ = h.Queries.IncrementUserThoughtCount(context.Background(), userID)
			if !thought.IsPrivate {
				_, _ = h.Queries.CreateActivity(context.Background(), db.CreateActivityParams{
					UserID:       userID,
					ActivityType: "created_thought",
					BookID:       thought.BookID,
					ThoughtID:    uuidToPgtype(thoughtID),
				})
			}
			xpSvc := service.NewXPService(h.Queries)
			xpAmount := service.XPThought
			if isBookSpecific {
				xpAmount = service.XPBookThought
			}
			_ = xpSvc.AwardXP(context.Background(), userID, "thought", xpAmount, &thoughtID)
			_, _ = h.Queries.RebuildUserLeaderboardStats(context.Background(), userID)
		}()
	}

	if h.RecommendationService != nil && shouldEmbedThought(thought.BookID.Valid, thought.IsPrivate) {
		thoughtIDStr := thoughtID.String()
		bookUUID := thought.BookID.Bytes
		content := req.Content
		go func() {
			ctx := context.Background()
			var bookTitle string
			var bookAuthors []string
			if book, err := h.Queries.GetBookByID(ctx, bookUUID); err == nil {
				bookTitle = book.Title
				bookAuthors = book.Authors
			}
			h.RecommendationService.EmbedThoughtAsync(thoughtIDStr, bookTitle, bookAuthors, content)
		}()
	}

	go func() {
		var bookIDPtr *uuid.UUID
		if thought.BookID.Valid {
			bid := uuid.UUID(thought.BookID.Bytes)
			bookIDPtr = &bid
		}
		h.EventSvc.Emit(context.Background(), service.EmitParams{
			UserID:    userID,
			BookID:    bookIDPtr,
			EventType: service.EventThoughtCreated,
			Source:    "server",
			Metadata:  map[string]any{"has_book": thought.BookID.Valid, "is_follow_up": isFollowUp},
		})
	}()

	types.WriteJSON(w, http.StatusCreated, h.singleThoughtResponse(r.Context(), thought, target, &userID))
}

// GetThought handles GET /api/v1/users/:username/thoughts/:thoughtId.
func (h *ThoughtHandler) GetThought(w http.ResponseWriter, r *http.Request) {
	thought, ok := h.thoughtFromURL(w, r)
	if !ok {
		return
	}
	requesterID := optionalUserID(r)
	if thought.IsPrivate && (requesterID == nil || *requesterID != thought.UserID) {
		types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "Thought not found")
		return
	}

	author, err := h.Queries.GetUserByID(r.Context(), thought.UserID)
	if err != nil {
		slog.Error("get author for thought", "error", err)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusOK, h.singleThoughtResponse(r.Context(), thought, author, requesterID))
}

// GetThoughtThread handles GET /api/v1/users/:username/thoughts/:thoughtId/thread.
// Any thought in the thread resolves to the whole thread.
func (h *ThoughtHandler) GetThoughtThread(w http.ResponseWriter, r *http.Request) {
	thought, ok := h.thoughtFromURL(w, r)
	if !ok {
		return
	}
	requesterID := optionalUserID(r)
	// Privacy is uniform across a thread, so the thought in hand decides it.
	if thought.IsPrivate && (requesterID == nil || *requesterID != thought.UserID) {
		types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "Thought not found")
		return
	}
	viewerID := uuid.Nil
	if requesterID != nil {
		viewerID = *requesterID
	}

	rows, err := h.Queries.GetThoughtThread(r.Context(), db.GetThoughtThreadParams{
		ViewerID: viewerID,
		RootID:   threadRootOf(thought),
	})
	if err != nil {
		slog.Error("get thought thread", "error", err)
		types.WriteInternalError(w)
		return
	}

	thoughts := make([]types.ThoughtResponse, len(rows))
	for i, row := range rows {
		var followUps int64
		if !row.ThreadRootID.Valid {
			followUps = int64(len(rows) - 1)
		}
		thoughts[i] = thoughtFieldsToResponse(thoughtFields{
			id: row.ID, userID: row.UserID, bookID: row.BookID, title: row.Title, content: row.Content,
			isPrivate: row.IsPrivate, rating: row.Rating, threadRootID: row.ThreadRootID,
			createdAt: row.CreatedAt, updatedAt: row.UpdatedAt,
			username: row.Username, name: row.Name, avatarURL: row.AvatarUrl,
			bookTitle: row.BookTitle, bookSlug: row.BookSlug, bookCoverURL: row.BookCoverUrl, bookAuthors: row.BookAuthors,
			likesCount: row.LikesCount, repostsCount: row.RepostsCount, threadCount: followUps,
			isLiked: row.IsLiked, isReposted: row.IsReposted,
		}, requesterID)
	}

	types.WriteJSON(w, http.StatusOK, types.ThoughtThreadResponse{Thoughts: thoughts})
}

// GetUserThoughts handles GET /api/v1/users/:username/thoughts — the profile's
// Thoughts tab: their thread-starting thoughts and the thoughts they reposted.
func (h *ThoughtHandler) GetUserThoughts(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	target, err := h.Queries.GetUserByUsername(r.Context(), strings.ToLower(username))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "User not found")
			return
		}
		slog.Error("get user for thoughts", "error", err)
		types.WriteInternalError(w)
		return
	}

	requesterID := optionalUserID(r)
	viewerID := uuid.Nil
	if requesterID != nil {
		viewerID = *requesterID
	}

	page, pageSize := parsePagination(r)

	rows, err := h.Queries.GetUserThoughtsFeed(r.Context(), db.GetUserThoughtsFeedParams{
		ProfileID: target.ID,
		ViewerID:  viewerID,
		Limit:     int32(pageSize),
		Offset:    int32((page - 1) * pageSize),
	})
	if err != nil {
		slog.Error("get user thoughts", "error", err)
		types.WriteInternalError(w)
		return
	}

	totalCount, _ := h.Queries.CountUserThoughtsFeed(r.Context(), db.CountUserThoughtsFeedParams{
		ProfileID: target.ID,
		ViewerID:  viewerID,
	})

	thoughts := make([]types.ThoughtResponse, len(rows))
	for i, row := range rows {
		thoughts[i] = thoughtFieldsToResponse(thoughtFields{
			id: row.ID, userID: row.UserID, bookID: row.BookID, title: row.Title, content: row.Content,
			isPrivate: row.IsPrivate, rating: row.Rating, threadRootID: row.ThreadRootID,
			createdAt: row.CreatedAt, updatedAt: row.UpdatedAt,
			username: row.Username, name: row.Name, avatarURL: row.AvatarUrl,
			bookTitle: row.BookTitle, bookSlug: row.BookSlug, bookCoverURL: row.BookCoverUrl, bookAuthors: row.BookAuthors,
			likesCount: row.LikesCount, repostsCount: row.RepostsCount, threadCount: row.ThreadCount,
			isLiked: row.IsLiked, isReposted: row.IsReposted,
		}, requesterID)
		if row.IsRepost {
			thoughts[i].RepostedBy = &types.ThoughtReposter{Username: target.Username, Name: target.Name.String}
		}
	}

	types.WriteJSON(w, http.StatusOK, types.ThoughtsResponse{
		Thoughts:   thoughts,
		TotalCount: int64(totalCount),
		Page:       page,
		PageSize:   pageSize,
	}.WithPagination())
}

// GetBookThoughts handles GET /api/v1/books/:id/thoughts.
func (h *ThoughtHandler) GetBookThoughts(w http.ResponseWriter, r *http.Request) {
	bookID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid book ID")
		return
	}

	requesterID := optionalUserID(r)
	page, pageSize := parsePagination(r)

	viewerID := uuid.Nil
	if requesterID != nil {
		viewerID = *requesterID
	}

	rows, err := h.Queries.GetBookThoughts(r.Context(), db.GetBookThoughtsParams{
		BookID:   uuidToPgtype(bookID),
		ViewerID: viewerID,
		Limit:    int32(pageSize),
		Offset:   int32((page - 1) * pageSize),
	})
	if err != nil {
		slog.Error("get book thoughts", "error", err)
		types.WriteInternalError(w)
		return
	}

	thoughts := make([]types.ThoughtResponse, len(rows))
	for i, row := range rows {
		thoughts[i] = thoughtFieldsToResponse(thoughtFields{
			id: row.ID, userID: row.UserID, bookID: row.BookID, title: row.Title, content: row.Content,
			rating: row.Rating, createdAt: row.CreatedAt, updatedAt: row.UpdatedAt,
			username: row.Username, name: row.Name, avatarURL: row.AvatarUrl,
		}, requesterID)
	}

	types.WriteJSON(w, http.StatusOK, map[string]any{
		"thoughts":  thoughts,
		"page":      page,
		"page_size": pageSize,
	})
}

// UpdateThought handles PUT /api/v1/users/:username/thoughts/:thoughtId.
func (h *ThoughtHandler) UpdateThought(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	thought, ok := h.thoughtFromURL(w, r)
	if !ok {
		return
	}
	if thought.UserID != userID {
		types.WriteError(w, http.StatusForbidden, types.ErrCodeForbidden, "Forbidden")
		return
	}

	var req types.UpdateThoughtRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}
	if req.Rating != nil && (*req.Rating < 1 || *req.Rating > 5) {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "rating must be between 1 and 5")
		return
	}
	if req.Title != nil && len(*req.Title) > 100 {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "title must be max 100 characters")
		return
	}

	// Merge updates onto current values
	titleParam := thought.Title
	if req.Title != nil {
		titleParam = pgtype.Text{String: *req.Title, Valid: true}
	}
	content := thought.Content
	if req.Content != nil {
		if len(*req.Content) == 0 {
			types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "content cannot be empty")
			return
		}
		content = *req.Content
	}
	// A follow-up's privacy is its thread's; only the first thought sets it.
	isRoot := !thought.ThreadRootID.Valid
	isPrivate := thought.IsPrivate
	if req.IsPrivate != nil && isRoot {
		isPrivate = *req.IsPrivate
	}
	ratingParam := thought.Rating
	if req.Rating != nil {
		ratingParam = pgtype.Int4{Int32: int32(*req.Rating), Valid: true}
	}

	updated, err := h.Queries.UpdateThought(r.Context(), db.UpdateThoughtParams{
		ID:        thought.ID,
		Title:     titleParam,
		Content:   content,
		IsPrivate: isPrivate,
		Rating:    ratingParam,
	})
	if err != nil {
		slog.Error("update thought", "error", err)
		types.WriteInternalError(w)
		return
	}

	if isRoot && isPrivate != thought.IsPrivate {
		if err := h.Queries.SetThreadPrivacy(r.Context(), db.SetThreadPrivacyParams{
			IsPrivate: isPrivate,
			RootID:    uuidToPgtype(thought.ID),
		}); err != nil {
			slog.Error("set thread privacy", "error", err, "thought_id", thought.ID)
			types.WriteInternalError(w)
			return
		}
	}

	// Flipping a thought to private must retract what the public version already
	// leaked: the stored vector and the cleartext embedding_text. Unconditional
	// on isPrivate rather than on the transition — it is idempotent, and a row
	// that predates the create-path guard may hold a vector while already private.
	if isPrivate && h.RecommendationService != nil {
		if err := h.RecommendationService.ClearThoughtEmbedding(r.Context(), thought.ID.String()); err != nil {
			slog.Warn("clear thought embedding on privacy flip", "error", err, "thought_id", thought.ID)
		}
	}

	owner, _ := h.Queries.GetUserByID(r.Context(), userID)
	types.WriteJSON(w, http.StatusOK, h.singleThoughtResponse(r.Context(), updated, owner, &userID))
}

// DeleteThought handles DELETE /api/v1/users/:username/thoughts/:thoughtId.
// Deleting the first thought of a thread deletes its follow-ups (FK cascade).
func (h *ThoughtHandler) DeleteThought(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	thought, ok := h.thoughtFromURL(w, r)
	if !ok {
		return
	}
	if thought.UserID != userID {
		types.WriteError(w, http.StatusForbidden, types.ErrCodeForbidden, "Forbidden")
		return
	}

	if err := h.Queries.DeleteThought(r.Context(), thought.ID); err != nil {
		slog.Error("delete thought", "error", err)
		types.WriteInternalError(w)
		return
	}

	if !thought.ThreadRootID.Valid {
		go func() {
			_ = h.Queries.DecrementUserThoughtCount(context.Background(), userID)
		}()
	}

	types.WriteJSON(w, http.StatusOK, types.SuccessResponse{Message: "Thought deleted"})
}

// LikeThought handles POST /api/v1/users/:username/thoughts/:thoughtId/like.
func (h *ThoughtHandler) LikeThought(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	thought, ok := h.thoughtFromURL(w, r)
	if !ok {
		return
	}
	if thought.IsPrivate && thought.UserID != userID {
		types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "Thought not found")
		return
	}
	already, err := h.Queries.CheckThoughtLiked(r.Context(), db.CheckThoughtLikedParams{
		UserID:    userID,
		ThoughtID: thought.ID,
	})
	if err != nil {
		slog.Error("check thought liked", "error", err)
		types.WriteInternalError(w)
		return
	}
	if already {
		types.WriteError(w, http.StatusConflict, types.ErrCodeConflict, "Already liked")
		return
	}

	if _, err := h.Queries.LikeThought(r.Context(), db.LikeThoughtParams{
		UserID:    userID,
		ThoughtID: thought.ID,
	}); err != nil {
		slog.Error("like thought", "error", err)
		types.WriteInternalError(w)
		return
	}

	go func() {
		ownerID := thought.UserID
		_, _ = h.Queries.CreateActivity(context.Background(), db.CreateActivityParams{
			UserID:       userID,
			ActivityType: "liked_thought",
			ThoughtID:    uuidToPgtype(thought.ID),
			TargetUserID: uuidToPgtype(ownerID),
		})
		xpSvc := service.NewXPService(h.Queries)
		tid := thought.ID
		_ = xpSvc.AwardXP(context.Background(), ownerID, "thought_liked", service.XPThoughtLiked, &tid)
	}()

	go func() {
		h.EventSvc.Emit(context.Background(), service.EmitParams{
			UserID:    userID,
			EventType: service.EventThoughtLiked,
			Source:    "server",
		})
	}()

	likesCount, _ := h.Queries.CountThoughtLikes(r.Context(), thought.ID)
	types.WriteJSON(w, http.StatusCreated, map[string]any{
		"message":     "Thought liked",
		"likes_count": likesCount,
	})
}

// UnlikeThought handles DELETE /api/v1/users/:username/thoughts/:thoughtId/like.
func (h *ThoughtHandler) UnlikeThought(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	thoughtID, err := uuid.Parse(chi.URLParam(r, "thoughtId"))
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid thought ID")
		return
	}

	if err := h.Queries.UnlikeThought(r.Context(), db.UnlikeThoughtParams{
		UserID:    userID,
		ThoughtID: thoughtID,
	}); err != nil {
		slog.Error("unlike thought", "error", err)
		types.WriteInternalError(w)
		return
	}

	likesCount, _ := h.Queries.CountThoughtLikes(r.Context(), thoughtID)
	types.WriteJSON(w, http.StatusOK, map[string]any{
		"message":     "Thought unliked",
		"likes_count": likesCount,
	})
}

// RepostThought handles POST /api/v1/users/:username/thoughts/:thoughtId/repost.
func (h *ThoughtHandler) RepostThought(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	thought, ok := h.thoughtFromURL(w, r)
	if !ok {
		return
	}

	author, err := h.Queries.GetUserByID(r.Context(), thought.UserID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) { // author deleted their account
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "Thought not found")
			return
		}
		slog.Error("get author for repost", "error", err)
		types.WriteInternalError(w)
		return
	}
	blocked, err := h.Queries.CheckBlockedEither(r.Context(), db.CheckBlockedEitherParams{
		BlockerID: userID,
		BlockedID: author.ID,
	})
	if err != nil {
		slog.Error("check block for repost", "error", err)
		types.WriteInternalError(w)
		return
	}
	if status, msg := repostDenial(author.ID == userID, thought.IsPrivate, author.IsPublic, blocked); status != 0 {
		code := types.ErrCodeNotFound
		switch status {
		case http.StatusBadRequest:
			code = types.ErrCodeValidation
		case http.StatusForbidden:
			code = types.ErrCodePrivateProfile
		}
		types.WriteError(w, status, code, msg)
		return
	}

	inserted, err := h.Queries.RepostThought(r.Context(), db.RepostThoughtParams{
		UserID:    userID,
		ThoughtID: thought.ID,
	})
	if err != nil {
		slog.Error("repost thought", "error", err)
		types.WriteInternalError(w)
		return
	}
	if inserted == 0 {
		types.WriteError(w, http.StatusConflict, types.ErrCodeConflict, "Already reposted")
		return
	}

	go func() {
		ctx := context.Background()
		_, _ = h.Queries.CreateActivity(ctx, db.CreateActivityParams{
			UserID:       userID,
			ActivityType: "reposted_thought",
			BookID:       thought.BookID,
			ThoughtID:    uuidToPgtype(thought.ID),
			TargetUserID: uuidToPgtype(author.ID),
		})
		h.EventSvc.Emit(ctx, service.EmitParams{
			UserID:    userID,
			EventType: service.EventThoughtReposted,
			Source:    "server",
		})
	}()

	count, _ := h.Queries.CountThoughtReposts(r.Context(), thought.ID)
	types.WriteJSON(w, http.StatusCreated, map[string]any{
		"message":       "Thought reposted",
		"reposts_count": count,
	})
}

// UnrepostThought handles DELETE /api/v1/users/:username/thoughts/:thoughtId/repost.
func (h *ThoughtHandler) UnrepostThought(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	thoughtID, err := uuid.Parse(chi.URLParam(r, "thoughtId"))
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid thought ID")
		return
	}

	removed, err := h.Queries.UnrepostThought(r.Context(), db.UnrepostThoughtParams{
		UserID:    userID,
		ThoughtID: thoughtID,
	})
	if err != nil {
		slog.Error("unrepost thought", "error", err)
		types.WriteInternalError(w)
		return
	}
	if removed > 0 {
		if err := h.Queries.DeleteRepostActivity(r.Context(), db.DeleteRepostActivityParams{
			UserID:    userID,
			ThoughtID: uuidToPgtype(thoughtID),
		}); err != nil {
			slog.Warn("delete repost activity", "error", err, "thought_id", thoughtID)
		}
	}

	count, _ := h.Queries.CountThoughtReposts(r.Context(), thoughtID)
	types.WriteJSON(w, http.StatusOK, map[string]any{
		"message":       "Thought unreposted",
		"reposts_count": count,
	})
}
