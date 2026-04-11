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
	"github.com/sqoia-dev/clonr/pkg/server"
)

var version = "dev"

func main() {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr}).
		With().
		Str("service", "clonr-serverd").
		Logger()

	log.Info().Str("version", version).Msg("clonr-serverd starting")

	cfg := config.Default()

	// Load config file if provided via first argument.
	if len(os.Args) > 1 {
		loaded, err := config.Load(os.Args[1])
		if err != nil {
			log.Fatal().Err(err).Msg("failed to load config")
		}
		cfg = loaded
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := server.New(cfg.Server.Addr)
	log.Info().Str("addr", cfg.Server.Addr).Msg("listening")

	if err := srv.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}

	log.Info().Msg("shutdown complete")
}
