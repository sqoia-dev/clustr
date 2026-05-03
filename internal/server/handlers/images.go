package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/sqoia-dev/clustr/internal/db"
	"github.com/sqoia-dev/clustr/internal/image"
	"github.com/sqoia-dev/clustr/internal/webhook"
	"github.com/sqoia-dev/clustr/pkg/api"
	"github.com/sqoia-dev/clustr/pkg/reconcile"
)

// defaultBlobMaxConcurrent is the default maximum number of simultaneous blob
// streams. Each stream can saturate a LAN link and consume significant memory
// for kernel socket buffers + the tar subprocess; limit to prevent OOM when an
// entire fleet reimages simultaneously.
const defaultBlobMaxConcurrent = 8

// ImagesHandler handles all /api/v1/images routes.
// ImageEventStoreIface is the subset of ImageEventStore used by ImagesHandler.
// Keeping it as an interface avoids a circular import between handlers and server.
type ImageEventStoreIface interface {
	Publish(event api.ImageEvent)
	Subscribe() (ch <-chan api.ImageEvent, cancel func())
}

// ImageReconcilerIface is the subset of server.Server used by ImagesHandler and
// ReimageHandler for the reconcile endpoint and pre-deploy guard. Using an
// interface avoids a circular import between handlers and server.
type ImageReconcilerIface interface {
	ReconcileImage(ctx context.Context, imageID string, opts reconcile.Opts) (*reconcile.Result, error)
}

type ImagesHandler struct {
	DB       *db.DB
	ImageDir string
	// Factory is used by FromURL to delegate URL-based image ingest through the
	// full pipeline (pull → extract → ISO build for .iso URLs). Required for
	// FromURL to function; returns 501 if nil.
	Factory *image.Factory
	// Progress is used by DeleteImage to check for active deploys.
	// It is optional — when nil, the active-deploy guard is skipped.
	Progress ProgressStoreIface
	// Audit records state-changing events.
	Audit *db.AuditService
	// GetActorInfo returns (actorID, actorLabel) for audit records.
	GetActorInfo func(r *http.Request) (id, label string)
	// WebhookDispatcher, when non-nil, fires image.ready on blob finalize (S4-2).
	WebhookDispatcher *webhook.Dispatcher
	// ImageEvents, when non-nil, receives lifecycle events for SSE fan-out.
	// Optional — if nil, no SSE events are published.
	ImageEvents ImageEventStoreIface
	// ImageReconciler, when non-nil, handles POST /images/:id/reconcile (#251).
	ImageReconciler ImageReconcilerIface
	// blobSem is the semaphore controlling max concurrent blob streams.
	// Initialised once on first use via blobSemaphoreOnce; always access via blobSemaphore().
	blobSem     chan struct{}
	blobSemOnce sync.Once
	// activeBlobStreams tracks the current count for metrics/logging.
	activeBlobStreams atomic.Int64
}

// blobSemaphore returns the blob concurrency semaphore, reading
// CLUSTR_BLOB_MAX_CONCURRENCY (preferred) or CLUSTR_BLOB_MAX_CONCURRENT (legacy)
// from the environment on first call.
// Initialization is protected by sync.Once to prevent data races when
// multiple concurrent requests hit DownloadBlob simultaneously.
func (h *ImagesHandler) blobSemaphore() chan struct{} {
	h.blobSemOnce.Do(func() {
		h.blobSem = make(chan struct{}, blobConcurrencyLimit())
	})
	return h.blobSem
}

// ListImages handles GET /api/v1/images
// Accepts optional ?page= and ?per_page= (default 50) for pagination.
// When pagination params are absent the full list is returned (backward compatible).
//
// ?kind=initramfs — return only images with build_method="initramfs"
// ?kind=base      — return only images that are NOT initramfs (build_method != "initramfs")
// (no kind param) — return all images (backward compatible)
func (h *ImagesHandler) ListImages(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	tag := r.URL.Query().Get("tag")
	kind := r.URL.Query().Get("kind")
	rawPage, rawPerPage, paging := parsePaginationQuery(r)

	images, err := h.DB.ListBaseImages(r.Context(), status, tag)
	if err != nil {
		log.Error().Err(err).Msg("list images")
		writeError(w, err)
		return
	}
	if images == nil {
		images = []api.BaseImage{}
	}

	// Filter by kind if requested. "initramfs" images are identified by build_method.
	if kind == "initramfs" {
		filtered := images[:0]
		for _, img := range images {
			if img.BuildMethod == "initramfs" {
				filtered = append(filtered, img)
			}
		}
		images = filtered
	} else if kind == "base" {
		filtered := images[:0]
		for _, img := range images {
			if img.BuildMethod != "initramfs" {
				filtered = append(filtered, img)
			}
		}
		images = filtered
	}

	total := len(images)
	resp := api.ListImagesResponse{Total: total}
	if paging {
		start, end, p := paginate(total, rawPage, rawPerPage)
		resp.Images = images[start:end]
		resp.Page = p.page
		resp.PerPage = p.perPage
		if end < total {
			resp.NextCursor = p.page + 1
		}
	} else {
		resp.Images = images
	}
	writeJSON(w, http.StatusOK, resp)
}

// CreateImage handles POST /api/v1/images
func (h *ImagesHandler) CreateImage(w http.ResponseWriter, r *http.Request) {
	var req api.CreateImageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeValidationError(w, "invalid JSON body")
		return
	}

	if req.Name == "" {
		writeValidationError(w, "name is required")
		return
	}
	if req.Format == "" {
		writeValidationError(w, "format is required")
		return
	}

	img := api.BaseImage{
		ID:         uuid.New().String(),
		Name:       req.Name,
		Version:    req.Version,
		OS:         req.OS,
		Arch:       req.Arch,
		Status:     api.ImageStatusBuilding,
		Format:     req.Format,
		DiskLayout: req.DiskLayout,
		Tags:       req.Tags,
		SourceURL:  req.SourceURL,
		Notes:      req.Notes,
	}
	if img.Tags == nil {
		img.Tags = []string{}
	}

	if err := h.DB.CreateBaseImage(r.Context(), img); err != nil {
		log.Error().Err(err).Msg("create image")
		writeError(w, err)
		return
	}

	if h.Audit != nil {
		aID, aLabel := "", ""
		if h.GetActorInfo != nil {
			aID, aLabel = h.GetActorInfo(r)
		}
		h.Audit.Record(r.Context(), aID, aLabel, db.AuditActionImageCreate, "image", img.ID,
			r.RemoteAddr, nil, map[string]string{"name": img.Name, "format": string(img.Format)})
	}

	if h.ImageEvents != nil {
		h.ImageEvents.Publish(api.ImageEvent{Kind: api.ImageEventCreated, Image: &img, ID: img.ID})
	}
	writeJSON(w, http.StatusCreated, img)
}

// GetImage handles GET /api/v1/images/:id
func (h *ImagesHandler) GetImage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	img, err := h.DB.GetBaseImage(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}

	// Lazy-detect architecture when the DB column is blank (images that were
	// created before arch detection was implemented). Same pattern as the
	// initramfs kernel-version lazy-extract.
	if img.Arch == "" {
		ctx := r.Context()
		arch, detectErr := h.detectArch(ctx, img)
		if detectErr == nil && arch != "" {
			img.Arch = arch
			// Back-fill in the DB so subsequent requests skip detection.
			if dbErr := h.DB.UpdateBaseImageArch(ctx, id, arch); dbErr != nil {
				log.Debug().Err(dbErr).Str("image_id", id).
					Msg("image: failed to back-fill arch in db (non-fatal)")
			}
		} else if detectErr != nil {
			log.Debug().Err(detectErr).Str("image_id", id).
				Msg("image: lazy arch detection failed (non-fatal)")
		}
	}

	writeJSON(w, http.StatusOK, img)
}

// detectArch attempts to determine the CPU architecture of img by reading the
// ELF header of a well-known binary from the image's rootfs.
//
// For "filesystem" format images the rootfs is an extracted directory at
// <ImageDir>/<id>/rootfs/. For all other formats (block images and legacy
// uploaded blobs) it is a gzip-compressed tar archive at the blob_path
// recorded in the DB.
func (h *ImagesHandler) detectArch(ctx context.Context, img api.BaseImage) (string, error) {
	if img.Format == api.ImageFormatFilesystem {
		rootfsDir := filepath.Join(h.ImageDir, img.ID, "rootfs")
		return image.DetectArchFromRootfsDir(rootfsDir)
	}

	// For blob-based images, retrieve the path from the DB.
	blobPath, err := h.DB.GetBlobPath(ctx, img.ID)
	if err != nil {
		return "", fmt.Errorf("image: get blob path for arch detect: %w", err)
	}
	if blobPath == "" {
		return "", fmt.Errorf("image: blob path empty, cannot detect arch")
	}
	return image.DetectArchFromTarball(blobPath)
}

// ArchiveImage handles DELETE /api/v1/images/:id (legacy — kept for back-compat).
func (h *ImagesHandler) ArchiveImage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.DB.ArchiveBaseImage(r.Context(), id); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DeleteImage handles DELETE /api/v1/images/:id (real delete).
//
// Query params:
//
//	?force=true — unassign all nodes using the image and delete anyway.
//
// Rejection rules (in order):
//  1. Image not found → 404.
//  2. Active deployment in progress (any phase != "complete"/"error") → 409.
//  3. Nodes referencing the image (without force) → 409 with node list.
//
// On success:
//   - Removes the blob directory from disk.
//   - Deletes the DB record.
//   - Returns 204 No Content.
func (h *ImagesHandler) DeleteImage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	force := r.URL.Query().Get("force") == "true"
	ctx := r.Context()

	// Confirm the image exists.
	img, err := h.DB.GetBaseImage(ctx, id)
	if err != nil {
		writeError(w, err)
		return
	}

	// Guard: reject if the image is currently being deployed (even with force).
	// We check the progress store for any non-terminal deploy phases.
	if h.Progress != nil {
		for _, p := range h.Progress.List() {
			phase := p.Phase
			if phase == "complete" || phase == "error" || phase == "" {
				continue
			}
			// Determine which image is being deployed by checking the log fields.
			// The progress entry only has node_mac; we fetch the node's assigned image.
			node, lookupErr := h.DB.GetNodeConfigByMAC(ctx, p.NodeMAC)
			if lookupErr != nil {
				continue
			}
			if node.BaseImageID == id {
				writeJSON(w, http.StatusConflict, api.ErrorResponse{
					Error: "image is currently being deployed to one or more nodes — wait for the deployment to complete or fail before deleting",
					Code:  "image_deploying",
				})
				return
			}
		}
	}

	// Guard: reject if nodes reference this image (unless force).
	nodes, err := h.DB.ListNodesByBaseImageID(ctx, id)
	if err != nil {
		log.Error().Err(err).Str("image_id", id).Msg("delete image: list nodes")
		writeError(w, err)
		return
	}
	if len(nodes) > 0 && !force {
		writeJSON(w, http.StatusConflict, api.ImageInUseResponse{
			Error: "image is assigned to one or more nodes; use ?force=true to unassign and delete",
			Code:  "image_in_use",
			Nodes: nodes,
		})
		return
	}

	// Force path: unassign all nodes first.
	if len(nodes) > 0 && force {
		if err := h.DB.ClearBaseImageOnNodes(ctx, id); err != nil {
			log.Error().Err(err).Str("image_id", id).Msg("delete image: clear nodes")
			writeError(w, err)
			return
		}
		log.Warn().Str("image_id", id).Int("node_count", len(nodes)).
			Msg("delete image: force-unassigned nodes")
	}

	// Remove blob directory from disk (best-effort — don't fail if missing).
	blobDir := filepath.Join(h.ImageDir, id)
	if rmErr := os.RemoveAll(blobDir); rmErr != nil && !os.IsNotExist(rmErr) {
		log.Error().Err(rmErr).Str("path", blobDir).Msg("delete image: remove blob dir")
		// Non-fatal: continue to remove the DB record so the image isn't
		// permanently undeletable due to a missing directory.
	}
	// Also remove legacy flat blob file (uploaded via /blob endpoint).
	legacyBlob := filepath.Join(h.ImageDir, id+".blob")
	_ = os.Remove(legacyBlob)

	// Delete the DB record.
	if err := h.DB.DeleteBaseImage(ctx, id); err != nil {
		log.Error().Err(err).Str("image_id", id).Msg("delete image: db delete")
		writeError(w, err)
		return
	}

	log.Warn().
		Str("image_id", id).
		Str("image_name", img.Name).
		Bool("force", force).
		Int("nodes_unassigned", len(nodes)).
		Msg("image deleted")

	if h.Audit != nil {
		aID, aLabel := "", ""
		if h.GetActorInfo != nil {
			aID, aLabel = h.GetActorInfo(r)
		}
		h.Audit.Record(r.Context(), aID, aLabel, db.AuditActionImageDelete, "image", id,
			r.RemoteAddr, map[string]string{"name": img.Name}, nil)
	}

	// Publish deletion event — image is nil since it's gone.
	if h.ImageEvents != nil {
		h.ImageEvents.Publish(api.ImageEvent{Kind: api.ImageEventDeleted, ID: id})
	}
	w.WriteHeader(http.StatusNoContent)
}

// CancelBuild handles POST /api/v1/images/:id/cancel (C3-5).
// Marks a building image as errored so it is no longer counted as active.
// The background goroutine will discover the status change and exit cleanly;
// there is no direct goroutine kill because per-build cancel functions are
// not yet tracked (full goroutine cancel is a v1.3 enhancement — D-N-3).
// Returns 200 on success, 409 if the image is not currently building.
func (h *ImagesHandler) CancelBuild(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	img, err := h.DB.GetBaseImage(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	if img.Status != api.ImageStatusBuilding {
		writeJSON(w, http.StatusConflict, api.ErrorResponse{
			Error: "image is not currently building",
			Code:  "conflict",
		})
		return
	}
	if err := h.DB.UpdateBaseImageStatus(r.Context(), id, api.ImageStatusError, "cancelled by user"); err != nil {
		log.Error().Err(err).Str("image_id", id).Msg("cancel build: update status")
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// GetImageStatus handles GET /api/v1/images/:id/status
func (h *ImagesHandler) GetImageStatus(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	img, err := h.DB.GetBaseImage(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            img.ID,
		"status":        img.Status,
		"error_message": img.ErrorMessage,
	})
}

// UpdateImageTags handles PUT /api/v1/images/:id/tags (S2-3).
// Body: {"tags": ["tag1", "tag2"]}
// Replaces the entire tags array for the image atomically.
func (h *ImagesHandler) UpdateImageTags(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeValidationError(w, "invalid JSON body")
		return
	}
	if err := h.DB.UpdateImageTags(r.Context(), id, req.Tags); err != nil {
		writeError(w, err)
		return
	}
	img, err := h.DB.GetBaseImage(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	if h.ImageEvents != nil {
		imgCopy := img
		h.ImageEvents.Publish(api.ImageEvent{Kind: api.ImageEventUpdated, Image: &imgCopy, ID: img.ID})
	}
	writeJSON(w, http.StatusOK, img)
}

// GetDiskLayout handles GET /api/v1/images/:id/disklayout
func (h *ImagesHandler) GetDiskLayout(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	img, err := h.DB.GetBaseImage(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, img.DiskLayout)
}

// PutDiskLayout handles PUT /api/v1/images/:id/disklayout
func (h *ImagesHandler) PutDiskLayout(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var layout api.DiskLayout
	if err := json.NewDecoder(r.Body).Decode(&layout); err != nil {
		writeValidationError(w, "invalid JSON body")
		return
	}

	// Fetch existing image to confirm it exists and is mutable.
	img, err := h.DB.GetBaseImage(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	if img.Status == api.ImageStatusArchived {
		writeValidationError(w, "cannot modify an archived image")
		return
	}

	img.DiskLayout = layout
	// Re-create by updating: simplest path since layout is stored as JSON column.
	// We use UpdateBaseImageStatus as a no-op to keep status, then update via SetBlobPath workaround.
	// Instead, we run a targeted update directly.
	if err := h.DB.UpdateDiskLayout(r.Context(), id, layout); err != nil {
		log.Error().Err(err).Msg("update disk layout")
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, layout)
}

// PutInstallInstructions handles PUT /api/v1/images/:id/install-instructions (#147).
// Body: {"instructions": [{opcode, target, payload}, ...]}
// Replaces the entire install_instructions array atomically. Validates each
// instruction opcode before writing. Archived images cannot be modified.
func (h *ImagesHandler) PutInstallInstructions(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req struct {
		Instructions []api.InstallInstruction `json:"instructions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeValidationError(w, "invalid JSON body")
		return
	}

	// Validate all instructions before touching the DB.
	validOpcodes := map[string]bool{"modify": true, "overwrite": true, "script": true}
	for i, instr := range req.Instructions {
		if !validOpcodes[instr.Opcode] {
			writeValidationError(w, fmt.Sprintf("instruction[%d]: opcode %q is not valid (must be modify, overwrite, or script)", i, instr.Opcode))
			return
		}
		if instr.Target == "" {
			writeValidationError(w, fmt.Sprintf("instruction[%d]: target is required", i))
			return
		}
	}

	// Confirm image exists and is mutable.
	img, err := h.DB.GetBaseImage(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	if img.Status == api.ImageStatusArchived {
		writeValidationError(w, "cannot modify an archived image")
		return
	}

	if err := h.DB.UpdateInstallInstructions(r.Context(), id, req.Instructions); err != nil {
		log.Error().Err(err).Msg("update install instructions")
		writeError(w, err)
		return
	}

	img, err = h.DB.GetBaseImage(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	if h.ImageEvents != nil {
		imgCopy := img
		h.ImageEvents.Publish(api.ImageEvent{Kind: api.ImageEventUpdated, Image: &imgCopy, ID: img.ID})
	}
	writeJSON(w, http.StatusOK, img)
}

// defaultBlobMaxBytes is the default upload size cap (50 GiB).
// Override via CLUSTR_BLOB_MAX_SIZE (bytes).
const defaultBlobMaxBytes = 50 * 1024 * 1024 * 1024 // 50 GiB

// defaultBlobDownloadTimeout is the default context deadline for background
// image URL downloads. Override via CLUSTR_BLOB_DOWNLOAD_TIMEOUT (e.g. "2h").
// Hard minimum of 1 minute is enforced to prevent misconfigurations that would
// cause all downloads to fail immediately.
const (
	defaultBlobDownloadTimeout = 6 * time.Hour
	minBlobDownloadTimeout     = time.Minute
)

// blobDownloadTimeout returns the effective download timeout by reading
// CLUSTR_BLOB_DOWNLOAD_TIMEOUT from the environment on every call.
func blobDownloadTimeout() time.Duration {
	if v := os.Getenv("CLUSTR_BLOB_DOWNLOAD_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= minBlobDownloadTimeout {
			return d
		}
	}
	return defaultBlobDownloadTimeout
}

// blobMaxBytes returns the effective upload size limit by reading
// CLUSTR_BLOB_MAX_SIZE from the environment on every call (cheap string parse,
// avoids package-level init ordering issues).
func blobMaxBytes() int64 {
	if v := os.Getenv("CLUSTR_BLOB_MAX_SIZE"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return defaultBlobMaxBytes
}

// UploadBlob handles POST /api/v1/images/:id/blob
// Streams the request body to disk and finalizes the image record.
// The SHA256 is always computed server-side from the bytes as they stream in;
// if the client supplied X-Checksum-SHA256, we compare and reject on mismatch.
// Upload size is capped at CLUSTR_BLOB_MAX_SIZE (default 50 GiB) to prevent
// OOM and disk exhaustion from unbounded uploads.
func (h *ImagesHandler) UploadBlob(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	maxBytes := blobMaxBytes()

	// Reject early if Content-Length is provided and already exceeds the limit.
	if r.ContentLength > 0 && r.ContentLength > maxBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
			"error": fmt.Sprintf("blob exceeds maximum allowed size of %d bytes", maxBytes),
			"code":  "blob_too_large",
		})
		return
	}

	img, err := h.DB.GetBaseImage(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	if img.Status == api.ImageStatusArchived {
		writeValidationError(w, "cannot upload blob for an archived image")
		return
	}

	if err := os.MkdirAll(h.ImageDir, 0o755); err != nil {
		log.Error().Err(err).Msg("create image dir")
		writeError(w, err)
		return
	}

	blobPath := filepath.Join(h.ImageDir, id+".blob")
	f, err := os.Create(blobPath)
	if err != nil {
		log.Error().Err(err).Str("path", blobPath).Msg("create blob file")
		writeError(w, err)
		return
	}
	defer f.Close()

	// Wrap r.Body with MaxBytesReader to enforce the upload size limit at the
	// streaming layer. If the body exceeds maxBytes, io.Copy will return an error
	// and we clean up the partial file before responding 413.
	limitedBody := http.MaxBytesReader(w, r.Body, maxBytes)

	// Compute SHA256 server-side as we write, using TeeReader.
	// This ensures the stored checksum reflects the actual bytes written, not
	// a value supplied (and potentially incorrect or malicious) from the client.
	hasher := sha256.New()
	n, err := io.Copy(f, io.TeeReader(limitedBody, hasher))
	if err != nil {
		_ = os.Remove(blobPath)
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
				"error": fmt.Sprintf("blob exceeds maximum allowed size of %d bytes", maxBytes),
				"code":  "blob_too_large",
			})
			return
		}
		log.Error().Err(err).Msg("write blob")
		writeError(w, err)
		return
	}
	serverChecksum := hex.EncodeToString(hasher.Sum(nil))

	// If the client provided a checksum header, verify it matches what we computed.
	if clientChecksum := r.Header.Get("X-Checksum-SHA256"); clientChecksum != "" {
		if clientChecksum != serverChecksum {
			_ = os.Remove(blobPath)
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("checksum mismatch: client sent %s, server computed %s", clientChecksum, serverChecksum),
				"code":  "checksum_mismatch",
			})
			return
		}
	}

	if err := h.DB.SetBlobPath(r.Context(), id, blobPath); err != nil {
		writeError(w, err)
		return
	}
	// Always persist the server-computed checksum, never the client-supplied one.
	if err := h.DB.FinalizeBaseImage(r.Context(), id, n, serverChecksum); err != nil {
		writeError(w, err)
		return
	}

	// S4-2: Fire image.ready webhook now that the blob is finalized.
	if h.WebhookDispatcher != nil {
		h.WebhookDispatcher.Dispatch(r.Context(), webhook.EventImageReady, webhook.Payload{
			ImageID: id,
		})
	}

	updated, err := h.DB.GetBaseImage(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	if h.ImageEvents != nil {
		updatedCopy := updated
		h.ImageEvents.Publish(api.ImageEvent{Kind: api.ImageEventFinalized, Image: &updatedCopy, ID: updated.ID})
	}
	writeJSON(w, http.StatusOK, updated)
}

// DownloadBlob handles GET /api/v1/images/:id/blob
//
// For "block" format images: streams the pre-packed blob file from disk.
// For "filesystem" format images: streams an uncompressed tar of rootfs/ on the fly.
//
// Two env-var knobs control throughput:
//
//	CLUSTR_BLOB_MAX_CONCURRENCY — maximum simultaneous blob streams (global semaphore).
//	                              Also accepts legacy name CLUSTR_BLOB_MAX_CONCURRENT.
//	                              Zero or unset = use default (8).
//	CLUSTR_BLOB_MAX_BPS         — per-stream byte rate cap in bytes/sec (token bucket).
//	                              Zero or unset = unlimited.
func (h *ImagesHandler) DownloadBlob(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	// Concurrency cap: limit simultaneous blob streams to prevent OOM under
	// fleet-wide reimage bursts. Return 503 with Retry-After when at capacity.
	sem := h.blobSemaphore()
	select {
	case sem <- struct{}{}:
		// Acquired a slot. Release it when the handler returns.
		active := h.activeBlobStreams.Add(1)
		defer func() {
			<-sem
			h.activeBlobStreams.Add(-1)
		}()
		log.Info().Int64("active_streams", active).Int("cap", cap(sem)).Msg("blob stream: acquired slot")
	default:
		active := h.activeBlobStreams.Load()
		log.Warn().Int64("active_streams", active).Int("cap", cap(sem)).
			Msg("blob stream: semaphore full — returning 503 to client")
		w.Header().Set("Retry-After", "10")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, `{"error":"too many concurrent blob streams (max %d) — retry in 10s"}`, cap(sem))
		return
	}

	// Apply per-stream bandwidth cap if CLUSTR_BLOB_MAX_BPS is set.
	// The token bucket is created fresh for each stream so each client gets its own
	// quota independent of other concurrent streams.
	if bps := blobMaxBPS(); bps > 0 {
		tb := newTokenBucket(bps)
		w = newRateLimitedResponseWriter(w, r.Context(), tb)
		log.Debug().Int64("bps", bps).Msg("blob stream: bandwidth cap active")
	}

	img, err := h.DB.GetBaseImage(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}

	// For filesystem images, check whether a pre-baked tar exists (blob_path
	// points to a file). If so, serve it directly — the DB checksum is the tar's
	// sha256, so the client's integrity check passes by construction.
	// Fall back to streamFilesystemBlob for legacy images whose blob_path still
	// points to the rootfs directory (built before this fix was deployed).
	if img.Format == api.ImageFormatFilesystem {
		blobPath, bpErr := h.DB.GetBlobPath(r.Context(), id)
		if bpErr == nil && blobPath != "" {
			if st, statErr := os.Stat(blobPath); statErr == nil && !st.IsDir() {
				// Pre-baked tar — serve as a static file.
				f, openErr := os.Open(blobPath)
				if openErr != nil {
					if os.IsNotExist(openErr) {
						writeError(w, api.ErrNotFound)
						return
					}
					log.Error().Err(openErr).Str("path", blobPath).Msg("open pre-baked tar blob")
					writeError(w, openErr)
					return
				}
				defer f.Close()
				if img.Checksum != "" {
					w.Header().Set("X-Clustr-Blob-SHA256", img.Checksum)
				}
				w.Header().Set("Content-Type", "application/x-tar")
				w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.tar"`, img.ID))
				http.ServeContent(w, r, filepath.Base(blobPath), st.ModTime(), f)
				return
			}
		}
		// Legacy path: blob_path is a directory or unset — stream tar on the fly.
		h.streamFilesystemBlob(w, r, img)
		return
	}

	// Default: serve the pre-packed blob file (block images, legacy uploads).
	blobPath, err := h.DB.GetBlobPath(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	if blobPath == "" {
		writeValidationError(w, "blob not yet uploaded for this image")
		return
	}

	f, err := os.Open(blobPath)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, api.ErrNotFound)
			return
		}
		log.Error().Err(err).Str("path", blobPath).Msg("open blob")
		writeError(w, err)
		return
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		writeError(w, err)
		return
	}

	// For block images, the DB checksum covers the blob file bytes directly —
	// safe to advertise to the client for end-to-end integrity verification.
	if img.Checksum != "" {
		w.Header().Set("X-Clustr-Blob-SHA256", img.Checksum)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, filepath.Base(blobPath), stat.ModTime(), f)
}

// tarChecksumPath returns the path of the tar-checksum sidecar file for a
// filesystem-format image. The sidecar stores the sha256 of the tar stream
// produced by streamFilesystemBlob. It is computed on the first successful
// stream and reused on all subsequent streams.
//
// Sprint 1 compromise: the DB `checksum` column for filesystem images holds a
// directory-level hash (file-by-file sha256 XOR), which does not match the tar
// stream bytes. Until the image finalization step computes a canonical tar
// checksum during build, we compute and cache it on first-stream here.
func tarChecksumPath(imageDir, imageID string) string {
	return filepath.Join(imageDir, imageID, "tar-sha256")
}

// loadTarChecksum reads the cached tar sha256 for imageID, returning "" if not
// yet computed.
func loadTarChecksum(imageDir, imageID string) string {
	data, err := os.ReadFile(tarChecksumPath(imageDir, imageID))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// saveTarChecksum persists the tar sha256 to the sidecar file.
func saveTarChecksum(imageDir, imageID, checksum string) {
	path := tarChecksumPath(imageDir, imageID)
	if err := os.WriteFile(path, []byte(checksum+"\n"), 0o644); err != nil {
		log.Warn().Err(err).Str("image_id", imageID).Str("path", path).
			Msg("blob stream: failed to persist tar checksum sidecar — next stream will recompute")
	}
}

// streamFilesystemBlob tars the rootfs/ directory of a filesystem-format image
// and streams it directly to the response writer as an uncompressed tar archive.
// No Content-Length is set — the response is streamed.
//
// The tar subprocess is NOT bound to the request context. Binding it caused
// SIGKILL mid-stream under concurrent load when the http.Server or an upstream
// component cancelled the context (e.g. write deadline, client-side timeout).
// Instead, we run tar freely and handle context cancellation manually: on client
// disconnect we give tar 2 seconds to flush remaining buffered output, then kill.
// A client disconnect is treated as informational, not a server error.
func (h *ImagesHandler) streamFilesystemBlob(w http.ResponseWriter, r *http.Request, img api.BaseImage) {
	rootfsPath := filepath.Join(h.ImageDir, img.ID, "rootfs")

	if _, err := os.Stat(rootfsPath); err != nil {
		if os.IsNotExist(err) {
			log.Error().Str("image_id", img.ID).Str("path", rootfsPath).Msg("blob stream: rootfs dir not found")
			writeError(w, api.ErrNotFound)
			return
		}
		log.Error().Err(err).Str("image_id", img.ID).Str("path", rootfsPath).Msg("blob stream: stat rootfs")
		writeError(w, err)
		return
	}

	// If we have a cached tar checksum from a prior stream, advertise it so the
	// deploy agent can verify end-to-end integrity. On the first stream the header
	// is absent — the client treats a missing header as "no verification possible"
	// and skips the check (with a warning). The sidecar is written after a
	// successful stream so subsequent downloads get the header.
	//
	// Sprint 1 compromise: for filesystem images the DB `checksum` column is a
	// directory-level hash (not a tar stream hash), so we maintain a separate
	// sidecar file. See tarChecksumPath for details.
	cachedTarChecksum := loadTarChecksum(h.ImageDir, img.ID)
	computeTarChecksum := cachedTarChecksum == "" // only compute on first stream

	log.Info().
		Str("image_id", img.ID).
		Str("format", string(img.Format)).
		Str("client", r.RemoteAddr).
		Bool("has_tar_checksum", cachedTarChecksum != "").
		Msg("blob stream: starting tar")

	// Wrap the response writer to count bytes streamed.
	// If this is the first stream, also tee through a sha256 hasher so we can
	// cache the tar checksum for all subsequent streams.
	// Headers are written AFTER the probe buffer fills (fail-fast path below).
	baseWriter := &countWriter{w: w}
	var tarHasher = sha256.New() // always created; only used when computeTarChecksum
	var cw io.Writer
	if computeTarChecksum {
		cw = io.MultiWriter(baseWriter, tarHasher)
	} else {
		cw = baseWriter
	}

	// Use exec.Command (no context) so the tar subprocess lifetime is NOT bound
	// to the HTTP request context. Context cancellation (client disconnect, proxy
	// timeout) was SIGKILL'ing tar mid-stream under concurrent load, delivering
	// truncated archives to the deploy agents.
	//
	// Excluded paths fall into two categories:
	//   1. Virtual/pseudo filesystems that are empty or populated at runtime
	//      (proc, sys, dev, selinux) — no useful data, and tar errors on them.
	//   2. Security-sensitive or restricted files that are mode 000 or SUID root
	//      on a locked-down system: tar exits 2 under NoNewPrivileges=yes because
	//      it cannot open them. These are intentionally excluded — the deployed
	//      node regenerates them on first boot from PAM/sssd/shadow-utils.
	cmd := exec.Command("tar", //nolint:gosec
		"-C", rootfsPath,
		// Virtual/runtime filesystems
		"--exclude=./proc/*",
		"--exclude=./sys/*",
		"--exclude=./dev/*",
		"--exclude=./.clustr-state",
		// Shadow / credential files — intentionally 000 or 640 root:shadow.
		// tar exits 2 trying to read these under NoNewPrivileges=yes.
		// The deployed node regenerates shadow from passwd + firstboot config.
		"--exclude=./etc/shadow",
		"--exclude=./etc/shadow-",
		"--exclude=./etc/gshadow",
		"--exclude=./etc/gshadow-",
		"--exclude=./etc/security/opasswd",
		// sssd / nslcd / chrony runtime state and logs — directories are mode 700
		// or 750, unreadable by the non-privileged tar process running under
		// NoNewPrivileges=yes. Exclude the directory itself (no /*) so tar skips
		// the entire subtree including the directory entry.
		"--exclude=./var/lib/sss",
		"--exclude=./var/lib/nslcd",
		"--exclude=./var/lib/chrony",
		"--exclude=./var/log/sssd",
		"--exclude=./var/log/chrony",
		// sudo binaries and helpers — SUID root (mode 4111/4111), cannot be
		// read by the non-privileged tar process running under NoNewPrivileges=yes.
		"--exclude=./usr/bin/sudo",
		"--exclude=./usr/bin/sudoreplay",
		"--exclude=./usr/libexec/sudo/sesh",
		// sssd configuration and runtime dirs — mode 711/750, owned root:sssd.
		// tar exits 2 trying to read these under NoNewPrivileges=yes.
		// The deployed node re-joins sssd/IPA on first boot via firstboot scripts.
		"--exclude=./etc/sssd",
		// polkit rules — mode 700 on strict SELinux systems; unreadable by tar.
		"--exclude=./etc/polkit-1/rules.d",
		// Home directory for the clustr deploy agent — should not be in the image
		// rootfs; exclude it so the deployed node gets a clean /home/clustr on setup.
		"--exclude=./home/clustr",
		// staprun (SystemTap) — SUID root binary (mode 4110), unreadable under
		// NoNewPrivileges=yes; excluded to prevent tar exit 2.
		"--exclude=./usr/bin/staprun",
		// Deterministic output flags — required for stable sha256 across repeated
		// streams of the same image content. Without these, entry order and embedded
		// timestamps vary between runs (directory readdir order, ctime drift),
		// producing different byte streams and therefore different hashes.
		//
		// --sort=name: emit entries in alphabetic order (POSIX-stable across runs).
		// --mtime: normalize all file timestamps to the UNIX epoch so a file whose
		//          mtime changed between streams doesn't invalidate the cached hash.
		//
		// Note: the sidecar hash is computed on the first stream and reused for all
		// subsequent streams. On server restart the sidecar file persists so the hash
		// survives across restarts without requiring a re-stream. If the sidecar is
		// missing, the next stream recomputes it.
		"--sort=name",
		"--mtime=1970-01-01 00:00:00",
		"-cf", "-",
		".",
	)
	stderrBuf := &bytes.Buffer{}
	cmd.Stderr = stderrBuf

	// Fail-fast: start tar and buffer the first 64 KB before writing HTTP
	// headers. If tar exits before filling the buffer (i.e. it failed
	// immediately with a fatal error), we can still return HTTP 500 rather
	// than committing to a 200 with a truncated body.
	// Once 64 KB is buffered we are confident tar is running correctly and we
	// commit to streaming: write headers and flush the buffer, then pipe the
	// remainder directly. After that point we cannot retroactively signal
	// failure to the client — but all the "known-bad" paths are excluded above.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Error().Err(err).Str("image_id", img.ID).Msg("blob stream: tar stdout pipe failed")
		writeError(w, err)
		return
	}

	if err := cmd.Start(); err != nil {
		log.Error().Err(err).Str("image_id", img.ID).Msg("blob stream: tar start failed")
		writeError(w, err)
		return
	}

	// Read up to 64 KB into a probe buffer. io.ReadFull returns io.EOF or
	// io.ErrUnexpectedEOF when fewer than 64 KB are available (i.e. tar exited
	// early). In that case check the process exit status; if non-zero, return
	// HTTP 500 — headers have not been written yet.
	const probeSize = 64 * 1024
	probeBuf := make([]byte, probeSize)
	probeN, probeErr := io.ReadFull(stdout, probeBuf)

	if probeErr == io.EOF || probeErr == io.ErrUnexpectedEOF {
		// Tar produced less than probeSize bytes — it may have finished cleanly
		// (tiny image) or died early. Check exit status.
		waitErr := cmd.Wait()
		if waitErr != nil {
			// Tar failed before streaming any meaningful data — safe to 500.
			log.Error().
				Err(waitErr).
				Str("image_id", img.ID).
				Str("stderr", stderrBuf.String()).
				Int("probe_bytes", probeN).
				Msg("blob stream: tar exited non-zero before headers were sent — returning HTTP 500")
			http.Error(w, "tar failed: "+stderrBuf.String(), http.StatusInternalServerError)
			return
		}
		// Tar finished cleanly with a small output — write headers and flush.
		if cachedTarChecksum != "" {
			w.Header().Set("X-Clustr-Blob-SHA256", cachedTarChecksum)
		}
		w.Header().Set("Content-Type", "application/x-tar")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.tar"`, img.ID))
		if rc := http.NewResponseController(w); rc != nil {
			_ = rc.SetWriteDeadline(time.Time{})
		}
		w.WriteHeader(http.StatusOK)
		if _, writeErr := cw.Write(probeBuf[:probeN]); writeErr != nil {
			log.Warn().Err(writeErr).Str("image_id", img.ID).Msg("blob stream: write probe buffer (small tar)")
		}
		if computeTarChecksum {
			checksum := hex.EncodeToString(tarHasher.Sum(nil))
			saveTarChecksum(h.ImageDir, img.ID, checksum)
			log.Info().Str("image_id", img.ID).Str("tar_sha256", checksum).
				Msg("blob stream: tar checksum computed and cached (small tar, sidecar written)")
		}
		log.Info().Str("image_id", img.ID).Int64("bytes_written", baseWriter.n).
			Msg("blob stream: tar complete (small tar, headers sent after wait)")
		return
	}

	if probeErr != nil {
		// Unexpected read error — kill tar and return 500.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		log.Error().Err(probeErr).Str("image_id", img.ID).Msg("blob stream: probe read error — aborting")
		http.Error(w, "blob stream probe failed", http.StatusInternalServerError)
		return
	}

	// We have a full probe buffer — tar is running. Commit to streaming now.
	// Write HTTP headers before flushing the probe buffer.
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.tar"`, img.ID))
	if cachedTarChecksum != "" {
		w.Header().Set("X-Clustr-Blob-SHA256", cachedTarChecksum)
	}
	w.WriteHeader(http.StatusOK)

	// Disable the per-request write deadline for this handler — the response is a
	// large streaming tar archive and a global WriteTimeout would kill it mid-stream
	// on slow links. http.ResponseController is available since Go 1.20.
	if rc := http.NewResponseController(w); rc != nil {
		_ = rc.SetWriteDeadline(time.Time{})
	}

	// Flush probe buffer through the counting/hashing writer.
	if _, writeErr := cw.Write(probeBuf[:probeN]); writeErr != nil {
		// Client disconnected immediately after headers — treat as disconnect.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		log.Info().Err(writeErr).Str("image_id", img.ID).Msg("blob stream: client disconnected after headers")
		return
	}

	// Stream the remainder of tar's stdout.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// Pipe remaining stdout through the counting/hashing writer.
	pipeRemaining := func() {
		if _, err := io.Copy(cw, stdout); err != nil {
			log.Warn().Err(err).Str("image_id", img.ID).Msg("blob stream: io.Copy interrupted")
		}
	}

	select {
	case <-r.Context().Done():
		// Client disconnected mid-stream — this is normal (e.g. agent restart,
		// network blip). Give tar 2 seconds to flush buffered output, then kill.
		timer := time.AfterFunc(2*time.Second, func() { _ = cmd.Process.Kill() })
		defer timer.Stop()
		<-done
		log.Info().
			Str("image_id", img.ID).
			Str("client", r.RemoteAddr).
			Int64("bytes_written", baseWriter.n).
			Msg("blob stream: client disconnected — cleanup complete")
		return
	default:
	}

	// Normal path: pipe remaining bytes synchronously, then wait.
	pipeRemaining()
	// pipeRemaining returns when tar's stdout reaches EOF, which happens after
	// tar exits. Wait for cmd.Wait() to collect the exit status. If the client
	// disconnected mid-stream via TCP reset (r.Context().Done() not yet
	// signalled above), tar may be blocked writing into a full kernel pipe
	// buffer — give it 5s to drain, then kill so cmd.Wait() can unblock.
	var tarErr error
	select {
	case tarErr = <-done:
		// tar exited on its own — normal completion.
	case <-time.After(5 * time.Second):
		// tar is still alive 5s after stdout EOF — client likely disconnected
		// via TCP reset without triggering context cancellation. Kill it.
		_ = cmd.Process.Kill()
		tarErr = <-done
	}

	if tarErr != nil {
		// Real tar failure after headers committed — log stderr for diagnosis.
		// Client already received partial data; we cannot retroactively 500.
		log.Error().
			Err(tarErr).
			Str("image_id", img.ID).
			Str("stderr", stderrBuf.String()).
			Int64("bytes_written", baseWriter.n).
			Msg("blob stream: tar exited non-zero after streaming began — response may be truncated")
		return
	}

	log.Info().
		Str("image_id", img.ID).
		Str("client", r.RemoteAddr).
		Int64("bytes_written", baseWriter.n).
		Msg("blob stream: tar complete")

	// On the first successful stream, persist the tar checksum sidecar so
	// subsequent downloads can serve X-Clustr-Blob-SHA256 and clients can
	// verify end-to-end integrity.
	if computeTarChecksum {
		checksum := hex.EncodeToString(tarHasher.Sum(nil))
		saveTarChecksum(h.ImageDir, img.ID, checksum)
		log.Info().Str("image_id", img.ID).Str("tar_sha256", checksum).
			Msg("blob stream: tar checksum computed and cached (sidecar written)")
	}
}

// GetImageMetadata handles GET /api/v1/images/:id/metadata
//
// Returns the ImageMetadata sidecar for the given image. The sidecar is read
// from the on-disk JSON file at ImageDir/<id>/metadata.json. If the DB column
// is populated it is used as a fast-path; the sidecar file is the fallback.
// Returns 404 if neither the image record nor the sidecar file exist.
func (h *ImagesHandler) GetImageMetadata(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ctx := r.Context()

	// Confirm the image record exists.
	if _, err := h.DB.GetBaseImage(ctx, id); err != nil {
		writeError(w, err)
		return
	}

	// Fast path: DB column populated by the build pipeline.
	rawJSON, err := h.DB.GetImageMetadataJSON(ctx, id)
	if err != nil {
		log.Warn().Err(err).Str("image_id", id).Msg("get image metadata: db lookup failed, falling back to sidecar")
	}

	if rawJSON == "" {
		// Fall back to the sidecar file on disk.
		meta, readErr := image.ReadMetadata(h.ImageDir, id)
		if readErr != nil {
			if os.IsNotExist(readErr) {
				writeJSON(w, http.StatusNotFound, api.ErrorResponse{
					Error: "metadata sidecar not yet available for this image; it is written after the first successful build",
					Code:  "metadata_not_found",
				})
				return
			}
			log.Error().Err(readErr).Str("image_id", id).Msg("get image metadata: read sidecar")
			writeError(w, readErr)
			return
		}
		// Lazily back-fill the DB column so future reads hit the fast path.
		if encoded, marshalErr := json.Marshal(meta); marshalErr == nil {
			if dbErr := h.DB.SetImageMetadataJSON(ctx, id, string(encoded)); dbErr != nil {
				log.Warn().Err(dbErr).Str("image_id", id).Msg("get image metadata: lazy db backfill failed")
			}
		}
		writeJSON(w, http.StatusOK, meta)
		return
	}

	// Serve the DB-cached JSON directly — avoid an unnecessary unmarshal/re-marshal.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(rawJSON))
}

// countWriter wraps an http.ResponseWriter and counts bytes written.
type countWriter struct {
	w http.ResponseWriter
	n int64
}

func (cw *countWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.n += int64(n)
	return n, err
}

// FromURL handles POST /api/v1/images/from-url.
//
// Request body: {"url":"https://…","name":"optional","expected_sha256":"optional"}
// (expected_sha256 is accepted for backward compatibility but ignored — the
// Factory pipeline computes its own checksums over the extracted rootfs.)
//
// Delegates entirely to Factory.PullImage so that .iso URLs reach the
// qemu+kickstart build pipeline instead of being persisted as raw blobs.
// Returns 202 immediately with {id, image_id, status:"building"}; the factory
// transitions the record to "ready" (or "error") asynchronously.
//
// SSRF guard: only http/https URLs are accepted; private RFC-1918 IPs are
// rejected unless CLUSTR_ALLOW_PRIVATE_IMAGE_URLS=true.
func (h *ImagesHandler) FromURL(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL            string `json:"url"`
		Name           string `json:"name"`
		ExpectedSHA256 string `json:"expected_sha256"` // accepted, not forwarded
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeValidationError(w, "invalid JSON body")
		return
	}
	if req.URL == "" {
		writeValidationError(w, "url is required")
		return
	}
	// Only http/https allowed.
	if !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "https://") {
		writeValidationError(w, "url must use http or https scheme")
		return
	}

	// SSRF guard: reject private/loopback IPs unless allowlisted.
	if !allowPrivateImageURLs() {
		if err := rejectPrivateURL(req.URL); err != nil {
			writeJSON(w, http.StatusBadRequest, api.ErrorResponse{
				Error: err.Error(),
				Code:  "ssrf_rejected",
			})
			return
		}
	}

	// Input is valid; require the factory to be wired before proceeding.
	if h.Factory == nil {
		writeJSON(w, http.StatusNotImplemented, api.ErrorResponse{
			Error: "image factory not configured",
			Code:  "factory_unavailable",
		})
		return
	}

	// Auto-suggest name from URL filename when not provided.
	name := req.Name
	if name == "" {
		parts := strings.Split(strings.TrimRight(req.URL, "/"), "/")
		base := parts[len(parts)-1]
		if idx := strings.Index(base, "?"); idx >= 0 {
			base = base[:idx]
		}
		if base != "" {
			name = base
		} else {
			name = "image-from-url"
		}
	}

	// Delegate to the factory. PullImage creates the DB record synchronously
	// (status=building) and dispatches the download+extract pipeline async.
	// For .iso URLs, pullAndExtract routes to buildFromISOFile which runs an
	// unattended QEMU install rather than treating the ISO as a raw disk.
	img, err := h.Factory.PullImage(r.Context(), api.PullRequest{
		URL:  req.URL,
		Name: name,
	})
	if err != nil {
		log.Error().Err(err).Msg("from-url: factory.PullImage failed")
		writeError(w, err)
		return
	}

	if h.ImageEvents != nil {
		imgCopy := *img
		h.ImageEvents.Publish(api.ImageEvent{Kind: api.ImageEventCreated, Image: &imgCopy, ID: img.ID})
	}

	if h.Audit != nil {
		aID, aLabel := "", ""
		if h.GetActorInfo != nil {
			aID, aLabel = h.GetActorInfo(r)
		}
		h.Audit.Record(r.Context(), aID, aLabel, db.AuditActionImageCreate, "image", img.ID,
			r.RemoteAddr, nil, map[string]string{"name": img.Name, "source_url": req.URL})
	}

	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"image_id": img.ID,
		"status":   img.Status,
		"id":       img.ID,
	})
}

// allowPrivateImageURLs returns true when CLUSTR_ALLOW_PRIVATE_IMAGE_URLS=true.
func allowPrivateImageURLs() bool {
	return strings.EqualFold(os.Getenv("CLUSTR_ALLOW_PRIVATE_IMAGE_URLS"), "true")
}

// rejectPrivateURL returns an error if the URL resolves to a private/loopback address.
func rejectPrivateURL(rawURL string) error {
	var privateRanges = []string{
		"10.", "172.16.", "172.17.", "172.18.", "172.19.",
		"172.20.", "172.21.", "172.22.", "172.23.", "172.24.",
		"172.25.", "172.26.", "172.27.", "172.28.", "172.29.",
		"172.30.", "172.31.", "192.168.", "127.", "169.254.",
		"::1", "fc00:", "fd",
	}
	// Extract host from URL.
	var host string
	if rest := strings.TrimPrefix(rawURL, "https://"); rest != rawURL {
		host = rest
	} else {
		host = strings.TrimPrefix(rawURL, "http://")
	}
	// Strip path.
	if idx := strings.Index(host, "/"); idx >= 0 {
		host = host[:idx]
	}
	// Strip port.
	if idx := strings.LastIndex(host, ":"); idx >= 0 && idx > strings.LastIndex(host, "]") {
		host = host[:idx]
	}
	host = strings.Trim(host, "[]")

	for _, prefix := range privateRanges {
		if strings.HasPrefix(host, prefix) {
			return fmt.Errorf("url resolves to a private or loopback address (%s); set CLUSTR_ALLOW_PRIVATE_IMAGE_URLS=true to override", host)
		}
	}
	// Also reject localhost.
	if host == "localhost" || host == "0.0.0.0" {
		return fmt.Errorf("url resolves to a private or loopback address (%s)", host)
	}
	return nil
}

// StreamImageEvents handles GET /api/v1/images/events — SSE-1.
// Streams api.ImageEvent as Server-Sent Events whenever an image lifecycle
// event occurs (create, update, delete, finalize). Replaces the 15s polling
// in the web /images view.
//
// Deprecated: UX-4. The web client now subscribes to the multiplexed
// /api/v1/events endpoint (EventsHandler). This per-resource endpoint is
// retained for backwards compatibility with external consumers and the CLI.
// Remove once all callers have migrated.
func (h *ImagesHandler) StreamImageEvents(w http.ResponseWriter, r *http.Request) {
	if h.ImageEvents == nil {
		http.Error(w, "image events not configured", http.StatusServiceUnavailable)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported by this server", http.StatusInternalServerError)
		return
	}

	ch, cancel := h.ImageEvents.Subscribe()
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case event, open := <-ch:
			if !open {
				return
			}
			data, err := json.Marshal(event)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// ─── POST /api/v1/images/:id/reconcile (#251) ────────────────────────────────

// reconcileImageRequest is the optional JSON body for POST /images/:id/reconcile.
type reconcileImageRequest struct {
	// ForceReFinalize, when true, accepts the on-disk SHA as the new truth.
	// Rewrites metadata.json and updates the DB record. Sets status=ready.
	// Rejected when the image is already in a healthy state.
	ForceReFinalize bool `json:"force_re_finalize"`
}

// ReconcileImage handles POST /api/v1/images/:id/reconcile.
// Admin scope required (this mutates DB and on-disk metadata in the
// force_re_finalize path).
//
// Query params:
//   - accept_drift=true: alias for force_re_finalize=true (operator shortcut).
func (h *ImagesHandler) ReconcileImage(w http.ResponseWriter, r *http.Request) {
	imageID := chi.URLParam(r, "id")
	if imageID == "" {
		writeValidationError(w, "image id is required")
		return
	}

	if h.ImageReconciler == nil {
		writeJSON(w, http.StatusServiceUnavailable, api.ErrorResponse{
			Error: "reconcile service not available",
			Code:  "reconcile_unavailable",
		})
		return
	}

	var body reconcileImageRequest
	// Body is optional; ignore decode errors for empty bodies.
	_ = json.NewDecoder(r.Body).Decode(&body)

	// Also accept ?accept_drift=true as a query param alias.
	if r.URL.Query().Get("accept_drift") == "true" {
		body.ForceReFinalize = true
	}

	opts := reconcile.Opts{
		CacheTTL:        0, // always re-hash when called via HTTP
		ForceReFinalize: body.ForceReFinalize,
	}

	result, err := h.ImageReconciler.ReconcileImage(r.Context(), imageID, opts)
	if err != nil && result == nil {
		// I/O failure — not a quarantine outcome.
		log.Error().Err(err).Str("image_id", imageID).Msg("reconcile endpoint: I/O error")
		writeJSON(w, http.StatusInternalServerError, api.ErrorResponse{
			Error: fmt.Sprintf("reconcile failed: %s", err.Error()),
			Code:  "reconcile_io_error",
		})
		return
	}

	writeJSON(w, http.StatusOK, result)
}
