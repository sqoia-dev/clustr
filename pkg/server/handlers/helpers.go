package handlers

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/sqoia-dev/clonr/pkg/api"
)

// writeJSON encodes v as JSON and writes it with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a structured error response, mapping sentinel errors to
// appropriate HTTP status codes.
func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, api.ErrNotFound):
		writeJSON(w, http.StatusNotFound, api.ErrorResponse{Error: err.Error(), Code: "not_found"})
	case errors.Is(err, api.ErrConflict):
		writeJSON(w, http.StatusConflict, api.ErrorResponse{Error: err.Error(), Code: "conflict"})
	case errors.Is(err, api.ErrBadRequest):
		writeJSON(w, http.StatusBadRequest, api.ErrorResponse{Error: err.Error(), Code: "bad_request"})
	default:
		writeJSON(w, http.StatusInternalServerError, api.ErrorResponse{Error: "internal server error", Code: "internal_error"})
	}
}

// writeValidationError writes a 400 with a custom message.
func writeValidationError(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, api.ErrorResponse{Error: msg, Code: "validation_error"})
}
