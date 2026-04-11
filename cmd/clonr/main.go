package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/client"
	"github.com/sqoia-dev/clonr/pkg/config"
	"github.com/sqoia-dev/clonr/pkg/deploy"
	"github.com/sqoia-dev/clonr/pkg/hardware"
	"github.com/sqoia-dev/clonr/pkg/ipmi"
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
		flagImage     string
		flagDisk      string
		flagMountRoot string
		flagFixEFI    bool
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
  7. Fix EFI boot entries (if --fix-efi is set)`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if flagImage == "" {
				return fmt.Errorf("--image is required")
			}

			ctx := context.Background()
			c := clientFromFlags()

			// Step 1: Discover hardware.
			fmt.Fprintln(os.Stderr, "[1/6] Discovering hardware...")
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

			nodeCfg, err := c.GetNodeConfigByMAC(ctx, primaryMAC)
			if err != nil {
				return fmt.Errorf("get node config (MAC %s): %w", primaryMAC, err)
			}
			fmt.Fprintf(os.Stderr, "    Node: %s (%s)\n", nodeCfg.Hostname, nodeCfg.ID)

			// Step 3: Get image details.
			fmt.Fprintln(os.Stderr, "[3/6] Fetching image details...")
			img, err := c.GetImage(ctx, flagImage)
			if err != nil {
				return fmt.Errorf("get image %s: %w", flagImage, err)
			}
			if img.Status != api.ImageStatusReady {
				return fmt.Errorf("image %s is not ready (status: %s)", img.ID, img.Status)
			}
			fmt.Fprintf(os.Stderr, "    Image: %s %s (%s)\n", img.Name, img.Version, img.Format)

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
			var deployer deploy.Deployer
			switch img.Format {
			case api.ImageFormatBlock:
				deployer = &deploy.BlockDeployer{}
			default:
				deployer = &deploy.FilesystemDeployer{}
			}

			if err := deployer.Preflight(ctx, img.DiskLayout, *hw); err != nil {
				return fmt.Errorf("preflight: %w", err)
			}

			// Step 5: Deploy.
			fmt.Fprintln(os.Stderr, "[5/6] Deploying image...")
			opts := deploy.DeployOpts{
				ImageURL:   blobURL,
				AuthToken:  cfg.AuthToken,
				TargetDisk: flagDisk,
				Format:     string(img.Format),
				MountRoot:  mountRoot,
			}

			start := time.Now()
			progressFn := func(written, total int64, phase string) {
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
				return fmt.Errorf("deploy: %w", err)
			}
			fmt.Fprintf(os.Stderr, "\n    Done in %s\n", time.Since(start).Round(time.Second))

			// Step 6: Finalize.
			fmt.Fprintln(os.Stderr, "[6/6] Applying node configuration...")
			if err := deployer.Finalize(ctx, *nodeCfg, mountRoot); err != nil {
				return fmt.Errorf("finalize: %w", err)
			}
			fmt.Fprintln(os.Stderr, "    Hostname, network, and SSH keys applied.")

			// Step 7: EFI boot repair (optional).
			if flagFixEFI {
				fmt.Fprintln(os.Stderr, "[+] Repairing EFI boot entries...")
				disk := flagDisk
				if disk == "" {
					disk = "/dev/sda"
				}
				label := img.Name
				if nodeCfg.Hostname != "" {
					label = img.Name
				}
				if err := deploy.FixEFIBoot(ctx, disk, 1, label, `\EFI\rocky\grubx64.efi`); err != nil {
					// Non-fatal — log the error but don't fail the deployment.
					fmt.Fprintf(os.Stderr, "    Warning: EFI boot repair failed: %v\n", err)
				} else {
					fmt.Fprintln(os.Stderr, "    EFI boot entry set.")
				}
			}

			fmt.Printf("\nDeployment complete.\n")
			fmt.Printf("  Node:     %s\n", nodeCfg.Hostname)
			fmt.Printf("  Image:    %s %s\n", img.Name, img.Version)
			fmt.Printf("  Duration: %s\n", time.Since(start).Round(time.Second))
			return nil
		},
	}

	cmd.Flags().StringVar(&flagImage, "image", "", "Image ID to deploy (required)")
	cmd.Flags().StringVar(&flagDisk, "disk", "", "Target block device, e.g. /dev/nvme0n1 (auto-detected if omitted)")
	cmd.Flags().StringVar(&flagMountRoot, "mount-root", "", "Temporary mount point directory (auto-created if omitted)")
	cmd.Flags().BoolVar(&flagFixEFI, "fix-efi", false, "Repair EFI boot entries after deployment")

	return cmd
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
