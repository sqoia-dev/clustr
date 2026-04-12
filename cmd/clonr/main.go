package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/chroot"
	"github.com/sqoia-dev/clonr/pkg/client"
	"github.com/sqoia-dev/clonr/pkg/config"
	"github.com/sqoia-dev/clonr/pkg/deploy"
	"github.com/sqoia-dev/clonr/pkg/hardware"
	"github.com/sqoia-dev/clonr/pkg/ipmi"
)

// ANSI colour codes used by the log viewer.
const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorYellow = "\033[33m"
	colorCyan   = "\033[36m"
	colorGray   = "\033[90m"
)

var version = "dev"

// Persistent flag values applied to every subcommand.
var (
	flagServer string
	flagToken  string
)

func main() {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

var rootCmd = &cobra.Command{
	Use:     "clonr",
	Short:   "Node cloning and image management for HPC clusters",
	Version: version,
}

func init() {
	// Persistent flags available on all subcommands.
	rootCmd.PersistentFlags().StringVar(&flagServer, "server", "", "clonr-serverd URL (env: CLONR_SERVER)")
	rootCmd.PersistentFlags().StringVar(&flagToken, "token", "", "API auth token (env: CLONR_TOKEN)")

	// image subcommand group.
	imageCmd := &cobra.Command{
		Use:   "image",
		Short: "Manage base images",
	}
	imageCmd.AddCommand(
		newImageListCmd(),
		newImageDetailsCmd(),
		newImagePullCmd(),
		newImageImportISOCmd(),
		newImageCaptureCmd(),
	)
	rootCmd.AddCommand(imageCmd)

	// node subcommand group.
	nodeCmd := &cobra.Command{
		Use:   "node",
		Short: "Manage node configurations",
	}
	nodeCmd.AddCommand(
		newNodeListCmd(),
		newNodeConfigCmd(),
	)
	rootCmd.AddCommand(nodeCmd)

	// ipmi subcommand group.
	ipmiCmd := &cobra.Command{
		Use:   "ipmi",
		Short: "IPMI / BMC management",
	}
	ipmiCmd.AddCommand(
		newIPMIStatusCmd(),
		newIPMIPowerCmd(),
		newIPMIConfigureCmd(),
		newIPMIPXECmd(),
		newIPMISensorsCmd(),
	)
	rootCmd.AddCommand(ipmiCmd)

	// Top-level commands.
	rootCmd.AddCommand(hardwareCmd)
	rootCmd.AddCommand(identifyCmd)
	rootCmd.AddCommand(newDeployCmd())
	rootCmd.AddCommand(newFixEFIBootCmd())
	rootCmd.AddCommand(newLogsCmd())
	rootCmd.AddCommand(newShellCmd())
}

// clientFromFlags builds an API client resolving server/token from flags then env.
func clientFromFlags() *client.Client {
	cfg := config.LoadClientConfig()
	if flagServer != "" {
		cfg.ServerURL = flagServer
	}
	if flagToken != "" {
		cfg.AuthToken = flagToken
	}
	return client.New(cfg.ServerURL, cfg.AuthToken)
}

// ─── image list ──────────────────────────────────────────────────────────────

func newImageListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all base images on the server",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			c := clientFromFlags()

			images, err := c.ListImages(ctx)
			if err != nil {
				return fmt.Errorf("list images: %w", err)
			}

			if len(images) == 0 {
				fmt.Println("No images found.")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tNAME\tVERSION\tOS\tARCH\tFORMAT\tSTATUS\tSIZE\tCREATED")
			for _, img := range images {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					shortID(img.ID),
					img.Name,
					img.Version,
					img.OS,
					img.Arch,
					img.Format,
					img.Status,
					humanBytes(img.SizeBytes),
					img.CreatedAt.Format("2006-01-02"),
				)
			}
			return w.Flush()
		},
	}
}

// ─── image details ───────────────────────────────────────────────────────────

func newImageDetailsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "details <id>",
		Short: "Show detailed metadata for an image",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			c := clientFromFlags()

			img, err := c.GetImage(ctx, args[0])
			if err != nil {
				return fmt.Errorf("get image: %w", err)
			}

			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(img)
		},
	}
}

// ─── image pull ──────────────────────────────────────────────────────────────

func newImagePullCmd() *cobra.Command {
	var (
		flagURL     string
		flagName    string
		flagVersion string
		flagOS      string
		flagArch    string
		flagFormat  string
		flagNotes   string
	)

	cmd := &cobra.Command{
		Use:   "pull",
		Short: "Pull an image from a URL into the server's image store",
		RunE: func(cmd *cobra.Command, args []string) error {
			if flagURL == "" {
				return fmt.Errorf("--url is required")
			}
			if flagName == "" {
				return fmt.Errorf("--name is required")
			}

			ctx := context.Background()
			c := clientFromFlags()

			req := api.PullRequest{
				URL:     flagURL,
				Name:    flagName,
				Version: flagVersion,
				OS:      flagOS,
				Arch:    flagArch,
				Format:  api.ImageFormat(flagFormat),
				Notes:   flagNotes,
			}

			fmt.Fprintf(os.Stderr, "Requesting pull of %s from %s...\n", flagName, flagURL)
			img, err := c.PullImage(ctx, req)
			if err != nil {
				return fmt.Errorf("pull image: %w", err)
			}

			fmt.Printf("Image pull initiated:\n")
			fmt.Printf("  ID:     %s\n", img.ID)
			fmt.Printf("  Name:   %s\n", img.Name)
			fmt.Printf("  Status: %s\n", img.Status)
			fmt.Printf("\nPoll status with: clonr image details %s\n", img.ID)
			return nil
		},
	}

	cmd.Flags().StringVar(&flagURL, "url", "", "Source URL for the image blob (required)")
	cmd.Flags().StringVar(&flagName, "name", "", "Image name (required)")
	cmd.Flags().StringVar(&flagVersion, "version", "1.0.0", "Image version")
	cmd.Flags().StringVar(&flagOS, "os", "", "OS name, e.g. 'Rocky Linux 9'")
	cmd.Flags().StringVar(&flagArch, "arch", "x86_64", "Target architecture")
	cmd.Flags().StringVar(&flagFormat, "format", "filesystem", "Image format: filesystem or block")
	cmd.Flags().StringVar(&flagNotes, "notes", "", "Free-text notes")

	return cmd
}

// ─── node list ───────────────────────────────────────────────────────────────

func newNodeListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all node configurations",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			c := clientFromFlags()

			nodes, err := c.ListNodes(ctx)
			if err != nil {
				return fmt.Errorf("list nodes: %w", err)
			}

			if len(nodes) == 0 {
				fmt.Println("No node configurations found.")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tHOSTNAME\tFQDN\tMAC\tIMAGE\tGROUPS")
			for _, node := range nodes {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					shortID(node.ID),
					node.Hostname,
					node.FQDN,
					node.PrimaryMAC,
					shortID(node.BaseImageID),
					strings.Join(node.Groups, ","),
				)
			}
			return w.Flush()
		},
	}
}

// ─── node config ─────────────────────────────────────────────────────────────

func newNodeConfigCmd() *cobra.Command {
	var flagMAC string

	cmd := &cobra.Command{
		Use:   "config [id]",
		Short: "Show node configuration by ID or MAC address",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			c := clientFromFlags()

			var (
				cfg *api.NodeConfig
				err error
			)

			switch {
			case len(args) == 1:
				cfg, err = c.GetNode(ctx, args[0])
			case flagMAC != "":
				cfg, err = c.GetNodeConfigByMAC(ctx, flagMAC)
			default:
				return fmt.Errorf("provide an ID as argument or --mac <address>")
			}

			if err != nil {
				return fmt.Errorf("get node config: %w", err)
			}

			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(cfg)
		},
	}

	cmd.Flags().StringVar(&flagMAC, "mac", "", "Lookup node by primary MAC address")
	return cmd
}

// ─── hardware ────────────────────────────────────────────────────────────────

var hardwareCmd = &cobra.Command{
	Use:   "hardware",
	Short: "Discover and print this node's hardware profile as JSON",
	Long: `hardware runs full hardware discovery (CPU, memory, disks, NICs, DMI)
and prints the result as formatted JSON to stdout. No server connection required.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		info, err := hardware.Discover()
		if err != nil {
			return fmt.Errorf("hardware discovery: %w", err)
		}

		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(info)
	},
}

// ─── identify ────────────────────────────────────────────────────────────────

// identifyCmd runs hardware discovery and prints the result as JSON.
// Kept for backward compatibility — functionally identical to hardware.
var identifyCmd = &cobra.Command{
	Use:   "identify",
	Short: "Discover and print this node's hardware profile as JSON (alias for hardware)",
	RunE: func(cmd *cobra.Command, args []string) error {
		info, err := hardware.Discover()
		if err != nil {
			return fmt.Errorf("hardware discovery: %w", err)
		}

		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(info)
	},
}

// ─── deploy ──────────────────────────────────────────────────────────────────

func newDeployCmd() *cobra.Command {
	var (
		flagImage      string
		flagDisk       string
		flagMountRoot  string
		flagFixEFI     bool
		flagAuto       bool
		flagNoRollback bool
		flagSkipVerify bool
		flagTimeout    string
	)

	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Deploy an image to this node",
		Long: `deploy performs a full deployment:
  1. Discover local hardware
  2. Fetch node config from server (matched by MAC address)
  3. Fetch image details from server
  4. Preflight: validate disk size and architecture
  5. Deploy: download and write the image
  6. Finalize: apply hostname, network, SSH keys
  7. Fix EFI boot entries (if --fix-efi is set)

With --auto: discovers hardware, registers with the server, and waits for an
admin to assign a base image before proceeding with deployment. Intended for
PXE-booted nodes running from initramfs.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// --auto mode: register with server, wait for image assignment, then deploy.
			if flagAuto {
				return runAutoDeployMode()
			}

			if flagImage == "" {
				return fmt.Errorf("--image is required")
			}

			// Resolve deployment timeout (env var overrides flag default).
			timeoutStr := flagTimeout
			if envTimeout := os.Getenv("CLONR_DEPLOY_TIMEOUT"); envTimeout != "" {
				timeoutStr = envTimeout
			}
			deployTimeout, err := time.ParseDuration(timeoutStr)
			if err != nil {
				return fmt.Errorf("invalid deployment timeout %q: %w", timeoutStr, err)
			}

			baseCtx := context.Background()
			ctx, cancelTimeout := context.WithTimeout(baseCtx, deployTimeout)
			defer cancelTimeout()

			c := clientFromFlags()

			// ── Remote logging setup ─────────────────────────────────────────
			// Discover a best-effort MAC for the log writer before hardware
			// discovery runs fully. We'll update nodeMAC after hardware is done.
			remoteWriter := client.NewRemoteLogWriter(c, "unknown", "", client.WithComponent("deploy"))
			defer remoteWriter.Close()

			// ── Structured progress reporter ─────────────────────────────────
			// Created early with a placeholder MAC; updated after hardware discovery.
			// Best-effort: failures to POST progress don't abort the deployment.
			progressReporter := client.NewProgressReporter(c, "unknown", "")
			defer func() { progressReporter.Complete() }()

			// Tee all zerolog output: local console + remote server.
			multi := zerolog.MultiLevelWriter(zerolog.ConsoleWriter{Out: os.Stderr}, remoteWriter)
			deployLog := zerolog.New(multi).With().Timestamp().Logger()
			// Wire the deploy package so subprocess output goes through the same logger.
			deploy.SetLogger(deployLog)
			// ─────────────────────────────────────────────────────────────────

			// Step 1: Discover hardware.
			fmt.Fprintln(os.Stderr, "[1/6] Discovering hardware...")
			deployLog.Info().Str("component", "hardware").Msg("starting hardware discovery")
			hw, err := hardware.Discover()
			if err != nil {
				return fmt.Errorf("hardware discovery: %w", err)
			}

			// Step 2: Get node config by primary MAC.
			fmt.Fprintln(os.Stderr, "[2/6] Fetching node config from server...")
			primaryMAC := primaryMACFromHW(hw)
			if primaryMAC == "" {
				return fmt.Errorf("no usable NIC found — cannot look up node config")
			}

			// Now that we have the MAC, update the remote writer identity.
			remoteWriter.SetNodeMAC(primaryMAC)
			progressReporter.SetNode(primaryMAC, "")
			deployLog.Info().Str("component", "deploy").Str("mac", primaryMAC).Msg("fetching node config")

			nodeCfg, err := c.GetNodeConfigByMAC(ctx, primaryMAC)
			if err != nil {
				deployLog.Error().Str("component", "deploy").Err(err).Msg("failed to fetch node config")
				return fmt.Errorf("get node config (MAC %s): %w", primaryMAC, err)
			}
			remoteWriter.SetHostname(nodeCfg.Hostname)
			progressReporter.SetNode(primaryMAC, nodeCfg.Hostname)
			fmt.Fprintf(os.Stderr, "    Node: %s (%s)\n", nodeCfg.Hostname, nodeCfg.ID)
			deployLog.Info().Str("component", "deploy").Str("hostname", nodeCfg.Hostname).Msg("node config loaded")

			// Step 3: Get image details.
			fmt.Fprintln(os.Stderr, "[3/6] Fetching image details...")
			deployLog.Info().Str("component", "deploy").Str("image_id", flagImage).Msg("fetching image details")
			img, err := c.GetImage(ctx, flagImage)
			if err != nil {
				deployLog.Error().Str("component", "deploy").Err(err).Msg("failed to fetch image")
				return fmt.Errorf("get image %s: %w", flagImage, err)
			}
			if img.Status != api.ImageStatusReady {
				return fmt.Errorf("image %s is not ready (status: %s)", img.ID, img.Status)
			}
			fmt.Fprintf(os.Stderr, "    Image: %s %s (%s)\n", img.Name, img.Version, img.Format)
			deployLog.Info().Str("component", "deploy").
				Str("image", img.Name).Str("version", img.Version).Str("format", string(img.Format)).
				Msg("image ready")

			// Resolve server URL for blob download.
			cfg := config.LoadClientConfig()
			if flagServer != "" {
				cfg.ServerURL = flagServer
			}
			blobURL := cfg.ServerURL + "/api/v1/images/" + img.ID + "/blob"

			// Resolve mount root.
			mountRoot := flagMountRoot
			if mountRoot == "" {
				tmp, err := os.MkdirTemp("", "clonr-deploy-*")
				if err != nil {
					return fmt.Errorf("create temp mount root: %w", err)
				}
				defer os.RemoveAll(tmp)
				mountRoot = tmp
			}

			// Step 4: Preflight.
			fmt.Fprintln(os.Stderr, "[4/6] Running preflight checks...")
			deployLog.Info().Str("component", "deploy").Msg("running preflight checks")
			progressReporter.StartPhase("preflight", 0)
			var deployer deploy.Deployer
			switch img.Format {
			case api.ImageFormatBlock:
				deployer = &deploy.BlockDeployer{}
			default:
				deployer = &deploy.FilesystemDeployer{}
			}

			if err := deployer.Preflight(ctx, img.DiskLayout, *hw); err != nil {
				deployLog.Error().Str("component", "deploy").Err(err).Msg("preflight failed")
				progressReporter.EndPhase(err.Error())
				return fmt.Errorf("preflight: %w", err)
			}
			deployLog.Info().Str("component", "deploy").Msg("preflight passed")
			progressReporter.EndPhase("")

			// Step 5: Deploy.
			fmt.Fprintln(os.Stderr, "[5/6] Deploying image...")
			deployLog.Info().Str("component", "deploy").Msg("starting image write")
			opts := deploy.DeployOpts{
				ImageURL:         blobURL,
				AuthToken:        cfg.AuthToken,
				TargetDisk:       flagDisk,
				Format:           string(img.Format),
				MountRoot:        mountRoot,
				NoRollback:       flagNoRollback,
				SkipVerify:       flagSkipVerify,
				ExpectedChecksum: img.Checksum,
				Reporter:         progressReporter,
			}

			start := time.Now()
			var lastPhase string
			progressFn := func(written, total int64, phase string) {
				if phase != lastPhase {
					if lastPhase != "" {
						fmt.Fprintln(os.Stderr) // newline after previous phase
					}
					lastPhase = phase
					deployLog.Info().Str("component", "deploy").Str("phase", phase).Msg("deployment phase started")
				}
				if total > 0 {
					pct := float64(written) / float64(total) * 100
					fmt.Fprintf(os.Stderr, "\r    %s: %.1f%% (%s / %s)",
						phase, pct, humanBytes(written), humanBytes(total))
				} else {
					fmt.Fprintf(os.Stderr, "\r    %s: %s written", phase, humanBytes(written))
				}
			}

			if err := deployer.Deploy(ctx, opts, progressFn); err != nil {
				fmt.Fprintln(os.Stderr) // newline after progress
				if ctx.Err() != nil {
					deployLog.Error().Str("component", "deploy").
						Dur("timeout", deployTimeout).
						Msg("deployment timed out — rollback attempted")
					return fmt.Errorf("deploy: timed out after %s (limit set by --timeout / CLONR_DEPLOY_TIMEOUT): %w",
						deployTimeout, err)
				}
				deployLog.Error().Str("component", "deploy").Err(err).Msg("image write failed")
				return fmt.Errorf("deploy: %w", err)
			}
			elapsed := time.Since(start).Round(time.Second)
			fmt.Fprintf(os.Stderr, "\n    Done in %s\n", elapsed)
			deployLog.Info().Str("component", "deploy").Str("duration", elapsed.String()).Msg("image write complete")

			// Step 6: Finalize.
			fmt.Fprintln(os.Stderr, "[6/6] Applying node configuration...")
			deployLog.Info().Str("component", "chroot").Msg("applying node configuration")
			progressReporter.StartPhase("finalizing", 0)
			if err := deployer.Finalize(ctx, *nodeCfg, mountRoot); err != nil {
				deployLog.Error().Str("component", "chroot").Err(err).Msg("finalize failed")
				progressReporter.EndPhase(err.Error())
				return fmt.Errorf("finalize: %w", err)
			}
			fmt.Fprintln(os.Stderr, "    Hostname, network, and SSH keys applied.")
			deployLog.Info().Str("component", "chroot").Msg("node configuration applied")
			progressReporter.EndPhase("")

			// Step 7: EFI boot repair (optional).
			if flagFixEFI {
				fmt.Fprintln(os.Stderr, "[+] Repairing EFI boot entries...")
				deployLog.Info().Str("component", "efiboot").Msg("repairing EFI boot entries")
				disk := flagDisk
				if disk == "" {
					disk = "/dev/sda"
				}
				label := img.Name
				if err := deploy.FixEFIBoot(ctx, disk, 1, label, `\EFI\rocky\grubx64.efi`); err != nil {
					// Non-fatal — log the error but don't fail the deployment.
					fmt.Fprintf(os.Stderr, "    Warning: EFI boot repair failed: %v\n", err)
					deployLog.Warn().Str("component", "efiboot").Err(err).Msg("EFI boot repair failed (non-fatal)")
				} else {
					fmt.Fprintln(os.Stderr, "    EFI boot entry set.")
					deployLog.Info().Str("component", "efiboot").Msg("EFI boot entry set")
				}
			}

			fmt.Printf("\nDeployment complete.\n")
			fmt.Printf("  Node:     %s\n", nodeCfg.Hostname)
			fmt.Printf("  Image:    %s %s\n", img.Name, img.Version)
			fmt.Printf("  Duration: %s\n", time.Since(start).Round(time.Second))
			return nil
		},
	}

	cmd.Flags().StringVar(&flagImage, "image", "", "Image ID to deploy (required without --auto)")
	cmd.Flags().StringVar(&flagDisk, "disk", "", "Target block device, e.g. /dev/nvme0n1 (auto-detected if omitted)")
	cmd.Flags().StringVar(&flagMountRoot, "mount-root", "", "Temporary mount point directory (auto-created if omitted)")
	cmd.Flags().BoolVar(&flagFixEFI, "fix-efi", false, "Repair EFI boot entries after deployment")
	cmd.Flags().BoolVar(&flagAuto, "auto", false,
		"Auto mode: register with server, wait for image assignment, then deploy (for PXE-booted nodes)")
	cmd.Flags().BoolVar(&flagNoRollback, "no-rollback", false,
		"Skip partition table backup/restore on failure (use when intentionally wiping a disk)")
	cmd.Flags().BoolVar(&flagSkipVerify, "skip-verify", false,
		"Skip image checksum verification (deploy even if the sha256 does not match)")
	cmd.Flags().StringVar(&flagTimeout, "timeout", "30m",
		"Maximum time allowed for the entire deployment (env: CLONR_DEPLOY_TIMEOUT, e.g. 30m, 1h)")

	return cmd
}

// runAutoDeployMode implements deploy --auto.
// It discovers hardware, registers the node with the server, then waits until
// an admin assigns a base image, at which point it proceeds with full deployment.
func runAutoDeployMode() error {
	ctx := context.Background()
	c := clientFromFlags()

	// Step 1: Discover hardware.
	fmt.Fprintln(os.Stderr, "[auto] Discovering hardware...")
	hw, err := hardware.Discover()
	if err != nil {
		return fmt.Errorf("hardware discovery: %w", err)
	}

	primaryMAC := primaryMACFromHW(hw)
	if primaryMAC == "" {
		return fmt.Errorf("no usable NIC found — cannot register node")
	}

	// Set up remote log writer once we have the MAC.
	remoteWriter := client.NewRemoteLogWriter(c, primaryMAC, hw.Hostname, client.WithComponent("deploy"))
	defer remoteWriter.Close()
	multi := zerolog.MultiLevelWriter(zerolog.ConsoleWriter{Out: os.Stderr}, remoteWriter)
	deployLog := zerolog.New(multi).With().Timestamp().Logger()
	// Wire the deploy package so subprocess output goes through the same logger.
	deploy.SetLogger(deployLog)

	deployLog.Info().Str("mac", primaryMAC).Str("hostname", hw.Hostname).
		Msg("hardware discovered, registering with server")

	// Step 2: Register with the server (upsert).
	hwJSON, err := json.Marshal(hw)
	if err != nil {
		return fmt.Errorf("marshal hardware profile: %w", err)
	}

	fmt.Fprintln(os.Stderr, "[auto] Registering with server...")
	regResp, err := c.RegisterNode(ctx, api.RegisterRequest{HardwareProfile: hwJSON})
	if err != nil {
		return fmt.Errorf("register node: %w", err)
	}

	deployLog.Info().
		Str("action", regResp.Action).
		Str("node_id", regResp.NodeConfig.ID).
		Msg("registered with server")

	// Step 3: Act on server directive.
	switch regResp.Action {
	case "deploy":
		fmt.Fprintln(os.Stderr, "[auto] Image assigned — proceeding with deployment")
		return runAutoDeployImage(ctx, c, *regResp.NodeConfig, deployLog, remoteWriter)

	case "wait":
		fmt.Fprintln(os.Stderr, "[auto] Waiting for admin to assign an image (polling every 30s)...")
		deployLog.Info().Msg("entering wait loop — assign an image via the clonr UI or API")
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-sleepCtx(ctx, 30*time.Second):
			}

			nodeCfg, err := c.GetNodeConfigByMAC(ctx, primaryMAC)
			if err != nil {
				deployLog.Warn().Err(err).Msg("poll failed, retrying")
				continue
			}
			if nodeCfg.BaseImageID != "" {
				deployLog.Info().Str("image_id", nodeCfg.BaseImageID).Msg("image assigned, starting deployment")
				fmt.Fprintln(os.Stderr, "[auto] Image assigned — proceeding with deployment")
				return runAutoDeployImage(ctx, c, *nodeCfg, deployLog, remoteWriter)
			}
			deployLog.Debug().Msg("no image assigned yet, still waiting")
		}

	case "capture":
		fmt.Fprintln(os.Stderr, "[auto] Capture mode not yet implemented")
		deployLog.Info().Msg("capture action received — not yet implemented")
		return nil

	default:
		return fmt.Errorf("unknown action from server: %s", regResp.Action)
	}
}

// sleepCtx returns a channel that closes after d, or immediately if ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		select {
		case <-ctx.Done():
		case <-time.After(d):
		}
	}()
	return ch
}

// runAutoDeployImage performs the full deployment given a NodeConfig with an assigned image.
// The node config must have BaseImageID set.
func runAutoDeployImage(ctx context.Context, c *client.Client, nodeCfg api.NodeConfig, deployLog zerolog.Logger, remoteWriter *client.RemoteLogWriter) (retErr error) {
	// Panic recovery: if any deployment sub-call panics (e.g. nil pointer in a
	// hardware probe or partition library), catch it here, flush buffered logs so
	// the last messages before the panic reach the server, then return as an error
	// rather than crashing PID 1 in the initramfs.
	defer func() {
		if r := recover(); r != nil {
			deployLog.Error().
				Interface("panic", r).
				Stack().
				Msg("deploy panicked — caught by recovery wrapper")
			retErr = fmt.Errorf("deploy panicked: %v", r)
			// Flush immediately so crash logs reach the server before we exit.
			remoteWriter.FlushSync()
		}
	}()

	cfg := config.LoadClientConfig()
	if flagServer != "" {
		cfg.ServerURL = flagServer
	}

	// Create progress reporter — best-effort, failures don't abort deployment.
	reporter := client.NewProgressReporter(c, nodeCfg.PrimaryMAC, nodeCfg.Hostname)
	defer reporter.Complete()

	// Fetch image details.
	img, err := c.GetImage(ctx, nodeCfg.BaseImageID)
	if err != nil {
		return fmt.Errorf("fetch image %s: %w", nodeCfg.BaseImageID, err)
	}
	if img.Status != api.ImageStatusReady {
		return fmt.Errorf("image %s is not ready (status: %s)", img.ID, img.Status)
	}

	deployLog.Info().Str("image", img.Name).Str("version", img.Version).
		Str("format", string(img.Format)).Msg("image details fetched")

	// Resolve hardware for preflight.
	hw, err := hardware.Discover()
	if err != nil {
		return fmt.Errorf("hardware discovery for preflight: %w", err)
	}

	mountRoot, err := os.MkdirTemp("", "clonr-auto-deploy-*")
	if err != nil {
		return fmt.Errorf("create temp mount root: %w", err)
	}
	defer os.RemoveAll(mountRoot)

	var deployer deploy.Deployer
	switch img.Format {
	case api.ImageFormatBlock:
		deployer = &deploy.BlockDeployer{}
	default:
		deployer = &deploy.FilesystemDeployer{}
	}

	deployLog.Info().Msg("running preflight checks")
	reporter.StartPhase("preflight", 0)
	if err := deployer.Preflight(ctx, img.DiskLayout, *hw); err != nil {
		reporter.EndPhase(err.Error())
		return fmt.Errorf("preflight: %w", err)
	}
	reporter.EndPhase("")

	blobURL := cfg.ServerURL + "/api/v1/images/" + img.ID + "/blob"
	deployLog.Info().Str("url", blobURL).Msg("starting image write")

	opts := deploy.DeployOpts{
		ImageURL:         blobURL,
		AuthToken:        cfg.AuthToken,
		TargetDisk:       "", // auto-detect
		Format:           string(img.Format),
		MountRoot:        mountRoot,
		ExpectedChecksum: img.Checksum,
		Reporter:         reporter,
	}

	var lastLoggedPct int64
	progressFn := func(written, total int64, phase string) {
		if total > 0 {
			pct := float64(written) / float64(total) * 100
			fmt.Fprintf(os.Stderr, "\r    %s: %.1f%% (%s / %s)",
				phase, pct, humanBytes(written), humanBytes(total))
			// Log via zerolog at every 10% milestone so the remote log stream
			// shows download progress even in silent initramfs environments.
			milestone := int64(pct/10) * 10
			if milestone > lastLoggedPct && milestone > 0 {
				lastLoggedPct = milestone
				deployLog.Info().
					Str("phase", phase).
					Int64("pct", milestone).
					Str("written", humanBytes(written)).
					Str("total", humanBytes(total)).
					Msg("image write progress")
			}
		} else {
			fmt.Fprintf(os.Stderr, "\r    %s: %s written", phase, humanBytes(written))
		}
	}

	deployLog.Info().Str("url", blobURL).Msg("downloading image blob from server")
	start := time.Now()
	if err := deployer.Deploy(ctx, opts, progressFn); err != nil {
		fmt.Fprintln(os.Stderr)
		deployLog.Error().Err(err).Msg("image deploy failed")
		return fmt.Errorf("deploy: %w", err)
	}
	elapsed := time.Since(start).Round(time.Second)
	fmt.Fprintf(os.Stderr, "\n    Image written in %s\n", elapsed)
	deployLog.Info().Str("duration", elapsed.String()).Msg("image write complete")

	deployLog.Info().Str("hostname", nodeCfg.Hostname).Msg("applying node configuration (hostname, network, SSH keys)")
	reporter.StartPhase("finalizing", 0)
	if err := deployer.Finalize(ctx, nodeCfg, mountRoot); err != nil {
		deployLog.Error().Err(err).Msg("finalize failed")
		reporter.EndPhase(err.Error())
		return fmt.Errorf("finalize: %w", err)
	}
	deployLog.Info().Str("hostname", nodeCfg.Hostname).Msg("node configuration applied")
	reporter.EndPhase("")

	// ── Auto boot-flip ──────────────────────────────────────────────────────
	// After a successful deploy, tell the server to set the next boot device to
	// disk via the node's configured power provider. The server handles both IPMI
	// and Proxmox. If no provider is configured, log and let the node reboot
	// normally via kernel reboot syscall (handled in finalize / reboot).
	if nodeCfg.PowerProvider != nil && nodeCfg.PowerProvider.Type != "" {
		reporter.StartPhase("flip-to-disk", 0)
		deployLog.Info().
			Str("hostname", nodeCfg.Hostname).
			Str("provider", nodeCfg.PowerProvider.Type).
			Msg("flipping next boot to disk via power provider")

		flipCtx, flipCancel := context.WithTimeout(ctx, 30*time.Second)
		flipErr := c.FlipToDisk(flipCtx, nodeCfg.ID, false)
		flipCancel()

		if flipErr != nil {
			// Non-fatal: log the error but don't abort. The OS will still boot
			// if the disk is first in the persistent boot order; this only sets
			// the one-time next-boot device.
			deployLog.Warn().Err(flipErr).
				Str("hostname", nodeCfg.Hostname).
				Msg("flip-to-disk failed (non-fatal) — node may PXE boot again on next restart")
			reporter.EndPhase(flipErr.Error())
		} else {
			deployLog.Info().Str("hostname", nodeCfg.Hostname).
				Msg("next boot set to disk")
			reporter.EndPhase("")
		}
	} else {
		deployLog.Info().Str("hostname", nodeCfg.Hostname).
			Msg("no power provider configured — manual reboot required to boot from disk")
	}
	// ───────────────────────────────────────────────────────────────────────

	deployLog.Info().Str("hostname", nodeCfg.Hostname).Str("duration",
		time.Since(start).Round(time.Second).String()).Msg("auto-deployment complete — rebooting")

	fmt.Printf("\n[auto] Deployment complete.\n")
	fmt.Printf("  Node:     %s\n", nodeCfg.Hostname)
	fmt.Printf("  Image:    %s %s\n", img.Name, img.Version)
	fmt.Printf("  Duration: %s\n", time.Since(start).Round(time.Second))
	return nil
}

// ─── fix-efiboot ─────────────────────────────────────────────────────────────

func newFixEFIBootCmd() *cobra.Command {
	var (
		flagDisk    string
		flagESPPart int
		flagLabel   string
		flagLoader  string
	)

	cmd := &cobra.Command{
		Use:   "fix-efiboot",
		Short: "Repair EFI boot entries on a deployed node",
		Long: `fix-efiboot creates or replaces EFI NVRAM boot entries for a deployed system.
It removes any existing entries with the same label, creates a fresh entry
pointing to the ESP partition, and sets it as the first boot target.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if flagDisk == "" {
				return fmt.Errorf("--disk is required")
			}

			ctx := context.Background()
			fmt.Fprintf(os.Stderr, "Repairing EFI boot entry on %s partition %d...\n", flagDisk, flagESPPart)

			if err := deploy.FixEFIBoot(ctx, flagDisk, flagESPPart, flagLabel, flagLoader); err != nil {
				return fmt.Errorf("fix-efiboot: %w", err)
			}

			fmt.Println("EFI boot entry set successfully.")
			return nil
		},
	}

	cmd.Flags().StringVar(&flagDisk, "disk", "", "Target disk device, e.g. /dev/nvme0n1 (required)")
	cmd.Flags().IntVar(&flagESPPart, "esp", 1, "ESP partition number (default: 1)")
	cmd.Flags().StringVar(&flagLabel, "label", "Linux", "Boot menu label")
	cmd.Flags().StringVar(&flagLoader, "loader", `\EFI\rocky\grubx64.efi`, "EFI loader path relative to ESP")

	return cmd
}

// ─── ipmi ────────────────────────────────────────────────────────────────────

// ipmiClientFromFlags builds an ipmi.Client from the standard remote flags.
// If host is empty, the client targets the local BMC.
func ipmiClientFromFlags(host, user, pass string) *ipmi.Client {
	return &ipmi.Client{
		Host:     host,
		Username: user,
		Password: pass,
	}
}

// newIPMIStatusCmd shows the local BMC network config and power state.
func newIPMIStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show local BMC network config and power status",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			c := ipmiClientFromFlags("", "", "")

			cfg, err := c.GetBMCConfig(ctx)
			if err != nil {
				return fmt.Errorf("get bmc config: %w", err)
			}

			fmt.Printf("BMC Network (channel %d):\n", cfg.Channel)
			fmt.Printf("  IP Address : %s\n", cfg.IPAddress)
			fmt.Printf("  Netmask    : %s\n", cfg.Netmask)
			fmt.Printf("  Gateway    : %s\n", cfg.Gateway)
			fmt.Printf("  IP Source  : %s\n", cfg.IPSource)

			users, err := c.GetBMCUsers(ctx)
			if err == nil && len(users) > 0 {
				fmt.Printf("\nBMC Users:\n")
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "  ID\tUSERNAME\tACCESS")
				for _, u := range users {
					fmt.Fprintf(w, "  %d\t%s\t%s\n", u.ID, u.Username, u.Access)
				}
				_ = w.Flush()
			}
			return nil
		},
	}
}

// newIPMIPowerCmd controls power on a remote node via its BMC.
func newIPMIPowerCmd() *cobra.Command {
	var flagHost, flagUser, flagPass string

	cmd := &cobra.Command{
		Use:   "power [on|off|cycle|reset]",
		Short: "Control power on a node via IPMI",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			action := strings.ToLower(args[0])
			ctx := context.Background()
			c := ipmiClientFromFlags(flagHost, flagUser, flagPass)

			switch action {
			case "on":
				if err := c.PowerOn(ctx); err != nil {
					return err
				}
				fmt.Println("Power on command sent.")
			case "off":
				if err := c.PowerOff(ctx); err != nil {
					return err
				}
				fmt.Println("Power off command sent.")
			case "cycle":
				if err := c.PowerCycle(ctx); err != nil {
					return err
				}
				fmt.Println("Power cycle command sent.")
			case "reset":
				if err := c.PowerReset(ctx); err != nil {
					return err
				}
				fmt.Println("Power reset command sent.")
			case "status":
				status, err := c.PowerStatus(ctx)
				if err != nil {
					return err
				}
				fmt.Printf("Power: %s\n", status)
			default:
				return fmt.Errorf("unknown power action %q — use on, off, cycle, reset, or status", action)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&flagHost, "host", "", "BMC IP address (required for remote)")
	cmd.Flags().StringVar(&flagUser, "user", "", "BMC username")
	cmd.Flags().StringVar(&flagPass, "pass", "", "BMC password")
	return cmd
}

// newIPMIConfigureCmd configures the local BMC network interface.
func newIPMIConfigureCmd() *cobra.Command {
	var flagIP, flagNetmask, flagGateway string

	cmd := &cobra.Command{
		Use:   "configure",
		Short: "Configure local BMC network (static IP)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if flagIP == "" {
				return fmt.Errorf("--ip is required")
			}
			if flagNetmask == "" {
				return fmt.Errorf("--netmask is required")
			}
			if flagGateway == "" {
				return fmt.Errorf("--gateway is required")
			}

			ctx := context.Background()
			c := ipmiClientFromFlags("", "", "")

			cfg := ipmi.BMCConfig{
				Channel:   1,
				IPAddress: flagIP,
				Netmask:   flagNetmask,
				Gateway:   flagGateway,
				IPSource:  "static",
			}
			if err := c.SetBMCNetwork(ctx, cfg); err != nil {
				return fmt.Errorf("configure bmc: %w", err)
			}
			fmt.Printf("BMC network configured:\n")
			fmt.Printf("  IP      : %s\n", flagIP)
			fmt.Printf("  Netmask : %s\n", flagNetmask)
			fmt.Printf("  Gateway : %s\n", flagGateway)
			return nil
		},
	}

	cmd.Flags().StringVar(&flagIP, "ip", "", "Static IP address for the BMC (required)")
	cmd.Flags().StringVar(&flagNetmask, "netmask", "", "Subnet mask (required)")
	cmd.Flags().StringVar(&flagGateway, "gateway", "", "Default gateway (required)")
	return cmd
}

// newIPMIPXECmd sets next boot to PXE and power cycles the target node.
func newIPMIPXECmd() *cobra.Command {
	var flagHost, flagUser, flagPass string

	cmd := &cobra.Command{
		Use:   "pxe",
		Short: "Set next boot to PXE and power cycle the node",
		RunE: func(cmd *cobra.Command, args []string) error {
			if flagHost == "" {
				return fmt.Errorf("--host is required")
			}

			ctx := context.Background()
			c := ipmiClientFromFlags(flagHost, flagUser, flagPass)

			fmt.Fprintf(os.Stderr, "Setting next boot to PXE on %s...\n", flagHost)
			if err := c.SetBootPXE(ctx); err != nil {
				return fmt.Errorf("set boot pxe: %w", err)
			}

			fmt.Fprintf(os.Stderr, "Power cycling...\n")
			if err := c.PowerCycle(ctx); err != nil {
				return fmt.Errorf("power cycle: %w", err)
			}

			fmt.Printf("Node %s will boot via PXE.\n", flagHost)
			return nil
		},
	}

	cmd.Flags().StringVar(&flagHost, "host", "", "BMC IP address (required)")
	cmd.Flags().StringVar(&flagUser, "user", "", "BMC username")
	cmd.Flags().StringVar(&flagPass, "pass", "", "BMC password")
	return cmd
}

// newIPMISensorsCmd displays sensor readings from a remote BMC.
func newIPMISensorsCmd() *cobra.Command {
	var flagHost, flagUser, flagPass string

	cmd := &cobra.Command{
		Use:   "sensors",
		Short: "Show IPMI sensor readings",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			c := ipmiClientFromFlags(flagHost, flagUser, flagPass)

			sensors, err := c.GetSensorData(ctx)
			if err != nil {
				return fmt.Errorf("get sensors: %w", err)
			}

			if len(sensors) == 0 {
				fmt.Println("No sensor data available.")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "SENSOR\tVALUE\tUNITS\tSTATUS")
			for _, s := range sensors {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.Name, s.Value, s.Units, s.Status)
			}
			return w.Flush()
		},
	}

	cmd.Flags().StringVar(&flagHost, "host", "", "BMC IP address (local BMC if omitted)")
	cmd.Flags().StringVar(&flagUser, "user", "", "BMC username")
	cmd.Flags().StringVar(&flagPass, "pass", "", "BMC password")
	return cmd
}

// ─── logs ────────────────────────────────────────────────────────────────────

// newLogsCmd creates the "clonr logs" command and its subcommands.
func newLogsCmd() *cobra.Command {
	var (
		flagMAC       string
		flagHostname  string
		flagLevel     string
		flagComponent string
		flagSince     string
		flagLimit     int
		flagFollow    bool
	)

	cmd := &cobra.Command{
		Use:   "logs",
		Short: "View deployment logs from the server",
		Long: `logs queries or tails the centralized deployment log stream.

Examples:
  clonr logs --mac aa:bb:cc:dd:ee:ff        # history for a specific node
  clonr logs --follow                        # live tail all nodes
  clonr logs --follow --mac aa:bb:cc:dd:ee:ff --level error
  clonr logs --component deploy --since 1h  # last hour of deploy phase logs`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			c := clientFromFlags()

			filter := api.LogFilter{
				NodeMAC:   flagMAC,
				Hostname:  flagHostname,
				Level:     flagLevel,
				Component: flagComponent,
				Limit:     flagLimit,
			}

			// Parse --since as a duration ("1h", "30m") or RFC3339 timestamp.
			if flagSince != "" {
				if d, err := time.ParseDuration(flagSince); err == nil {
					t := time.Now().UTC().Add(-d)
					filter.Since = &t
				} else if t, err := time.Parse(time.RFC3339, flagSince); err == nil {
					filter.Since = &t
				} else {
					return fmt.Errorf("--since: expected a duration (e.g. 1h, 30m) or RFC3339 timestamp")
				}
			}

			if flagFollow {
				return tailLogs(ctx, c, filter)
			}
			return queryLogs(ctx, c, filter)
		},
	}

	cmd.Flags().StringVar(&flagMAC, "mac", "", "Filter by node MAC address")
	cmd.Flags().StringVar(&flagHostname, "hostname", "", "Filter by hostname")
	cmd.Flags().StringVar(&flagLevel, "level", "", "Filter by log level (debug, info, warn, error)")
	cmd.Flags().StringVar(&flagComponent, "component", "", "Filter by component (hardware, deploy, chroot, ipmi, efiboot)")
	cmd.Flags().StringVar(&flagSince, "since", "", "Show logs since a duration ago (e.g. 1h, 30m) or RFC3339 timestamp")
	cmd.Flags().IntVar(&flagLimit, "limit", 100, "Max number of log entries to return")
	cmd.Flags().BoolVar(&flagFollow, "follow", false, "Tail the live log stream (SSE)")

	return cmd
}

// queryLogs fetches and prints historical logs.
func queryLogs(ctx context.Context, c *client.Client, filter api.LogFilter) error {
	entries, err := c.QueryLogs(ctx, filter)
	if err != nil {
		return fmt.Errorf("query logs: %w", err)
	}
	if len(entries) == 0 {
		fmt.Println("No log entries found.")
		return nil
	}
	// Entries come back newest-first; reverse for chronological output.
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	for _, e := range entries {
		printLogEntry(e)
	}
	return nil
}

// tailLogs opens an SSE stream and prints entries as they arrive.
func tailLogs(ctx context.Context, c *client.Client, filter api.LogFilter) error {
	ch, cancel, err := c.StreamLogs(ctx, filter)
	if err != nil {
		return fmt.Errorf("stream logs: %w", err)
	}
	defer cancel()

	fmt.Fprintln(os.Stderr, "Streaming live logs (Ctrl-C to stop)...")
	for entry := range ch {
		printLogEntry(entry)
	}
	return nil
}

// printLogEntry writes a formatted log line to stdout.
func printLogEntry(e api.LogEntry) {
	ts := e.Timestamp.Local().Format("2006-01-02 15:04:05")

	levelStr := levelColored(e.Level)
	node := e.Hostname
	if node == "" {
		node = e.NodeMAC
	}

	fmt.Printf("%s  %s  [%s] %s%s%s  %s\n",
		colorGray+ts+colorReset,
		levelStr,
		e.Component,
		colorGray+node+colorReset,
		sep(node),
		colorReset,
		e.Message,
	)
}

func sep(s string) string {
	if s == "" {
		return ""
	}
	return "  "
}

func levelColored(level string) string {
	switch strings.ToLower(level) {
	case "error":
		return colorRed + "ERR" + colorReset
	case "warn":
		return colorYellow + "WRN" + colorReset
	case "debug":
		return colorGray + "DBG" + colorReset
	default:
		return colorCyan + "INF" + colorReset
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// shortID returns the first 8 characters of a UUID for compact display.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// humanBytes formats a byte count as a human-readable string.
func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// primaryMACFromHW returns the MAC address of the first non-loopback physical NIC.
func primaryMACFromHW(hw *hardware.SystemInfo) string {
	for _, nic := range hw.NICs {
		if nic.Name == "lo" || nic.MAC == "" || nic.MAC == "00:00:00:00:00:00" {
			continue
		}
		return nic.MAC
	}
	return ""
}

// ─── image import-iso ────────────────────────────────────────────────────────

// newImageImportISOCmd creates "clonr image import-iso <path>".
// It passes the absolute ISO path to the server via POST /api/v1/factory/import-path.
// This requires the CLI and server share a filesystem (same host or NFS mount).
func newImageImportISOCmd() *cobra.Command {
	var (
		flagName    string
		flagVersion string
	)

	cmd := &cobra.Command{
		Use:   "import-iso <path>",
		Short: "Import an ISO image into the server's image store",
		Long: `import-iso passes a server-local ISO path to clonr-serverd, which mounts
the ISO, extracts the root filesystem, and creates a new BaseImage.

The ISO file must be accessible from the server process (same host or shared
mount). The command returns immediately; poll with "clonr image details <id>".`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			isoPath := args[0]
			if flagName == "" {
				base := filepath.Base(isoPath)
				flagName = strings.TrimSuffix(base, filepath.Ext(base))
			}

			absPath, err := filepath.Abs(isoPath)
			if err != nil {
				return fmt.Errorf("resolve path: %w", err)
			}

			ctx := context.Background()
			c := clientFromFlags()

			fmt.Fprintf(os.Stderr, "Importing ISO %s as %q...\n", absPath, flagName)
			img, err := c.ImportISOPath(ctx, absPath, flagName, flagVersion)
			if err != nil {
				return fmt.Errorf("import iso: %w", err)
			}

			fmt.Printf("ISO import initiated:\n")
			fmt.Printf("  ID:     %s\n", img.ID)
			fmt.Printf("  Name:   %s\n", img.Name)
			fmt.Printf("  Status: %s\n", img.Status)
			fmt.Printf("\nPoll status with: clonr image details %s\n", img.ID)
			return nil
		},
	}

	cmd.Flags().StringVar(&flagName, "name", "", "Image name (default: ISO filename without extension)")
	cmd.Flags().StringVar(&flagVersion, "version", "1.0.0", "Image version")
	return cmd
}

// ─── shell ───────────────────────────────────────────────────────────────────

// newShellCmd creates "clonr shell <image-id>".
//
// Flow (local path — CLI on same host as server):
//  1. Verify image is ready/building.
//  2. Open a server-side session (triggers vfs mounts on the server).
//  3. Create a local chroot.Session against the returned rootfs path.
//  4. Drop into an interactive shell (stdin/stdout/stderr attached).
//  5. Close the server-side session on exit (unmounts vfs).
func newShellCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "shell <image-id>",
		Short: "Open an interactive chroot shell inside an image",
		Long: `shell drops you into an interactive bash shell inside the specified image's
root filesystem. The image must have status "ready" or "building".

The chroot mounts /proc, /sys, /dev, /dev/pts, and /run before dropping you
into the shell. All mounts are cleaned up on exit.

NOTE: Requires root privileges and that the CLI runs on the same host as
clonr-serverd (rootfs is accessed directly via local filesystem path).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			imageID := args[0]
			ctx := context.Background()
			c := clientFromFlags()

			img, err := c.GetImage(ctx, imageID)
			if err != nil {
				return fmt.Errorf("get image: %w", err)
			}
			if img.Status != api.ImageStatusReady && img.Status != api.ImageStatusBuilding {
				return fmt.Errorf("image %s has status %q — must be ready or building", img.ID, img.Status)
			}
			fmt.Fprintf(os.Stderr, "Opening shell in image: %s %s (%s)\n", img.Name, img.Version, img.ID)

			// Open a server-side session to trigger vfs mounts.
			sess, err := c.OpenShellSession(ctx, imageID)
			if err != nil {
				return fmt.Errorf("open shell session: %w", err)
			}
			defer func() {
				if closeErr := c.CloseShellSession(context.Background(), imageID, sess.SessionID); closeErr != nil {
					fmt.Fprintf(os.Stderr, "warning: close session: %v\n", closeErr)
				}
			}()

			// Create a local chroot.Session using the server's rootfs path.
			// Skip Enter() — the server-side session owns the mounts.
			localSess, err := chroot.NewSession(sess.RootDir)
			if err != nil {
				return fmt.Errorf("create local chroot: %w", err)
			}
			defer func() { _ = localSess.Close() }()

			fmt.Fprintf(os.Stderr, "Entering chroot at %s\n", sess.RootDir)
			fmt.Fprintf(os.Stderr, "Type 'exit' to leave the chroot.\n")

			if err := localSess.Shell(); err != nil {
				fmt.Fprintf(os.Stderr, "shell exited: %v\n", err)
			}
			return nil
		},
	}
	return cmd
}

// ─── image capture ───────────────────────────────────────────────────────────

// newImageCaptureCmd creates "clonr image capture".
// It instructs clonr-serverd to SSH into the target host and rsync its
// filesystem into a new BaseImage. The server must have network access
// to the source host, and either a private key or password must be provided.
// SSH host key verification is disabled (StrictHostKeyChecking=no) — only
// use this on trusted golden nodes on a management network.
func newImageCaptureCmd() *cobra.Command {
	var (
		flagFrom        string
		flagSSHKey      string
		flagSSHPassword string
		flagSSHPort     int
		flagName        string
		flagVersion     string
		flagOS          string
		flagArch        string
		flagExclude     []string
		flagNotes       string
	)

	cmd := &cobra.Command{
		Use:   "capture",
		Short: "Capture a live server as a base image via SSH rsync",
		Long: `capture instructs clonr-serverd to SSH into the source host and stream
its filesystem into a new BaseImage via rsync. The server must be able to reach
the source host over the network.

SSH host key verification is disabled by default (StrictHostKeyChecking=no).
Only use this against trusted golden nodes on a management network.

If --ssh-key is omitted, the server's default SSH key (~/.ssh/id_rsa or the
key configured by the server's user environment) is used. Provide --ssh-password
only when key-based auth is unavailable; sshpass must be installed on the server.

The capture runs asynchronously. Poll with: clonr image details <id>

Examples:
  clonr image capture --from root@192.168.1.10 --name rocky9-golden --version 1.0.0
  clonr image capture --from 10.0.0.5 --ssh-key /etc/clonr/keys/golden --name hpc-compute`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if flagFrom == "" {
				return fmt.Errorf("--from is required (e.g. root@192.168.1.10)")
			}
			if flagName == "" {
				return fmt.Errorf("--name is required")
			}

			ctx := context.Background()
			c := clientFromFlags()

			req := api.CaptureRequest{
				SourceHost:   flagFrom,
				SSHKeyPath:   flagSSHKey,
				SSHPassword:  flagSSHPassword,
				SSHPort:      flagSSHPort,
				Name:         flagName,
				Version:      flagVersion,
				OS:           flagOS,
				Arch:         flagArch,
				ExcludePaths: flagExclude,
				Notes:        flagNotes,
				Tags:         []string{},
			}

			fmt.Fprintf(os.Stderr, "Requesting capture of %s from %s...\n", flagName, flagFrom)
			img, err := c.CaptureImage(ctx, req)
			if err != nil {
				return fmt.Errorf("capture: %w", err)
			}

			fmt.Printf("Capture initiated:\n")
			fmt.Printf("  ID:     %s\n", img.ID)
			fmt.Printf("  Name:   %s\n", img.Name)
			fmt.Printf("  Status: %s\n", img.Status)
			fmt.Printf("\nThe server is now rsyncing from %s — this may take several minutes.\n", flagFrom)
			fmt.Printf("Poll status with: clonr image details %s\n", img.ID)
			return nil
		},
	}

	cmd.Flags().StringVar(&flagFrom, "from", "", "Source host in user@host or host form (required)")
	cmd.Flags().StringVar(&flagSSHKey, "ssh-key", "", "Server-local path to SSH private key (uses server default key if omitted)")
	cmd.Flags().StringVar(&flagSSHPassword, "ssh-password", "", "SSH password (requires sshpass on server host; prefer key auth)")
	cmd.Flags().IntVar(&flagSSHPort, "ssh-port", 22, "SSH port on the source host")
	cmd.Flags().StringVar(&flagName, "name", "", "Image name (required)")
	cmd.Flags().StringVar(&flagVersion, "version", "1.0.0", "Image version")
	cmd.Flags().StringVar(&flagOS, "os", "", "OS name, e.g. 'Rocky Linux 9'")
	cmd.Flags().StringVar(&flagArch, "arch", "x86_64", "Target architecture")
	cmd.Flags().StringSliceVar(&flagExclude, "exclude", nil, "Additional rsync --exclude paths (repeatable)")
	cmd.Flags().StringVar(&flagNotes, "notes", "", "Free-text notes")

	return cmd
}
