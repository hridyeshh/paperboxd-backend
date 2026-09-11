package handler

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/reqctx"
	"github.com/hridyesh/paperboxd-backend/internal/types"
)

// BookReaderStats is what Paperboxd's own readers did with a book. Every number
// is counted live off the shelf: books.total_reads_count and total_tbr_count
// have been zero since migration 000003 and nothing ever writes them, and
// books.average_rating is Google's number, not ours.
type BookReaderStats struct {
	Rating       *float64 `json:"rating"`
	RatingsCount int32    `json:"ratings_count"`
	// Histogram is counts for 1★…5★, index 0 = 1★.
	Histogram [5]int32 `json:"histogram"`
	Reads     int32    `json:"reads"`
	Reading   int32    `json:"reading"`
	TBR       int32    `json:"tbr"`
	TBR30d    int32    `json:"tbr_30d"`
}

// BookFriend is one person the viewer follows who has this book on their shelf.
type BookFriend struct {
	UserID      string  `json:"user_id"`
	Username    string  `json:"username"`
	Name        string  `json:"name"`
	AvatarURL   *string `json:"avatar_url"`
	Status      string  `json:"status"`
	Rating      *int    `json:"rating"`
	CurrentPage *int    `json:"current_page"`
	StartedAt   *string `json:"started_at"`
}

// BookList is a public list this book appears in.
type BookList struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Username  string   `json:"username"`
	OwnerName string   `json:"owner_name"`
	AvatarURL *string  `json:"avatar_url"`
	BookCount int32    `json:"book_count"`
	SaveCount int32    `json:"save_count"`
	CoverURLs []string `json:"cover_urls"`
}

// BookSocialResponse is the payload of GET /api/v1/books/{id}/social.
type BookSocialResponse struct {
	Readers BookReaderStats `json:"readers"`
	// Friends is empty for anonymous viewers and for readers who follow nobody
	// with this book.
	Friends        []BookFriend `json:"friends"`
	FriendsRead    int32        `json:"friends_read"`
	FriendsReading int32        `json:"friends_reading"`
	FriendsTBR     int32        `json:"friends_tbr"`
	Lists          []BookList   `json:"lists"`
}

// GetBookSocial handles GET /api/v1/books/{id}/social (optional auth).
// One round trip for everything the book page needs to answer "what do
// Paperboxd readers think, and does anyone I follow care?".
func (h *BookHandler) GetBookSocial(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	bookID, err := h.resolveBookID(ctx, chi.URLParam(r, "id"))
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, err.Error())
		return
	}

	stats, err := h.Queries.GetBookReaderStats(ctx, bookID)
	if err != nil {
		slog.Error("get book reader stats", "error", err, "book_id", bookID)
		types.WriteInternalError(w)
		return
	}

	resp := BookSocialResponse{
		Readers: BookReaderStats{
			RatingsCount: stats.RatingsCount,
			Histogram:    [5]int32{stats.Rating1, stats.Rating2, stats.Rating3, stats.Rating4, stats.Rating5},
			Reads:        stats.Reads,
			Reading:      stats.Reading,
			TBR:          stats.Tbr,
			TBR30d:       stats.Tbr30d,
		},
		Friends: []BookFriend{},
		Lists:   []BookList{},
	}
	// A single rating is not an average worth showing; below the floor the
	// clients fall back to the publisher rating and say so.
	if stats.RatingsCount > 0 {
		rounded := float64(int(stats.Rating*10+0.5)) / 10
		resp.Readers.Rating = &rounded
	}

	if lists, err := h.Queries.GetListsContainingBook(ctx, db.GetListsContainingBookParams{
		BookID: bookID,
		Limit:  12,
	}); err != nil {
		slog.Error("get lists containing book", "error", err, "book_id", bookID)
	} else {
		for _, row := range lists {
			item := BookList{
				ID:        row.ID.String(),
				Title:     row.Title,
				Username:  row.Username,
				OwnerName: row.Username,
				BookCount: row.BookCount,
				SaveCount: row.SaveCount,
				CoverURLs: []string{},
			}
			if row.Name.Valid && row.Name.String != "" {
				item.OwnerName = row.Name.String
			}
			if row.AvatarUrl.Valid && row.AvatarUrl.String != "" {
				item.AvatarURL = &row.AvatarUrl.String
			}
			// ponytail: one cover query per list, capped at 12 lists per book.
			if covers, err := h.Queries.GetListCoverURLs(ctx, row.ID); err == nil {
				for _, c := range covers {
					if c != "" {
						item.CoverURLs = append(item.CoverURLs, c)
					}
				}
			}
			resp.Lists = append(resp.Lists, item)
		}
	}

	// Friends need a signed-in viewer; anonymous readers get the rest.
	if userIDStr, ok := reqctx.GetUserID(ctx); ok {
		if userID, err := uuid.Parse(userIDStr); err == nil {
			h.attachFriends(ctx, &resp, userID, bookID)
		}
	}

	types.WriteJSON(w, http.StatusOK, resp)
}

func (h *BookHandler) attachFriends(ctx context.Context, resp *BookSocialResponse, userID, bookID uuid.UUID) {
	rows, err := h.Queries.GetFriendsReadingBook(ctx, db.GetFriendsReadingBookParams{
		FollowerID: userID,
		BookID:     bookID,
	})
	if err != nil {
		slog.Error("get friends with book", "error", err, "book_id", bookID)
		return
	}
	for _, row := range rows {
		f := BookFriend{
			UserID:   row.UserID.String(),
			Username: row.Username,
			Name:     row.Username,
			Status:   row.Status,
		}
		if row.Name.Valid && row.Name.String != "" {
			f.Name = row.Name.String
		}
		if row.AvatarUrl.Valid && row.AvatarUrl.String != "" {
			f.AvatarURL = &row.AvatarUrl.String
		}
		if row.Rating.Valid {
			v := int(row.Rating.Int32)
			f.Rating = &v
		}
		if row.CurrentPage.Valid {
			v := int(row.CurrentPage.Int32)
			f.CurrentPage = &v
		}
		if row.StartedAt.Valid {
			s := row.StartedAt.Time.Format(time.RFC3339)
			f.StartedAt = &s
		}
		switch row.Status {
		case "read":
			resp.FriendsRead++
		case "reading":
			resp.FriendsReading++
		case "to-read":
			resp.FriendsTBR++
		}
		resp.Friends = append(resp.Friends, f)
	}
}
