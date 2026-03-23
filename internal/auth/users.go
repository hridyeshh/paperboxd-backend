package auth

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/hridyesh/paperboxd-backend/internal/reqctx"
	"github.com/hridyesh/paperboxd-backend/internal/types"
	"github.com/jackc/pgx/v5"
)

// Me handles GET /api/v1/users/me
func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	userIDStr, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}

	uid, err := uuid.Parse(userIDStr)
	if err != nil {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}

	user, err := h.queries.GetUserByID(r.Context(), uid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "User not found")
			return
		}
		slog.Error("get user by id", "error", err, "user_id", userIDStr)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusOK, toUserResponse(user))
}
