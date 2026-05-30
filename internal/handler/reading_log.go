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

// LastLoggedBookResponse is returned from GET /api/v1/users/:username/reading/last.
// last_book is null when the user has never logged reading progress.
type LastLoggedBookResponse struct {
	LastBook *TodayLastBook `json:"last_book"`
}

// GetLastLoggedBook handles GET /api/v1/users/:username/reading/last.
// Unlike /reading/today, this returns the most recently logged book regardless
// of date, so the profile card always reflects the real last book the user read.
func (h *UserHandler) GetLastLoggedBook(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	target, err := h.Queries.GetUserByUsername(r.Context(), strings.ToLower(username))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "User not found")
			return
		}
		slog.Error("get user for last logged book", "error", err)
		types.WriteInternalError(w)
		return
	}

	var lastBook *TodayLastBook
	row, err := h.Queries.GetLastLoggedBook(r.Context(), target.ID)
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
	} else if !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("get last logged book", "error", err)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusOK, LastLoggedBookResponse{LastBook: lastBook})
}

// StreakResponse is returned from GET /api/v1/users/:username/streak.
type StreakResponse struct {
	Streak int `json:"streak"`
}

// GetStreak handles GET /api/v1/users/:username/streak.
// The streak is computed server-side from reading_log: a "streak day" is any UTC
// calendar day with >=1 page logged; the current streak is the run of consecutive
// UTC days ending today or yesterday. Works for both own and other users.
func (h *UserHandler) GetStreak(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	target, err := h.Queries.GetUserByUsername(r.Context(), strings.ToLower(username))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "User not found")
			return
		}
		slog.Error("get user for streak", "error", err)
		types.WriteInternalError(w)
		return
	}

	streak, err := h.Queries.GetCurrentStreak(r.Context(), target.ID)
	if err != nil {
		slog.Error("get current streak", "error", err)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusOK, StreakResponse{Streak: int(streak)})
}
