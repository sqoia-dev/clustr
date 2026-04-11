// Package pxe provides a built-in DHCP/TFTP/iPXE server for clonr-serverd.
// It handles PXE boot requests from bare-metal nodes on the provisioning network,
// assigns IPs, and chainloads into iPXE.
package pxe

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
	"github.com/rs/zerolog/log"
)

// leaseEntry holds an IP lease for a client.
type leaseEntry struct {
	IP        net.IP
	ExpiresAt time.Time
}

// DHCPServer is a lightweight DHCP server that only responds to PXE clients.
type DHCPServer struct {
	iface    string
	serverIP net.IP
	rangeStart net.IP
	rangeEnd   net.IP
	leaseDur   time.Duration

	mu     sync.Mutex
	leases map[string]leaseEntry // keyed by MAC string
	pool   []net.IP              // pre-built list of IPs in range

	server *server4.Server
}

// newDHCPServer creates a DHCPServer from config. It does not start listening.
func newDHCPServer(iface string, serverIP net.IP, ipRange string) (*DHCPServer, error) {
	start, end, err := parseIPRange(ipRange)
	if err != nil {
		return nil, fmt.Errorf("pxe/dhcp: parse ip range: %w", err)
	}

	pool := buildPool(start, end)
	if len(pool) == 0 {
		return nil, fmt.Errorf("pxe/dhcp: ip range %s produced no addresses", ipRange)
	}

	return &DHCPServer{
		iface:      iface,
		serverIP:   serverIP,
		rangeStart: start,
		rangeEnd:   end,
		leaseDur:   24 * time.Hour,
		leases:     make(map[string]leaseEntry),
		pool:       pool,
	}, nil
}

// Start begins listening for DHCP requests on the configured interface.
func (d *DHCPServer) Start(ctx context.Context) error {
	handler := func(conn net.PacketConn, peer net.Addr, req *dhcpv4.DHCPv4) {
		d.handleDHCP(conn, peer, req)
	}

	srv, err := server4.NewServer(d.iface, nil, handler)
	if err != nil {
		return fmt.Errorf("pxe/dhcp: create server on %s: %w", d.iface, err)
	}
	d.server = srv

	log.Info().Str("interface", d.iface).Str("server_ip", d.serverIP.String()).
		Str("range", fmt.Sprintf("%s-%s", d.rangeStart, d.rangeEnd)).
		Msg("DHCP server listening")

	go func() {
		<-ctx.Done()
		srv.Close()
	}()

	if err := srv.Serve(); err != nil {
		// Serve returns on Close — only treat as error if context is not done.
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("pxe/dhcp: serve: %w", err)
	}
	return nil
}

// handleDHCP is the per-packet handler called by the server4.Server.
func (d *DHCPServer) handleDHCP(conn net.PacketConn, peer net.Addr, req *dhcpv4.DHCPv4) {
	// Only serve PXE clients. Option 60 (VendorClassIdentifier) must contain
	// "PXEClient". Clients already running iPXE set user-class to "iPXE".
	vendorClass := req.ClassIdentifier()
	userClass := string(req.Options.Get(dhcpv4.OptionUserClassInformation))

	isPXEClient := strings.HasPrefix(vendorClass, "PXEClient")
	isIPXE := strings.Contains(userClass, "iPXE")

	if !isPXEClient && !isIPXE {
		// Not a PXE/iPXE client — ignore.
		return
	}

	mac := req.ClientHWAddr.String()
	log.Debug().Str("mac", mac).Str("type", req.MessageType().String()).
		Bool("ipxe", isIPXE).Msg("DHCP PXE request")

	switch req.MessageType() {
	case dhcpv4.MessageTypeDiscover:
		d.handleDiscover(conn, peer, req, isIPXE)
	case dhcpv4.MessageTypeRequest:
		d.handleRequest(conn, peer, req, isIPXE)
	}
}

func (d *DHCPServer) handleDiscover(conn net.PacketConn, peer net.Addr, req *dhcpv4.DHCPv4, isIPXE bool) {
	ip := d.acquireOrAssignIP(req.ClientHWAddr.String())
	if ip == nil {
		log.Warn().Str("mac", req.ClientHWAddr.String()).Msg("DHCP pool exhausted")
		return
	}

	resp, err := dhcpv4.NewReplyFromRequest(req)
	if err != nil {
		log.Error().Err(err).Msg("DHCP: build reply")
		return
	}
	resp.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeOffer))
	d.populateBootOptions(resp, req, ip, isIPXE)

	if _, err := conn.WriteTo(resp.ToBytes(), peer); err != nil {
		log.Error().Err(err).Msg("DHCP: send offer")
	}
}

func (d *DHCPServer) handleRequest(conn net.PacketConn, peer net.Addr, req *dhcpv4.DHCPv4, isIPXE bool) {
	ip := d.acquireOrAssignIP(req.ClientHWAddr.String())
	if ip == nil {
		log.Warn().Str("mac", req.ClientHWAddr.String()).Msg("DHCP pool exhausted on request")
		return
	}

	resp, err := dhcpv4.NewReplyFromRequest(req)
	if err != nil {
		log.Error().Err(err).Msg("DHCP: build reply")
		return
	}
	resp.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeAck))
	d.populateBootOptions(resp, req, ip, isIPXE)

	log.Info().Str("mac", req.ClientHWAddr.String()).Str("ip", ip.String()).Msg("DHCP ACK")

	if _, err := conn.WriteTo(resp.ToBytes(), peer); err != nil {
		log.Error().Err(err).Msg("DHCP: send ack")
	}
}

// populateBootOptions fills in yiaddr, next-server, boot-file, and lease time.
func (d *DHCPServer) populateBootOptions(resp *dhcpv4.DHCPv4, req *dhcpv4.DHCPv4, ip net.IP, isIPXE bool) {
	resp.YourIPAddr = ip
	resp.ServerIPAddr = d.serverIP

	// Subnet mask — /24 for the provisioning range.
	resp.UpdateOption(dhcpv4.OptSubnetMask(net.CIDRMask(24, 32)))
	resp.UpdateOption(dhcpv4.OptRouter(d.serverIP))
	resp.UpdateOption(dhcpv4.OptIPAddressLeaseTime(d.leaseDur))
	resp.UpdateOption(dhcpv4.OptServerIdentifier(d.serverIP))

	// Next-server (siaddr) always points to self.
	resp.ServerIPAddr = d.serverIP

	bootFile := bootFilename(req, isIPXE, d.serverIP)
	if bootFile != "" {
		resp.BootFileName = bootFile
	}
}

// bootFilename selects the appropriate boot file based on client architecture.
// Arch type is carried in DHCP option 93 (ClientSystemArchitectureType).
// If the client is already running iPXE, return the HTTP URL to the boot script.
func bootFilename(req *dhcpv4.DHCPv4, isIPXE bool, serverIP net.IP) string {
	if isIPXE {
		// Already chainloaded into iPXE — give it the boot script URL.
		return fmt.Sprintf("http://%s:8080/api/v1/boot/ipxe", serverIP)
	}

	// Read option 93 — client system architecture.
	archOpt := req.Options.Get(dhcpv4.OptionClientSystemArchitectureType)
	if len(archOpt) >= 2 {
		archType := uint16(archOpt[0])<<8 | uint16(archOpt[1])
		switch archType {
		case 7, 9: // UEFI x86-64
			return "ipxe.efi"
		case 6: // UEFI 32-bit
			return "ipxe.efi"
		case 10: // UEFI ARM64
			return "ipxe.efi"
		}
	}
	// Default: BIOS (arch type 0 or unset).
	return "undionly.kpxe"
}

// acquireOrAssignIP finds an existing lease or assigns a new IP from the pool.
func (d *DHCPServer) acquireOrAssignIP(mac string) net.IP {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()

	// Check existing (non-expired) lease.
	if lease, ok := d.leases[mac]; ok && lease.ExpiresAt.After(now) {
		return lease.IP
	}

	// Collect IPs currently in use by non-expired leases.
	inUse := make(map[string]bool, len(d.leases))
	for _, l := range d.leases {
		if l.ExpiresAt.After(now) {
			inUse[l.IP.String()] = true
		}
	}

	// Pick first free IP from pool.
	for _, ip := range d.pool {
		if !inUse[ip.String()] {
			d.leases[mac] = leaseEntry{
				IP:        ip,
				ExpiresAt: now.Add(d.leaseDur),
			}
			return ip
		}
	}
	return nil
}

// parseIPRange parses a "start-end" IP range string.
func parseIPRange(r string) (net.IP, net.IP, error) {
	parts := strings.SplitN(r, "-", 2)
	if len(parts) != 2 {
		return nil, nil, fmt.Errorf("expected format start-end, got %q", r)
	}
	start := net.ParseIP(strings.TrimSpace(parts[0])).To4()
	end := net.ParseIP(strings.TrimSpace(parts[1])).To4()
	if start == nil || end == nil {
		return nil, nil, fmt.Errorf("invalid IP addresses in range %q", r)
	}
	return start, end, nil
}

// buildPool constructs a flat list of IPs from start to end (inclusive).
func buildPool(start, end net.IP) []net.IP {
	var pool []net.IP
	cur := cloneIP(start)
	for !ipGreaterThan(cur, end) {
		pool = append(pool, cloneIP(cur))
		incrementIP(cur)
	}
	return pool
}

func cloneIP(ip net.IP) net.IP {
	c := make(net.IP, len(ip))
	copy(c, ip)
	return c
}

func incrementIP(ip net.IP) {
	for i := len(ip) - 1; i >= 0; i-- {
		ip[i]++
		if ip[i] != 0 {
			break
		}
	}
}

func ipGreaterThan(a, b net.IP) bool {
	for i := range a {
		if a[i] > b[i] {
			return true
		}
		if a[i] < b[i] {
			return false
		}
	}
	return false
}
