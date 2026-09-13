package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/hridyesh/paperboxd-backend/internal/reqctx"
	"github.com/hridyesh/paperboxd-backend/internal/service"
	"github.com/hridyesh/paperboxd-backend/internal/types"
)

// fusionLinkBase is where invite links open: the web join page, which the
// apps also claim as a universal / app link.
const fusionLinkBase = referralBaseURL + "/fusion/"

// FusionHandler serves Fusion invites and stories.
type FusionHandler struct {
	svc *service.RecommendationService
}

func NewFusionHandler(svc *service.RecommendationService) *FusionHandler {
	return &FusionHandler{svc: svc}
}

// CreateInvite returns the caller's live link, making one if needed.
// POST /api/v1/fusions/invites
func (h *FusionHandler) CreateInvite(w http.ResponseWriter, r *http.Request) {
	userID, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}
	inv, err := h.svc.CreateFusionInvite(r.Context(), userID)
	if err != nil {
		slog.Error("fusion: create invite", "error", err, "user_id", userID)
		types.WriteInternalError(w)
		return
	}
	inv.URL = fusionLinkBase + inv.Token
	types.WriteJSON(w, http.StatusOK, inv)
}

// CancelInvite retires an unused link.
// DELETE /api/v1/fusions/invites/{token}
func (h *FusionHandler) CancelInvite(w http.ResponseWriter, r *http.Request) {
	userID, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}
	token := chi.URLParam(r, "token")
	if !service.ValidFusionToken(token) {
		types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "Invite not found")
		return
	}
	if err := h.svc.CancelFusionInvite(r.Context(), userID, token); err != nil {
		slog.Error("fusion: cancel invite", "error", err, "user_id", userID)
		types.WriteInternalError(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PreviewInvite describes a link to whoever opened it. Auth is optional: a
// signed-out visitor sees who invited them; a signed-in one also learns
// whether it is their own link or one they already used.
// GET /api/v1/fusions/invites/{token}
func (h *FusionHandler) PreviewInvite(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	if !service.ValidFusionToken(token) {
		types.WriteJSON(w, http.StatusOK, service.FusionInvitePreview{Status: service.FusionInviteUnavailable})
		return
	}
	viewerID, _ := reqctx.GetUserID(r.Context())
	p, err := h.svc.PreviewFusionInvite(r.Context(), token, viewerID)
	if err != nil {
		slog.Error("fusion: preview invite", "error", err)
		types.WriteInternalError(w)
		return
	}
	types.WriteJSON(w, http.StatusOK, p)
}

// AcceptInvite spends the link — the invitee's one tap on Fuse.
// POST /api/v1/fusions/invites/{token}/accept
func (h *FusionHandler) AcceptInvite(w http.ResponseWriter, r *http.Request) {
	userID, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}
	token := chi.URLParam(r, "token")
	if !service.ValidFusionToken(token) {
		writeInviteError(w, service.FusionInviteUnavailable)
		return
	}
	id, err := h.svc.AcceptFusionInvite(r.Context(), token, userID)
	switch {
	case err == nil:
		types.WriteJSON(w, http.StatusOK, map[string]string{"fusion_id": id})
	case errors.Is(err, service.ErrFusionInviteExpired):
		writeInviteError(w, service.FusionInviteExpired)
	case errors.Is(err, service.ErrFusionInviteUsed):
		writeInviteError(w, service.FusionInviteUsed)
	case errors.Is(err, service.ErrFusionInviteOwn):
		writeInviteError(w, service.FusionInviteOwn)
	case errors.Is(err, service.ErrFusionInviteUnavailable), errors.Is(err, service.ErrFusionNotFound):
		writeInviteError(w, service.FusionInviteUnavailable)
	default:
		slog.Error("fusion: accept invite", "error", err, "user_id", userID)
		types.WriteInternalError(w)
	}
}

// writeInviteError is the usual {error, code} 409 plus the preview's status
// word, so clients render one set of link-state pages for both.
func writeInviteError(w http.ResponseWriter, status string) {
	types.WriteJSON(w, http.StatusConflict, map[string]string{
		"error":  "Invite " + status,
		"code":   types.ErrCodeConflict,
		"status": status,
	})
}

// List returns the caller's waiting link and Fusions.
// GET /api/v1/fusions
func (h *FusionHandler) List(w http.ResponseWriter, r *http.Request) {
	userID, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}
	list, err := h.svc.ListFusions(r.Context(), userID)
	if err != nil {
		slog.Error("fusion: list", "error", err, "user_id", userID)
		types.WriteInternalError(w)
		return
	}
	if list.Invite != nil {
		list.Invite.URL = fusionLinkBase + list.Invite.Token
	}
	types.WriteJSON(w, http.StatusOK, list)
}

// Get returns the story from the caller's side. Building it can take a few
// seconds the first time; the apps show the Analyzing page meanwhile.
// GET /api/v1/fusions/{id}
func (h *FusionHandler) Get(w http.ResponseWriter, r *http.Request) {
	userID, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "Fusion not found")
		return
	}
	v, err := h.svc.GetFusion(r.Context(), id, userID)
	switch {
	case err == nil:
		types.WriteJSON(w, http.StatusOK, v)
	case errors.Is(err, service.ErrFusionNotFound):
		types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "Fusion not found")
	default:
		slog.Error("fusion: get", "error", err, "user_id", userID, "fusion_id", id)
		types.WriteInternalError(w)
	}
}

// Delete removes a Fusion for both readers.
// DELETE /api/v1/fusions/{id}
func (h *FusionHandler) Delete(w http.ResponseWriter, r *http.Request) {
	userID, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "Fusion not found")
		return
	}
	err := h.svc.DeleteFusion(r.Context(), id, userID)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, service.ErrFusionNotFound):
		types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "Fusion not found")
	default:
		slog.Error("fusion: delete", "error", err, "user_id", userID, "fusion_id", id)
		types.WriteInternalError(w)
	}
}
