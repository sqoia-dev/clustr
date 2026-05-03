package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/sqoia-dev/clustr/pkg/api"
	"github.com/sqoia-dev/clustr/internal/config"
	"github.com/sqoia-dev/clustr/internal/db"
	"github.com/sqoia-dev/clustr/internal/image/isoinstaller"
	"github.com/sqoia-dev/clustr/internal/pxe"
	"github.com/sqoia-dev/clustr/internal/secrets"
	"github.com/sqoia-dev/clustr/internal/server"
)

var (
	version   = "dev"
	commitSHA = "unknown"
	buildTime = "unknown"
)

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
	Use:   "clustr-serverd",
	Short: "clustr provisioning server",
	RunE:  runServer,
}

var flagPXE bool

func init() {
	rootCmd.Flags().BoolVar(&flagPXE, "pxe", false,
		"Enable built-in DHCP/TFTP PXE server (also set via CLUSTR_PXE_ENABLED=true)")

	// apikey subcommand — for operator key rotation.
	apikeyCmd := &cobra.Command{
		Use:   "apikey",
		Short: "Manage clustr-serverd API keys",
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
	// Invoked by ExtractViaSubprocess; runs under clustr-builders.slice so it
	// has the capabilities (CAP_SYS_ADMIN, CAP_MKNOD, etc.) needed for
	// losetup/mount operations without relaxing clustr-serverd's own unit.
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
		Str("service", "clustr-serverd").
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
	fmt.Printf("Raw key (save this — it will NOT be shown again):\n\n  clustr-%s-%s\n\n", scopeStr, rawKey)
	return nil
}

// runExtract is the implementation of "clustr-serverd extract".  It is designed
// to be invoked as a subprocess via systemd-run under clustr-builders.slice so
// that losetup/mount calls inherit the slice's capability grants rather than
// clustr-serverd's hardened unit restrictions.
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

	log.Info().Str("version", version).Str("addr", cfg.ListenAddr).Msg("clustr-serverd starting")

	// S3-10: CLUSTR_AUTH_DEV_MODE loopback guard.
	// If dev mode is enabled, refuse to start unless the listen address is a loopback address.
	// This prevents accidental exposure of an unauthenticated API in production.
	if cfg.AuthDevMode {
		host, _, splitErr := net.SplitHostPort(cfg.ListenAddr)
		if splitErr != nil {
			host = cfg.ListenAddr
		}
		listenIP := net.ParseIP(host)
		isLoopback := host == "" || host == "localhost" ||
			(listenIP != nil && listenIP.IsLoopback())
		if !isLoopback {
			return fmt.Errorf("CLUSTR_AUTH_DEV_MODE=1 is set but the listen address %q is not a loopback address — "+
				"refusing to start to prevent accidental exposure of an unauthenticated API. "+
				"Either unset CLUSTR_AUTH_DEV_MODE or bind to 127.0.0.1 / localhost", cfg.ListenAddr)
		}
		log.Warn().Str("addr", cfg.ListenAddr).Msg("CLUSTR_AUTH_DEV_MODE=1 — running on loopback only (dev mode)")
	}

	// Ensure all required runtime directories exist on first run.
	// This prevents panics and confusing errors when the server is installed fresh.
	requiredDirs := []string{
		filepath.Dir(cfg.DBPath), // parent dir for the SQLite database file
		cfg.ImageDir,
		cfg.PXE.BootDir,
		cfg.PXE.TFTPDir,
		cfg.LogArchiveDir,
		cfg.RepoDir,
	}
	for _, d := range requiredDirs {
		if d == "" {
			continue
		}
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("failed to create required dir %s: %w", d, err)
		}
	}
	log.Info().Msg("required runtime directories ensured")

	// Ensure TMPDIR exists (set by systemd unit; Go's os.MkdirTemp uses it).
	if td := os.Getenv("TMPDIR"); td != "" {
		if err := os.MkdirAll(td, 0o755); err != nil {
			return fmt.Errorf("failed to create tmpdir %s: %w", td, err)
		}
	}

	// GAP-23: Warn about stale clonr.db left from the pre-rename installation.
	// The file is harmless but confuses operators who see an unexpected DB file.
	// We do NOT auto-delete — this is an explicit operator action.
	// Check both the DB directory and its parent: old installs placed clonr.db at
	// the data root (/var/lib/clustr/clonr.db) while the current layout uses a
	// db/ subdirectory (/var/lib/clustr/db/clustr.db).
	for _, staleCandidate := range []string{
		filepath.Join(filepath.Dir(cfg.DBPath), "clonr.db"),           // same dir as DB
		filepath.Join(filepath.Dir(filepath.Dir(cfg.DBPath)), "clonr.db"), // parent dir (data root)
	} {
		if _, statErr := os.Stat(staleCandidate); statErr == nil {
			if cfg.DBPath != staleCandidate { // don't warn when both paths resolve to the same file
				log.Warn().Str("path", staleCandidate).
					Msg("stale clonr.db found from pre-rename installation; can be safely deleted")
			}
		}
	}

	// Open database (applies migrations on first run).
	database, err := db.Open(cfg.DBPath)

	if err != nil {
		return fmt.Errorf("failed to open database %s: %w", cfg.DBPath, err)
	}
	defer database.Close()

	log.Info().Str("db", cfg.DBPath).Msg("database ready")

	// #243: Bootstrap control-plane host row (idempotent, creates on first run).
	{
		ctx := context.Background()
		if _, err := database.BootstrapControlPlaneHost(ctx); err != nil {
			log.Warn().Err(err).Msg("startup: failed to bootstrap control-plane host row (non-fatal)")
		}
	}

	// S1-15/16: Validate CLUSTR_SECRET_KEY in non-dev mode.
	// The server hard-fails if the key is unset to prevent credentials being stored
	// in plaintext (LDAP passwords, BMC passwords) after the encryption migrations.
	if !cfg.AuthDevMode {
		if err := secrets.ValidateKey(); err != nil {
			return fmt.Errorf("startup: %w", err)
		}
		log.Info().Msg("secret key: validated")
	} else {
		log.Warn().Msg("CLUSTR_AUTH_DEV_MODE=1 — CLUSTR_SECRET_KEY not validated (dev mode only)")
	}

	// S1-15: Re-encrypt any plaintext LDAP credentials from pre-038 deployments.
	{
		ctx := context.Background()
		changed, err := database.MigrateLDAPCredentials(ctx)
		if err != nil {
			log.Warn().Err(err).Msg("startup: LDAP credential migration failed (non-fatal; credentials remain plaintext until re-saved)")
		} else if changed {
			log.Info().Msg("startup: LDAP credentials re-encrypted at rest (migration 038)")
		}
	}

	// S1-16: Re-encrypt any plaintext BMC/power_provider credentials from pre-039 deployments.
	{
		ctx := context.Background()
		changed, err := database.MigrateBMCCredentials(ctx)
		if err != nil {
			log.Warn().Err(err).Msg("startup: BMC credential migration failed (non-fatal; credentials remain plaintext until re-saved)")
		} else if changed {
			log.Info().Msg("startup: BMC/power_provider credentials re-encrypted at rest (migration 039)")
		}
	}

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

	// pxeSrv is created before the HTTP server but wired with the switch-discovery
	// callback after srv is constructed (networkMgr lives inside srv).
	var pxeSrv *pxe.Server
	if cfg.PXE.Enabled {
		var err error
		pxeSrv, err = pxe.New(cfg.PXE)
		if err != nil {
			return fmt.Errorf("failed to init PXE server: %w", err)
		}
	}

	// Bootstrap first-run user account (ADR-0007). Must run before BootstrapAdminKey
	// so the UI flow (username+password) is set up even on brand-new installs.
	if !cfg.AuthDevMode {
		if err := server.BootstrapDefaultUser(ctx, database); err != nil {
			return fmt.Errorf("failed to bootstrap default user: %w", err)
		}
	}

	// WARN if the default "clustr" admin is absent — indicates operator ran
	// bootstrap-admin --username X without --replace-default and the default
	// account was never created, or it was explicitly removed. We do NOT
	// auto-recreate; we just make the absence loud. The web UI exposes the
	// default_admin_present field from /api/v1/auth/status for the same signal.
	server.WarnIfDefaultAdminMissing(ctx, database)

	// One-shot idempotent migration: clear must_change_password for all users.
	// Operators upgrading from a build that set must_change_password=1 will not
	// get stuck in a forced-change loop. Safe to run on every boot.
	if err := database.ClearAllMustChangePassword(ctx); err != nil {
		return fmt.Errorf("failed to clear must_change_password flags: %w", err)
	}

	// Bootstrap initial admin API key if none exists.
	// Prints the raw key to stdout ONCE — operator must capture it.
	if !cfg.AuthDevMode {
		if err := server.BootstrapAdminKey(ctx, database); err != nil {
			return fmt.Errorf("failed to bootstrap admin key: %w", err)
		}
	}

	// Wire up and start the HTTP server.
	srv := server.New(cfg, database, server.BuildInfo{
		Version:            version,
		CommitSHA:          commitSHA,
		BuildTime:          buildTime,
		SlurmVersion:       builtinSlurmVersion,
		SlurmBundleVersion: builtinSlurmBundleVersion,
		SlurmBundleSHA256:  builtinSlurmBundleSHA256,
	})

	// Wire PXE callbacks now that the network manager and database are available.
	if pxeSrv != nil {
		netMgr := srv.NetworkManager()
		pxeSrv.DHCPServer.OnSwitchDiscovered = func(mac, vendor, ip string) {
			if err := netMgr.HandleDiscoveredSwitch(context.Background(), mac, vendor, ip); err != nil {
				log.Error().Err(err).Str("mac", mac).Str("vendor", vendor).
					Msg("DHCP: switch auto-discovery failed")
			}
		}
		// Wire DHCP MAC→IP reservation lookup so nodes with a static IP configured
		// on their InterfaceConfig are always offered that IP rather than a pool address.
		pxeSrv.DHCPServer.ResolveReservedIP = func(mac string) net.IP {
			nodeCfg, err := database.GetNodeConfigByMAC(context.Background(), strings.ToLower(mac))
			if err != nil {
				// Node not registered — fall through to pool allocation.
				return nil
			}
			for _, iface := range nodeCfg.Interfaces {
				if iface.IPAddress != "" {
					ip, _, parseErr := net.ParseCIDR(iface.IPAddress)
					if parseErr == nil {
						return ip
					}
				}
			}
			// Registered node but no IP configured — fall through to pool allocation.
			return nil
		}
		// Wire DB-backed pool guard: before handing out a pool IP, confirm it
		// isn't already reserved as a static address for a different registered node.
		// Fail-open: if the DB query errors, allow the assignment rather than
		// blocking DHCP entirely.
		pxeSrv.DHCPServer.IsIPReservedByOtherMAC = func(ip string, mac string) bool {
			reserved, err := database.IsIPReservedForOtherNode(context.Background(), ip, strings.ToLower(mac))
			if err != nil {
				log.Warn().Err(err).Str("ip", ip).Str("mac", mac).
					Msg("DHCP: IP reservation check failed — allowing pool assignment (fail-open)")
				return false
			}
			return reserved
		}
		// Wire DHCP lease lookup into the registration handler so it can
		// auto-populate node interfaces from the DHCP-assigned IP on first boot.
		srv.SetDHCPLeaseLookup(pxeSrv.DHCPServer.GetLeaseIP)
		// Wire the DHCP server into the system handler for PXE-in-flight detection.
		srv.SetDHCPLeasesOnSystemHandler(pxeSrv.DHCPServer)
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

	// Startup firmware/layout consistency audit.
	// Log warnings for any image whose declared firmware contradicts its stored
	// default_layout — e.g. firmware=bios with an ESP partition (the symptom that
	// caused VM207's grub2-install failure).  These are advisory only; the server
	// continues to start normally.
	if imgs, auditErr := database.ListBaseImages(ctx, "", ""); auditErr == nil {
		for _, img := range imgs {
			if warn := imageFirmwareLayoutMismatch(img); warn != "" {
				log.Error().
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

	// Reconcile any initramfs builds left in 'pending' state by a prior crash
	// or server restart mid-build. Attempts to self-heal builds whose staging
	// artifact is intact; marks the rest as failed.
	if err := srv.ReconcileStuckInitramfsBuilds(ctx); err != nil {
		log.Error().Err(err).Msg("reconcile stuck initramfs builds failed (non-fatal)")
	}

	// Clean up orphaned clustr-initramfs-build-* temp dirs left by builds that
	// crashed before their defer os.RemoveAll ran. 24h window avoids touching
	// dirs that might belong to a very recent crash.
	srv.CleanupOrphanedInitramfsTmpDirs(24 * time.Hour)

	// #249: Startup image blob reconcile pass — walks all 'ready'/'corrupt'/
	// 'blob_missing' images and checks on-disk artifacts against DB records.
	// Runs in a goroutine so the HTTP listener comes up immediately.
	go func() {
		log.Info().Msg("image-reconcile: startup pass starting (background)")
		srv.ReconcileAllImages(ctx)
	}()

	// #113 — posixid reconciliation pass.
	// Corrects system_accounts rows whose UID was mis-allocated into LDAP user
	// space (>= 1000) by the Sprint 13 single-range allocator.  Gated behind
	// CLUSTR_RECONCILE_SYSACCOUNTS=1 so it only fires when the operator
	// explicitly requests it.  Read the controller node ID from the environment
	// (CLUSTR_RECONCILE_CONTROLLER_NODE_ID).
	if os.Getenv("CLUSTR_RECONCILE_SYSACCOUNTS") == "1" {
		controllerNodeID := os.Getenv("CLUSTR_RECONCILE_CONTROLLER_NODE_ID")
		if controllerNodeID == "" {
			log.Warn().Msg("CLUSTR_RECONCILE_SYSACCOUNTS=1 set but CLUSTR_RECONCILE_CONTROLLER_NODE_ID is empty — skipping posixid reconciliation")
		} else {
			log.Info().Str("controller_node_id", controllerNodeID).Msg("posixid reconciliation: starting (CLUSTR_RECONCILE_SYSACCOUNTS=1)")
			if err := srv.SysAccountsManager().ReconcileFromNode(ctx, srv.ClientdHub(), controllerNodeID); err != nil {
				log.Error().Err(err).Msg("posixid reconciliation failed (non-fatal)")
			}
		}
	}

	// Start background workers (reimage scheduler, etc.) before accepting traffic.
	srv.StartBackgroundWorkers(ctx)

	if err := srv.ListenAndServe(ctx); err != nil {
		return fmt.Errorf("server error: %w", err)
	}

	log.Info().Msg("shutdown complete")
	return nil
}
