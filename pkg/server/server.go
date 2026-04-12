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
	"github.com/sqoia-dev/clonr/pkg/server/handlers"
	"github.com/sqoia-dev/clonr/pkg/server/ui"
)

// Server wraps the HTTP server and all its dependencies.
type Server struct {
	cfg          config.ServerConfig
	db           *db.DB
	broker       *LogBroker
	progress     *ProgressStore
	shells       *image.ShellManager
	powerCache   *PowerCache
	router       chi.Router
	http         *http.Server
}

// New creates a Server wired with the given config and database.
func New(cfg config.ServerConfig, database *db.DB) *Server {
	shells := image.NewShellManager(database, cfg.ImageDir, log.Logger)
	s := &Server{
		cfg:        cfg,
		db:         database,
		broker:     NewLogBroker(),
		progress:   NewProgressStore(),
		shells:     shells,
		powerCache: NewPowerCache(15 * time.Second),
	}
	s.router = s.buildRouter()
	s.http = &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: s.router,
	}
	return s
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
	images := &handlers.ImagesHandler{DB: s.db, ImageDir: s.cfg.ImageDir}
	nodes := &handlers.NodesHandler{DB: s.db}
	imgFactory := &image.Factory{
		Store:    s.db,
		ImageDir: s.cfg.ImageDir,
		Logger:   log.Logger,
	}
	factory := &handlers.FactoryHandler{
		DB:       s.db,
		ImageDir: s.cfg.ImageDir,
		Factory:  imgFactory,
		Shells:   s.shells,
	}
	logs := &handlers.LogsHandler{DB: s.db, Broker: s.broker}
	progress := &handlers.ProgressHandler{Store: s.progress}
	ipmiH := &handlers.IPMIHandler{DB: s.db, Cache: s.powerCache}
	boot := &handlers.BootHandler{
		BootDir:   s.cfg.PXE.BootDir,
		TFTPDir:   s.cfg.PXE.TFTPDir,
		ServerURL: serverURL,
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
			r.Delete("/images/{id}", images.ArchiveImage)
			r.Get("/images/{id}/status", images.GetImageStatus)
			r.Get("/images/{id}/disklayout", images.GetDiskLayout)
			r.Put("/images/{id}/disklayout", images.PutDiskLayout)
			r.Post("/images/{id}/blob", images.UploadBlob)
			r.Get("/images/{id}/blob", images.DownloadBlob)

			// Factory
			r.Post("/factory/pull", factory.Pull)
			r.Post("/factory/import", factory.Import)
			r.Post("/factory/import-path", factory.ImportPath)
			r.Post("/factory/import-iso", factory.ImportPath) // alias used by the web UI
			r.Post("/factory/capture", factory.Capture)

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

			// IPMI / power management — subpaths of /nodes/{id} must be
			// registered in the same chi group so the auth middleware applies.
			r.Get("/nodes/{id}/power", ipmiH.GetPowerStatus)
			r.Post("/nodes/{id}/power/on", ipmiH.PowerOn)
			r.Post("/nodes/{id}/power/off", ipmiH.PowerOff)
			r.Post("/nodes/{id}/power/cycle", ipmiH.PowerCycle)
			r.Post("/nodes/{id}/power/reset", ipmiH.PowerReset)
			r.Post("/nodes/{id}/power/pxe", ipmiH.SetBootPXE)
			r.Post("/nodes/{id}/power/disk", ipmiH.SetBootDisk)
			r.Get("/nodes/{id}/sensors", ipmiH.GetSensors)

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
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
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
