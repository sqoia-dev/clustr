// Package ipmi provides IPMI/BMC management operations via ipmitool.
// Local operations (no host flag) run on-node against the local BMC.
// Remote operations require a Client configured with Host, Username, and Password.
package ipmi

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// PowerStatus represents the current power state of a node.
type PowerStatus string

const (
	PowerOn  PowerStatus = "on"
	PowerOff PowerStatus = "off"
)

// BMCConfig holds the network configuration for a BMC/IPMI channel.
type BMCConfig struct {
	Channel   int    `json:"channel"`    // LAN channel number, usually 1
	IPAddress string `json:"ip_address"`
	Netmask   string `json:"netmask"`
	Gateway   string `json:"gateway"`
	IPSource  string `json:"ip_source"` // "static" or "dhcp"
}

// BMCUser represents a user account on the BMC.
type BMCUser struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
	Access   string `json:"access"` // "ADMINISTRATOR", "OPERATOR", "USER"
}

// Sensor holds a single IPMI sensor reading.
type Sensor struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Units  string `json:"units"`
	Status string `json:"status"` // "ok", "warning", "critical", "ns" (not present)
}

// BootDevice identifies the boot target for chassis bootdev / raw bootparam.
type BootDevice uint8

const (
	// BootDevDisk forces boot from the default hard drive.
	BootDevDisk BootDevice = 0x08
	// BootDevPXE forces PXE network boot.
	BootDevPXE BootDevice = 0x04
	// BootDevBIOS forces entry into BIOS/UEFI setup utility.
	BootDevBIOS BootDevice = 0x18
	// BootDevCD forces boot from CD/DVD.
	BootDevCD BootDevice = 0x14
)

// BootOpts configures how a boot-device override is applied.
type BootOpts struct {
	// Persistent causes the override to survive across all future power cycles
	// until explicitly cleared. Without this many BMCs consume the override on
	// the very first boot — if the node reboots mid-deploy the override is gone
	// and the node spins in a PXE boot loop.
	//
	// Default: true for deploy operations, false for one-time diagnostic boots.
	Persistent bool

	// EFI requests UEFI boot mode. Set this for nodes that boot via the UEFI
	// firmware. On BIOS/legacy systems leave it false.
	EFI bool

	// UseRaw forces the raw `ipmitool raw 0x00 0x08 …` path instead of the
	// friendly `ipmitool chassis bootdev` command. Set automatically for BMC
	// vendors known to ignore the friendly command (e.g. old Dell iDRAC5/6,
	// Supermicro X9). Can also be forced globally via $CLONR_IPMI_USE_RAW=true.
	UseRaw bool
}

// BMCVendor identifies the hardware vendor of the BMC/management controller.
type BMCVendor string

const (
	VendorDell       BMCVendor = "dell"        // iDRAC
	VendorHPE        BMCVendor = "hpe"         // iLO
	VendorSupermicro BMCVendor = "supermicro"  // IPMI / SuperDoctor
	VendorLenovo     BMCVendor = "lenovo"      // XCC / IMM2
	VendorGeneric    BMCVendor = "generic"
)

// BootParamResult carries the parsed output of `ipmitool chassis bootparam get 5`.
type BootParamResult struct {
	// Raw is the full ipmitool output, useful for debugging.
	Raw string
	// DataBytes is the 5-byte boot flags parameter (parameter data field).
	// Index 0 corresponds to the first data byte in the ipmitool output.
	DataBytes [5]byte
	// Valid is true when bit 7 of byte 0 is set (boot flag valid).
	Valid bool
	// Persistent is true when bit 6 of byte 0 is set.
	Persistent bool
	// EFI is true when bit 5 of byte 0 is set.
	EFI bool
	// Device is the raw boot device selector nibble from byte 1.
	Device BootDevice
}

// Client is an ipmitool wrapper. Set Host/Username/Password for remote access;
// leave Host empty to operate on the local BMC (no -H flag).
type Client struct {
	Host     string // BMC IP address; empty means local
	Username string
	Password string
}

// args builds the ipmitool argument list, prepending remote flags when c.Host is set.
// The BMC password is passed via the IPMITOOL_PASSWORD environment variable and
// the -E flag rather than -P, so it never appears in the process argument list
// (which is visible to other processes via /proc/<pid>/cmdline on Linux).
func (c *Client) args(sub ...string) []string {
	if c.Host == "" {
		return sub
	}
	// -E tells ipmitool to read the password from $IPMITOOL_PASSWORD.
	remote := []string{"-I", "lanplus", "-H", c.Host, "-U", c.Username, "-E"}
	return append(remote, sub...)
}

// run executes ipmitool with the given subcommand arguments and returns stdout.
// When c.Password is set, it is injected into the child process environment as
// IPMITOOL_PASSWORD; the variable is not inherited from the parent process env
// in a way that exposes it — it is appended to the environment slice only for
// this invocation.
func (c *Client) run(ctx context.Context, sub ...string) (string, error) {
	args := c.args(sub...)
	cmd := exec.CommandContext(ctx, "ipmitool", args...)
	if c.Host != "" && c.Password != "" {
		// Inherit the parent environment (PATH, HOME, etc.) so ipmitool can
		// locate its binaries, then override the password variable.
		cmd.Env = append(os.Environ(), "IPMITOOL_PASSWORD="+c.Password)
	}
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		return "", fmt.Errorf("ipmi: ipmitool %s: %w%s", strings.Join(sub, " "), err,
			func() string {
				if stderr != "" {
					return ": " + stderr
				}
				return ""
			}())
	}
	return strings.TrimSpace(string(out)), nil
}

// ─── Local operations ────────────────────────────────────────────────────────

// GetBMCConfig reads the current LAN configuration from the local BMC.
// Uses channel 1 by default (override by setting c.Host and calling via remote).
func (c *Client) GetBMCConfig(ctx context.Context) (*BMCConfig, error) {
	out, err := c.run(ctx, "lan", "print", "1")
	if err != nil {
		return nil, err
	}
	return parseBMCConfig(out, 1), nil
}

// SetBMCNetwork configures static IP, netmask, and gateway on BMC LAN channel 1.
func (c *Client) SetBMCNetwork(ctx context.Context, cfg BMCConfig) error {
	ch := strconv.Itoa(cfg.Channel)
	if ch == "0" {
		ch = "1"
	}

	steps := [][]string{
		{"lan", "set", ch, "ipsrc", "static"},
		{"lan", "set", ch, "ipaddr", cfg.IPAddress},
		{"lan", "set", ch, "netmask", cfg.Netmask},
		{"lan", "set", ch, "defgw", "ipaddr", cfg.Gateway},
	}
	for _, s := range steps {
		if _, err := c.run(ctx, s...); err != nil {
			return err
		}
	}
	return nil
}

// SetBMCUser creates or updates a BMC user account. The user is granted
// ADMINISTRATOR access and enabled on channel 1.
func (c *Client) SetBMCUser(ctx context.Context, userID int, username, password string) error {
	id := strconv.Itoa(userID)
	steps := [][]string{
		{"user", "set", "name", id, username},
		{"user", "set", "password", id, password},
		{"channel", "setaccess", "1", id, "link=on", "ipmi=on", "callin=on", "privilege=4"},
		{"user", "enable", id},
	}
	for _, s := range steps {
		if _, err := c.run(ctx, s...); err != nil {
			return err
		}
	}
	return nil
}

// GetBMCUsers returns the list of configured BMC user accounts.
func (c *Client) GetBMCUsers(ctx context.Context) ([]BMCUser, error) {
	out, err := c.run(ctx, "user", "list", "1")
	if err != nil {
		return nil, err
	}
	return parseBMCUsers(out), nil
}

// ─── Remote power / boot operations ─────────────────────────────────────────

// PowerStatus returns the current power state of the managed node.
func (c *Client) PowerStatus(ctx context.Context) (PowerStatus, error) {
	out, err := c.run(ctx, "power", "status")
	if err != nil {
		return "", err
	}
	lower := strings.ToLower(out)
	if strings.Contains(lower, " on") {
		return PowerOn, nil
	}
	return PowerOff, nil
}

// PowerOn turns on the managed node.
func (c *Client) PowerOn(ctx context.Context) error {
	_, err := c.run(ctx, "power", "on")
	return err
}

// PowerOff performs a graceful shutdown (chassis power off).
func (c *Client) PowerOff(ctx context.Context) error {
	_, err := c.run(ctx, "power", "off")
	return err
}

// PowerCycle performs a power cycle (off then on).
func (c *Client) PowerCycle(ctx context.Context) error {
	_, err := c.run(ctx, "power", "cycle")
	return err
}

// PowerReset issues a hard reset (does not cycle power).
func (c *Client) PowerReset(ctx context.Context) error {
	_, err := c.run(ctx, "power", "reset")
	return err
}

// SetBootPXE sets the next one-time boot device to PXE network boot.
// Deprecated: Use SetBootDevWithOpts for production deploys — one-time overrides
// are consumed on the first boot and lost if the node reboots mid-deploy.
func (c *Client) SetBootPXE(ctx context.Context) error {
	_, err := c.run(ctx, "chassis", "bootdev", "pxe", "options=efiboot")
	return err
}

// SetBootDisk sets the next one-time boot device to the local disk.
// Deprecated: Use SetBootDevWithOpts for production deploys.
func (c *Client) SetBootDisk(ctx context.Context) error {
	_, err := c.run(ctx, "chassis", "bootdev", "disk", "options=efiboot")
	return err
}

// SetBootDevWithOpts sets the boot device override with full control over
// persistence, EFI mode, and raw vs. friendly command path.
//
// Strategy:
//  1. If opts.UseRaw or the $CLONR_IPMI_USE_RAW env var is set, go directly to
//     the raw chassis bootparam command (IPMI spec table 28-14).
//  2. Otherwise attempt the friendly `ipmitool chassis bootdev` command first.
//  3. If that fails (non-zero exit), fall back to the raw command automatically.
//
// This ensures the override sticks on BMCs that silently ignore the friendly
// path (old Dell iDRAC5/6, Supermicro X9/X10 in certain firmware revisions).
func (c *Client) SetBootDevWithOpts(ctx context.Context, dev BootDevice, opts BootOpts) error {
	// Env override: CLONR_IPMI_USE_RAW=true forces raw path.
	if os.Getenv("CLONR_IPMI_USE_RAW") == "true" {
		opts.UseRaw = true
	}
	// Env override: CLONR_IPMI_EFI=true forces UEFI mode when auto-detect fails.
	if os.Getenv("CLONR_IPMI_EFI") == "true" {
		opts.EFI = true
	}

	if opts.UseRaw {
		return c.setBootDevRaw(ctx, dev, opts)
	}

	// Try the friendly command first.
	if err := c.setBootDevFriendly(ctx, dev, opts); err != nil {
		// Fall back to raw on any error. The friendly command on some BMCs
		// exits 0 but silently ignores the request; we can't detect that here
		// without reading back the setting, which VerifyBootParam does separately.
		return c.setBootDevRaw(ctx, dev, opts)
	}
	return nil
}

// setBootDevFriendly executes `ipmitool chassis bootdev <dev> options=<flags>`.
// The options string encodes persistence and EFI mode per the ipmitool spec.
func (c *Client) setBootDevFriendly(ctx context.Context, dev BootDevice, opts BootOpts) error {
	devStr, err := bootDevToName(dev)
	if err != nil {
		return err
	}

	// Build the options string. ipmitool accepts a comma-separated list:
	//   "persistent"  → override persists across all future boots
	//   "efiboot"     → request UEFI boot mode
	// Absence of "persistent" means one-time only.
	var optParts []string
	if opts.Persistent {
		optParts = append(optParts, "persistent")
	}
	if opts.EFI {
		optParts = append(optParts, "efiboot")
	}

	args := []string{"chassis", "bootdev", devStr}
	if len(optParts) > 0 {
		args = append(args, "options="+strings.Join(optParts, ","))
	}
	_, err = c.run(ctx, args...)
	return err
}

// setBootDevRaw issues the raw IPMI chassis boot flag command (IPMI spec §28.12,
// command 0x08 in network function 0x00). This reaches BMCs that ignore the
// higher-level `chassis bootdev` abstraction.
//
// Byte layout of the 6-byte raw payload (after network function and command byte):
//
//	byte 0: parameter selector = 0x05 (boot flags)
//	byte 1: flags byte
//	         bit 7: valid (always 1)
//	         bit 6: persistent (1 = all future boots, 0 = next boot only)
//	         bit 5: EFI (1 = UEFI, 0 = legacy/BIOS)
//	         bits 4-0: reserved
//	byte 2: boot device selector (device byte)
//	byte 3: reserved = 0x00
//	byte 4: reserved = 0x00
//	byte 5: reserved = 0x00
func (c *Client) setBootDevRaw(ctx context.Context, dev BootDevice, opts BootOpts) error {
	flags, devByte := buildRawBootBytes(dev, opts)
	_, err := c.run(ctx,
		"raw", "0x00", "0x08",
		"0x05",
		fmt.Sprintf("0x%02X", flags),
		fmt.Sprintf("0x%02X", devByte),
		"0x00", "0x00", "0x00",
	)
	return err
}

// buildRawBootBytes computes the flags byte and device byte for the raw chassis
// boot parameter set command. Exported as a pure function so unit tests can
// exercise the bit-packing logic without an ipmitool binary.
//
// Bit layout of flags byte:
//
//	bit 7 (0x80): valid — must always be 1 for the BMC to honour the setting
//	bit 6 (0x40): persistent — survive all future power cycles
//	bit 5 (0x20): EFI mode — request UEFI firmware path
func buildRawBootBytes(dev BootDevice, opts BootOpts) (flags byte, devByte byte) {
	flags = 0x80 // valid bit always set
	if opts.Persistent {
		flags |= 0x40
	}
	if opts.EFI {
		flags |= 0x20
	}
	// The device selector occupies the upper nibble of byte 1, shifted left by 2
	// per IPMI spec table 28-14. The raw value we carry in BootDevice is already
	// the shifted value (0x04, 0x08, etc.) matching the spec directly.
	devByte = byte(dev)
	return flags, devByte
}

// VerifyBootParam reads back the chassis boot parameter 5 and parses the result.
// Call this after SetBootDevWithOpts to confirm the BMC accepted the setting.
//
// Lenovo XCC/IMM2 note: the read-back often does not reflect the just-applied
// setting even when the BMC correctly honours it at boot time. Log a warning
// rather than returning an error when the setting doesn't match — use the
// caller's discretion on whether to retry.
func (c *Client) VerifyBootParam(ctx context.Context) (*BootParamResult, error) {
	out, err := c.run(ctx, "chassis", "bootparam", "get", "5")
	if err != nil {
		return nil, fmt.Errorf("bootparam get 5: %w", err)
	}
	return parseBootParam(out), nil
}

// ─── BMC vendor detection ─────────────────────────────────────────────────────

// DetectVendor queries the BMC management controller info (`ipmitool mc info`)
// and returns the vendor constant that matches the Manufacturer Name field.
//
// The IPMI Manufacturer ID (numeric) is the authoritative field. The
// Manufacturer Name string is a convenience fallback for BMCs that don't
// report a numeric ID.
func (c *Client) DetectVendor(ctx context.Context) (BMCVendor, error) {
	out, err := c.run(ctx, "mc", "info")
	if err != nil {
		return VendorGeneric, fmt.Errorf("mc info: %w", err)
	}
	return parseVendor(out), nil
}

// parseVendor extracts the BMC vendor from `ipmitool mc info` output.
// It checks the numeric Manufacturer ID first (authoritative), then falls
// back to the Manufacturer Name string (case-insensitive substring match).
func parseVendor(mcInfo string) BMCVendor {
	var mfrID int
	var mfrName string

	for _, line := range strings.Split(mcInfo, "\n") {
		k, v, ok := cutColon(line)
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)

		switch {
		case strings.EqualFold(k, "Manufacturer ID"):
			// Value may be decimal or "0x<hex>"; strip trailing descriptions.
			fields := strings.Fields(v)
			if len(fields) > 0 {
				id, err := strconv.ParseInt(strings.TrimPrefix(fields[0], "0x"), 0, 32)
				if err == nil {
					mfrID = int(id)
				}
			}
		case strings.EqualFold(k, "Manufacturer Name"):
			mfrName = v
		}
	}

	// Numeric Manufacturer IDs per IANA enterprise numbers / IPMI spec:
	//   674   = Dell Inc (iDRAC)
	//   11    = HP / HPE (iLO)
	//   10876 = Super Micro Computer
	//   19046 = Lenovo
	//   2     = IBM (older Lenovo/IBM POWER BMCs)
	switch mfrID {
	case 674:
		return VendorDell
	case 11:
		return VendorHPE
	case 10876:
		return VendorSupermicro
	case 19046, 2:
		return VendorLenovo
	}

	// Fall back to name matching when the ID is 0 or unrecognised.
	lower := strings.ToLower(mfrName)
	switch {
	case strings.Contains(lower, "dell"):
		return VendorDell
	case strings.Contains(lower, "hewlett") || strings.Contains(lower, "hpe") || lower == "hp":
		return VendorHPE
	case strings.Contains(lower, "supermicro") || strings.Contains(lower, "super micro"):
		return VendorSupermicro
	case strings.Contains(lower, "lenovo") || strings.Contains(lower, "ibm"):
		return VendorLenovo
	}

	return VendorGeneric
}

// VendorQuirks describes operational quirks for a specific BMC vendor that
// callers should apply when sequencing SetBootDevWithOpts + PowerCycle.
type VendorQuirks struct {
	// UseRaw forces the raw bootparam path. Set for BMC families that silently
	// ignore ipmitool chassis bootdev (old Dell iDRAC5/6, Supermicro X9/X10).
	UseRaw bool

	// ForcePersistent overrides the caller's BootOpts.Persistent = false when
	// the one-time override is known to be unreliable (Supermicro X9/X10).
	ForcePersistent bool

	// PowerCycleDelay is the minimum pause between SetBootDev and PowerCycle.
	// HPE iLO requires ~3 seconds or the override doesn't stick.
	PowerCycleDelay time.Duration

	// SkipVerify suppresses the post-set bootparam read-back check. Lenovo
	// XCC/IMM2 often reports a stale value after write; the actual boot
	// behaviour is correct, but the read-back will mismatch.
	SkipVerify bool

	// Notes is a human-readable description of the quirks for diagnostic output.
	Notes string
}

// QuirksFor returns the known quirks for a given BMC vendor. Callers should
// apply these before calling SetBootDevWithOpts and before PowerCycle.
func QuirksFor(vendor BMCVendor) VendorQuirks {
	switch vendor {
	case VendorDell:
		// iDRAC7+ handles chassis bootdev correctly with persistent.
		// Pre-iDRAC7 (R6xx generation) ignores one-time override and may ignore
		// the friendly command entirely. Use persistent to be safe; raw fallback
		// is handled automatically inside SetBootDevWithOpts on error.
		return VendorQuirks{
			ForcePersistent: true,
			Notes:           "Dell iDRAC: one-time override unreliable pre-iDRAC7; forcing persistent. Raw fallback applied on friendly-command failure.",
		}
	case VendorHPE:
		// HPE iLO accepts the friendly chassis bootdev command but the setting
		// is not flushed to non-volatile storage until the BMC has had time to
		// process it. A power cycle issued too quickly (< ~3 s) races the flush
		// and the node boots from whatever was previously set.
		return VendorQuirks{
			PowerCycleDelay: 3 * time.Second,
			Notes:           "HPE iLO: 3-second pause required between SetNextBoot and PowerCycle or the override is not flushed before the node resets.",
		}
	case VendorSupermicro:
		// Supermicro X9 and X10 boards have a known firmware bug where the
		// one-time override bit is ignored and the BMC resets the flag on the
		// first completed POST regardless of whether the OS actually booted.
		// Always use persistent mode and the raw command path.
		return VendorQuirks{
			UseRaw:          true,
			ForcePersistent: true,
			Notes:           "Supermicro X9/X10: one-time override silently broken; forcing raw command + persistent.",
		}
	case VendorLenovo:
		// Lenovo XCC (ThinkSystem) and IMM2 (System x) accept the standard IPMI
		// command but the read-back of bootparam 5 after set does not reflect the
		// new value in the same session. Do not fail on verify mismatch.
		return VendorQuirks{
			SkipVerify: true,
			Notes:      "Lenovo XCC/IMM2: bootparam read-back after set is stale; skipping verify. Boot behaviour is correct.",
		}
	default:
		return VendorQuirks{
			Notes: "Generic BMC: using standard ipmitool chassis bootdev with persistent option.",
		}
	}
}

// SOLActivate opens a Serial Over LAN console session. This call blocks until
// the SOL session is terminated (Ctrl+] or BMC disconnect). It inherits the
// current process's stdin/stdout/stderr so the caller's terminal is used.
func (c *Client) SOLActivate(ctx context.Context) error {
	args := c.args("sol", "activate")
	cmd := exec.CommandContext(ctx, "ipmitool", args...)
	if c.Host != "" && c.Password != "" {
		cmd.Env = append(os.Environ(), "IPMITOOL_PASSWORD="+c.Password)
	}
	cmd.Stdin = nil  // SOL manages its own TTY
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}

// GetSensorData retrieves all IPMI sensor readings from the managed BMC.
func (c *Client) GetSensorData(ctx context.Context) ([]Sensor, error) {
	out, err := c.run(ctx, "sdr", "elist", "all")
	if err != nil {
		return nil, err
	}
	return parseSensors(out), nil
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

// bootDevToName maps a BootDevice constant to the ipmitool chassis bootdev
// device name string.
func bootDevToName(dev BootDevice) (string, error) {
	switch dev {
	case BootDevPXE:
		return "pxe", nil
	case BootDevDisk:
		return "disk", nil
	case BootDevBIOS:
		return "bios", nil
	case BootDevCD:
		return "cdrom", nil
	}
	return "", fmt.Errorf("ipmi: unsupported boot device 0x%02X", byte(dev))
}

// ─── Output parsers ───────────────────────────────────────────────────────────

// parseBMCConfig parses the output of `ipmitool lan print <ch>`.
// Example line: "IP Address              : 10.0.0.1"
func parseBMCConfig(out string, channel int) *BMCConfig {
	cfg := &BMCConfig{Channel: channel}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := cutColon(line)
		if !ok {
			continue
		}
		switch {
		case strings.Contains(k, "IP Address Source"):
			src := strings.ToLower(v)
			if strings.Contains(src, "static") {
				cfg.IPSource = "static"
			} else if strings.Contains(src, "dhcp") {
				cfg.IPSource = "dhcp"
			} else {
				cfg.IPSource = v
			}
		case k == "IP Address":
			cfg.IPAddress = v
		case strings.Contains(k, "Subnet Mask"):
			cfg.Netmask = v
		case strings.Contains(k, "Default Gateway IP"):
			cfg.Gateway = v
		}
	}
	return cfg
}

// parseBMCUsers parses the output of `ipmitool user list 1`.
// Header line is skipped; data lines have format: ID  Name  ... Access
func parseBMCUsers(out string) []BMCUser {
	var users []BMCUser
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "ID") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		id, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		u := BMCUser{
			ID:       id,
			Username: fields[1],
		}
		// Last field is the privilege level when present.
		if len(fields) >= 5 {
			u.Access = fields[len(fields)-1]
		}
		users = append(users, u)
	}
	return users
}

// parseSensors parses the output of `ipmitool sdr elist all`.
// Each line format: "Sensor Name      | hex  | status | entity | value"
// We also handle the simpler `ipmitool sensor` format.
func parseSensors(out string) []Sensor {
	var sensors []Sensor
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < 5 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		rawVal := strings.TrimSpace(parts[4])
		status := strings.ToLower(strings.TrimSpace(parts[2]))

		// Normalise status to ok/warning/critical/ns
		var normStatus string
		switch {
		case strings.Contains(status, "ok") || strings.Contains(status, "present"):
			normStatus = "ok"
		case strings.Contains(status, "warn") || strings.Contains(status, "nc"):
			normStatus = "warning"
		case strings.Contains(status, "crit") || strings.Contains(status, "ucr") || strings.Contains(status, "lcr"):
			normStatus = "critical"
		case strings.Contains(status, "ns") || strings.Contains(status, "no reading"):
			normStatus = "ns"
		default:
			normStatus = status
		}

		// Split "42.000 degrees C" into value + units
		value, units := splitValueUnits(rawVal)

		sensors = append(sensors, Sensor{
			Name:   name,
			Value:  value,
			Units:  units,
			Status: normStatus,
		})
	}
	return sensors
}

// parseBootParam parses the output of `ipmitool chassis bootparam get 5`.
//
// Example output:
//
//	Boot parameter version: 1
//	Boot parameter 5 is valid/unlocked
//	Boot parameter data: E008000000
//	Boot Flags :
//	 - Boot Flag Valid
//	 - Options apply to all future boots
//	 - BIOS EFI boot
//	 - Boot Device Selector : Force Boot from default Hard-Drive
func parseBootParam(out string) *BootParamResult {
	r := &BootParamResult{Raw: out}

	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)

		// "Boot parameter data: E008000000"
		if strings.HasPrefix(line, "Boot parameter data:") {
			hexStr := strings.TrimSpace(strings.TrimPrefix(line, "Boot parameter data:"))
			// Strip any whitespace within the hex string.
			hexStr = strings.ReplaceAll(hexStr, " ", "")
			if len(hexStr) >= 10 {
				for i := 0; i < 5; i++ {
					b, err := strconv.ParseUint(hexStr[i*2:i*2+2], 16, 8)
					if err == nil {
						r.DataBytes[i] = byte(b)
					}
				}
				// Decode the flag byte (first byte).
				flags := r.DataBytes[0]
				r.Valid = flags&0x80 != 0
				r.Persistent = flags&0x40 != 0
				r.EFI = flags&0x20 != 0
				// Device is the second byte (boot device selector).
				r.Device = BootDevice(r.DataBytes[1])
			}
		}
	}
	return r
}

// cutColon splits "Key Name   : value" into trimmed key and value.
func cutColon(line string) (key, val string, ok bool) {
	idx := strings.Index(line, ":")
	if idx < 0 {
		return "", "", false
	}
	return strings.TrimSpace(line[:idx]), strings.TrimSpace(line[idx+1:]), true
}

// splitValueUnits separates a string like "42.000 degrees C" into ("42.000", "degrees C").
// Returns the full string as value with empty units when there is no space.
func splitValueUnits(s string) (value, units string) {
	idx := strings.Index(s, " ")
	if idx < 0 {
		return s, ""
	}
	return s[:idx], strings.TrimSpace(s[idx+1:])
}
