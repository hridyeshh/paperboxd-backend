package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/reqctx"
	"github.com/hridyesh/paperboxd-backend/internal/types"
)

// maxDeviceTokenLen bounds what we will store. FCM registration tokens run
// ~160-260 chars and APNs device tokens are 64 hex chars, so this is generous
// headroom that still stops an unbounded string reaching the database.
const maxDeviceTokenLen = 4096

// DeviceTokenHandler holds dependencies for push token registration.
type DeviceTokenHandler struct {
	Queries *db.Queries
}

func NewDeviceTokenHandler(queries *db.Queries) *DeviceTokenHandler {
	return &DeviceTokenHandler{Queries: queries}
}

type deviceTokenRequest struct {
	Platform string `json:"platform"`
	Token    string `json:"token"`
}

// Register upserts the caller's push token.
// POST /api/mobile/users/me/device-token
//
// Safe to call on every app start: the upsert keys on the token, so a repeat
// registration refreshes updated_at instead of adding a row, and a token that
// previously belonged to another account is reassigned to the caller.
func (h *DeviceTokenHandler) Register(w http.ResponseWriter, r *http.Request) {
	userID, ok := authenticatedUserID(w, r)
	if !ok {
		return
	}

	var req deviceTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}

	// Check platform here rather than letting the CHECK constraint reject it —
	// a constraint violation surfaces as a 500, which tells the client nothing.
	if req.Platform != "android" && req.Platform != "ios" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "platform must be 'android' or 'ios'")
		return
	}
	if req.Token == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "token is required")
		return
	}
	if len(req.Token) > maxDeviceTokenLen {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "token is too long")
		return
	}

	if _, err := h.Queries.UpsertDeviceToken(r.Context(), db.UpsertDeviceTokenParams{
		UserID:   userID,
		Platform: req.Platform,
		Token:    req.Token,
	}); err != nil {
		slog.Error("upsert device token", "error", err, "platform", req.Platform)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusOK, map[string]any{"message": "Device token registered"})
}

// Deregister removes the caller's push token, called on logout so a signed-out
// device stops receiving notifications.
// DELETE /api/mobile/users/me/device-token
//
// Scoped to the caller: one account cannot deregister another's device even
// with a valid token string. Deleting an unknown token is a no-op success —
// logout should not fail because the token was already gone.
func (h *DeviceTokenHandler) Deregister(w http.ResponseWriter, r *http.Request) {
	userID, ok := authenticatedUserID(w, r)
	if !ok {
		return
	}

	var req deviceTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}
	if req.Token == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "token is required")
		return
	}

	if err := h.Queries.DeleteDeviceToken(r.Context(), db.DeleteDeviceTokenParams{
		UserID: userID,
		Token:  req.Token,
	}); err != nil {
		slog.Error("delete device token", "error", err)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusOK, map[string]any{"message": "Device token removed"})
}

// authenticatedUserID pulls the caller's ID out of the request context, writing
// the 401 envelope and reporting false when it is absent or malformed.
func authenticatedUserID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	userIDStr, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return uuid.UUID{}, false
	}
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return uuid.UUID{}, false
	}
	return userID, true
}
