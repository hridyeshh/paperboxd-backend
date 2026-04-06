// cmd/dbutil is a temporary utility for cleanup and verification during migration.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5"
)

func main() {
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	pgURL := os.Getenv("POSTGRES_URL")
	if pgURL == "" {
		log.Fatal("POSTGRES_URL not set")
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, pgURL)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	switch cmd {
	case "cleanup":
		cleanup(ctx, conn)
	case "verify":
		verify(ctx, conn)
	case "users":
		listUsers(ctx, conn)
	default:
		log.Fatalf("unknown command: %q (use 'cleanup', 'verify', or 'users')", cmd)
	}
}

func cleanup(ctx context.Context, conn *pgx.Conn) {
	stmts := []string{
		"DELETE FROM activities",
		"DELETE FROM diary_entry_likes",
		"DELETE FROM diary_entries",
		"DELETE FROM list_books",
		"DELETE FROM list_access",
		"DELETE FROM saved_lists",
		"DELETE FROM lists",
		"DELETE FROM favorites",
		"DELETE FROM bookshelf",
		"DELETE FROM likes",
		"DELETE FROM follows",
		"DELETE FROM refresh_tokens",
		"DELETE FROM newsletters",
		"DELETE FROM account_deletions",
		"DELETE FROM books",
		"DELETE FROM users",
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		log.Fatalf("begin: %v", err)
	}

	for _, stmt := range stmts {
		tag, err := tx.Exec(ctx, stmt)
		if err != nil {
			_ = tx.Rollback(ctx)
			log.Fatalf("exec %q: %v", stmt, err)
		}
		fmt.Printf("  %-40s  deleted %d rows\n", stmt, tag.RowsAffected())
	}

	if err := tx.Commit(ctx); err != nil {
		log.Fatalf("commit: %v", err)
	}
	fmt.Println("Cleanup committed.")
}

func listUsers(ctx context.Context, conn *pgx.Conn) {
	rows, err := conn.Query(ctx, `SELECT email, username, LEFT(password_hash, 10) as hash_prefix FROM users WHERE password_hash IS NOT NULL AND password_hash != '' ORDER BY created_at LIMIT 5`)
	if err != nil {
		log.Fatalf("query: %v", err)
	}
	defer rows.Close()
	fmt.Printf("%-40s  %-15s  %s\n", "email", "username", "hash_prefix")
	fmt.Printf("%-40s  %-15s  %s\n", "-----", "--------", "-----------")
	for rows.Next() {
		var email, username, hashPrefix string
		rows.Scan(&email, &username, &hashPrefix)
		fmt.Printf("%-40s  %-15s  %s\n", email, username, hashPrefix)
	}
}

func verify(ctx context.Context, conn *pgx.Conn) {
	tables := []string{
		"users", "books", "bookshelf", "lists", "list_books",
		"diary_entries", "diary_entry_likes", "activities",
		"follows", "likes", "favorites", "newsletters", "account_deletions",
	}
	fmt.Printf("%-25s  %s\n", "table", "count")
	fmt.Printf("%-25s  %s\n", "-----", "-----")
	for _, t := range tables {
		var n int64
		_ = conn.QueryRow(ctx, "SELECT COUNT(*) FROM "+t).Scan(&n)
		fmt.Printf("%-25s  %d\n", t, n)
	}
}
