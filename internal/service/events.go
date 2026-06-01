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

// EmitParams describes a single analytics event. UserID is required because
// events.user_id is NOT NULL — callers must verify the user is authenticated
// before calling Emit, and skip the emit entirely otherwise.
type EmitParams struct {
	UserID    uuid.UUID // required — user_id is NOT NULL
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
	if p.EventType == "" {
		return
	}
	source := p.Source
	if source == "" {
		source = "server"
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
		   (user_id, book_id, event_type, metadata, session_id, source, path)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		p.UserID, bookID, p.EventType,
		p.Metadata, sessionID, source, p.Path,
	)
	if err != nil {
		slog.Error("analytics emit failed",
			"event_type", p.EventType,
			"user_id", p.UserID,
			"error", err,
		)
	}
}
