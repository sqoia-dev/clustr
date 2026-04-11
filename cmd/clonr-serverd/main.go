package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/sqoia-dev/clonr/pkg/config"
	"github.com/sqoia-dev/clonr/pkg/db"
	"github.com/sqoia-dev/clonr/pkg/server"
)

var version = "dev"

func main() {
	// Bootstrap a console logger early so startup failures are readable.
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr}).
		With().
		Str("service", "clonr-serverd").
		Logger()

	cfg := config.LoadServerConfig()

	// Set log level from config.
	level, err := zerolog.ParseLevel(cfg.LogLevel)
	if err != nil {
		level = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(level)

	log.Info().Str("version", version).Str("addr", cfg.ListenAddr).Msg("clonr-serverd starting")

	// Ensure image directory exists.
	if err := os.MkdirAll(cfg.ImageDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create image dir %s: %v\n", cfg.ImageDir, err)
		os.Exit(1)
	}

	// Open database (applies migrations on first run).
	database, err := db.Open(cfg.DBPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open database %s: %v\n", cfg.DBPath, err)
		os.Exit(1)
	}
	defer database.Close()

	log.Info().Str("db", cfg.DBPath).Msg("database ready")

	// Wire up the server.
	srv := server.New(cfg, database)

	// Graceful shutdown on SIGINT / SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := srv.ListenAndServe(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}

	log.Info().Msg("shutdown complete")
}
