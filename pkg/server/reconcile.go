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
// corresponding goroutine — it will never progress. This pass marks them
// interrupted and resumable (Feature F3) rather than hard-failing them.
//
// Decision logic per image:
//  1. If build-state.json shows the last known phase, set resume_from_phase to
//     that phase so the resume endpoint can re-enter at the right point.
//  2. If <imagedir>/<id>/rootfs/ exists and is non-empty, the installer finished
//     but the process died during finalization — mark resumable at "finalizing".
//  3. All other cases — mark interrupted/resumable at "downloading_iso" so the
//     operator can resume from scratch via the Resume button.
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
		phase := s.classifyStuckPhase(img.ID)
		log.Warn().
			Str("image_id", img.ID).
			Str("image_name", img.Name).
			Str("resume_from_phase", phase).
			Msg("reconcile: marking stuck build as interrupted/resumable")

		if err := s.db.SetImageResumable(ctx, img.ID, phase); err != nil {
			log.Error().Err(err).Str("image_id", img.ID).
				Msg("reconcile: failed to set image resumable")
			continue
		}

		// Synthesise an interrupted entry in the in-memory BuildProgressStore so that
		// any UI client that happens to poll immediately after startup gets a
		// sensible response instead of a 404.
		h := s.buildProgress.Start(img.ID)
		h.Fail("build interrupted — server was restarted; use Resume to continue")

		reconciled++
	}

	if reconciled > 0 {
		log.Info().Int("count", reconciled).Msg("reconcile: marked stuck builds as interrupted/resumable")
	}
	return nil
}

// AutoResumeBuilds is called at startup when CLONR_BUILD_AUTO_RESUME=1 is set.
// It scans for resumable=true builds and re-submits them to the factory.
// The factory field is injected so this function doesn't import pkg/image (would be circular).
func (s *Server) AutoResumeBuilds(ctx context.Context, resumeFn func(imageID, phase string)) error {
	images, err := s.db.ListResumableImages(ctx)
	if err != nil {
		return err
	}
	if len(images) == 0 {
		return nil
	}
	for _, img := range images {
		phase, resumable, err := s.db.GetImageResumePhase(ctx, img.ID)
		if err != nil || !resumable {
			continue
		}
		log.Info().
			Str("image_id", img.ID).
			Str("phase", phase).
			Msg("auto-resume: re-submitting interrupted build")
		resumeFn(img.ID, phase)
	}
	return nil
}

// classifyStuckPhase inspects the image directory to determine what phase the
// build was in, returning the best phase to resume from.
func (s *Server) classifyStuckPhase(imageID string) string {
	imageDir := filepath.Join(s.cfg.ImageDir, imageID)

	// Check persisted build-state.json for the last known phase before restart.
	stateFile := filepath.Join(imageDir, "build-state.json")
	if data, err := os.ReadFile(stateFile); err == nil {
		var state api.BuildState
		if json.Unmarshal(data, &state) == nil {
			if state.Phase != "" && state.Phase != PhaseFailed && state.Phase != PhaseCanceled {
				return state.Phase
			}
		}
	}

	// Check if build.json (the successful build manifest) already exists.
	if _, err := os.Stat(filepath.Join(imageDir, "build.json")); err == nil {
		return PhaseFinalizing
	}

	// Check if rootfs directory exists and is non-empty (extraction finished).
	rootfsDir := filepath.Join(imageDir, "rootfs")
	if entries, err := os.ReadDir(rootfsDir); err == nil && len(entries) > 0 {
		return PhaseFinalizing
	}

	return PhaseDownloadingISO
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
