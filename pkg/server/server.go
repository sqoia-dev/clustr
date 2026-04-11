// Package server provides the clonr-serverd REST API.
package server

import (
	"context"
	"fmt"
	"net/http"
)

// Server wraps the HTTP server and its dependencies.
type Server struct {
	addr string
	mux  *http.ServeMux
	http *http.Server
}

// New creates a Server bound to the given address (e.g. ":8080").
func New(addr string) *Server {
	mux := http.NewServeMux()
	s := &Server{
		addr: addr,
		mux:  mux,
		http: &http.Server{
			Addr:    addr,
			Handler: mux,
		},
	}
	s.registerRoutes()
	return s
}

// Start begins listening and serving. It blocks until the context is cancelled.
func (s *Server) Start(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		_ = s.http.Shutdown(context.Background())
	}()

	if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server: %w", err)
	}
	return nil
}
