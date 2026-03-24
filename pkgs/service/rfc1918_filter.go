package service

import (
	"net"
	"os"
	"strings"

	"github.com/libp2p/go-libp2p/core/connmgr"
	"github.com/libp2p/go-libp2p/core/control"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
	log "github.com/sirupsen/logrus"
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

// isIPv4UnsuitableForPublicMesh is true for addresses that must not be advertised in the DHT
// (loopback, link-local e.g. 169.254.x.x, and RFC1918/CGNAT/benchmark ranges).
func isIPv4UnsuitableForPublicMesh(ip net.IP) bool {
	if ip == nil {
		return false
	}
	ipv4 := ip.To4()
	if ipv4 == nil {
		return false
	}
	if ipv4.IsLoopback() || ipv4.IsLinkLocalUnicast() {
		return true
	}
	return IsReservedIP(ipv4)
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

		if ip != nil && isIPv4UnsuitableForPublicMesh(ip) {
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

	return isIPv4UnsuitableForPublicMesh(ip)
}

// RFC1918ConnectionGater blocks connections to reserved IP addresses.
// Hetzner requirement: prevent outbound scanning of internal networks.
// In Docker bridge mode, all inbound connections appear from the bridge gateway
// (e.g. 172.21.0.1), so we whitelist configured gateway IPs to avoid rejecting
// legitimate public peers whose source IP was rewritten by Docker NAT.
type RFC1918ConnectionGater struct {
	dockerBridgeGateways []net.IP
}

// NewRFC1918ConnectionGater reads DOCKER_BRIDGE_GATEWAY_IPS (comma-separated)
// and whitelists those IPs in InterceptAccept / InterceptSecured / InterceptUpgraded.
// Example: DOCKER_BRIDGE_GATEWAY_IPS=172.21.0.1
func NewRFC1918ConnectionGater() *RFC1918ConnectionGater {
	gater := &RFC1918ConnectionGater{}
	if gwStr := os.Getenv("DOCKER_BRIDGE_GATEWAY_IPS"); gwStr != "" {
		for _, s := range strings.Split(gwStr, ",") {
			s = strings.TrimSpace(s)
			if ip := net.ParseIP(s); ip != nil {
				gater.dockerBridgeGateways = append(gater.dockerBridgeGateways, ip)
				log.Infof("connection gater: whitelisting Docker bridge gateway %s for inbound connections", s)
			}
		}
	}
	return gater
}

func (g *RFC1918ConnectionGater) isWhitelistedGateway(addr ma.Multiaddr) bool {
	if len(g.dockerBridgeGateways) == 0 {
		return false
	}
	var ip net.IP
	ma.ForEach(addr, func(c ma.Component) bool {
		if c.Protocol().Code == ma.P_IP4 {
			ip = net.IP(c.RawValue())
			return false
		}
		return true
	})
	if ip == nil {
		return false
	}
	ipv4 := ip.To4()
	if ipv4 == nil {
		return false
	}
	for _, gw := range g.dockerBridgeGateways {
		if gw.To4().Equal(ipv4) {
			return true
		}
	}
	return false
}

// InterceptPeerDial blocks dialing to peers with RFC1918 addresses
func (g *RFC1918ConnectionGater) InterceptPeerDial(p peer.ID) (allow bool) {
	// Allow peer dial - we'll check addresses in InterceptAddrDial
	return true
}

// InterceptAddrDial blocks dialing to reserved IP addresses
func (g *RFC1918ConnectionGater) InterceptAddrDial(pid peer.ID, addr ma.Multiaddr) (allow bool) {
	if HasRFC1918Address(addr) {
		log.Debugf("connection gater: block outbound dial peer=%s addr=%s", pid, addr.String())
		return false
	}
	return true
}

func (g *RFC1918ConnectionGater) InterceptAccept(conn network.ConnMultiaddrs) (allow bool) {
	remoteAddr := conn.RemoteMultiaddr()
	if g.isWhitelistedGateway(remoteAddr) {
		return true
	}
	if HasRFC1918Address(remoteAddr) {
		log.Infof("connection gater: reject inbound (InterceptAccept) remote=%s", remoteAddr.String())
		return false
	}
	return true
}

func (g *RFC1918ConnectionGater) InterceptSecured(direction network.Direction, pid peer.ID, conn network.ConnMultiaddrs) (allow bool) {
	remoteAddr := conn.RemoteMultiaddr()
	if g.isWhitelistedGateway(remoteAddr) {
		return true
	}
	if HasRFC1918Address(remoteAddr) {
		log.Infof("connection gater: reject secured (InterceptSecured) dir=%v peer=%s remote=%s", direction, pid, remoteAddr.String())
		return false
	}
	return true
}

func (g *RFC1918ConnectionGater) InterceptUpgraded(conn network.Conn) (allow bool, reason control.DisconnectReason) {
	remoteAddr := conn.RemoteMultiaddr()
	if g.isWhitelistedGateway(remoteAddr) {
		return true, control.DisconnectReason(0)
	}
	if HasRFC1918Address(remoteAddr) {
		log.Infof("connection gater: reject upgraded (InterceptUpgraded) remote=%s", remoteAddr.String())
		return false, control.DisconnectReason(0)
	}
	return true, control.DisconnectReason(0)
}

// Ensure RFC1918ConnectionGater implements connmgr.ConnectionGater
var _ connmgr.ConnectionGater = (*RFC1918ConnectionGater)(nil)
