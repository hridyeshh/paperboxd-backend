package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hridyesh/paperboxd-backend/internal/reqctx"
	"github.com/hridyesh/paperboxd-backend/internal/types"
)

// These cases all reject the request before any query runs, so a nil Queries is
// safe and the tests need no database. The happy paths are covered by the curl
// smoke test against a live deploy.
func newDeviceTokenRequest(t *testing.T, method, body, userID string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, "/api/mobile/users/me/device-token", strings.NewReader(body))
	if userID != "" {
		req = req.WithContext(reqctx.WithUserID(req.Context(), userID))
	}
	return req
}

func decodeErr(t *testing.T, rec *httptest.ResponseRecorder) types.ErrorResponse {
	t.Helper()
	var got types.ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("response was not the standard error envelope: %v", err)
	}
	return got
}

const testUserID = "11111111-1111-1111-1111-111111111111"

func TestDeviceTokenRegisterRejects(t *testing.T) {
	h := NewDeviceTokenHandler(nil)

	for _, tc := range []struct {
		name     string
		body     string
		userID   string
		wantCode int
		wantErr  string
	}{
		{"no auth context", `{"platform":"ios","token":"abc"}`, "", http.StatusUnauthorized, types.ErrCodeUnauthorized},
		{"malformed user id", `{"platform":"ios","token":"abc"}`, "not-a-uuid", http.StatusUnauthorized, types.ErrCodeUnauthorized},
		{"invalid json", `{`, testUserID, http.StatusBadRequest, types.ErrCodeInvalidRequest},
		{"missing platform", `{"token":"abc"}`, testUserID, http.StatusBadRequest, types.ErrCodeValidation},
		{"unknown platform", `{"platform":"web","token":"abc"}`, testUserID, http.StatusBadRequest, types.ErrCodeValidation},
		{"platform case mismatch", `{"platform":"iOS","token":"abc"}`, testUserID, http.StatusBadRequest, types.ErrCodeValidation},
		{"empty token", `{"platform":"ios","token":""}`, testUserID, http.StatusBadRequest, types.ErrCodeValidation},
		{"oversized token", `{"platform":"ios","token":"` + strings.Repeat("a", maxDeviceTokenLen+1) + `"}`, testUserID, http.StatusBadRequest, types.ErrCodeValidation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.Register(rec, newDeviceTokenRequest(t, http.MethodPost, tc.body, tc.userID))

			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantCode, rec.Body)
			}
			if got := decodeErr(t, rec); got.Code != tc.wantErr {
				t.Fatalf("code = %q, want %q", got.Code, tc.wantErr)
			}
		})
	}
}

func TestDeviceTokenDeregisterRejects(t *testing.T) {
	h := NewDeviceTokenHandler(nil)

	for _, tc := range []struct {
		name     string
		body     string
		userID   string
		wantCode int
		wantErr  string
	}{
		{"no auth context", `{"token":"abc"}`, "", http.StatusUnauthorized, types.ErrCodeUnauthorized},
		{"invalid json", `nope`, testUserID, http.StatusBadRequest, types.ErrCodeInvalidRequest},
		{"empty token", `{"token":""}`, testUserID, http.StatusBadRequest, types.ErrCodeValidation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.Deregister(rec, newDeviceTokenRequest(t, http.MethodDelete, tc.body, tc.userID))

			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantCode, rec.Body)
			}
			if got := decodeErr(t, rec); got.Code != tc.wantErr {
				t.Fatalf("code = %q, want %q", got.Code, tc.wantErr)
			}
		})
	}
}
