package handler

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/reqctx"
	"github.com/hridyesh/paperboxd-backend/internal/types"
)

// pagesPerHour is the reading speed used to turn logged pages into a reading
// time. We do not record session durations, so the hours in a Wrapped are an
// estimate and the JSON says so in the field names.
const pagesPerHour = 40.0

// stallDays is how long a book must sit untouched before the month ends to
// count as abandoned rather than merely slow.
const stallDays = 7

// WrappedHandler serves GET /api/v1/users/me/wrapped.
type WrappedHandler struct {
	Queries *db.Queries
}

func NewWrappedHandler(q *db.Queries) *WrappedHandler {
	return &WrappedHandler{Queries: q}
}

// ── Response ───────────────────────────────────────────────────────────────

type WrappedResponse struct {
	// HasData is false when the reader logged nothing that month; every other
	// field is then zero-valued and the client shows an empty state instead of
	// a story about nothing.
	HasData    bool                  `json:"has_data"`
	Month      string                `json:"month"`
	MonthShort string                `json:"month_short"`
	Year       string                `json:"year"`
	NextMonth  string                `json:"next_month"`
	Reader     WrappedReader         `json:"reader"`
	Totals     WrappedTotals         `json:"totals"`
	Books      []WrappedBook         `json:"books"`
	Authors    []WrappedAuthor       `json:"authors"`
	Genres     []WrappedGenre        `json:"genres"`
	Rhythm     WrappedRhythm         `json:"rhythm"`
	Streak     WrappedStreak         `json:"streak"`
	TopRated   *WrappedTopRated      `json:"top_rated"`
	Abandoned  *WrappedAbandonedBook `json:"abandoned"`
	Rank       WrappedRank           `json:"rank"`
	Archetype  WrappedArchetype      `json:"archetype"`
	Dare       WrappedDare           `json:"dare"`
}

type WrappedReader struct {
	Name   string `json:"name"`
	Handle string `json:"handle"`
	First  string `json:"first"`
}

type WrappedTotals struct {
	Books int `json:"books"`
	Pages int `json:"pages"`
	// EstimatedHours/Minutes come from pages, not from a timer — see pagesPerHour.
	EstimatedHours   int    `json:"estimated_hours"`
	EstimatedMinutes int    `json:"estimated_minutes"`
	Sessions         int    `json:"sessions"`
	ActiveDays       int    `json:"active_days"`
	BiggestDayPages  int    `json:"biggest_day_pages"`
	BiggestDay       string `json:"biggest_day"`
}

type WrappedBook struct {
	Title  string  `json:"title"`
	Author string  `json:"author"`
	Cover  string  `json:"cover"`
	Pages  int     `json:"pages"`
	Days   int     `json:"days"`
	Rating float64 `json:"rating"`
}

type WrappedAuthor struct {
	Name  string `json:"name"`
	Books int    `json:"books"`
	Pages int    `json:"pages"`
	Note  string `json:"note,omitempty"`
}

type WrappedGenre struct {
	Name string `json:"name"`
	Pct  int    `json:"pct"`
}

type WrappedRhythm struct {
	Label            string `json:"label"`
	Peak             string `json:"peak"`
	PctAfterMidnight int    `json:"pct_after_midnight"`
	Line             string `json:"line"`
	// Hours is always 24 slots, 0–100, normalised against the busiest hour.
	Hours []int `json:"hours"`
}

type WrappedStreak struct {
	Days        int    `json:"days"`
	Start       string `json:"start"`
	End         string `json:"end"`
	Broke       string `json:"broke"`
	LongestEver int    `json:"longest_ever"`
	// Calendar has one entry per day of the month, in pages.
	Calendar    []int `json:"calendar"`
	StreakStart int   `json:"streak_start"`
	StreakEnd   int   `json:"streak_end"`
	BrokeIndex  int   `json:"broke_index"`
}

type WrappedTopRated struct {
	Title  string  `json:"title"`
	Author string  `json:"author"`
	Cover  string  `json:"cover"`
	Rating float64 `json:"rating"`
	Date   string  `json:"date"`
	Review string  `json:"review"`
}

type WrappedAbandonedBook struct {
	Title      string `json:"title"`
	Author     string `json:"author"`
	Page       int    `json:"page"`
	Of         int    `json:"of"`
	Started    string `json:"started"`
	LastOpened string `json:"last_opened"`
	Roast      string `json:"roast"`
}

type WrappedRank struct {
	Percentile int    `json:"percentile"`
	Label      string `json:"label"`
	Readers    int    `json:"readers"`
	Beat       int    `json:"beat"`
	Line       string `json:"line"`
}

type WrappedArchetype struct {
	Name       string   `json:"name"`
	Kicker     string   `json:"kicker"`
	Definition string   `json:"definition"`
	Traits     []string `json:"traits"`
	// StatLabel/StatValue back the badge on the archetype chapter. Unlike a
	// "rarity" percentage we cannot compute, this is a real number from the
	// reader's own month.
	StatLabel string `json:"stat_label"`
	StatValue string `json:"stat_value"`
	Pairs     string `json:"pairs"`
}

type WrappedDare struct {
	Title  string `json:"title"`
	Body   string `json:"body"`
	Target string `json:"target"`
	Tag    string `json:"tag"`
}

// ── Handler ────────────────────────────────────────────────────────────────

// Get handles GET /api/v1/users/me/wrapped?month=YYYY-MM&tz=Area/City.
//
// The month window and every day/hour bucket are computed in tz, so the story
// matches the reader's own clock rather than the server's.
func (h *WrappedHandler) Get(w http.ResponseWriter, r *http.Request) {
	userIDStr, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Authentication required")
		return
	}
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Invalid user")
		return
	}

	// An unknown or missing zone falls back to UTC rather than failing: a
	// slightly-off Wrapped beats no Wrapped.
	loc := time.UTC
	if tz := r.URL.Query().Get("tz"); tz != "" {
		if parsed, err := time.LoadLocation(tz); err == nil {
			loc = parsed
		}
	}

	monthStart, err := parseMonth(r.URL.Query().Get("month"), loc)
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "month must be YYYY-MM")
		return
	}
	monthEnd := monthStart.AddDate(0, 1, 0)

	user, err := h.Queries.GetUserByID(r.Context(), userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "User not found")
			return
		}
		slog.Error("wrapped: get user", "error", err)
		types.WriteInternalError(w)
		return
	}

	ctx := r.Context()
	tzName := loc.String()
	start := tsz(monthStart)
	end := tsz(monthEnd)

	totals, err := h.Queries.WrappedTotals(ctx, db.WrappedTotalsParams{
		Tz: tzName, UserID: userID, MonthStart: start, MonthEnd: end,
	})
	if err != nil {
		slog.Error("wrapped: totals", "error", err)
		types.WriteInternalError(w)
		return
	}

	resp := blankWrapped(monthStart, readerOf(user))

	// Nothing logged: everything downstream would be a story about zero.
	if totals.Pages == 0 && totals.Sessions == 0 {
		types.WriteJSON(w, http.StatusOK, resp)
		return
	}
	resp.HasData = true

	booksFinished, err := h.Queries.WrappedBooksFinished(ctx, db.WrappedBooksFinishedParams{
		UserID: userID, MonthStart: start, MonthEnd: end,
	})
	if err != nil {
		slog.Error("wrapped: books finished", "error", err)
		types.WriteInternalError(w)
		return
	}

	hours := int(math.Floor(float64(totals.Pages) / pagesPerHour))
	minutes := int(math.Round((float64(totals.Pages)/pagesPerHour - float64(hours)) * 60))
	if minutes == 60 {
		hours, minutes = hours+1, 0
	}
	resp.Totals = WrappedTotals{
		Books:            int(booksFinished),
		Pages:            int(totals.Pages),
		EstimatedHours:   hours,
		EstimatedMinutes: minutes,
		Sessions:         int(totals.Sessions),
		ActiveDays:       int(totals.ActiveDays),
		BiggestDayPages:  int(totals.BiggestDayPages),
	}
	if totals.BiggestDay.Valid {
		resp.Totals.BiggestDay = totals.BiggestDay.Time.Format("Jan 2")
	}

	// ── Books, authors, genres ──────────────────────────────────────────────
	if rows, err := h.Queries.WrappedTopBooks(ctx, db.WrappedTopBooksParams{
		Tz: tzName, UserID: userID, MonthStart: start, MonthEnd: end, RowLimit: 5,
	}); err != nil {
		slog.Error("wrapped: top books", "error", err)
	} else {
		for _, b := range rows {
			resp.Books = append(resp.Books, WrappedBook{
				Title:  b.Title,
				Author: firstAuthor(b.Authors),
				Cover:  textOr(b.CoverUrl, ""),
				Pages:  int(b.Pages),
				Days:   int(b.Days),
				Rating: ratingOf(b.Rating),
			})
		}
	}

	if rows, err := h.Queries.WrappedTopAuthors(ctx, db.WrappedTopAuthorsParams{
		UserID: userID, MonthStart: start, MonthEnd: end, RowLimit: 5,
	}); err != nil {
		slog.Error("wrapped: top authors", "error", err)
	} else {
		for _, a := range rows {
			author := WrappedAuthor{Name: a.Name, Books: int(a.Books), Pages: int(a.Pages)}
			if a.Books > 1 {
				author.Note = fmt.Sprintf("%d books this month.", a.Books)
			}
			resp.Authors = append(resp.Authors, author)
		}
	}

	if rows, err := h.Queries.WrappedGenres(ctx, db.WrappedGenresParams{
		UserID: userID, MonthStart: start, MonthEnd: end, RowLimit: 5,
	}); err != nil {
		slog.Error("wrapped: genres", "error", err)
	} else {
		var genreTotal int
		for _, g := range rows {
			genreTotal += int(g.Pages)
		}
		for _, g := range rows {
			pct := 0
			if genreTotal > 0 {
				pct = int(math.Round(float64(g.Pages) / float64(genreTotal) * 100))
			}
			resp.Genres = append(resp.Genres, WrappedGenre{Name: g.Name, Pct: pct})
		}
	}

	// ── Rhythm ──────────────────────────────────────────────────────────────
	if rows, err := h.Queries.WrappedHourHistogram(ctx, db.WrappedHourHistogramParams{
		Tz: tzName, UserID: userID, MonthStart: start, MonthEnd: end,
	}); err != nil {
		slog.Error("wrapped: hour histogram", "error", err)
	} else {
		raw := make([]int, 24)
		for _, row := range rows {
			if row.Hour >= 0 && row.Hour < 24 {
				raw[row.Hour] = int(row.Pages)
			}
		}
		resp.Rhythm = rhythmOf(raw)
	}

	// ── Streak ──────────────────────────────────────────────────────────────
	daysInMonth := monthEnd.AddDate(0, 0, -1).Day()
	calendar := make([]int, daysInMonth)
	if rows, err := h.Queries.WrappedDailyPages(ctx, db.WrappedDailyPagesParams{
		Tz: tzName, UserID: userID, MonthStart: start, MonthEnd: end,
	}); err != nil {
		slog.Error("wrapped: daily pages", "error", err)
	} else {
		for _, row := range rows {
			if !row.LogDate.Valid {
				continue
			}
			if d := row.LogDate.Time.Day() - 1; d >= 0 && d < daysInMonth {
				calendar[d] = int(row.Pages)
			}
		}
	}
	resp.Streak = streakOf(calendar, monthStart, intOr(user.LongestStreak, 0))

	// ── The month's best book ───────────────────────────────────────────────
	if row, err := h.Queries.WrappedTopRated(ctx, db.WrappedTopRatedParams{
		UserID: userID, MonthStart: start, MonthEnd: end,
	}); err == nil {
		top := &WrappedTopRated{
			Title:  row.Title,
			Author: firstAuthor(row.Authors),
			Cover:  textOr(row.CoverUrl, ""),
			Rating: ratingOf(row.Rating),
			Review: textOr(row.Review, ""),
		}
		if row.FinishedAt.Valid {
			top.Date = row.FinishedAt.Time.In(loc).Format("Jan 2")
		}
		resp.TopRated = top
	} else if !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("wrapped: top rated", "error", err)
	}

	// ── The one left unfinished ─────────────────────────────────────────────
	stallBefore := monthEnd.AddDate(0, 0, -stallDays)
	if row, err := h.Queries.WrappedAbandoned(ctx, db.WrappedAbandonedParams{
		UserID: userID, MonthEnd: end, StallBefore: tsz(stallBefore),
	}); err == nil {
		page := intOr(row.CurrentPage, 0)
		of := intOr(row.PageCount, 0)
		ab := &WrappedAbandonedBook{
			Title:  row.Title,
			Author: firstAuthor(row.Authors),
			Page:   page,
			Of:     of,
			Roast:  roastOf(page, of),
		}
		if row.StartedAt.Valid {
			ab.Started = row.StartedAt.Time.In(loc).Format("Jan 2")
		}
		if row.LastLogged.Valid {
			ab.LastOpened = row.LastLogged.Time.In(loc).Format("Jan 2")
		}
		resp.Abandoned = ab
	} else if !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("wrapped: abandoned", "error", err)
	}

	// ── Rank ────────────────────────────────────────────────────────────────
	if row, err := h.Queries.WrappedRank(ctx, db.WrappedRankParams{
		MonthStart: start, MonthEnd: end, UserID: userID,
	}); err != nil {
		slog.Error("wrapped: rank", "error", err)
	} else {
		resp.Rank = rankOf(int(row.Readers), int(row.Beaten))
	}

	resp.Archetype = archetypeOf(resp)
	resp.Dare = dareOf(resp)

	types.WriteJSON(w, http.StatusOK, resp)
}

// ── Derivations ────────────────────────────────────────────────────────────

// blankWrapped is the shape every response starts from. Every slice starts
// empty rather than nil: a nil slice marshals to `null`, and the mobile
// clients decode these as non-optional arrays, so one null fails the whole
// story rather than one chapter.
func blankWrapped(monthStart time.Time, reader WrappedReader) WrappedResponse {
	return WrappedResponse{
		Month:      monthStart.Format("January"),
		MonthShort: strings.ToUpper(monthStart.Format("Jan")),
		Year:       monthStart.Format("2006"),
		NextMonth:  monthStart.AddDate(0, 1, 0).Format("January"),
		Reader:     reader,
		Books:      []WrappedBook{},
		Authors:    []WrappedAuthor{},
		Genres:     []WrappedGenre{},
		Rhythm:     WrappedRhythm{Hours: make([]int, 24)},
		Streak:     WrappedStreak{Calendar: []int{}, StreakStart: -1, StreakEnd: -1, BrokeIndex: -1},
		Archetype:  WrappedArchetype{Traits: []string{}},
	}
}

// parseMonth reads a YYYY-MM parameter, defaulting to the current month in loc.
func parseMonth(raw string, loc *time.Location) (time.Time, error) {
	if raw == "" {
		now := time.Now().In(loc)
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc), nil
	}
	t, err := time.ParseInLocation("2006-01", raw, loc)
	if err != nil {
		return time.Time{}, err
	}
	return t, nil
}

func readerOf(u db.User) WrappedReader {
	name := textOr(u.Name, u.Username)
	first := name
	if i := strings.IndexByte(name, ' '); i > 0 {
		first = name[:i]
	}
	return WrappedReader{Name: name, Handle: "@" + u.Username, First: first}
}

// rhythmOf turns a 24-slot page histogram into the reading-rhythm chapter:
// a normalised shape, the peak window, and the label that names the reader.
func rhythmOf(raw []int) WrappedRhythm {
	total, peakHour, peakPages := 0, 0, 0
	for h, p := range raw {
		total += p
		if p > peakPages {
			peakHour, peakPages = h, p
		}
	}

	shape := make([]int, 24)
	for h, p := range raw {
		if peakPages > 0 {
			shape[h] = int(math.Round(float64(p) / float64(peakPages) * 100))
		}
	}

	afterMidnight := 0
	if total > 0 {
		var late int
		for h := 0; h < 5; h++ {
			late += raw[h]
		}
		late += raw[23]
		afterMidnight = int(math.Round(float64(late) / float64(total) * 100))
	}

	label, line := "The Steady Reader", "You read in the gaps the day left you."
	switch {
	case peakHour >= 22 || peakHour <= 3:
		label, line = "Night Owl", "You read when the rest of the house had given up."
	case peakHour >= 4 && peakHour <= 8:
		label, line = "Early Bird", "You got to the page before the day could take it."
	case peakHour >= 9 && peakHour <= 11:
		label, line = "Morning Reader", "Your best pages happened before lunch."
	case peakHour >= 12 && peakHour <= 15:
		label, line = "Afternoon Reader", "You read through the flattest part of the day."
	case peakHour >= 16 && peakHour <= 18:
		label, line = "Commuter", "You read on the way back from everything."
	case peakHour >= 19 && peakHour <= 21:
		label, line = "Evening Reader", "You read once the day had stopped asking for things."
	}

	return WrappedRhythm{
		Label:            label,
		Peak:             hourWindow(peakHour),
		PctAfterMidnight: afterMidnight,
		Line:             line,
		Hours:            shape,
	}
}

func hourWindow(h int) string {
	return fmt.Sprintf("%s — %s", clock(h), clock((h+2)%24))
}

func clock(h int) string {
	switch {
	case h == 0:
		return "12AM"
	case h < 12:
		return fmt.Sprintf("%dAM", h)
	case h == 12:
		return "12PM"
	default:
		return fmt.Sprintf("%dPM", h-12)
	}
}

// streakOf finds the month's longest unbroken run of reading days and the day
// it ended on.
func streakOf(calendar []int, monthStart time.Time, longestEver int) WrappedStreak {
	s := WrappedStreak{Calendar: calendar, LongestEver: longestEver, StreakStart: -1, StreakEnd: -1, BrokeIndex: -1}

	bestStart, bestLen, runStart, runLen := -1, 0, -1, 0
	for i, pages := range calendar {
		if pages > 0 {
			if runLen == 0 {
				runStart = i
			}
			runLen++
			if runLen > bestLen {
				bestStart, bestLen = runStart, runLen
			}
			continue
		}
		runLen = 0
	}
	if bestLen == 0 {
		return s
	}

	s.Days = bestLen
	s.StreakStart = bestStart
	s.StreakEnd = bestStart + bestLen - 1
	s.Start = monthStart.AddDate(0, 0, bestStart).Format("Jan 2")
	s.End = monthStart.AddDate(0, 0, s.StreakEnd).Format("Jan 2")
	if broke := s.StreakEnd + 1; broke < len(calendar) {
		s.BrokeIndex = broke
		s.Broke = monthStart.AddDate(0, 0, broke).Format("Jan 2")
	}
	if longestEver < bestLen {
		s.LongestEver = bestLen
	}
	return s
}

func rankOf(readers, beaten int) WrappedRank {
	if readers <= 1 {
		return WrappedRank{
			Percentile: 100, Label: "Top 100%", Readers: readers, Beat: 0,
			Line: "You are the only person we caught reading this month.",
		}
	}
	beatPct := int(math.Round(float64(beaten) / float64(readers-1) * 100))
	percentile := 100 - beatPct
	if percentile < 1 {
		percentile = 1
	}
	return WrappedRank{
		Percentile: percentile,
		Label:      fmt.Sprintf("Top %d%%", percentile),
		Readers:    readers,
		Beat:       beatPct,
		Line:       fmt.Sprintf("You out-read %d out of every 100 people on PaperBoxd this month.", beatPct),
	}
}

func roastOf(page, of int) string {
	if of > page && page > 0 {
		return fmt.Sprintf("You left it on page %d. It is still waiting, and it has %d pages of things to tell you.", page, of-page)
	}
	return "You started it, and then you did not finish it. It noticed."
}

// archetypeOf names the reader from the month they actually had. The rules are
// checked in order, so the most distinctive signal wins.
func archetypeOf(w WrappedResponse) WrappedArchetype {
	nights := w.Rhythm.PctAfterMidnight
	finished := w.Totals.Books
	topGenre := 0
	if len(w.Genres) > 0 {
		topGenre = w.Genres[0].Pct
	}
	loyal := 0
	if len(w.Authors) > 0 {
		loyal = w.Authors[0].Books
	}
	bigDayShare := 0
	if w.Totals.Pages > 0 {
		bigDayShare = int(math.Round(float64(w.Totals.BiggestDayPages) / float64(w.Totals.Pages) * 100))
	}

	a := WrappedArchetype{Kicker: "Your " + w.Month + " type", Pairs: "The Archivist"}
	switch {
	case w.Rhythm.Label == "Night Owl" && nights >= 30:
		a.Name = "The Midnight Romantic"
		a.Definition = "Reads for feeling, not for finishing. Will stay up for one more chapter and then three more."
		a.Traits = []string{"Nocturnal", "One more chapter", "Slow, then all at once"}
		a.StatLabel = "AFTER DARK"
		a.StatValue = fmt.Sprintf("%d%%", nights)
		a.Pairs = "The Early Riser"
	case bigDayShare >= 35:
		a.Name = "The Marathoner"
		a.Definition = "Does not read daily so much as disappear into a book for an entire day and come back changed."
		a.Traits = []string{"All or nothing", "One-sitting finisher", "Immersive"}
		a.StatLabel = "BIGGEST DAY"
		a.StatValue = fmt.Sprintf("%d%%", bigDayShare)
		a.Pairs = "The Steady Hand"
	case loyal >= 3:
		a.Name = "The Loyalist"
		a.Definition = "Finds an author and reads them to the end of the shelf before considering anyone else."
		a.Traits = []string{"Loyal to one author", "Deep, not wide", "Completist"}
		a.StatLabel = "ONE AUTHOR"
		a.StatValue = fmt.Sprintf("%d books", loyal)
		a.Pairs = "The Wanderer"
	case topGenre >= 55:
		a.Name = "The Specialist"
		a.Definition = "Knows exactly what they like and sees no reason to pretend otherwise."
		a.Traits = []string{"Single-lane", "Sure of taste", "Repeat customer"}
		a.StatLabel = w.Genres[0].Name
		a.StatValue = fmt.Sprintf("%d%%", topGenre)
		a.Pairs = "The Wanderer"
	case w.Totals.ActiveDays >= 20:
		a.Name = "The Steady Hand"
		a.Definition = "Reads a little almost every day, which is how the big numbers happen without anyone noticing."
		a.Traits = []string{"Daily", "Unhurried", "Quietly relentless"}
		a.StatLabel = "DAYS READ"
		a.StatValue = fmt.Sprintf("%d", w.Totals.ActiveDays)
		a.Pairs = "The Marathoner"
	case finished == 0 && w.Totals.Pages > 0:
		a.Name = "The Wanderer"
		a.Definition = "Starts more than they finish, and gets more out of the starting than most people get out of finishing."
		a.Traits = []string{"Curious", "Many bookmarks", "Commitment-optional"}
		a.StatLabel = "PAGES"
		a.StatValue = fmt.Sprintf("%d", w.Totals.Pages)
		a.Pairs = "The Loyalist"
	default:
		a.Name = "The Reader"
		a.Definition = "No single tell, no fixed lane — just a month of reading that went the way months go."
		a.Traits = []string{"Balanced", "Unpredictable", "Hard to shelve"}
		a.StatLabel = "BOOKS"
		a.StatValue = fmt.Sprintf("%d", finished)
	}
	return a
}

// dareOf sets next month's challenge from the shape of this one.
func dareOf(w WrappedResponse) WrappedDare {
	tag := "THE " + strings.ToUpper(w.NextMonth) + " DARE"
	switch {
	case w.Abandoned != nil:
		return WrappedDare{
			Title:  "Finish what you started.",
			Body:   fmt.Sprintf("%s is sitting on page %d with %d pages left. %s dares you to close it properly.", w.Abandoned.Title, w.Abandoned.Page, maxInt(w.Abandoned.Of-w.Abandoned.Page, 0), w.NextMonth),
			Target: "1 book",
			Tag:    tag,
		}
	case len(w.Genres) > 0 && w.Genres[0].Pct >= 55:
		return WrappedDare{
			Title:  "Read outside your lane.",
			Body:   fmt.Sprintf("%d%% of your month was %s. %s dares you to finish one book that has nothing to do with it.", w.Genres[0].Pct, strings.ToLower(w.Genres[0].Name), w.NextMonth),
			Target: "1 book",
			Tag:    tag,
		}
	case w.Totals.ActiveDays < 10:
		return WrappedDare{
			Title:  "Read on more days.",
			Body:   fmt.Sprintf("You read on %d days this month, in bigger bites than most people manage. %s dares you to double the days, not the pages.", w.Totals.ActiveDays, w.NextMonth),
			Target: fmt.Sprintf("%d days", minInt(w.Totals.ActiveDays*2, 28)),
			Tag:    tag,
		}
	default:
		return WrappedDare{
			Title:  "Beat this month.",
			Body:   fmt.Sprintf("%d pages is the number to beat. %s only has to be one page better.", w.Totals.Pages, w.NextMonth),
			Target: fmt.Sprintf("%d pages", w.Totals.Pages+1),
			Tag:    tag,
		}
	}
}

// ── Small helpers ──────────────────────────────────────────────────────────

func tsz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func firstAuthor(authors []string) string {
	if len(authors) == 0 {
		return ""
	}
	return authors[0]
}

func textOr(t pgtype.Text, fallback string) string {
	if t.Valid && t.String != "" {
		return t.String
	}
	return fallback
}

func intOr(v pgtype.Int4, fallback int) int {
	if v.Valid {
		return int(v.Int32)
	}
	return fallback
}

// Ratings are stored as whole stars; the clients render halves.
func ratingOf(v pgtype.Int4) float64 {
	if !v.Valid {
		return 0
	}
	return float64(v.Int32)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
