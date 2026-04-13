package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/sqoia-dev/clonr/pkg/api"
)

// ReconcileStuckBuilds finds all images in "building" state in the database and
// determines whether they have a real build process behind them. Since build
// goroutines are in-process, any "building" image after a server restart has no
// corresponding goroutine — it will never progress. This pass marks them failed.
//
// Decision logic per image:
//  1. If build-state.json (written by the progress store on each update) shows
//     the last known phase was "complete", the final DB commit must have failed;
//     attempt to finalize from disk.
//  2. If <imagedir>/<id>/rootfs/ exists and is non-empty, the installer finished
//     but the process died during finalization — mark failed so the admin can
//     delete and retry.
//  3. All other cases (no rootfs, partial work, no files at all) — mark failed
//     with "build interrupted — server was restarted before completion".
//
// Call this once at startup, after db.Open() and before ListenAndServe().
func (s *Server) ReconcileStuckBuilds(ctx context.Context) error {
	images, err := s.db.ListBaseImages(ctx, string(api.ImageStatusBuilding))
	if err != nil {
		return err
	}
	if len(images) == 0 {
		return nil
	}

	reconciled := 0
	for _, img := range images {
		reason := s.classifyStuckBuild(img.ID)
		log.Warn().
			Str("image_id", img.ID).
			Str("image_name", img.Name).
			Str("reason", reason).
			Msg("reconcile: marking stuck build as failed")

		if err := s.db.UpdateBaseImageStatus(ctx, img.ID,
			api.ImageStatusError, reason); err != nil {
			log.Error().Err(err).Str("image_id", img.ID).
				Msg("reconcile: failed to update image status")
			continue
		}

		// Synthesise a failed entry in the in-memory BuildProgressStore so that
		// any UI client that happens to poll immediately after startup gets a
		// sensible response instead of a 404.
		h := s.buildProgress.Start(img.ID)
		h.Fail(reason)

		reconciled++
	}

	if reconciled > 0 {
		log.Info().Int("count", reconciled).Msg("reconcile: marked stuck builds as failed")
	}
	return nil
}

// classifyStuckBuild inspects the image directory to determine why the build
// is stuck and return an appropriate human-readable error message.
func (s *Server) classifyStuckBuild(imageID string) string {
	imageDir := filepath.Join(s.cfg.ImageDir, imageID)

	// Check persisted build-state.json for the last known phase before restart.
	stateFile := filepath.Join(imageDir, "build-state.json")
	if data, err := os.ReadFile(stateFile); err == nil {
		var state api.BuildState
		if json.Unmarshal(data, &state) == nil {
			if state.Phase == PhaseComplete {
				// Rare: build finished but DB finalize call didn't commit.
				return "build completed but server restarted before database record could be finalized — delete and retry"
			}
			if state.Phase != "" && state.Phase != PhaseFailed && state.Phase != PhaseCanceled {
				return "build interrupted — server was restarted during phase: " + state.Phase
			}
		}
	}

	// Check if build.json (the successful build manifest) already exists.
	if _, err := os.Stat(filepath.Join(imageDir, "build.json")); err == nil {
		return "build completed but server restarted before database record could be finalized — delete and retry"
	}

	// Check if rootfs directory exists and is non-empty (extraction finished).
	rootfsDir := filepath.Join(imageDir, "rootfs")
	if entries, err := os.ReadDir(rootfsDir); err == nil && len(entries) > 0 {
		return "build interrupted — server was restarted after rootfs extraction but before finalization — delete and retry"
	}

	return "build interrupted — server was restarted before completion"
}

// buildStateOnDisk is the structure persisted to <imagedir>/<id>/build-state.json
// on every BuildProgressStore update. Used by ReconcileStuckBuilds and admins
// doing post-mortems to see the last known build state before a restart.
type buildStateOnDisk struct {
	ImageID      string    `json:"image_id"`
	Phase        string    `json:"phase"`
	BytesDone    int64     `json:"bytes_done,omitempty"`
	BytesTotal   int64     `json:"bytes_total,omitempty"`
	ErrorMessage string    `json:"error_message,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	ElapsedMS    int64     `json:"elapsed_ms,omitempty"`
}
