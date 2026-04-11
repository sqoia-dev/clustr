package handlers

import (
	"net/http"

	"github.com/sqoia-dev/clonr/pkg/api"
)

// HealthHandler returns a simple liveness check.
type HealthHandler struct {
	Version string
}

func (h *HealthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, api.HealthResponse{
		Status:  "ok",
		Version: h.Version,
	})
}
