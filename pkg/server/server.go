// Package server provides the clonr-serverd HTTP API built on chi.
package server

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog/log"
	"github.com/sqoia-dev/clonr/pkg/config"
	"github.com/sqoia-dev/clonr/pkg/db"
	"github.com/sqoia-dev/clonr/pkg/image"
	"github.com/sqoia-dev/clonr/pkg/power"
	ipmipower "github.com/sqoia-dev/clonr/pkg/power/ipmi"
	proxmoxpower "github.com/sqoia-dev/clonr/pkg/power/proxmox"
	"github.com/sqoia-dev/clonr/pkg/reimage"
	"github.com/sqoia-dev/clonr/pkg/server/handlers"
	"github.com/sqoia-dev/clonr/pkg/server/ui"
)

// Server wraps the HTTP server and all its dependencies.
type Server struct {
	cfg                 config.ServerConfig
	db                  *db.DB
	broker              *LogBroker
	progress            *ProgressStore
	buildProgress       *BuildProgressStore
	shells              *image.ShellManager
	powerCache          *PowerCache
	powerRegistry       *power.Registry
	reimageOrchestrator *reimage.Orchestrator
	router              chi.Router
	http                *http.Server
}

// buildProgressAdapter adapts *BuildProgressStore to image.BuildProgressReporter.
// The image package defines an interface with Start returning a BuildHandle interface;
// this adapter bridges the concrete server types to that interface.
type buildProgressAdapter struct {
	store *BuildProgressStore
}

// buildHandleAdapter wraps *BuildHandle (server) to satisfy image.BuildHandle.
type buildHandleAdapter struct {
	h *BuildHandle
}

func (a buildHandleAdapter) SetPhase(phase string)      { a.h.SetPhase(phase) }
func (a buildHandleAdapter) SetProgress(d, t int64)     { a.h.SetProgress(d, t) }
func (a buildHandleAdapter) AddSerialLine(line string)   { a.h.AddSerialLine(line) }
func (a buildHandleAdapter) AddStderrLine(line string)   { a.h.AddStderrLine(line) }
func (a buildHandleAdapter) Fail(msg string)             { a.h.Fail(msg) }
func (a buildHandleAdapter) Complete()                   { a.h.Complete() }

func (a buildProgressAdapter) Start(imageID string) image.BuildHandle {
	h := a.store.Start(imageID)
	return buildHandleAdapter{h: h}
}

// New creates a Server wired with the given config and database.
func New(cfg config.ServerConfig, database *db.DB) *Server {
	// Build the power provider registry and register all supported backends.
	registry := power.NewRegistry()
	ipmipower.Register(registry)
	proxmoxpower.Register(registry)

	reimageOrch := reimage.New(database, registry, log.Logger)

	shells := image.NewShellManager(database, cfg.ImageDir, log.Logger)
	buildProg := NewBuildProgressStore(cfg.ImageDir)
	s := &Server{
		cfg:                 cfg,
		db:                  database,
		broker:              NewLogBroker(),
		progress:            NewProgressStore(),
		buildProgress:       buildProg,
		shells:              shells,
		powerCache:          NewPowerCache(15 * time.Second),
		powerRegistry:       registry,
		reimageOrchestrator: reimageOrch,
	}
	s.router = s.buildRouter()
	s.http = &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: s.router,
	}
	return s
}

// StartBackgroundWorkers starts long-running background goroutines.
// Call this after New() and before ListenAndServe().
func (s *Server) StartBackgroundWorkers(ctx context.Context) {
	go s.reimageOrchestrator.Scheduler(ctx)
}

// buildRouter constructs the chi router and registers all routes.
func (s *Server) buildRouter() chi.Router {
	r := chi.NewRouter()

	// Global middleware stack.
	r.Use(panicRecovery)
	r.Use(requestLogger)
	r.Use(chimiddleware.StripSlashes)

	if s.cfg.AuthToken == "" {
		log.Warn().Msg("CLONR_AUTH_TOKEN not set — auth is disabled (dev mode only)")
	}
	// bearerAuth is applied only to the /api/v1 subrouter below,
	// so that the embedded web UI at / and /ui/* is always accessible.

	// Derive public server URL from listen addr for boot script generation.
	// Use net.SplitHostPort to extract only the port from ListenAddr (which may
	// be "0.0.0.0:8080"), then combine it with the PXE ServerIP.
	_, port, splitErr := net.SplitHostPort(s.cfg.ListenAddr)
	if splitErr != nil {
		// ListenAddr had no port component — fall back to the raw value.
		port = s.cfg.ListenAddr
	}
	var serverURL string
	if s.cfg.PXE.ServerIP != "" {
		serverURL = "http://" + s.cfg.PXE.ServerIP + ":" + port
	} else {
		// Fallback: use localhost when PXE is not configured.
		serverURL = "http://localhost:" + port
	}

	// Handler instances.
	health := &handlers.HealthHandler{Version: "dev"}
	images := &handlers.ImagesHandler{DB: s.db, ImageDir: s.cfg.ImageDir, Progress: s.progress}
	nodes := &handlers.NodesHandler{DB: s.db}
	nodeGroups := &handlers.NodeGroupsHandler{DB: s.db}
	layoutH := &handlers.LayoutHandler{DB: s.db}
	imgFactory := &image.Factory{
		Store:         s.db,
		ImageDir:      s.cfg.ImageDir,
		Logger:        log.Logger,
		BuildProgress: buildProgressAdapter{store: s.buildProgress},
	}
	factory := &handlers.FactoryHandler{
		DB:       s.db,
		ImageDir: s.cfg.ImageDir,
		Factory:  imgFactory,
		Shells:   s.shells,
	}
	buildProgressH := &handlers.BuildProgressHandler{
		Store:    s.buildProgress,
		ImageDir: s.cfg.ImageDir,
	}
	logs := &handlers.LogsHandler{DB: s.db, Broker: s.broker}
	progress := &handlers.ProgressHandler{Store: s.progress}
	ipmiH := &handlers.IPMIHandler{DB: s.db, Cache: s.powerCache, Registry: s.powerRegistry}
	powerH := &handlers.PowerHandler{DB: s.db, Registry: s.powerRegistry}
	reimageH := &handlers.ReimageHandler{DB: s.db, Orchestrator: s.reimageOrchestrator}
	boot := &handlers.BootHandler{
		BootDir:   s.cfg.PXE.BootDir,
		TFTPDir:   s.cfg.PXE.TFTPDir,
		ServerURL: serverURL,
		DB:        s.db,
	}

	// Embedded web UI — served without bearer auth.
	// The UI JavaScript talks to /api/v1 which enforces auth when a token is set.
	staticFS, _ := fs.Sub(ui.StaticFiles, "static")
	fileServer := http.FileServer(http.FS(staticFS))
	r.Handle("/ui/*", http.StripPrefix("/ui", fileServer))
	r.Get("/", serveIndex(staticFS))

	r.Route("/api/v1", func(r chi.Router) {
		// Public endpoints — no auth required.
		// PXE-booted nodes fetch boot files and register themselves before any
		// admin has configured credentials on them.
		r.Get("/boot/ipxe", boot.ServeIPXEScript)
		r.Get("/boot/vmlinuz", boot.ServeVMLinuz)
		r.Get("/boot/initramfs.img", boot.ServeInitramfs)
		r.Get("/boot/ipxe.efi", boot.ServeIPXEEFI)
		r.Get("/boot/undionly.kpxe", boot.ServeUndionlyKPXE)
		r.Post("/nodes/register", nodes.RegisterNode)
		r.Post("/logs", logs.IngestLogs)                   // nodes ship logs without tokens
		r.Post("/deploy/progress", progress.IngestProgress) // nodes ship progress without tokens

		// Authenticated endpoints.
		r.Group(func(r chi.Router) {
			r.Use(bearerAuth(s.cfg.AuthToken))

			// Health
			r.Get("/health", health.ServeHTTP)

			// Images
			r.Get("/images", images.ListImages)
			r.Post("/images", images.CreateImage)
			r.Get("/images/{id}", images.GetImage)
			r.Delete("/images/{id}", images.DeleteImage)
			r.Get("/images/{id}/status", images.GetImageStatus)
			r.Get("/images/{id}/disklayout", images.GetDiskLayout)
			r.Put("/images/{id}/disklayout", images.PutDiskLayout)
			r.Post("/images/{id}/blob", images.UploadBlob)
			r.Get("/images/{id}/blob", images.DownloadBlob)

			// Factory
			r.Get("/image-roles", factory.ListImageRoles)
			r.Post("/factory/pull", factory.Pull)
			r.Post("/factory/import", factory.Import)
			r.Post("/factory/import-path", factory.ImportPath)
			r.Post("/factory/import-iso", factory.ImportPath) // alias used by the web UI
			r.Post("/factory/capture", factory.Capture)
			r.Post("/factory/build-from-iso", factory.BuildFromISO)

			// ISO build observability — stream must come before plain snapshot route.
			r.Get("/images/{id}/build-progress/stream", buildProgressH.StreamBuildProgress)
			r.Get("/images/{id}/build-progress", buildProgressH.GetBuildProgress)
			r.Get("/images/{id}/build-log", buildProgressH.GetBuildLog)
			r.Get("/images/{id}/build-manifest", buildProgressH.GetBuildManifest)

			// Shell sessions
			r.Post("/images/{id}/shell-session", factory.OpenShellSession)
			r.Delete("/images/{id}/shell-session/{sid}", factory.CloseShellSession)
			r.Post("/images/{id}/shell-session/{sid}/exec", factory.ExecInSession)
			r.Get("/images/{id}/shell-session/{sid}/ws", factory.ShellWS)

			// Active deploy detection (for shell modal warning)
			r.Get("/images/{id}/active-deploys", factory.ActiveDeploys)

			// Nodes — by-mac must be before /{id} to avoid chi match ambiguity.
			r.Get("/nodes/by-mac/{mac}", nodes.GetNodeByMAC)
			r.Get("/nodes", nodes.ListNodes)
			r.Post("/nodes", nodes.CreateNode)
			r.Get("/nodes/{id}", nodes.GetNode)
			r.Put("/nodes/{id}", nodes.UpdateNode)
			r.Delete("/nodes/{id}", nodes.DeleteNode)

			// Deploy lifecycle callbacks — called by the node after finalize.
			// These are the mechanism by which the PXE server learns whether a
			// node should boot from disk or run another deploy on next PXE boot.
			r.Post("/nodes/{id}/deploy-complete", nodes.DeployComplete)
			r.Post("/nodes/{id}/deploy-failed", nodes.DeployFailed)

			// Disk layout hierarchy — node-level overrides, group assignment,
			// hardware-aware recommendations, and validation.
			r.Get("/nodes/{id}/layout-recommendation", layoutH.GetLayoutRecommendation)
			r.Get("/nodes/{id}/effective-layout", layoutH.GetEffectiveLayout)
			r.Put("/nodes/{id}/layout-override", layoutH.SetNodeLayoutOverride)
			r.Post("/nodes/{id}/layout/validate", layoutH.ValidateLayout)
			r.Put("/nodes/{id}/group", layoutH.AssignNodeGroup)
			r.Get("/nodes/{id}/effective-mounts", layoutH.GetEffectiveMounts)

			// Node groups — named sets of nodes sharing a disk layout override.
			r.Get("/node-groups", nodeGroups.ListNodeGroups)
			r.Post("/node-groups", nodeGroups.CreateNodeGroup)
			r.Get("/node-groups/{id}", nodeGroups.GetNodeGroup)
			r.Put("/node-groups/{id}", nodeGroups.UpdateNodeGroup)
			r.Delete("/node-groups/{id}", nodeGroups.DeleteNodeGroup)

			// IPMI / power management — subpaths of /nodes/{id} must be
			// registered in the same chi group so the auth middleware applies.
			r.Get("/nodes/{id}/power", ipmiH.GetPowerStatus)
			r.Post("/nodes/{id}/power/on", ipmiH.PowerOn)
			r.Post("/nodes/{id}/power/off", ipmiH.PowerOff)
			r.Post("/nodes/{id}/power/cycle", ipmiH.PowerCycle)
			r.Post("/nodes/{id}/power/reset", ipmiH.PowerReset)
			r.Post("/nodes/{id}/power/pxe", ipmiH.SetBootPXE)
			r.Post("/nodes/{id}/power/disk", ipmiH.SetBootDisk)
			r.Post("/nodes/{id}/power/flip-to-disk", powerH.FlipToDisk)
			r.Get("/nodes/{id}/sensors", ipmiH.GetSensors)

			// Reimage — queue, track and retry node reimages via the power provider.
			r.Post("/nodes/{id}/reimage", reimageH.Create)
			r.Get("/nodes/{id}/reimage", reimageH.ListForNode)
			r.Get("/reimage/{id}", reimageH.Get)
			r.Delete("/reimage/{id}", reimageH.Cancel)
			r.Post("/reimage/{id}/retry", reimageH.Retry)

			// Logs — stream must be registered before plain /logs.
			r.Get("/logs/stream", logs.StreamLogs)
			r.Get("/logs", logs.QueryLogs)

			// Deployment progress — stream must be registered before plain routes.
			r.Get("/deploy/progress/stream", progress.StreamProgress)
			r.Get("/deploy/progress/{mac}", progress.GetProgress)
			r.Get("/deploy/progress", progress.ListProgress)
		})
	})

	return r
}

// serveIndex serves index.html from the embedded static FS.
func serveIndex(staticFS fs.FS) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f, err := staticFS.Open("index.html")
		if err != nil {
			http.Error(w, "UI not found", http.StatusNotFound)
			return
		}
		defer f.Close()
		stat, err := f.Stat()
		if err != nil {
			http.Error(w, "UI not found", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.ServeContent(w, r, "index.html", stat.ModTime(), f.(io.ReadSeeker))
	}
}

// Handler returns the underlying http.Handler for use in tests.
func (s *Server) Handler() http.Handler {
	return s.router
}

// ListenAndServe starts the HTTP server and blocks until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	go func() {
		<-ctx.Done()

		// Give in-flight builds up to 25 seconds to finish naturally before
		// we force-cancel them. HTTP shutdown gets its own 5-second window on
		// top of that. Total wall-clock budget: 30 seconds.
		drainCtx, drainCancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer drainCancel()

		log.Info().Msg("shutdown: waiting for in-flight builds to complete (up to 25s)")
		s.buildProgress.WaitForActive(drainCtx)

		// Any builds still active after the drain window are stuck — cancel them
		// so the DB record gets updated and the UI doesn't spin forever.
		s.buildProgress.CancelAllActive("server shutting down")

		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := s.http.Shutdown(shutdownCtx); err != nil {
			log.Error().Err(err).Msg("graceful shutdown error")
		}
		if err := s.shells.CloseAll(); err != nil {
			log.Error().Err(err).Msg("shell session cleanup error")
		}
	}()

	log.Info().Str("addr", s.cfg.ListenAddr).Msg("server listening")
	if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server: %w", err)
	}
	return nil
}
