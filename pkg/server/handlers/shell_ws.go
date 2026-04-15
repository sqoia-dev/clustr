package handlers

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/creack/pty"
	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
	"github.com/sqoia-dev/clonr/pkg/api"
)

// systemdRunAvailable returns true if systemd-run(1) is present on PATH.
// Used to decide whether to wrap nspawn in a systemd-run --scope.
var systemdRunAvailable = func() bool {
	_, err := exec.LookPath("systemd-run")
	return err == nil
}()

// wrapNspawnInScope wraps a systemd-nspawn invocation in a systemd-run
// --scope --slice=clonr-shells.slice so the nspawn process runs outside the
// clonr-serverd cgroup and is not subject to its NoNewPrivileges=true or
// CapabilityBoundingSet restrictions.  Without this wrapping, nspawn fails
// with "Failed to move root directory: Operation not permitted" because it
// cannot call pivot_root(2) without CAP_SYS_ADMIN.
//
// Falls back to a direct systemd-nspawn invocation when systemd-run is not
// available (e.g. inside a Docker container or minimal install).
func wrapNspawnInScope(sessionID string, nspawnArgs []string) *exec.Cmd {
	if !systemdRunAvailable {
		return exec.Command("systemd-nspawn", nspawnArgs...)
	}
	scopeName := "clonr-shell-" + sessionID + ".scope"
	args := []string{
		"--scope",
		"--slice=clonr-shells.slice",
		"--unit=" + scopeName,
		"--quiet",
		"--",
		"systemd-nspawn",
	}
	args = append(args, nspawnArgs...)
	return exec.Command("systemd-run", args...)
}

// invalidateImageSidecar deletes the tar-sha256 sidecar file for imageID so
// that the next blob fetch recomputes the tarball hash from scratch.
//
// This is a temporary hotfix for the shell-session mutation problem: when a
// shell session writes files into the image rootfs (e.g. /root/.bash_history),
// the stored tar-sha256 no longer matches the new tarball content. Deploy agents
// verify the hash and fail with ExitDownload(5) when there is a mismatch.
//
// Proper fix: use an overlayfs-backed shell session so writes never touch the
// base rootfs (ADR-0009 overlayfs model). Until that lands, invalidating the
// sidecar forces a fresh hash computation on next blob fetch, which avoids the
// mismatch at the cost of one extra full-tar pass.
func invalidateImageSidecar(imageDir, imageID string) {
	sidecarPath := filepath.Join(imageDir, imageID, "tar-sha256")
	err := os.Remove(sidecarPath)
	if err != nil && !os.IsNotExist(err) {
		log.Warn().Err(err).
			Str("image_id", imageID).
			Str("path", sidecarPath).
			Msg("shell session close: failed to remove tar-sha256 sidecar")
		return
	}
	if !os.IsNotExist(err) {
		log.Info().
			Str("image_id", imageID).
			Str("path", sidecarPath).
			Msg("shell session closed — invalidated tar-sha256 sidecar; next blob fetch will recompute")
	}
}

// wsUpgrader allows all origins for the embedded UI (same-origin in prod,
// localhost in dev). In a future release this should be locked to the server's
// own origin.
var wsUpgrader = websocket.Upgrader{
	HandshakeTimeout: 10 * time.Second,
	CheckOrigin:      func(r *http.Request) bool { return true },
	ReadBufferSize:   4096,
	WriteBufferSize:  4096,
}

// wsMsg is the JSON envelope used by the browser xterm WebSocket protocol.
// Types:
//
//	"data"   — terminal I/O bytes (base64 encoded for reliable JSON transport)
//	"resize" — terminal resize, carries Cols and Rows
//	"ping"   — keepalive from client (server ignores)
type wsMsg struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"` // raw bytes as string (browser sends UTF-8)
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
}

// ShellWSHandler handles GET /api/v1/images/:id/shell-session/:sid/ws
//
// Upgrades the HTTP connection to WebSocket, forks a shell inside the image
// using systemd-nspawn (providing UTS, PID, and mount namespace isolation)
// with a PTY attached, then bidirectionally pipes:
//
//	client keystrokes → PTY stdin
//	PTY stdout        → client
//
// The session identified by :sid must already exist (created via
// POST /api/v1/images/:id/shell-session). On WebSocket close, the nspawn
// process is killed and the PTY is released.
func (h *FactoryHandler) ShellWS(w http.ResponseWriter, r *http.Request) {
	imageID := chi.URLParam(r, "id")
	sessionID := chi.URLParam(r, "sid")

	// Resolve the session — must already exist.
	sessions := h.Shells.ListSessions()
	var rootDir string
	for _, s := range sessions {
		if s.ID == sessionID && s.ImageID == imageID {
			rootDir = s.RootDir
			break
		}
	}
	if rootDir == "" {
		http.Error(w, "shell session not found or expired", http.StatusNotFound)
		return
	}

	// Upgrade to WebSocket. Auth token may be supplied via query param for
	// browsers that cannot set custom headers during WebSocket handshake.
	// (The bearer auth middleware already checked the Authorization header;
	// if we are here the auth layer already approved the request.)
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote the HTTP error response.
		log.Warn().Err(err).Str("session_id", sessionID).Msg("shell ws: upgrade failed")
		return
	}
	defer conn.Close()

	log.Info().Str("session_id", sessionID).Str("image_id", imageID).Str("rootdir", rootDir).
		Msg("shell ws: terminal session started")

	// Determine which shell binary is available inside the image.
	shell := "/bin/bash"
	if _, statErr := os.Stat(rootDir + shell); statErr != nil {
		shell = "/bin/sh"
	}

	// Use systemd-nspawn for proper namespace isolation: UTS (hostname),
	// PID, and mount namespaces are all handled automatically. This prevents
	// the shell from inheriting the management server's hostname and avoids
	// the need to manually bind-mount /proc, /sys, /dev, etc.
	//
	// Wrap in systemd-run --scope --slice=clonr-shells.slice so the nspawn
	// process runs outside the clonr-serverd cgroup and is not subject to its
	// NoNewPrivileges=true restriction. Without this, pivot_root(2) fails with
	// "Operation not permitted" because CAP_SYS_ADMIN cannot be used through
	// a NoNewPrivileges boundary.
	nspawnArgs := []string{
		"--quiet",
		"-D", rootDir,
		"--",
		shell, "--login",
	}
	cmd := wrapNspawnInScope(sessionID, nspawnArgs)
	cmd.Env = []string{
		"TERM=xterm-256color",
		"HOME=/root",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"SHELL=" + shell,
	}

	ptmx, err := pty.Start(cmd)
	if err != nil {
		writeWSError(conn, fmt.Sprintf("failed to start shell: %v", err))
		log.Error().Err(err).Str("session_id", sessionID).Msg("shell ws: pty start failed")
		return
	}
	defer func() {
		ptmx.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		log.Info().Str("session_id", sessionID).Str("image_id", imageID).
			Msg("shell ws: terminal session ended")
		// Invalidate the tar-sha256 sidecar so the next blob fetch recomputes
		// the tarball hash. The shell session may have written files into the
		// rootfs (e.g. /root/.bash_history) that would cause a hash mismatch
		// and fail the deploy agent's integrity check (ExitDownload 5).
		//
		// TODO(ADR-0009): remove this hotfix once the overlayfs shell model
		// lands — overlayfs sessions write into an ephemeral upper layer and
		// never mutate the base rootfs.
		invalidateImageSidecar(h.ImageDir, imageID)
	}()

	// PTY → WebSocket: stream shell output to browser.
	ptyClosed := make(chan struct{})
	go func() {
		defer close(ptyClosed)
		buf := make([]byte, 4096)
		for {
			n, readErr := ptmx.Read(buf)
			if n > 0 {
				msg := wsMsg{Type: "data", Data: string(buf[:n])}
				if writeErr := conn.WriteJSON(msg); writeErr != nil {
					return
				}
			}
			if readErr != nil {
				// PTY closed — shell exited.
				return
			}
		}
	}()

	// WebSocket → PTY: relay client keystrokes and resize events.
	for {
		var msg wsMsg
		if err := conn.ReadJSON(&msg); err != nil {
			// Client disconnected.
			break
		}

		switch msg.Type {
		case "data":
			if _, writeErr := io.WriteString(ptmx, msg.Data); writeErr != nil {
				log.Debug().Err(writeErr).Str("session_id", sessionID).Msg("shell ws: pty write error")
				return
			}
		case "resize":
			if msg.Cols > 0 && msg.Rows > 0 {
				_ = pty.Setsize(ptmx, &pty.Winsize{Cols: msg.Cols, Rows: msg.Rows})
			}
		}

		// If the PTY closed while we were reading, stop.
		select {
		case <-ptyClosed:
			writeWSError(conn, "shell exited")
			return
		default:
		}
	}
}

// writeWSError sends an error message to the WebSocket client as terminal output.
func writeWSError(conn *websocket.Conn, msg string) {
	_ = conn.WriteJSON(wsMsg{Type: "data", Data: "\r\n\033[31m[clonr] " + msg + "\033[0m\r\n"})
}

// ActiveDeploys handles GET /api/v1/images/:id/active-deploys
// Scans recent deploy log entries (last 30 minutes, component=deploy) for any
// that reference this image ID. Returns a count and isActive flag so the
// browser shell modal can show a warning when opening a shell on a live image.
func (h *FactoryHandler) ActiveDeploys(w http.ResponseWriter, r *http.Request) {
	imageID := chi.URLParam(r, "id")

	since := time.Now().Add(-30 * time.Minute)
	entries, err := h.DB.QueryLogs(r.Context(), api.LogFilter{
		Component: "deploy",
		Since:     &since,
		Limit:     500,
	})
	if err != nil {
		writeError(w, err)
		return
	}

	// Count entries that mention this image ID in any field.
	activeCount := 0
	for _, e := range entries {
		// Check if any log field contains the image ID.
		found := false
		for _, v := range e.Fields {
			if s, ok := v.(string); ok && s == imageID {
				found = true
				break
			}
		}
		if found {
			activeCount++
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"image_id":     imageID,
		"active_count": activeCount,
		"is_active":    activeCount > 0,
	})
}
