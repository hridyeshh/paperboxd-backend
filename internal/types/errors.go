package types

import (
	"encoding/json"
	"net/http"
)

// ErrorResponse is the JSON envelope for error responses.
// Shape (mobile + web contract):
//
//	{ "error": "human readable message", "code": "SNAKE_CASE_CODE" }
type ErrorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// Common error codes returned by all handlers.
const (
	ErrCodeInvalidRequest = "VALIDATION_ERROR"
	ErrCodeValidation     = "VALIDATION_ERROR"
	ErrCodeUnauthorized   = "UNAUTHORIZED"
	ErrCodeForbidden      = "FORBIDDEN"
	ErrCodeNotFound       = "NOT_FOUND"
	ErrCodeConflict       = "CONFLICT"
	ErrCodeRateLimited    = "RATE_LIMITED"
	ErrCodeInternalServer = "INTERNAL_ERROR"
	ErrCodeInvalidToken   = "INVALID_TOKEN"
	ErrCodeExpiredToken   = "EXPIRED_TOKEN"
	ErrCodePrivateProfile = "PRIVATE_PROFILE"
)

// AppError is kept for backwards source-compatibility with code that previously
// constructed structured errors. New callers should use WriteError directly.
type AppError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WriteError writes a JSON error response in the flat {error, code} shape.
func WriteError(w http.ResponseWriter, statusCode int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(ErrorResponse{
		Error: message,
		Code:  code,
	})
}

// WriteInternalError writes a 500 error.
func WriteInternalError(w http.ResponseWriter) {
	WriteError(w, http.StatusInternalServerError, ErrCodeInternalServer, "An internal server error occurred")
}
