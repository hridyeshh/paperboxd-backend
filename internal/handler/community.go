package handler

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/hridyesh/paperboxd-backend/internal/cache"
	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/types"
)

// communityCacheTTL: the page is the same for every visitor, so one build per
// five minutes is plenty and keeps the four aggregate queries off the hot path.
const communityCacheTTL = 5 * time.Minute

const communityCacheKey = "community:v1"

// CommunityHandler serves GET /api/v1/community — the public "what is
// happening on Paperboxd" snapshot used by the landing page, the logged-in
// home when a reader follows nobody yet, and the mobile homes.
type CommunityHandler struct {
	Queries *db.Queries
	Cache   *cache.Client
}

// NewCommunityHandler creates a CommunityHandler.
func NewCommunityHandler(queries *db.Queries, cache *cache.Client) *CommunityHandler {
	return &CommunityHandler{Queries: queries, Cache: cache}
}

// CommunityTrendingBook is a BookResponse plus how many shelves it landed on
// this week, so clients can say "shelved by 12 readers this week" truthfully.
type CommunityTrendingBook struct {
	types.BookResponse
	Adds7d int32 `json:"adds_7d"`
}

// CommunityList is a public list with its owner attached, since the visitor
// has no profile context to look the owner up from.
type CommunityList struct {
	types.ListResponse
	OwnerName      string  `json:"owner_name"`
	OwnerAvatarURL *string `json:"owner_avatar_url"`
}

// CommunityReader is a public profile card: identity + Top 4 covers.
type CommunityReader struct {
	Username       string   `json:"username"`
	Name           string   `json:"name"`
	AvatarURL      *string  `json:"avatar_url"`
	Bio            *string  `json:"bio"`
	FollowersCount int32    `json:"followers_count"`
	BooksReadCount int32    `json:"books_read_count"`
	FavoriteCovers []string `json:"favorite_covers"`
}

// CommunityShelfBook is a book on a discovery shelf, carrying the one number
// that justifies its place there ("shelved by 7 readers this week"). Label is
// server-authored so every client makes the same claim.
type CommunityShelfBook struct {
	types.BookResponse
	Label string `json:"label"`
}

// CommunityResponse is the payload of GET /api/v1/community.
type CommunityResponse struct {
	TrendingBooks []CommunityTrendingBook  `json:"trending_books"`
	PopularBooks  []types.BookResponse     `json:"popular_books"`
	Rising        []CommunityShelfBook     `json:"rising"`
	MostTBR       []CommunityShelfBook     `json:"most_tbr"`
	HiddenGems    []CommunityShelfBook     `json:"hidden_gems"`
	Activity      []types.ActivityResponse `json:"activity"`
	Lists         []CommunityList          `json:"lists"`
	Readers       []CommunityReader        `json:"readers"`
	GeneratedAt   time.Time                `json:"generated_at"`
}

// Get handles GET /api/v1/community.
func (h *CommunityHandler) Get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if h.Cache != nil {
		var cached CommunityResponse
		if err := h.Cache.GetJSON(ctx, communityCacheKey, &cached); err == nil {
			w.Header().Set("Cache-Control", "public, max-age=60")
			types.WriteJSON(w, http.StatusOK, cached)
			return
		}
	}

	resp, err := h.build(ctx)
	if err != nil {
		slog.Error("build community snapshot", "error", err)
		types.WriteInternalError(w)
		return
	}

	if h.Cache != nil {
		if err := h.Cache.SetJSON(ctx, communityCacheKey, resp, communityCacheTTL); err != nil {
			slog.Warn("cache community snapshot", "error", err)
		}
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	types.WriteJSON(w, http.StatusOK, resp)
}

func (h *CommunityHandler) build(ctx context.Context) (CommunityResponse, error) {
	resp := CommunityResponse{
		TrendingBooks: []CommunityTrendingBook{},
		PopularBooks:  []types.BookResponse{},
		Rising:        []CommunityShelfBook{},
		MostTBR:       []CommunityShelfBook{},
		HiddenGems:    []CommunityShelfBook{},
		Activity:      []types.ActivityResponse{},
		Lists:         []CommunityList{},
		Readers:       []CommunityReader{},
		GeneratedAt:   time.Now().UTC(),
	}

	trending, err := h.Queries.GetTrendingBooks(ctx, 12)
	if err != nil {
		return resp, err
	}
	for _, row := range trending {
		resp.TrendingBooks = append(resp.TrendingBooks, CommunityTrendingBook{
			BookResponse: bookToResponse(row.Book),
			Adds7d:       row.Adds7d,
		})
	}

	popular, err := h.Queries.GetPopularBooks(ctx, db.GetPopularBooksParams{Limit: 12, Offset: 0})
	if err != nil {
		return resp, err
	}
	for _, b := range popular {
		resp.PopularBooks = append(resp.PopularBooks, bookToResponse(b))
	}

	// Discovery shelves. Each is allowed to come back empty — a quiet week
	// should render fewer shelves, not invented ones — so a failure here logs
	// and leaves the shelf out rather than failing the whole snapshot.
	if rising, err := h.Queries.GetRisingBooks(ctx, 12); err != nil {
		slog.Error("get rising books", "error", err)
	} else {
		for _, row := range rising {
			resp.Rising = append(resp.Rising, CommunityShelfBook{
				BookResponse: bookToResponse(row.Book),
				// gain is why it ranked; the label states the honest count.
				Label: pluralReaders(row.Recent, "shelved by %d %s this week"),
			})
		}
	}

	if tbr, err := h.Queries.GetMostTBRBooks(ctx, 12); err != nil {
		slog.Error("get most-tbr books", "error", err)
	} else {
		for _, row := range tbr {
			resp.MostTBR = append(resp.MostTBR, CommunityShelfBook{
				BookResponse: bookToResponse(row.Book),
				Label:        pluralReaders(row.Tbr7d, "added by %d %s this week"),
			})
		}
	}

	if gems, err := h.Queries.GetHiddenGems(ctx, 12); err != nil {
		slog.Error("get hidden gems", "error", err)
	} else {
		for _, row := range gems {
			resp.HiddenGems = append(resp.HiddenGems, CommunityShelfBook{
				BookResponse: bookToResponse(row.Book),
				Label: fmt.Sprintf("%.1f from %d %s",
					row.Rating, row.RatingsCount, plural(row.RatingsCount, "rating", "ratings")),
			})
		}
	}

	activityRows, err := h.Queries.GetPublicActivities(ctx, 80)
	if err != nil {
		return resp, err
	}
	resp.Activity = collapsePublicActivity(activityRows, 24)

	lists, err := h.Queries.GetPublicLists(ctx, 12)
	if err != nil {
		return resp, err
	}
	for _, row := range lists {
		item := CommunityList{
			ListResponse: types.ListResponse{
				ID:        row.ID.String(),
				UserID:    row.UserID.String(),
				Username:  row.Username,
				Title:     row.Title,
				BookCount: int64(row.BookCount),
				SaveCount: int64(row.SaveCount),
				CanView:   true,
				CoverURLs: []string{},
				CreatedAt: row.CreatedAt.Time,
				UpdatedAt: row.UpdatedAt.Time,
			},
			OwnerName: row.Username,
		}
		if row.Description.Valid {
			item.Description = &row.Description.String
		}
		if row.Name.Valid && row.Name.String != "" {
			item.OwnerName = row.Name.String
		}
		if row.AvatarUrl.Valid && row.AvatarUrl.String != "" {
			item.OwnerAvatarURL = &row.AvatarUrl.String
		}
		// ponytail: one cover query per list (6 lists, cached 5 min); fold into
		// GetPublicLists with array_agg if the section ever grows past a handful.
		if covers, err := h.Queries.GetListCoverURLs(ctx, row.ID); err == nil {
			for _, c := range covers {
				if c != "" {
					item.CoverURLs = append(item.CoverURLs, c)
				}
			}
		}
		resp.Lists = append(resp.Lists, item)
	}

	readers, err := h.Queries.GetPopularReaders(ctx, 8)
	if err != nil {
		return resp, err
	}
	for _, row := range readers {
		item := CommunityReader{
			Username:       row.Username,
			Name:           row.Username,
			FollowersCount: row.FollowersCount.Int32,
			BooksReadCount: row.BooksReadCount,
			FavoriteCovers: []string{},
		}
		if row.Name.Valid && row.Name.String != "" {
			item.Name = row.Name.String
		}
		if row.AvatarUrl.Valid && row.AvatarUrl.String != "" {
			item.AvatarURL = &row.AvatarUrl.String
		}
		if row.Bio.Valid && row.Bio.String != "" {
			item.Bio = &row.Bio.String
		}
		for _, c := range row.FavoriteCovers {
			if c != "" {
				item.FavoriteCovers = append(item.FavoriteCovers, c)
			}
		}
		resp.Readers = append(resp.Readers, item)
	}

	return resp, nil
}

func plural(n int32, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func pluralReaders(n int32, format string) string {
	return fmt.Sprintf(format, n, plural(n, "reader", "readers"))
}

// collapsePublicActivity turns the raw newest-first rows into a feed a stranger
// can read: one row per (user, object) and at most three per user, so a bulk
// import or a re-save spree cannot crowd everyone else out.
func collapsePublicActivity(rows []db.GetPublicActivitiesRow, limit int) []types.ActivityResponse {
	out := make([]types.ActivityResponse, 0, limit)
	seenObject := map[string]bool{}
	perUser := map[string]int{}
	for _, row := range rows {
		if len(out) >= limit {
			break
		}
		objectKey := row.UserID.String() + "|"
		// Entry before book: a diary note about Dune is different news from
		// finishing Dune, and created_diary_entry rows carry both ids.
		switch {
		case row.EntryID.Valid:
			objectKey += "e:" + uuid.UUID(row.EntryID.Bytes).String()
		case row.ListID.Valid:
			objectKey += "l:" + uuid.UUID(row.ListID.Bytes).String()
		case row.BookID.Valid:
			objectKey += "b:" + uuid.UUID(row.BookID.Bytes).String()
		default:
			objectKey += row.ID.String()
		}
		if seenObject[objectKey] || perUser[row.UserID.String()] >= 3 {
			continue
		}
		seenObject[objectKey] = true
		perUser[row.UserID.String()]++
		out = append(out, activityRowToResponse(activityRowFields{
			id: row.ID, userID: row.UserID, activityType: row.ActivityType,
			bookID: row.BookID, listID: row.ListID, entryID: row.EntryID, targetUserID: row.TargetUserID,
			createdAt: row.CreatedAt, username: row.Username, name: row.Name, avatarUrl: row.AvatarUrl,
			bookTitle: row.BookTitle, bookSlug: row.BookSlug,
			listTitle: row.ListTitle, entryTitle: row.EntryTitle, targetUsername: row.TargetUsername,
			metadata: row.Metadata, bookCover: row.BookCover,
		}))
	}
	return out
}
