package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/sqoia-dev/clonr/pkg/hardware"
)

var version = "dev"

func main() {
	// Human-readable console output for the CLI.
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
	// Image creation
	rootCmd.AddCommand(newStubCmd("new-image", "Capture a new image from a running node"))
	rootCmd.AddCommand(newStubCmd("pull-image", "Pull an image from a remote clonr server"))
	rootCmd.AddCommand(newStubCmd("import-iso", "Import an ISO as a clonr base image"))

	// Image customization
	rootCmd.AddCommand(newStubCmd("shell", "Enter an interactive chroot shell in an image"))
	rootCmd.AddCommand(newStubCmd("update-filesystem", "Apply filesystem changes to an image"))
	rootCmd.AddCommand(newStubCmd("update-disklayout", "Update the disk layout definition for an image"))

	// Image metadata
	rootCmd.AddCommand(newStubCmd("image-list", "List all stored images"))
	rootCmd.AddCommand(newStubCmd("image-details", "Show detailed metadata for an image"))
	rootCmd.AddCommand(newStubCmd("config", "Show or edit clonr configuration"))
	rootCmd.AddCommand(newStubCmd("disklayout-upload", "Upload a disk layout file to the server"))

	// HPC-specific
	rootCmd.AddCommand(identifyCmd)
	rootCmd.AddCommand(newStubCmd("biossettings", "Read or apply BIOS/UEFI configuration"))
	rootCmd.AddCommand(newStubCmd("script", "Run a custom script inside an image chroot"))

	// Deployment
	rootCmd.AddCommand(newStubCmd("multicast-image", "Deploy an image to multiple nodes via multicast"))
	rootCmd.AddCommand(newStubCmd("fix-efiboot", "Repair EFI boot entries on a deployed node"))
}

// identifyCmd runs hardware discovery and prints the result as JSON.
var identifyCmd = &cobra.Command{
	Use:   "identify",
	Short: "Discover and print this node's hardware profile as JSON",
	Long: `identify runs full hardware discovery (CPU, memory, disks, NICs, DMI)
and prints the result as formatted JSON to stdout. Useful for verifying
hardware detection and generating inventory records.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		info, err := hardware.Discover()
		if err != nil {
			return fmt.Errorf("hardware discovery: %w", err)
		}

		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(info); err != nil {
			return fmt.Errorf("encode: %w", err)
		}
		return nil
	},
}

// newStubCmd creates a placeholder command that prints a "not implemented yet"
// message. Each stub will be replaced with a real implementation as the
// corresponding package is built out.
func newStubCmd(use, short string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintf(os.Stderr, "%s: not implemented yet\n", cmd.Use)
			os.Exit(1)
		},
	}
}
