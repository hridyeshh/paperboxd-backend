package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

// preflightPostgresSchema ensures migration 000008 (and prior) have been applied.
// Without this, the Go migrator fails with obscure "column does not exist" errors mid-run.
func preflightPostgresSchema(ctx context.Context, pg *pgxpool.Pool) error {
	checks := []struct {
		name  string
		query string
	}{
		{
			"users.reading_goal_year",
			`SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = 'public' AND table_name = 'users' AND column_name = 'reading_goal_year'
			)`,
		},
		{
			"likes.reason",
			`SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = 'public' AND table_name = 'likes' AND column_name = 'reason'
			)`,
		},
		{
			"bookshelf.notes",
			`SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = 'public' AND table_name = 'bookshelf' AND column_name = 'notes'
			)`,
		},
		{
			"table user_authors_read",
			`SELECT EXISTS (
				SELECT 1 FROM information_schema.tables
				WHERE table_schema = 'public' AND table_name = 'user_authors_read'
			)`,
		},
		{
			"table user_preferences_legacy",
			`SELECT EXISTS (
				SELECT 1 FROM information_schema.tables
				WHERE table_schema = 'public' AND table_name = 'user_preferences_legacy'
			)`,
		},
	}

	for _, c := range checks {
		var ok bool
		if err := pg.QueryRow(ctx, c.query).Scan(&ok); err != nil {
			return fmt.Errorf("schema check %q: %w", c.name, err)
		}
		if !ok {
			return fmt.Errorf("missing %q — apply SQL migrations first (through 000008_mongo_remaining_data)", c.name)
		}
	}

	slog.Info("postgres schema preflight OK (000008 columns/tables present)")
	return nil
}
