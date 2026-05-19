package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/hridyesh/paperboxd-backend/internal/types"
	"github.com/jackc/pgx/v5/pgxpool"
)

var emailRegex = regexp.MustCompile(`^\S+@\S+\.\S+$`)

// NewsletterHandler holds dependencies for newsletter endpoints.
type NewsletterHandler struct {
	Pool *pgxpool.Pool
}

// NewNewsletterHandler creates a NewsletterHandler.
func NewNewsletterHandler(pool *pgxpool.Pool) *NewsletterHandler {
	return &NewsletterHandler{Pool: pool}
}

// Subscribe handles POST /api/v1/newsletter/subscribe
func (h *NewsletterHandler) Subscribe(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Invalid request body")
		return
	}

	email := strings.ToLower(strings.TrimSpace(body.Email))
	if email == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "email is required")
		return
	}
	if !emailRegex.MatchString(email) {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "invalid email format")
		return
	}

	_, err := h.Pool.Exec(r.Context(),
		`INSERT INTO newsletter_subscriptions (email) VALUES ($1) ON CONFLICT (email) DO NOTHING`,
		email,
	)
	if err != nil {
		slog.Error("newsletter subscribe", "error", err)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusOK, map[string]string{"message": "Subscribed successfully"})
}
