// Package server provides the clonr-serverd HTTP API built on chi.
package server

import (
	"context"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog/log"
	"github.com/sqoia-dev/clonr/pkg/config"
	"github.com/sqoia-dev/clonr/pkg/db"
	"github.com/sqoia-dev/clonr/pkg/server/handlers"
)

// Server wraps the HTTP server and all its dependencies.
type Server struct {
	cfg    config.ServerConfig
	db     *db.DB
	broker *LogBroker
	router chi.Router
	http   *http.Server
}

// New creates a Server wired with the given config and database.
func New(cfg config.ServerConfig, database *db.DB) *Server {
	s := &Server{
		cfg:    cfg,
		db:     database,
		broker: NewLogBroker(),
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
	r.Use(bearerAuth(s.cfg.AuthToken))

	// Handler instances.
	health := &handlers.HealthHandler{Version: "dev"}
	images := &handlers.ImagesHandler{DB: s.db, ImageDir: s.cfg.ImageDir}
	nodes := &handlers.NodesHandler{DB: s.db}
	factory := &handlers.FactoryHandler{DB: s.db, ImageDir: s.cfg.ImageDir}
	logs := &handlers.LogsHandler{DB: s.db, Broker: s.broker}

	r.Route("/api/v1", func(r chi.Router) {
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

		// Nodes — by-mac must be registered before /{id} to avoid chi match ambiguity.
		r.Get("/nodes/by-mac/{mac}", nodes.GetNodeByMAC)
		r.Get("/nodes", nodes.ListNodes)
		r.Post("/nodes", nodes.CreateNode)
		r.Get("/nodes/{id}", nodes.GetNode)
		r.Put("/nodes/{id}", nodes.UpdateNode)
		r.Delete("/nodes/{id}", nodes.DeleteNode)

		// Logs — stream must be registered before plain /logs to avoid ambiguity.
		r.Get("/logs/stream", logs.StreamLogs)
		r.Get("/logs", logs.QueryLogs)
		r.Post("/logs", logs.IngestLogs)
	})

	return r
}

// Handler returns the underlying http.Handler for use in tests.
func (s *Server) Handler() http.Handler {
	return s.router
}

// ListenAndServe starts the HTTP server and blocks until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		shutdownCtx := context.Background()
		if err := s.http.Shutdown(shutdownCtx); err != nil {
			log.Error().Err(err).Msg("graceful shutdown error")
		}
	}()

	log.Info().Str("addr", s.cfg.ListenAddr).Msg("server listening")
	if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server: %w", err)
	}
	return nil
}
