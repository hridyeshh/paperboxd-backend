package middleware

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/reqctx"
	"github.com/hridyesh/paperboxd-backend/internal/types"
)

// RequireProfileAccess gates every read under /users/{username} when that user
// has a private profile. Mounted once on the whole subtree so a route added
// later is covered by default rather than by remembering to gate it.
//
// Three things deliberately pass through:
//   - non-GET requests, which carry their own authorization in the handler,
//   - the bare profile GET, which returns a redacted stub so a stranger can
//     still see who they are about to send a follow request to,
//   - unknown usernames, so the handler still answers 404 rather than 403
//     (a 403 here would confirm the account exists).
//
// Requires OptionalAuthenticate ahead of it to identify the viewer.
func RequireProfileAccess(q *db.Queries) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			username := chi.URLParam(r, "username")
			if username == "" || r.Method != http.MethodGet || isProfileRoot(r.URL.Path, username) {
				next.ServeHTTP(w, r)
				return
			}

			ctx := r.Context()
			target, err := q.GetUserByUsername(ctx, strings.ToLower(username))
			if err != nil {
				next.ServeHTTP(w, r) // let the handler answer 404
				return
			}
			if target.IsPublic {
				next.ServeHTTP(w, r)
				return
			}

			viewerIDStr, ok := reqctx.GetUserID(ctx)
			if ok {
				if viewerID, err := uuid.Parse(viewerIDStr); err == nil {
					if viewerID == target.ID {
						next.ServeHTTP(w, r)
						return
					}
					following, err := q.CheckFollowing(ctx, db.CheckFollowingParams{
						FollowerID:  viewerID,
						FollowingID: target.ID,
					})
					if err != nil {
						slog.Error("check following for private profile", "error", err, "username", username)
						types.WriteInternalError(w)
						return
					}
					if following {
						next.ServeHTTP(w, r)
						return
					}
				}
			}

			types.WriteError(w, http.StatusForbidden, types.ErrCodePrivateProfile,
				"This account is private")
		})
	}
}

// isProfileRoot reports whether the path addresses the profile itself
// (/api/v1/users/alice) rather than something under it (/…/alice/diary).
//
// It anchors on the "users" segment and requires the username to be the LAST
// segment of the path. Matching the trailing segment alone would open the gate
// for any nested route whose final segment happened to equal the username
// (/users/alice/lists/alice) — an authorization check must not depend on which
// route shapes happen to exist today.
func isProfileRoot(path, username string) bool {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	for i, seg := range segments {
		if seg != "users" {
			continue
		}
		// The profile root is the segment right after "users", and nothing after it.
		if i+2 == len(segments) && strings.EqualFold(segments[i+1], username) {
			return true
		}
	}
	return false
}
