package handler

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// activeStreak must not report a run the reader has already broken:
// users.current_streak is only ever advanced, never decayed.
func TestActiveStreak(t *testing.T) {
	day := func(d int) pgtype.Date {
		return pgtype.Date{
			Time:  time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, d),
			Valid: true,
		}
	}
	n := func(v int32) pgtype.Int4 { return pgtype.Int4{Int32: v, Valid: true} }

	cases := []struct {
		name   string
		streak pgtype.Int4
		last   pgtype.Date
		want   int
	}{
		{"active today", n(7), day(0), 7},
		{"active yesterday still counts", n(7), day(-1), 7},
		{"two days ago is broken", n(7), day(-2), 0},
		{"long gone is broken", n(40), day(-30), 0},
		{"never active", n(3), pgtype.Date{}, 0},
		{"no streak stored", pgtype.Int4{}, day(0), 0},
		{"zero streak", n(0), day(0), 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := activeStreak(c.streak, c.last); got != c.want {
				t.Errorf("activeStreak() = %d, want %d", got, c.want)
			}
		})
	}
}
