package db

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// ── LogReadingProgress ────────────────────────────────────────────────────────

const logReadingProgress = `
INSERT INTO reading_log (user_id, book_id, pages_delta)
VALUES ($1, $2, $3)
`

type LogReadingProgressParams struct {
	UserID     uuid.UUID `json:"user_id"`
	BookID     uuid.UUID `json:"book_id"`
	PagesDelta int32     `json:"pages_delta"`
}

func (q *Queries) LogReadingProgress(ctx context.Context, arg LogReadingProgressParams) error {
	_, err := q.db.Exec(ctx, logReadingProgress, arg.UserID, arg.BookID, arg.PagesDelta)
	return err
}

// ── GetTodayReadingStats ──────────────────────────────────────────────────────

const getTodayReadingStats = `
SELECT
    COALESCE(SUM(pages_delta), 0)::INT AS total_pages,
    COUNT(DISTINCT book_id)::INT       AS total_books
FROM reading_log
WHERE user_id  = $1
  AND (logged_at AT TIME ZONE 'UTC')::DATE = (NOW() AT TIME ZONE 'UTC')::DATE
`

type GetTodayReadingStatsRow struct {
	TotalPages int32 `json:"total_pages"`
	TotalBooks int32 `json:"total_books"`
}

func (q *Queries) GetTodayReadingStats(ctx context.Context, userID uuid.UUID) (GetTodayReadingStatsRow, error) {
	row := q.db.QueryRow(ctx, getTodayReadingStats, userID)
	var r GetTodayReadingStatsRow
	err := row.Scan(&r.TotalPages, &r.TotalBooks)
	return r, err
}

// ── GetLastLoggedBookToday ────────────────────────────────────────────────────

const getLastLoggedBookToday = `
SELECT
    rl.book_id,
    b.title,
    b.slug,
    b.authors,
    b.cover_url,
    b.page_count,
    bs.current_page,
    rl.logged_at
FROM reading_log rl
JOIN books     b  ON b.id  = rl.book_id
JOIN bookshelf bs ON bs.user_id = rl.user_id AND bs.book_id = rl.book_id
WHERE rl.user_id  = $1
  AND (rl.logged_at AT TIME ZONE 'UTC')::DATE = (NOW() AT TIME ZONE 'UTC')::DATE
ORDER BY rl.logged_at DESC
LIMIT 1
`

type GetLastLoggedBookTodayRow struct {
	BookID      uuid.UUID          `json:"book_id"`
	Title       string             `json:"title"`
	Slug        string             `json:"slug"`
	Authors     []string           `json:"authors"`
	CoverUrl    pgtype.Text        `json:"cover_url"`
	PageCount   pgtype.Int4        `json:"page_count"`
	CurrentPage pgtype.Int4        `json:"current_page"`
	LoggedAt    pgtype.Timestamptz `json:"logged_at"`
}

func (q *Queries) GetLastLoggedBookToday(ctx context.Context, userID uuid.UUID) (GetLastLoggedBookTodayRow, error) {
	row := q.db.QueryRow(ctx, getLastLoggedBookToday, userID)
	var r GetLastLoggedBookTodayRow
	err := row.Scan(
		&r.BookID,
		&r.Title,
		&r.Slug,
		&r.Authors,
		&r.CoverUrl,
		&r.PageCount,
		&r.CurrentPage,
		&r.LoggedAt,
	)
	return r, err
}

// ── GetWeeklyReadingStats ─────────────────────────────────────────────────────

const getWeeklyReadingStats = `
SELECT
    (logged_at AT TIME ZONE 'UTC')::DATE AS log_date,
    COALESCE(SUM(pages_delta), 0)::INT   AS pages,
    COUNT(DISTINCT book_id)::INT         AS books
FROM reading_log
WHERE user_id  = $1
  AND (logged_at AT TIME ZONE 'UTC')::DATE >= (NOW() AT TIME ZONE 'UTC')::DATE - INTERVAL '6 days'
GROUP BY (logged_at AT TIME ZONE 'UTC')::DATE
ORDER BY log_date
`

type GetWeeklyReadingStatsRow struct {
	LogDate time.Time `json:"log_date"`
	Pages   int32     `json:"pages"`
	Books   int32     `json:"books"`
}

func (q *Queries) GetWeeklyReadingStats(ctx context.Context, userID uuid.UUID) ([]GetWeeklyReadingStatsRow, error) {
	rows, err := q.db.Query(ctx, getWeeklyReadingStats, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []GetWeeklyReadingStatsRow
	for rows.Next() {
		var r GetWeeklyReadingStatsRow
		if err := rows.Scan(&r.LogDate, &r.Pages, &r.Books); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
