package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/hridyesh/paperboxd-backend/internal/reqctx"
	"github.com/hridyesh/paperboxd-backend/internal/types"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EventsHandler holds dependencies for event tracking endpoints.
type EventsHandler struct {
	Pool *pgxpool.Pool
}

// NewEventsHandler creates an EventsHandler.
func NewEventsHandler(pool *pgxpool.Pool) *EventsHandler {
	return &EventsHandler{Pool: pool}
}

// Track handles POST /api/v1/events
func (h *EventsHandler) Track(w http.ResponseWriter, r *http.Request) {
	userIDStr, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}

	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}

	var body struct {
		EventType string         `json:"event_type"`
		BookID    *string        `json:"book_id"`
		Metadata  map[string]any `json:"metadata"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Invalid request body")
		return
	}

	if body.EventType == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "event_type is required")
		return
	}

	var bookID pgtype.UUID
	if body.BookID != nil {
		parsed, err := uuid.Parse(*body.BookID)
		if err != nil {
			types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "invalid book_id")
			return
		}
		bookID = pgtype.UUID{Bytes: parsed, Valid: true}
	}

	var metadata []byte
	if body.Metadata != nil {
		metadata, _ = json.Marshal(body.Metadata)
	}

	_, err = h.Pool.Exec(r.Context(),
		`INSERT INTO events (user_id, book_id, event_type, metadata)
		 VALUES ($1, $2, $3, $4)`,
		userID, bookID, body.EventType, metadata,
	)
	if err != nil {
		slog.Error("insert event", "error", err)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusCreated, map[string]string{"status": "ok"})
}
