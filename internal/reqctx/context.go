// Package reqctx provides shared request context keys and helpers.
// It exists as a separate package to avoid import cycles between
// the auth and middleware packages.
package reqctx

import "context"

type contextKey string

// UserIDKey is the context key for the authenticated user's ID.
const UserIDKey contextKey = "user_id"

// GetUserID extracts the authenticated user ID from the context.
// Returns ("", false) if not present.
func GetUserID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(UserIDKey).(string)
	return id, ok && id != ""
}

// WithUserID returns a new context with the user ID set.
func WithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, UserIDKey, userID)
}
