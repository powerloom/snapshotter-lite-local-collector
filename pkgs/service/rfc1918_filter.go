package service

import (
	"net"

	ma "github.com/multiformats/go-multiaddr"
	log "github.com/sirupsen/logrus"
)

// RFC1918 private IP ranges as per https://tools.ietf.org/html/rfc1918
var (
	// 10.0.0.0/8
	rfc1918Range1 = &net.IPNet{
		IP:   net.IP{10, 0, 0, 0},
		Mask: net.CIDRMask(8, 32),
	}
	// 172.16.0.0/12
	rfc1918Range2 = &net.IPNet{
		IP:   net.IP{172, 16, 0, 0},
		Mask: net.CIDRMask(12, 32),
	}
	// 192.168.0.0/16
	rfc1918Range3 = &net.IPNet{
		IP:   net.IP{192, 168, 0, 0},
		Mask: net.CIDRMask(16, 32),
	}
)

// IsRFC1918 checks if an IP address is in the RFC1918 private address space
func IsRFC1918(ip net.IP) bool {
	if ip == nil {
		return false
	}
	// Convert to IPv4 if it's IPv4-mapped IPv6
	ipv4 := ip.To4()
	if ipv4 == nil {
		return false // Not IPv4, so not RFC1918
	}
	return rfc1918Range1.Contains(ipv4) || rfc1918Range2.Contains(ipv4) || rfc1918Range3.Contains(ipv4)
}

// FilterRFC1918Multiaddrs filters out multiaddrs with RFC1918 IP addresses
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

		if ip != nil && IsRFC1918(ip) {
			filteredCount++
			log.Debugf("Filtered out RFC1918 address: %s", addr.String())
			continue
		}

		filtered = append(filtered, addr)
	}

	if filteredCount > 0 {
		log.Infof("Filtered %d RFC1918 private IP addresses from peer addresses", filteredCount)
	}

	return filtered, filteredCount
}

// HasRFC1918Address checks if a multiaddr contains an RFC1918 IP address
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

	return IsRFC1918(ip)
}
