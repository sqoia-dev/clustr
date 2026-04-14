package handlers

import (
	"bufio"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/db"
)

//go:embed scripts/build-initramfs.sh
var buildInitramfsScript []byte // embedded at compile time — no on-disk dependency at runtime

// InitramfsHandler handles system-level initramfs management endpoints.
type InitramfsHandler struct {
	DB            *db.DB
	ScriptPath    string // path to build-initramfs.sh (abs)
	InitramfsPath string // final output path (e.g. /var/lib/clonr/boot/initramfs-clonr.img)
	ClonrBinPath  string // path to the clonr static binary passed to the script

	mu          sync.Mutex // serialises concurrent rebuild requests
	running     bool
	liveSHA256  string // sha256 of the on-disk initramfs; cached to avoid per-request file reads
}

// InitLiveSHA256 computes and caches the sha256 of the current on-disk initramfs.
// Call this once at startup (non-fatal if the file does not yet exist).
func (h *InitramfsHandler) InitLiveSHA256() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.liveSHA256 = computeFileSHA256(h.InitramfsPath)
}

// computeFileSHA256 returns the hex sha256 of the file at path, or "" on error.
func computeFileSHA256(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return ""
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

// InitramfsBuildInfo is the shape returned by GET /api/v1/system/initramfs.
type InitramfsBuildInfo struct {
	SHA256        string                      `json:"sha256"`
	SizeBytes     int64                       `json:"size_bytes"`
	BuildTime     *time.Time                  `json:"build_time,omitempty"`
	KernelVersion string                      `json:"kernel_version,omitempty"`
	History       []db.InitramfsBuildRecord   `json:"history"`
}

// GetInitramfs handles GET /api/v1/system/initramfs.
// Returns current sha256, size, build_time, kernel version, and last 5 history rows.
func (h *InitramfsHandler) GetInitramfs(w http.ResponseWriter, r *http.Request) {
	info := InitramfsBuildInfo{}

	// Read current file stats.
	if stat, err := os.Stat(h.InitramfsPath); err == nil {
		info.SizeBytes = stat.Size()
		mtime := stat.ModTime().UTC()
		info.BuildTime = &mtime
		// Use the cached live sha256 — avoids a 27 MB synchronous file read per
		// request.  The cache is populated at startup and after each successful
		// rebuild, so it is always current.
		h.mu.Lock()
		info.SHA256 = h.liveSHA256
		h.mu.Unlock()
	}

	// Load history.
	history, err := h.DB.ListInitramfsBuilds(r.Context(), 5)
	if err != nil {
		log.Warn().Err(err).Msg("initramfs: list history")
	}
	if history == nil {
		history = []db.InitramfsBuildRecord{}
	}
	info.History = history

	writeJSON(w, http.StatusOK, info)
}

// RebuildInitramfs handles POST /api/v1/system/initramfs/rebuild.
// Guards:
//   - Rejects 409 if any node has an active (non-terminal) deploy progress.
//   - Rejects 409 if a rebuild is already in flight.
//
// Shells out to build-initramfs.sh in a staging dir, sha256-checks the result,
// then atomically renames it into place.
func (h *InitramfsHandler) RebuildInitramfs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Guard: reject if a rebuild is already in flight.
	h.mu.Lock()
	if h.running {
		h.mu.Unlock()
		writeJSON(w, http.StatusConflict, api.ErrorResponse{
			Error: "an initramfs rebuild is already in progress",
			Code:  "rebuild_in_progress",
		})
		return
	}
	h.running = true
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.running = false
		h.mu.Unlock()
	}()

	// Guard: reject if any node has an active deploy.
	if hasActive, nodeID := h.hasActiveDeployViaDB(ctx); hasActive {
		writeJSON(w, http.StatusConflict, api.ErrorResponse{
			Error: fmt.Sprintf("node %s has an active deployment — wait for it to complete before rebuilding initramfs", nodeID),
			Code:  "deploy_active",
		})
		return
	}

	// Determine triggered_by from the API key prefix in the Authorization header.
	triggeredBy := extractKeyPrefix(r)

	// Create DB record.
	buildID := uuid.New().String()
	record := db.InitramfsBuildRecord{
		ID:               buildID,
		StartedAt:        time.Now().UTC(),
		TriggeredByPrefix: triggeredBy,
		Outcome:          "pending",
	}
	if err := h.DB.CreateInitramfsBuild(ctx, record); err != nil {
		log.Error().Err(err).Msg("initramfs rebuild: create db record")
		writeError(w, err)
		return
	}

	// Audit log start.
	log.Info().
		Str("build_id", buildID).
		Str("triggered_by", triggeredBy).
		Msg("initramfs rebuild: started")

	// Staging path — write next to final so rename is atomic (same filesystem).
	stagingPath := h.InitramfsPath + ".building"

	// Build in a temp work dir.
	workDir, err := os.MkdirTemp("", "clonr-initramfs-*")
	if err != nil {
		h.failBuild(buildID, fmt.Errorf("create work dir: %w", err))
		writeError(w, err)
		return
	}
	defer os.RemoveAll(workDir)

	// Prepare the build progress store for streaming.
	// We use the build ID as the "image ID" key so the SSE stream can subscribe
	// to progress using the existing BuildProgressStore.
	lines := make(chan string, 256)

	// Run the script in a goroutine and collect output.
	var buildErr error
	var scriptSHA256 string
	var scriptSize int64
	var kernelVer string

	done := make(chan struct{})
	go func() {
		defer close(done)
		buildErr = h.runScript(workDir, stagingPath, lines)
		// Compute sha256 + size of staging file if script succeeded.
		if buildErr == nil {
			scriptSHA256, scriptSize, kernelVer = h.inspectStagingFile(stagingPath)
		}
	}()

	// Collect all lines; the response is returned after completion (not streamed
	// per-line for simplicity — the UI SSE approach polls the build-log endpoint).
	var logLines []string
	for line := range lines {
		logLines = append(logLines, line)
	}
	<-done

	if buildErr != nil {
		h.failBuild(buildID, buildErr)
		// Log collected output so the error is visible in the service journal.
		if len(logLines) > 0 {
			log.Error().
				Str("build_id", buildID).
				Strs("script_output", logLines).
				Msg("initramfs rebuild: script output before failure")
		}
		log.Error().Err(buildErr).Str("build_id", buildID).Msg("initramfs rebuild: failed")
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":     fmt.Sprintf("initramfs rebuild failed: %v", buildErr),
			"code":      "rebuild_failed",
			"log_lines": logLines,
		})
		return
	}

	// Atomic rename: staging → final.
	if err := os.Rename(stagingPath, h.InitramfsPath); err != nil {
		h.failBuild(buildID, fmt.Errorf("atomic rename failed: %w", err))
		writeError(w, err)
		return
	}

	// Update the in-memory live sha256 cache now that the new image is on disk.
	h.mu.Lock()
	h.liveSHA256 = scriptSHA256
	h.mu.Unlock()

	// Finalize DB record — use a background context so a slow or disconnected
	// HTTP client does not cancel the write after a successful build.
	dbCtx, dbCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dbCancel()
	outcome := "success"
	if err := h.DB.FinishInitramfsBuild(dbCtx, buildID, scriptSHA256, scriptSize, kernelVer, outcome); err != nil {
		log.Warn().Err(err).Str("build_id", buildID).Msg("initramfs rebuild: failed to update db record (non-fatal)")
	}
	// Trim history to 5 rows.
	if err := h.DB.TrimInitramfsBuilds(dbCtx, 5); err != nil {
		log.Warn().Err(err).Msg("initramfs rebuild: trim history failed (non-fatal)")
	}

	log.Info().
		Str("build_id", buildID).
		Str("sha256", scriptSHA256).
		Int64("size_bytes", scriptSize).
		Str("kernel_version", kernelVer).
		Msg("initramfs rebuild: complete")

	writeJSON(w, http.StatusOK, map[string]any{
		"build_id":       buildID,
		"sha256":         scriptSHA256,
		"size_bytes":     scriptSize,
		"kernel_version": kernelVer,
		"log_lines":      logLines,
	})
}

// runScript executes build-initramfs.sh and streams output to lines.
// The script is written from the embedded copy to a temp file at call time,
// making the binary self-contained with no on-disk script dependency.
// Closes lines when done.
func (h *InitramfsHandler) runScript(workDir, outputPath string, lines chan<- string) error {
	defer close(lines)

	// Write the embedded script to a temp file so the binary is self-contained.
	// The handler's ScriptPath field is ignored at runtime; the embedded bytes
	// are always used. This fixes "exit status 127" caused by relative ScriptPath
	// not existing in the service's WorkingDirectory (/var/lib/clonr).
	tmpScript, err := os.CreateTemp("", "clonr-build-initramfs-*.sh")
	if err != nil {
		return fmt.Errorf("create temp script: %w", err)
	}
	tmpScriptPath := tmpScript.Name()
	defer os.Remove(tmpScriptPath)

	if _, err := tmpScript.Write(buildInitramfsScript); err != nil {
		tmpScript.Close()
		return fmt.Errorf("write temp script: %w", err)
	}
	if err := tmpScript.Chmod(0o700); err != nil {
		tmpScript.Close()
		return fmt.Errorf("chmod temp script: %w", err)
	}
	tmpScript.Close()

	scriptPath := tmpScriptPath

	clonrBin := h.ClonrBinPath
	if clonrBin == "" {
		// Default: look for clonr-static alongside the running binary.
		exe, _ := os.Executable()
		clonrBin = filepath.Join(filepath.Dir(exe), "clonr-static")
	} else if !filepath.IsAbs(clonrBin) {
		// Relative path: resolve relative to the running binary's directory so
		// the path is stable regardless of WorkingDirectory in the systemd unit.
		exe, _ := os.Executable()
		clonrBin = filepath.Join(filepath.Dir(exe), clonrBin)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", scriptPath, clonrBin, outputPath) //nolint:gosec
	cmd.Dir = workDir
	// Include /root/bin in PATH so busybox-static and other root-installed tools
	// are visible to the script even when running under a restricted systemd unit.
	env := os.Environ()
	for i, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			env[i] = e + ":/root/bin"
			break
		}
	}
	cmd.Env = append(env,
		"CLONR_SERVER_HOST=127.0.0.1",
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = cmd.Stdout // merge stderr into stdout

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start script: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		select {
		case lines <- line:
		default:
		}
	}

	return cmd.Wait()
}

// inspectStagingFile computes sha256, size, and detects kernel version from the staging file.
func (h *InitramfsHandler) inspectStagingFile(path string) (sha256sum string, size int64, kernelVer string) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, ""
	}
	defer f.Close()

	hasher := sha256.New()
	n, err := io.Copy(hasher, f)
	if err != nil {
		return "", 0, ""
	}
	sha256sum = hex.EncodeToString(hasher.Sum(nil))
	size = n

	// Try to detect kernel version from uname -r on the host (the initramfs
	// is built for the running kernel).
	out, err := exec.Command("uname", "-r").Output()
	if err == nil {
		kernelVer = strings.TrimSpace(string(out))
	}
	return sha256sum, size, kernelVer
}

// failBuild records a failure outcome in the DB.
func (h *InitramfsHandler) failBuild(buildID string, buildErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msg := ""
	if buildErr != nil {
		msg = buildErr.Error()
	}
	_ = h.DB.FinishInitramfsBuild(ctx, buildID, "", 0, "", "failed: "+msg)
}

// hasActiveDeployViaDB checks the deploy_progress table for any non-terminal entry.
// We read directly from the ProgressStore through a DB query on reimage_requests.
func (h *InitramfsHandler) hasActiveDeployViaDB(ctx context.Context) (bool, string) {
	// Query reimage_requests for any 'running' or 'pending' status.
	rows, err := h.DB.SQL().QueryContext(ctx, `
		SELECT node_id FROM reimage_requests
		WHERE status IN ('running', 'pending', 'triggered')
		LIMIT 1
	`)
	if err != nil {
		// Non-fatal: can't check, allow rebuild to proceed.
		return false, ""
	}
	defer rows.Close()
	if rows.Next() {
		var nodeID string
		_ = rows.Scan(&nodeID)
		return true, nodeID
	}
	return false, ""
}

// extractKeyPrefix pulls the first 8 chars of the key from the Authorization header.
func extractKeyPrefix(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if after, ok := strings.CutPrefix(auth, "Bearer "); ok {
		// Strip typed prefix.
		key := after
		for _, pfx := range []string{"clonr-admin-", "clonr-node-"} {
			if strings.HasPrefix(key, pfx) {
				key = strings.TrimPrefix(key, pfx)
				break
			}
		}
		if len(key) > 8 {
			key = key[:8]
		}
		return key
	}
	return "session"
}

// DeleteInitramfsHistory handles DELETE /api/v1/system/initramfs/history/{id}.
// Deletes a single history entry by ID regardless of its outcome (success or
// failure), UNLESS the entry's sha256 matches the sha256 of the initramfs file
// currently on disk — that entry is the live image and must not be deleted.
//
// Guard logic:
//  1. Compute sha256 of the on-disk initramfs file (h.InitramfsPath).
//  2. Fetch the target record's sha256 from the DB.
//  3. If they match → 409 live_entry_cannot_delete.
//  4. Otherwise → proceed with deletion regardless of outcome field.
//
// This allows deletion of older successful entries that have been superseded by
// a newer rebuild, while still protecting the currently-serving image.
func (h *InitramfsHandler) DeleteInitramfsHistory(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, api.ErrorResponse{Error: "missing id", Code: "bad_request"})
		return
	}

	ctx := r.Context()

	// Fetch the target entry's sha256 from history.
	history, err := h.DB.ListInitramfsBuilds(ctx, 20)
	if err != nil {
		writeError(w, err)
		return
	}

	// Compare against the cached live sha256 — no synchronous file read required.
	// liveSHA256 is set at startup and updated after every successful rebuild.
	h.mu.Lock()
	liveSHA256 := h.liveSHA256
	h.mu.Unlock()

	if liveSHA256 != "" {
		for _, rec := range history {
			if rec.ID == id && rec.SHA256 == liveSHA256 {
				writeJSON(w, http.StatusConflict, api.ErrorResponse{
					Error: "cannot delete the live initramfs entry — its sha256 matches the file currently on disk",
					Code:  "live_entry_cannot_delete",
				})
				return
			}
		}
	}

	if err := h.DB.DeleteInitramfsBuild(ctx, id); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetBuildLog returns the full build log for an initramfs build.
// This is a stub — production would store lines in the DB or a temp file.
// For now we return an informative 204.
func (h *InitramfsHandler) GetBuildLog(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// parseInitramfsBuildInfo is a helper to unmarshal the rebuild response.
func parseInitramfsBuildInfo(body []byte) (sha256sum, kernelVersion string, err error) {
	var resp struct {
		SHA256        string `json:"sha256"`
		KernelVersion string `json:"kernel_version"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", "", err
	}
	return resp.SHA256, resp.KernelVersion, nil
}
