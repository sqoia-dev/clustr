package pxe

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/rs/zerolog/log"
	"github.com/sqoia-dev/clonr/pkg/config"
)

// Server orchestrates the DHCP and TFTP sub-servers that make up the PXE stack.
type Server struct {
	cfg        config.PXEConfig
	DHCPServer *DHCPServer
	TFTPServer *TFTPServer
}

// New creates a PXE Server from config. Call Start to begin serving.
func New(cfg config.PXEConfig) (*Server, error) {
	serverIP, err := resolveServerIP(cfg)
	if err != nil {
		return nil, fmt.Errorf("pxe: resolve server IP: %w", err)
	}
	cfg.ServerIP = serverIP

	// Ensure TFTP directory exists.
	if err := os.MkdirAll(cfg.TFTPDir, 0o755); err != nil {
		return nil, fmt.Errorf("pxe: create tftp dir %s: %w", cfg.TFTPDir, err)
	}
	// Ensure boot directory exists.
	if err := os.MkdirAll(cfg.BootDir, 0o755); err != nil {
		return nil, fmt.Errorf("pxe: create boot dir %s: %w", cfg.BootDir, err)
	}

	httpPort := cfg.HTTPPort
	if httpPort == "" {
		httpPort = "8080"
	}
	dhcp, err := newDHCPServer(cfg.Interface, net.ParseIP(serverIP), cfg.IPRange, httpPort)
	if err != nil {
		return nil, fmt.Errorf("pxe: init dhcp server: %w", err)
	}

	tftp := newTFTPServer(cfg.TFTPDir)

	return &Server{
		cfg:        cfg,
		DHCPServer: dhcp,
		TFTPServer: tftp,
	}, nil
}

// Start launches both DHCP and TFTP servers. It blocks until ctx is cancelled.
// Both servers run in goroutines; the first fatal error is returned.
func (s *Server) Start(ctx context.Context) error {
	log.Info().
		Str("server_ip", s.cfg.ServerIP).
		Str("interface", s.cfg.Interface).
		Str("ip_range", s.cfg.IPRange).
		Str("tftp_dir", s.cfg.TFTPDir).
		Msg("PXE server starting")

	errCh := make(chan error, 2)

	go func() {
		if err := s.DHCPServer.Start(ctx); err != nil {
			errCh <- fmt.Errorf("dhcp: %w", err)
		}
	}()

	go func() {
		if err := s.TFTPServer.Start(ctx); err != nil {
			errCh <- fmt.Errorf("tftp: %w", err)
		}
	}()

	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err
	}
}

// resolveServerIP determines the IP address to advertise as the PXE server.
// If cfg.ServerIP is already set, it is returned as-is. Otherwise, the IP is
// auto-detected from cfg.Interface (first non-loopback IPv4 address).
func resolveServerIP(cfg config.PXEConfig) (string, error) {
	if cfg.ServerIP != "" {
		return cfg.ServerIP, nil
	}

	iface := cfg.Interface
	if iface == "" {
		// Auto-detect: pick first interface with a non-loopback IPv4 address.
		return autoDetectIP()
	}

	netIface, err := net.InterfaceByName(iface)
	if err != nil {
		return "", fmt.Errorf("interface %q not found: %w", iface, err)
	}

	addrs, err := netIface.Addrs()
	if err != nil {
		return "", fmt.Errorf("get addrs for %s: %w", iface, err)
	}

	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err != nil {
			continue
		}
		if ip.IsLoopback() || ip.To4() == nil {
			continue
		}
		return ip.String(), nil
	}
	return "", fmt.Errorf("no IPv4 address on interface %s", iface)
}

// autoDetectIP returns the first non-loopback IPv4 address on any interface.
func autoDetectIP() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", fmt.Errorf("list interfaces: %w", err)
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.To4() == nil {
				continue
			}
			// Skip link-local.
			if strings.HasPrefix(ip.String(), "169.254.") {
				continue
			}
			return ip.String(), nil
		}
	}
	return "", fmt.Errorf("no usable IPv4 address found on any interface")
}
