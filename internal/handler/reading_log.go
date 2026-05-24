package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/types"
)

// TodayProgressResponse is returned from GET /api/v1/users/:username/reading/today.
type TodayProgressResponse struct {
	TodayPages int                  `json:"today_pages"`
	TodayBooks int                  `json:"today_books"`
	LastBook   *TodayLastBook       `json:"last_book,omitempty"`
	WeekBars   []WeekBar            `json:"week_bars"`
}

type TodayLastBook struct {
	BookID      string `json:"book_id"`
	Title       string `json:"title"`
	Slug        string `json:"slug"`
	Author      string `json:"author"`
	Cover       string `json:"cover"`
	CurrentPage int    `json:"current_page"`
	TotalPages  int    `json:"total_pages"`
}

type WeekBar struct {
	Date  string `json:"date"`
	Pages int    `json:"pages"`
	Books int    `json:"books"`
}

// GetTodayProgress handles GET /api/v1/users/:username/reading/today.
func (h *UserHandler) GetTodayProgress(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	target, err := h.Queries.GetUserByUsername(r.Context(), strings.ToLower(username))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "User not found")
			return
		}
		slog.Error("get user for today progress", "error", err)
		types.WriteInternalError(w)
		return
	}

	// Today's stats
	stats, err := h.Queries.GetTodayReadingStats(r.Context(), target.ID)
	if err != nil {
		slog.Error("get today reading stats", "error", err)
		types.WriteInternalError(w)
		return
	}

	// Last logged book today
	var lastBook *TodayLastBook
	row, err := h.Queries.GetLastLoggedBookToday(r.Context(), target.ID)
	if err == nil {
		lb := &TodayLastBook{
			BookID: row.BookID.String(),
			Title:  row.Title,
			Slug:   row.Slug,
		}
		if len(row.Authors) > 0 {
			lb.Author = row.Authors[0]
		}
		if row.CoverUrl.Valid {
			lb.Cover = row.CoverUrl.String
		}
		if row.CurrentPage.Valid {
			lb.CurrentPage = int(row.CurrentPage.Int32)
		}
		if row.PageCount.Valid {
			lb.TotalPages = int(row.PageCount.Int32)
		}
		lastBook = lb
	}

	// Weekly bars — last 7 days, fill missing days with zeros
	weekRows, _ := h.Queries.GetWeeklyReadingStats(r.Context(), target.ID)
	weekMap := make(map[string]db.GetWeeklyReadingStatsRow)
	for _, wr := range weekRows {
		weekMap[wr.LogDate.Time.Format("2006-01-02")] = wr
	}
	weekBars := make([]WeekBar, 7)
	for i := 6; i >= 0; i-- {
		d := time.Now().UTC().AddDate(0, 0, -i)
		ds := d.Format("2006-01-02")
		bar := WeekBar{Date: ds}
		if wr, ok := weekMap[ds]; ok {
			bar.Pages = int(wr.Pages)
			bar.Books = int(wr.Books)
		}
		weekBars[6-i] = bar
	}

	types.WriteJSON(w, http.StatusOK, TodayProgressResponse{
		TodayPages: int(stats.TotalPages),
		TodayBooks: int(stats.TotalBooks),
		LastBook:   lastBook,
		WeekBars:   weekBars,
	})
}
