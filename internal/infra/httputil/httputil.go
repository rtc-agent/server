// Package httputil provides common HTTP response helper functions.
//
// Eliminates duplicate JSON response writing logic across oauth2, mockoauth2, and other packages.
package httputil

import (
	"context"
	"encoding/json"
	"net/http"

	"go.uber.org/zap"

	"github.com/rtc-agent/server/pkg/logger"
)

// WriteJSON writes a JSON response.
//
// Sets Content-Type to application/json, writes the status code, and encodes data into the response body.
// Logs an error on encoding failure (headers already sent, cannot change status code).
func WriteJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		logger.Error(context.Background(), "WriteJSON: failed to encode response",
			zap.Int("status", status), zap.Error(err))
	}
}

// WriteError writes a structured error response.
//
// Response format: {"error": errCode, "error_description": description}.
// Also sets Cache-Control / Pragma headers to prevent clients from caching error responses.
func WriteError(w http.ResponseWriter, status int, errCode, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	resp := map[string]string{
		"error": errCode,
	}
	if description != "" {
		resp["error_description"] = description
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		logger.Error(context.Background(), "WriteError: failed to encode response",
			zap.Int("status", status), zap.String("err_code", errCode), zap.Error(err))
	}
}
