package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/hridyesh/paperboxd-backend/internal/config"
	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/external"
	"github.com/hridyesh/paperboxd-backend/internal/reqctx"
	"github.com/hridyesh/paperboxd-backend/internal/types"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ScanHandler handles the /scan/* endpoints.
type ScanHandler struct {
	Pool    *pgxpool.Pool
	Queries *db.Queries
	Config  *config.Config
	ISBNdb  *external.ISBNdbClient
}

func NewScanHandler(pool *pgxpool.Pool, queries *db.Queries, cfg *config.Config, isbndb *external.ISBNdbClient) *ScanHandler {
	return &ScanHandler{
		Pool:    pool,
		Queries: queries,
		Config:  cfg,
		ISBNdb:  isbndb,
	}
}

// Analyze handles POST /api/v1/scan/analyze.
// Validates the user's scan quota then returns book metadata from ISBNdb.
func (h *ScanHandler) Analyze(w http.ResponseWriter, r *http.Request) {
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

	var req struct {
		ISBN string `json:"isbn"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Invalid JSON body")
		return
	}
	isbn := strings.TrimSpace(req.ISBN)
	if isbn == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "isbn is required")
		return
	}

	var scansRemaining int32
	err = h.Pool.QueryRow(r.Context(),
		"SELECT scan_uses_remaining FROM users WHERE id = $1",
		userID,
	).Scan(&scansRemaining)
	if err != nil {
		slog.Error("query scan_uses_remaining", "error", err, "user_id", userID)
		types.WriteInternalError(w)
		return
	}

	if scansRemaining == 0 {
		types.WriteJSON(w, http.StatusForbidden, map[string]any{
			"error":           "scans_exhausted",
			"scans_remaining": 0,
		})
		return
	}

	if h.ISBNdb == nil {
		types.WriteError(w, http.StatusInternalServerError, types.ErrCodeInternalServer, "Book lookup not configured")
		return
	}

	book, err := h.ISBNdb.GetByISBN(r.Context(), isbn)
	if err != nil {
		if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "400") {
			types.WriteJSON(w, http.StatusNotFound, map[string]any{
				"error":   "book_not_found",
				"message": "Couldn't find this book — try searching by title",
			})
			return
		}
		slog.Error("isbndb get by isbn", "error", err, "isbn", isbn)
		types.WriteInternalError(w)
		return
	}
	if book == nil {
		types.WriteJSON(w, http.StatusNotFound, map[string]any{
			"error":   "book_not_found",
			"message": "Couldn't find this book — try searching by title",
		})
		return
	}

	isbn13 := book.ISBN13
	if isbn13 == "" {
		isbn13 = book.ISBN
	}
	if isbn13 == "" {
		isbn13 = isbn
	}

	authors := book.Authors
	if authors == nil {
		authors = []string{}
	}
	genres := book.Subjects
	if genres == nil {
		genres = []string{}
	}

	types.WriteJSON(w, http.StatusOK, map[string]any{
		"book": map[string]any{
			"isbn":        isbn13,
			"title":       book.Title,
			"authors":     authors,
			"genres":      genres,
			"pages":       book.Pages,
			"description": book.Synopsis,
			"cover_url":   book.Image,
		},
		"scans_remaining": scansRemaining,
	})
}
