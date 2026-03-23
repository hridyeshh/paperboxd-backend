package types

import (
	"encoding/json"
	"net/http"
)

// AppError represents a structured API error.
type AppError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ErrorResponse is the JSON envelope for error responses.
type ErrorResponse struct {
	Error AppError `json:"error"`
}

// Common error codes.
const (
	ErrCodeInvalidRequest   = "INVALID_REQUEST"
	ErrCodeUnauthorized     = "UNAUTHORIZED"
	ErrCodeForbidden        = "FORBIDDEN"
	ErrCodeNotFound         = "NOT_FOUND"
	ErrCodeConflict         = "CONFLICT"
	ErrCodeInternalServer   = "INTERNAL_SERVER_ERROR"
	ErrCodeValidation       = "VALIDATION_ERROR"
	ErrCodeInvalidToken     = "INVALID_TOKEN"
	ErrCodeExpiredToken     = "EXPIRED_TOKEN"
)

// WriteError writes a JSON error response.
func WriteError(w http.ResponseWriter, statusCode int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	resp := ErrorResponse{
		Error: AppError{
			Code:    code,
			Message: message,
		},
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// WriteInternalError writes a 500 error.
func WriteInternalError(w http.ResponseWriter) {
	WriteError(w, http.StatusInternalServerError, ErrCodeInternalServer, "An internal server error occurred")
}
