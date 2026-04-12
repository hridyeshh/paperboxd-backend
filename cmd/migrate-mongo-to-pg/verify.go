package main

import (
	"context"
	"log/slog"
)

// verify runs row-count checks between MongoDB and PostgreSQL after migration.
func verify(ctx context.Context, conn *Connections) {
	type check struct {
		label    string
		mongoCol string
		pgTable  string
	}

	checks := []check{
		{"books", "books", "books"},
		{"users", "users", "users"},
		{"newsletters", "newsletters", "newsletters"},
		{"account_deletions", "accountdeletions", "account_deletions"},
	}

	for _, c := range checks {
		mongoCount, err := conn.Mongo.Collection(c.mongoCol).EstimatedDocumentCount(ctx)
		if err != nil {
			slog.Warn("verify: mongo count failed", "collection", c.mongoCol, "error", err)
			continue
		}

		var pgCount int64
		err = conn.PG.QueryRow(ctx, "SELECT COUNT(*) FROM "+c.pgTable).Scan(&pgCount)
		if err != nil {
			slog.Warn("verify: pg count failed", "table", c.pgTable, "error", err)
			continue
		}

		status := "OK"
		if pgCount < mongoCount {
			status = "WARN (pg < mongo)"
		}
		slog.Info("verify",
			"label", c.label,
			"mongo", mongoCount,
			"pg", pgCount,
			"status", status,
		)
	}

	// Additional derived counts
	type derivedCheck struct {
		label string
		query string
	}
	derived := []derivedCheck{
		{"bookshelf_rows", "SELECT COUNT(*) FROM bookshelf"},
		{"lists_rows", "SELECT COUNT(*) FROM lists"},
		{"list_books_rows", "SELECT COUNT(*) FROM list_books"},
		{"diary_entries", "SELECT COUNT(*) FROM diary_entries"},
		{"diary_entry_likes", "SELECT COUNT(*) FROM diary_entry_likes"},
		{"follows", "SELECT COUNT(*) FROM follows"},
		{"likes", "SELECT COUNT(*) FROM likes"},
		{"favorites", "SELECT COUNT(*) FROM favorites"},
		{"activities", "SELECT COUNT(*) FROM activities"},
		{"user_authors_read", "SELECT COUNT(*) FROM user_authors_read"},
		{"user_preferences_legacy", "SELECT COUNT(*) FROM user_preferences_legacy"},
	}

	for _, d := range derived {
		var n int64
		_ = conn.PG.QueryRow(ctx, d.query).Scan(&n)
		slog.Info("pg count", "table", d.label, "rows", n)
	}
}
