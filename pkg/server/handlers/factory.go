package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"
	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/db"
	"github.com/sqoia-dev/clonr/pkg/image"
)

// FactoryHandler handles image ingest operations and chroot shell sessions.
type FactoryHandler struct {
	DB       *db.DB
	ImageDir string
	Factory  *image.Factory
	Shells   *image.ShellManager
}

// Pull handles POST /api/v1/factory/pull
// Delegates to image.Factory.PullImage, which returns immediately with a
// "building" record and downloads/extracts in the background.
func (h *FactoryHandler) Pull(w http.ResponseWriter, r *http.Request) {
	var req api.PullRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeValidationError(w, "invalid JSON body")
		return
	}
	if req.URL == "" {
		writeValidationError(w, "url is required")
		return
	}
	if req.Name == "" {
		writeValidationError(w, "name is required")
		return
	}

	img, err := h.Factory.PullImage(r.Context(), req)
	if err != nil {
		log.Error().Err(err).Msg("factory pull")
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, img)
}

// Import handles POST /api/v1/factory/import
// Accepts a multipart upload: field "iso" = the ISO file, field "meta" = JSON
// ImportISORequest. Saves the ISO to a temp file and calls Factory.ImportISO.
func (h *FactoryHandler) Import(w http.ResponseWriter, r *http.Request) {
	// Limit upload to 16 GiB in memory parsing (actual stream to disk).
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeValidationError(w, "failed to parse multipart form")
		return
	}

	// Parse metadata from the "meta" field.
	var meta api.ImportISORequest
	if metaStr := r.FormValue("meta"); metaStr != "" {
		if err := json.Unmarshal([]byte(metaStr), &meta); err != nil {
			writeValidationError(w, "invalid meta JSON")
			return
		}
	}
	if meta.Name == "" {
		writeValidationError(w, "meta.name is required")
		return
	}

	// Stream the "iso" file field to a temp file.
	file, _, err := r.FormFile("iso")
	if err != nil {
		writeValidationError(w, "iso file field is required")
		return
	}
	defer file.Close()

	tmp, err := os.CreateTemp("", "clonr-import-*.iso")
	if err != nil {
		log.Error().Err(err).Msg("factory import: create temp file")
		writeError(w, err)
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmp, file); err != nil {
		tmp.Close()
		log.Error().Err(err).Msg("factory import: write temp file")
		writeError(w, err)
		return
	}
	tmp.Close()

	img, err := h.Factory.ImportISO(r.Context(), tmpPath, meta.Name, meta.Version)
	if err != nil {
		log.Error().Err(err).Msg("factory import ISO")
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, img)
}

// defaultISODir is the allowed base directory for server-local ISO imports.
// Override with CLONR_ISO_DIR environment variable.
const defaultISODir = "/var/lib/clonr/iso"

// ImportPath handles POST /api/v1/factory/import-path (and /factory/import-iso alias)
// For server-local ISO imports: accepts a JSON body with "path", "name", "version".
// Only useful when the CLI is running on the same host as the server.
// The path must be within CLONR_ISO_DIR (default /var/lib/clonr/iso).
func (h *FactoryHandler) ImportPath(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path    string `json:"path"`
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeValidationError(w, "invalid JSON body")
		return
	}
	if body.Path == "" {
		writeValidationError(w, "path is required")
		return
	}
	if body.Name == "" {
		writeValidationError(w, "name is required")
		return
	}
	// Resolve to absolute so the async goroutine doesn't lose cwd context.
	absPath, err := filepath.Abs(body.Path)
	if err != nil {
		writeValidationError(w, "cannot resolve path")
		return
	}

	// Enforce that the path is under the configured ISO directory to prevent
	// arbitrary host path access.
	isoDir := os.Getenv("CLONR_ISO_DIR")
	if isoDir == "" {
		isoDir = defaultISODir
	}
	isoDir = filepath.Clean(isoDir)
	if !strings.HasPrefix(absPath, isoDir+string(filepath.Separator)) && absPath != isoDir {
		log.Warn().Str("path", absPath).Str("iso_dir", isoDir).Msg("factory import-path: path outside allowed directory")
		writeValidationError(w, "path must be within the configured ISO directory (CLONR_ISO_DIR)")
		return
	}

	img, err := h.Factory.ImportISO(r.Context(), absPath, body.Name, body.Version)
	if err != nil {
		log.Error().Err(err).Str("path", absPath).Msg("factory import-path")
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, img)
}

// Capture handles POST /api/v1/factory/capture
// Accepts a CaptureRequest and rsyncs from the given source into a new image.
func (h *FactoryHandler) Capture(w http.ResponseWriter, r *http.Request) {
	var req api.CaptureRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeValidationError(w, "invalid JSON body")
		return
	}
	if req.Source == "" {
		writeValidationError(w, "source is required")
		return
	}
	if req.Name == "" {
		writeValidationError(w, "name is required")
		return
	}

	captureReq := image.CaptureRequest{
		Source:  req.Source,
		Name:    req.Name,
		Version: req.Version,
		OS:      req.OS,
		Arch:    req.Arch,
		Tags:    req.Tags,
		Notes:   req.Notes,
	}

	img, err := h.Factory.CaptureNode(r.Context(), captureReq)
	if err != nil {
		log.Error().Err(err).Msg("factory capture")
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, img)
}

// OpenShellSession handles POST /api/v1/images/:id/shell-session
// Creates and enters a chroot session for the specified image.
func (h *FactoryHandler) OpenShellSession(w http.ResponseWriter, r *http.Request) {
	imageID := chi.URLParam(r, "id")

	sess, err := h.Shells.OpenSession(r.Context(), imageID)
	if err != nil {
		log.Error().Err(err).Str("image_id", imageID).Msg("open shell session")
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, api.ShellSessionResponse{
		SessionID: sess.ID,
		ImageID:   sess.ImageID,
		RootDir:   sess.RootDir,
	})
}

// CloseShellSession handles DELETE /api/v1/images/:id/shell-session/:sid
func (h *FactoryHandler) CloseShellSession(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sid")

	if err := h.Shells.CloseSession(sessionID); err != nil {
		log.Error().Err(err).Str("session_id", sessionID).Msg("close shell session")
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ExecInSession handles POST /api/v1/images/:id/shell-session/:sid/exec
func (h *FactoryHandler) ExecInSession(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sid")

	var req api.ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeValidationError(w, "invalid JSON body")
		return
	}
	if req.Command == "" {
		writeValidationError(w, "command is required")
		return
	}

	out, err := h.Shells.ExecInSession(r.Context(), sessionID, req.Command, req.Args...)
	if err != nil {
		log.Error().Err(err).Str("session_id", sessionID).Str("cmd", req.Command).Msg("exec in session")
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, api.ExecResponse{Output: string(out)})
}
