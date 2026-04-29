package network

import (
	"net"
)

type NICInfo struct {
	IP   net.IP
	Mask net.IPMask
	MAC  net.HardwareAddr
	Name string
}

// GetLocalNICs returns all active non-loopback IPv4 interfaces
func GetLocalNICs() []NICInfo {
	var result []NICInfo

	ifaces, err := net.Interfaces()
	if err != nil {
		return result
	}

	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil {
				continue // skip IPv6
			}
			result = append(result, NICInfo{
				IP:   ip,
				Mask: ipnet.Mask,
				MAC:  iface.HardwareAddr,
				Name: iface.Name,
			})
		}
	}
	return result
}

// SubnetIPs returns all host IPs in the subnet, excluding network addr, broadcast, and our own IP
func SubnetIPs(info NICInfo) []net.IP {
	var ips []net.IP

	network := info.IP.Mask(info.Mask)
	broadcast := make(net.IP, 4)
	for i := 0; i < 4; i++ {
		broadcast[i] = network[i] | ^info.Mask[i]
	}

	for ip := nextIP(network); !ip.Equal(broadcast); ip = nextIP(ip) {
		if !ip.Equal(info.IP) {
			ips = append(ips, cloneIP(ip))
		}
	}
	return ips
}

// PrimaryMAC returns the MAC of the first active non-loopback interface
func PrimaryMAC() string {
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp != 0 && iface.Flags&net.FlagLoopback == 0 {
			if len(iface.HardwareAddr) > 0 {
				return iface.HardwareAddr.String()
			}
		}
	}
	return "00:00:00:00:00:00"
}

func nextIP(ip net.IP) net.IP {
	next := cloneIP(ip)
	for i := len(next) - 1; i >= 0; i-- {
		next[i]++
		if next[i] != 0 {
			break
		}
	}
	return next
}

func cloneIP(ip net.IP) net.IP {
	cp := make(net.IP, len(ip))
	copy(cp, ip)
	return cp
}
