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
	remote := []string{"-H", c.Host, "-U", c.Username, "-E"}
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
func (c *Client) SetBootPXE(ctx context.Context) error {
	_, err := c.run(ctx, "chassis", "bootdev", "pxe", "options=efiboot")
	return err
}

// SetBootDisk sets the next one-time boot device to the local disk.
func (c *Client) SetBootDisk(ctx context.Context) error {
	_, err := c.run(ctx, "chassis", "bootdev", "disk", "options=efiboot")
	return err
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
