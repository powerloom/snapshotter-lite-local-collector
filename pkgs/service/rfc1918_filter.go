package service

import (
	"net"

	"github.com/libp2p/go-libp2p/core/connmgr"
	"github.com/libp2p/go-libp2p/core/control"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

// Reserved IP ranges that should be blocked
var (
	// RFC1918 private IP ranges: https://tools.ietf.org/html/rfc1918
	rfc1918Range1 = &net.IPNet{IP: net.IP{10, 0, 0, 0}, Mask: net.CIDRMask(8, 32)}     // 10.0.0.0/8
	rfc1918Range2 = &net.IPNet{IP: net.IP{172, 16, 0, 0}, Mask: net.CIDRMask(12, 32)}  // 172.16.0.0/12
	rfc1918Range3 = &net.IPNet{IP: net.IP{192, 168, 0, 0}, Mask: net.CIDRMask(16, 32)} // 192.168.0.0/16

	// RFC6598 CGNAT/Shared Address Space: https://tools.ietf.org/html/rfc6598
	rfc6598Range = &net.IPNet{IP: net.IP{100, 64, 0, 0}, Mask: net.CIDRMask(10, 32)} // 100.64.0.0/10

	// RFC2544 Benchmark Testing: https://tools.ietf.org/html/rfc2544
	rfc2544Range = &net.IPNet{IP: net.IP{198, 18, 0, 0}, Mask: net.CIDRMask(15, 32)} // 198.18.0.0/15
)

// IsReservedIP checks if an IP address is in any reserved/private address space
// This includes RFC1918, RFC6598 (CGNAT), and RFC2544 (Benchmark) ranges
func IsReservedIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	// Convert to IPv4 if it's IPv4-mapped IPv6
	ipv4 := ip.To4()
	if ipv4 == nil {
		return false // Not IPv4
	}
	return rfc1918Range1.Contains(ipv4) || rfc1918Range2.Contains(ipv4) || rfc1918Range3.Contains(ipv4) ||
		rfc6598Range.Contains(ipv4) || rfc2544Range.Contains(ipv4)
}

// IsRFC1918 checks if an IP address is in the RFC1918 private address space
// Kept for backward compatibility
func IsRFC1918(ip net.IP) bool {
	return IsReservedIP(ip) // Now includes all reserved ranges
}

// FilterRFC1918Multiaddrs filters out multiaddrs with reserved IP addresses
// Returns the filtered list and count of filtered addresses
func FilterRFC1918Multiaddrs(addrs []ma.Multiaddr) ([]ma.Multiaddr, int) {
	var filtered []ma.Multiaddr
	filteredCount := 0

	for _, addr := range addrs {
		// Extract IP from multiaddr
		var ip net.IP
		ma.ForEach(addr, func(c ma.Component) bool {
			if c.Protocol().Code == ma.P_IP4 {
				ip = net.IP(c.RawValue())
				return false // Stop iteration
			}
			return true // Continue iteration
		})

		if ip != nil && IsReservedIP(ip) {
			filteredCount++
			continue
		}

		filtered = append(filtered, addr)
	}

	return filtered, filteredCount
}

// HasRFC1918Address checks if a multiaddr contains a reserved IP address
// Kept for backward compatibility - now checks all reserved ranges
func HasRFC1918Address(addr ma.Multiaddr) bool {
	var ip net.IP
	ma.ForEach(addr, func(c ma.Component) bool {
		if c.Protocol().Code == ma.P_IP4 {
			ip = net.IP(c.RawValue())
			return false // Stop iteration
		}
		return true // Continue iteration
	})

	if ip == nil {
		return false
	}

	return IsReservedIP(ip)
}

// RFC1918ConnectionGater blocks connections to reserved IP addresses
// This includes RFC1918, RFC6598 (CGNAT), and RFC2544 (Benchmark) ranges
// This is required by Hetzner to prevent scanning of internal networks
// CRITICAL: InterceptAccept prevents TCP RST responses to incoming connection attempts
type RFC1918ConnectionGater struct{}

// InterceptPeerDial blocks dialing to peers with RFC1918 addresses
func (g *RFC1918ConnectionGater) InterceptPeerDial(p peer.ID) (allow bool) {
	// Allow peer dial - we'll check addresses in InterceptAddrDial
	return true
}

// InterceptAddrDial blocks dialing to reserved IP addresses
func (g *RFC1918ConnectionGater) InterceptAddrDial(pid peer.ID, addr ma.Multiaddr) (allow bool) {
	if HasRFC1918Address(addr) {
		// Silently block - this is expected behavior and logging would be too noisy
		return false
	}
	return true
}

// InterceptAccept blocks incoming connections from reserved IP addresses
// CRITICAL: This prevents TCP RST responses to incoming connection attempts from reserved IPs
// Hetzner detects these RST packets as abuse, so we must silently reject at this layer
func (g *RFC1918ConnectionGater) InterceptAccept(conn network.ConnMultiaddrs) (allow bool) {
	remoteAddr := conn.RemoteMultiaddr()
	if HasRFC1918Address(remoteAddr) {
		// Silently reject - don't log to avoid spam
		// This prevents TCP RST responses that Hetzner flags as abuse
		return false
	}
	return true
}

// InterceptSecured blocks secured connections to reserved IP addresses
func (g *RFC1918ConnectionGater) InterceptSecured(direction network.Direction, pid peer.ID, conn network.ConnMultiaddrs) (allow bool) {
	remoteAddr := conn.RemoteMultiaddr()
	if HasRFC1918Address(remoteAddr) {
		// Silently block - this is expected behavior and logging would be too noisy
		return false
	}
	return true
}

// InterceptUpgraded blocks upgraded connections to reserved IP addresses
func (g *RFC1918ConnectionGater) InterceptUpgraded(conn network.Conn) (allow bool, reason control.DisconnectReason) {
	remoteAddr := conn.RemoteMultiaddr()
	if HasRFC1918Address(remoteAddr) {
		// Silently block - this is expected behavior and logging would be too noisy
		return false, control.DisconnectReason(0) // No specific reason needed
	}
	return true, control.DisconnectReason(0)
}

// Ensure RFC1918ConnectionGater implements connmgr.ConnectionGater
var _ connmgr.ConnectionGater = (*RFC1918ConnectionGater)(nil)
