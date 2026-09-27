package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/external"
	"github.com/hridyesh/paperboxd-backend/internal/reqctx"
	"github.com/hridyesh/paperboxd-backend/internal/service"
	"github.com/hridyesh/paperboxd-backend/internal/types"
)

// MessageHandler serves DMs: the inbox, a conversation, and sending.
type MessageHandler struct {
	svc         *service.MessageService
	queries     *db.Queries
	googleBooks *external.GoogleBooksClient
	isbndb      *external.ISBNdbClient
}

func NewMessageHandler(svc *service.MessageService, queries *db.Queries, gb *external.GoogleBooksClient, isbndb *external.ISBNdbClient) *MessageHandler {
	return &MessageHandler{svc: svc, queries: queries, googleBooks: gb, isbndb: isbndb}
}

// writeMessageError maps service errors to responses; anything unknown is a 500.
func writeMessageError(w http.ResponseWriter, err error, op, userID string) {
	switch {
	case errors.Is(err, service.ErrInvalidMessage):
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, strings.TrimPrefix(err.Error(), service.ErrInvalidMessage.Error()+": "))
	case errors.Is(err, service.ErrConversationNotFound):
		types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "Conversation not found")
	case errors.Is(err, service.ErrMessageNotFound):
		types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "Message not found")
	case errors.Is(err, service.ErrRecipientNotFound):
		types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "Reader not found")
	case errors.Is(err, service.ErrCannotMessage):
		types.WriteError(w, http.StatusForbidden, types.ErrCodeForbidden, "You can't message this reader")
	case errors.Is(err, service.ErrAttachmentUnavailable):
		types.WriteError(w, http.StatusUnprocessableEntity, types.ErrCodeValidation, "Something you attached isn't available to share")
	case errors.Is(err, service.ErrRequestLimit):
		types.WriteError(w, http.StatusTooManyRequests, types.ErrCodeRateLimited, "Wait for them to accept your message request")
	case errors.Is(err, service.ErrDailyConversationCap):
		types.WriteError(w, http.StatusTooManyRequests, types.ErrCodeRateLimited, "You've started a lot of conversations today. Try again tomorrow")
	default:
		slog.Error("messages: "+op, "error", err, "user_id", userID)
		types.WriteInternalError(w)
	}
}

func (h *MessageHandler) userID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
	}
	return id, ok
}

func queryInt64(r *http.Request, key string) int64 {
	n, _ := strconv.ParseInt(r.URL.Query().Get(key), 10, 64)
	return n
}

// List returns the inbox, or message requests with ?box=request.
// GET /api/v1/conversations?box=&before=
func (h *MessageHandler) List(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userID(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	list, err := h.svc.ListConversations(r.Context(), userID, q.Get("box"), q.Get("before"))
	if err != nil {
		writeMessageError(w, err, "list", userID)
		return
	}
	types.WriteJSON(w, http.StatusOK, list)
}

// Badge is the header count.
// GET /api/v1/conversations/badge
func (h *MessageHandler) Badge(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userID(w, r)
	if !ok {
		return
	}
	b, err := h.svc.Badge(r.Context(), userID)
	if err != nil {
		writeMessageError(w, err, "badge", userID)
		return
	}
	types.WriteJSON(w, http.StatusOK, b)
}

// With finds the conversation with a reader; conversation_id is "" when none
// exists yet and the first send should go to "to": [username].
// GET /api/v1/conversations/with/{username}
func (h *MessageHandler) With(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userID(w, r)
	if !ok {
		return
	}
	id, them, err := h.svc.ConversationWith(r.Context(), userID, chi.URLParam(r, "username"))
	if err != nil {
		writeMessageError(w, err, "with", userID)
		return
	}
	types.WriteJSON(w, http.StatusOK, map[string]any{"conversation_id": id, "them": them})
}

// Messages returns a page of the conversation, oldest first. ?after=<id> is
// the polling path; ?before=<id> loads older history.
// GET /api/v1/conversations/{id}/messages
func (h *MessageHandler) Messages(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userID(w, r)
	if !ok {
		return
	}
	page, err := h.svc.Messages(r.Context(), chi.URLParam(r, "id"), userID, queryInt64(r, "before"), queryInt64(r, "after"))
	if err != nil {
		writeMessageError(w, err, "messages", userID)
		return
	}
	types.WriteJSON(w, http.StatusOK, page)
}

// Shared lists everything sent in the conversation.
// GET /api/v1/conversations/{id}/shared?kind=&before=
func (h *MessageHandler) Shared(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userID(w, r)
	if !ok {
		return
	}
	list, err := h.svc.Shared(r.Context(), chi.URLParam(r, "id"), userID, r.URL.Query().Get("kind"), queryInt64(r, "before"))
	if err != nil {
		writeMessageError(w, err, "shared", userID)
		return
	}
	types.WriteJSON(w, http.StatusOK, list)
}

// Send posts a message to a conversation or to up to five readers.
// POST /api/v1/messages
//
//	{"conversation_id": "...", "body": "...", "attachments": [{"kind": "book", "ref_id": "..."}], "client_id": "<uuid>"}
//	{"to": ["maya", "arjun"], "body": "...", "attachments": [...]}
func (h *MessageHandler) Send(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userID(w, r)
	if !ok {
		return
	}
	var req struct {
		ConversationID string                  `json:"conversation_id"`
		To             []string                `json:"to"`
		Body           string                  `json:"body"`
		Attachments    []service.AttachmentRef `json:"attachments"`
		ClientID       string                  `json:"client_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}
	// Books arrive as whatever id the client has (UUID, Google volume, ISBN);
	// the service only deals in Paperboxd UUIDs.
	for i, a := range req.Attachments {
		if a.Kind != service.AttachBook {
			continue
		}
		if len(req.Attachments) > service.MaxMessageAttachments {
			break // the service rejects the count; don't fetch first
		}
		id, err := resolveBookIDParam(r.Context(), h.queries, h.googleBooks, h.isbndb, a.RefID)
		if err != nil {
			types.WriteError(w, http.StatusUnprocessableEntity, types.ErrCodeValidation, "Something you attached isn't available to share")
			return
		}
		req.Attachments[i].RefID = id.String()
	}
	res, err := h.svc.Send(r.Context(), userID, service.SendInput{
		ConversationID: req.ConversationID,
		To:             req.To,
		Body:           req.Body,
		Attachments:    req.Attachments,
		ClientID:       req.ClientID,
	})
	if err != nil {
		writeMessageError(w, err, "send", userID)
		return
	}
	types.WriteJSON(w, http.StatusCreated, res)
}

// Unsend wipes one of the caller's messages.
// DELETE /api/v1/messages/{id}
func (h *MessageHandler) Unsend(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userID(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "Message not found")
		return
	}
	if err := h.svc.Unsend(r.Context(), id, userID); err != nil {
		writeMessageError(w, err, "unsend", userID)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Read marks the conversation read up to its latest message.
// POST /api/v1/conversations/{id}/read
func (h *MessageHandler) Read(w http.ResponseWriter, r *http.Request) {
	h.memberAction(w, r, "read", func(convID, userID string) error {
		return h.svc.MarkRead(r.Context(), convID, userID)
	})
}

// Accept moves a message request into the inbox.
// POST /api/v1/conversations/{id}/accept
func (h *MessageHandler) Accept(w http.ResponseWriter, r *http.Request) {
	h.memberAction(w, r, "accept", func(convID, userID string) error {
		return h.svc.Accept(r.Context(), convID, userID)
	})
}

// Update sets per-reader options. Only muted for now.
// PATCH /api/v1/conversations/{id}  {"muted": true}
func (h *MessageHandler) Update(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Muted *bool `json:"muted"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Muted == nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "muted is required")
		return
	}
	h.memberAction(w, r, "mute", func(convID, userID string) error {
		return h.svc.SetMuted(r.Context(), convID, userID, *req.Muted)
	})
}

// Clear is "delete chat" for the caller only.
// DELETE /api/v1/conversations/{id}
func (h *MessageHandler) Clear(w http.ResponseWriter, r *http.Request) {
	h.memberAction(w, r, "clear", func(convID, userID string) error {
		return h.svc.Clear(r.Context(), convID, userID)
	})
}

func (h *MessageHandler) memberAction(w http.ResponseWriter, r *http.Request, op string, fn func(convID, userID string) error) {
	userID, ok := h.userID(w, r)
	if !ok {
		return
	}
	if err := fn(chi.URLParam(r, "id"), userID); err != nil {
		writeMessageError(w, err, op, userID)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
