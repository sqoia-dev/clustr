package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/config"
	"github.com/sqoia-dev/clonr/pkg/db"
	"github.com/sqoia-dev/clonr/pkg/image/isoinstaller"
	"github.com/sqoia-dev/clonr/pkg/pxe"
	"github.com/sqoia-dev/clonr/pkg/server"
)

var version = "dev"

// imageFirmwareLayoutMismatch returns a non-empty description when img's declared
// firmware field contradicts its stored default_layout.  Returns "" when consistent.
//
// Known mismatches:
//   - firmware=bios  + ESP partition (vfat/boot/efi) → should be biosboot
//   - firmware=uefi  + biosboot partition             → should be ESP
func imageFirmwareLayoutMismatch(img api.BaseImage) string {
	hasESP := false
	hasBiosBoot := false
	for _, p := range img.DiskLayout.Partitions {
		if p.MountPoint == "/boot/efi" || p.Filesystem == "vfat" {
			hasESP = true
		}
		for _, flag := range p.Flags {
			if flag == "bios_grub" || flag == "biosboot" {
				hasBiosBoot = true
			}
		}
		if p.Filesystem == "biosboot" || p.Filesystem == "bios_grub" {
			hasBiosBoot = true
		}
	}
	switch img.Firmware {
	case api.FirmwareBIOS:
		if hasESP {
			return "firmware=bios but layout contains an ESP (vfat/EFI) partition — layout should use biosboot (EF02)"
		}
	case api.FirmwareUEFI:
		if hasBiosBoot {
			return "firmware=uefi but layout contains a biosboot partition — layout should use ESP (EF00)"
		}
	}
	return ""
}

var rootCmd = &cobra.Command{
	Use:   "clonr-serverd",
	Short: "clonr provisioning server",
	RunE:  runServer,
}

var flagPXE bool

func init() {
	rootCmd.Flags().BoolVar(&flagPXE, "pxe", false,
		"Enable built-in DHCP/TFTP PXE server (also set via CLONR_PXE_ENABLED=true)")

	// apikey subcommand — for operator key rotation.
	apikeyCmd := &cobra.Command{
		Use:   "apikey",
		Short: "Manage clonr-serverd API keys",
	}

	var flagScope string
	var flagDesc string
	createCmd := &cobra.Command{
		Use:   "create",
		Short: "Generate a new API key for the given scope",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runApikeyCreate(flagScope, flagDesc)
		},
	}
	createCmd.Flags().StringVar(&flagScope, "scope", "", "Key scope: admin or node (required)")
	createCmd.Flags().StringVar(&flagDesc, "description", "", "Human-readable description for this key")
	_ = createCmd.MarkFlagRequired("scope")

	apikeyCmd.AddCommand(createCmd)
	rootCmd.AddCommand(apikeyCmd)

	// extract subcommand — rootfs extraction in a subprocess.
	// Invoked by ExtractViaSubprocess; runs under clonr-builders.slice so it
	// has the capabilities (CAP_SYS_ADMIN, CAP_MKNOD, etc.) needed for
	// losetup/mount operations without relaxing clonr-serverd's own unit.
	var flagDisk, flagOut string
	extractCmd := &cobra.Command{
		Use:    "extract",
		Short:  "Extract rootfs from a raw disk image (internal — invoked via systemd-run)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runExtract(flagDisk, flagOut)
		},
	}
	extractCmd.Flags().StringVar(&flagDisk, "disk", "", "Path to the raw disk image (required)")
	extractCmd.Flags().StringVar(&flagOut, "out", "", "Destination directory for the extracted rootfs (required)")
	_ = extractCmd.MarkFlagRequired("disk")
	_ = extractCmd.MarkFlagRequired("out")
	rootCmd.AddCommand(extractCmd)
}

func main() {
	// Bootstrap a console logger early so startup failures are readable.
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr}).
		With().
		Str("service", "clonr-serverd").
		Logger()

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func runApikeyCreate(scopeStr, description string) error {
	scope := api.KeyScope(scopeStr)
	if scope != api.KeyScopeAdmin && scope != api.KeyScopeNode {
		return fmt.Errorf("invalid scope %q: must be 'admin' or 'node'", scopeStr)
	}

	cfg := config.LoadServerConfig()
	database, err := db.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	ctx := context.Background()
	rawKey, id, err := server.CreateAPIKey(ctx, database, scope, description)
	if err != nil {
		return fmt.Errorf("create api key: %w", err)
	}

	fmt.Printf("\nNew %s API key created (ID: %s)\n", scopeStr, id)
	fmt.Printf("Raw key (save this — it will NOT be shown again):\n\n  clonr-%s-%s\n\n", scopeStr, rawKey)
	return nil
}

// runExtract is the implementation of "clonr-serverd extract".  It is designed
// to be invoked as a subprocess via systemd-run under clonr-builders.slice so
// that losetup/mount calls inherit the slice's capability grants rather than
// clonr-serverd's hardened unit restrictions.
//
// Progress output is written to stdout/stderr so the parent process (via pipe)
// can forward it to the build's progress store.
func runExtract(disk, out string) error {
	opts := isoinstaller.ExtractOptions{
		RawDiskPath:   disk,
		RootfsDestDir: out,
	}
	if err := isoinstaller.ExtractRootfs(opts); err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	fmt.Printf("extract: rootfs extracted successfully to %s\n", out)
	return nil
}

func runServer(cmd *cobra.Command, args []string) error {
	cfg := config.LoadServerConfig()

	// --pxe flag overrides env var.
	if flagPXE {
		cfg.PXE.Enabled = true
	}

	// Set log level from config.
	level, err := zerolog.ParseLevel(cfg.LogLevel)
	if err != nil {
		level = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(level)

	log.Info().Str("version", version).Str("addr", cfg.ListenAddr).Msg("clonr-serverd starting")

	// Ensure image directory exists.
	if err := os.MkdirAll(cfg.ImageDir, 0o755); err != nil {
		return fmt.Errorf("failed to create image dir %s: %w", cfg.ImageDir, err)
	}

	// Open database (applies migrations on first run).
	database, err := db.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("failed to open database %s: %w", cfg.DBPath, err)
	}
	defer database.Close()

	log.Info().Str("db", cfg.DBPath).Msg("database ready")

	// Graceful shutdown on SIGINT / SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Inject the HTTP port into PXEConfig so the DHCP server can build correct
	// iPXE chainload URLs without hardcoding port 8080.
	if _, port, err := net.SplitHostPort(cfg.ListenAddr); err == nil {
		cfg.PXE.HTTPPort = port
	} else {
		cfg.PXE.HTTPPort = "8080" // safe fallback
	}

	// Start PXE server if enabled.
	if cfg.PXE.Enabled {
		pxeSrv, err := pxe.New(cfg.PXE)
		if err != nil {
			return fmt.Errorf("failed to init PXE server: %w", err)
		}
		go func() {
			if err := pxeSrv.Start(ctx); err != nil {
				log.Error().Err(err).Msg("PXE server error")
			}
		}()
		log.Info().
			Str("interface", cfg.PXE.Interface).
			Str("range", cfg.PXE.IPRange).
			Msg("PXE server enabled")
	}

	// Bootstrap initial admin API key if none exists.
	// Prints the raw key to stdout ONCE — operator must capture it.
	if !cfg.AuthDevMode {
		if err := server.BootstrapAdminKey(ctx, database); err != nil {
			return fmt.Errorf("failed to bootstrap admin key: %w", err)
		}
	}

	// Wire up and start the HTTP server.
	srv := server.New(cfg, database)

	// Startup firmware/layout consistency audit.
	// Log warnings for any image whose declared firmware contradicts its stored
	// default_layout — e.g. firmware=bios with an ESP partition (the symptom that
	// caused VM207's grub2-install failure).  These are advisory only; the server
	// continues to start normally.
	if imgs, auditErr := database.ListBaseImages(ctx, ""); auditErr == nil {
		for _, img := range imgs {
			if warn := imageFirmwareLayoutMismatch(img); warn != "" {
				log.Warn().
					Str("image_id", img.ID).
					Str("image_name", img.Name).
					Str("firmware", string(img.Firmware)).
					Msg("firmware/layout mismatch: " + warn)
			}
		}
	}

	// Reconcile any images stuck in "building" state from before the restart.
	// These have no live goroutine behind them and will never progress on their own.
	if err := srv.ReconcileStuckBuilds(ctx); err != nil {
		log.Error().Err(err).Msg("reconcile stuck builds failed (non-fatal)")
	}

	// Start background workers (reimage scheduler, etc.) before accepting traffic.
	srv.StartBackgroundWorkers(ctx)

	if err := srv.ListenAndServe(ctx); err != nil {
		return fmt.Errorf("server error: %w", err)
	}

	log.Info().Msg("shutdown complete")
	return nil
}
