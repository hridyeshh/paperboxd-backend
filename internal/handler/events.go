package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/hridyesh/paperboxd-backend/internal/reqctx"
	"github.com/hridyesh/paperboxd-backend/internal/service"
	"github.com/hridyesh/paperboxd-backend/internal/types"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EventsHandler holds dependencies for event tracking endpoints.
type EventsHandler struct {
	Pool   *pgxpool.Pool
	Events *service.EventService
}

// NewEventsHandler creates an EventsHandler.
func NewEventsHandler(pool *pgxpool.Pool, events *service.EventService) *EventsHandler {
	return &EventsHandler{Pool: pool, Events: events}
}

// Track handles POST /api/v1/events.
//
// Registered with OptionalAuthenticate, not Authenticate: the acquisition
// half of the funnel (landing_viewed, signup_started) happens before an account
// exists, and requiring a bearer token made those events unrecordable. An
// unauthenticated caller must supply anon_id and may only send one of the
// event types in service.AllowsAnonymous.
func (h *EventsHandler) Track(w http.ResponseWriter, r *http.Request) {
	var userID pgtype.UUID
	if userIDStr, ok := reqctx.GetUserID(r.Context()); ok {
		if parsed, err := uuid.Parse(userIDStr); err == nil {
			userID = pgtype.UUID{Bytes: parsed, Valid: true}
		}
	}

	var body struct {
		EventType string         `json:"event_type"`
		BookID    *string        `json:"book_id"`
		AnonID    *string        `json:"anon_id"`
		SessionID *string        `json:"session_id"`
		Source    string         `json:"source"`
		Path      string         `json:"path"`
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

	// Shipped mobile builds still send the pre-000042 names; translate rather
	// than reject, and refuse anything that is neither canonical nor a known
	// alias so the table cannot grow a fourth convention.
	eventType, ok := service.NormalizeEventType(body.EventType)
	if !ok {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "unknown event_type")
		return
	}

	var anonID pgtype.UUID
	if body.AnonID != nil {
		if parsed, err := uuid.Parse(*body.AnonID); err == nil {
			anonID = pgtype.UUID{Bytes: parsed, Valid: true}
		}
	}

	if !userID.Valid {
		if !anonID.Valid {
			types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
			return
		}
		if !service.AllowsAnonymous(eventType) {
			types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
			return
		}
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

	// Parse session_id when provided; skip silently on a bad UUID (don't 400).
	var sessionID pgtype.UUID
	if body.SessionID != nil {
		if parsed, err := uuid.Parse(*body.SessionID); err == nil {
			sessionID = pgtype.UUID{Bytes: parsed, Valid: true}
		}
	}

	source := body.Source
	if source == "" {
		source = "web"
	}

	_, err := h.Pool.Exec(r.Context(),
		`INSERT INTO events (user_id, anon_id, book_id, event_type, metadata, session_id, source, path)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		userID, anonID, bookID, eventType, metadata, sessionID, source, body.Path,
	)
	if err != nil {
		slog.Error("insert event", "error", err)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusCreated, map[string]string{"status": "ok"})
}
