package service

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EventService writes analytics events to the events table. It is intentionally
// standalone (no recommendation_service dependency) so any handler can emit.
type EventService struct {
	pool *pgxpool.Pool
}

func NewEventService(pool *pgxpool.Pool) *EventService {
	return &EventService{pool: pool}
}

// EmitParams describes a single analytics event.
//
// Exactly one actor is required. UserID is the normal case; AnonID carries a
// client-generated id for the pre-signup events listed in AllowsAnonymous, so
// the acquisition funnel can be measured without inventing a user row. A
// zero UserID with a nil AnonID is dropped rather than written -- 000042's
// events_actor_present constraint would reject it anyway, and losing an
// analytics row is never worth an error log on a hot path.
type EmitParams struct {
	UserID    uuid.UUID // uuid.Nil when the actor is anonymous
	AnonID    *uuid.UUID
	BookID    *uuid.UUID
	EventType string
	SessionID *uuid.UUID
	Source    string // "web" | "mobile" | "server"
	Path      string
	Metadata  map[string]any
}

// Emit inserts an event row. Intended to be called in a goroutine.
// Errors are logged but never returned — analytics must not break mutations.
func (s *EventService) Emit(ctx context.Context, p EmitParams) {
	if !ValidEventType(p.EventType) {
		// A name that is not in the canonical list is a bug at the call site,
		// not user input. Log loudly: silently accepting it is how the table
		// grew three naming conventions in the first place.
		slog.Error("analytics emit rejected: unknown event_type", "event_type", p.EventType)
		return
	}

	anonymous := p.UserID == uuid.Nil
	if anonymous && (p.AnonID == nil || !AllowsAnonymous(p.EventType)) {
		slog.Warn("analytics emit dropped: no valid actor",
			"event_type", p.EventType, "has_anon_id", p.AnonID != nil)
		return
	}

	source := p.Source
	if source == "" {
		source = "server"
	}

	var userID pgtype.UUID
	if !anonymous {
		userID = pgtype.UUID{Bytes: p.UserID, Valid: true}
	}
	var anonID pgtype.UUID
	if p.AnonID != nil {
		anonID = pgtype.UUID{Bytes: *p.AnonID, Valid: true}
	}
	var bookID pgtype.UUID
	if p.BookID != nil {
		bookID = pgtype.UUID{Bytes: *p.BookID, Valid: true}
	}
	var sessionID pgtype.UUID
	if p.SessionID != nil {
		sessionID = pgtype.UUID{Bytes: *p.SessionID, Valid: true}
	}

	_, err := s.pool.Exec(ctx,
		`INSERT INTO events
		   (user_id, anon_id, book_id, event_type, metadata, session_id, source, path)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		userID, anonID, bookID, p.EventType, p.Metadata,
		sessionID, source, p.Path,
	)
	if err != nil {
		slog.Error("analytics emit failed",
			"event_type", p.EventType,
			"user_id", p.UserID,
			"error", err,
		)
	}
}
