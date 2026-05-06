package network

import (
	"net"
	"testing"
)

// Synthetic two-NIC teacher: NIC A on 192.168.1.0/24, NIC B on 10.20.2.0/24.
// Mirrors the real-world scenario the multi-NIC bug fix targets — Stud1 lives
// on the 192.168.1.x subnet, Stud2 on 10.20.2.x.
func twoNICs() []NICInfo {
	return []NICInfo{
		{
			IP:   net.IPv4(192, 168, 1, 50).To4(),
			Mask: net.CIDRMask(24, 32),
			Name: "NIC-A",
		},
		{
			IP:   net.IPv4(10, 20, 2, 5).To4(),
			Mask: net.CIDRMask(24, 32),
			Name: "NIC-B",
		},
	}
}

func TestPickAdvertiseAddr_MatchesPerSubnet(t *testing.T) {
	nics := twoNICs()
	cases := []struct {
		studentIP string
		want      string
	}{
		{"192.168.1.15", "192.168.1.50:47820"}, // Stud1 → NIC-A
		{"10.20.2.17", "10.20.2.5:47820"},      // Stud2 → NIC-B
		{"192.168.1.250", "192.168.1.50:47820"},
		{"10.20.2.1", "10.20.2.5:47820"},
		{"127.0.0.1", "127.0.0.1:47820"}, // dev-mode loopback
	}
	for _, tc := range cases {
		got := pickAdvertiseAddr(tc.studentIP, 47820, nics)
		if got != tc.want {
			t.Errorf("pickAdvertiseAddr(%q) = %q, want %q", tc.studentIP, got, tc.want)
		}
	}
}

func TestPickAdvertiseAddr_OffSubnetFallsBackToFirstNIC(t *testing.T) {
	// A cached IP from a NIC that's no longer present should still produce
	// *some* address rather than crashing — fall back to nics[0]. (The probe
	// will fail, but that's expected; it's no worse than the old behaviour.)
	nics := twoNICs()
	got := pickAdvertiseAddr("172.16.0.5", 47820, nics)
	if got != "192.168.1.50:47820" {
		t.Errorf("off-subnet fallback = %q, want first NIC %q", got, "192.168.1.50:47820")
	}
}

func TestPickAdvertiseAddr_NoNICs(t *testing.T) {
	got := pickAdvertiseAddr("192.168.1.10", 47820, nil)
	if got != "0.0.0.0:47820" {
		t.Errorf("no-NIC fallback = %q, want %q", got, "0.0.0.0:47820")
	}
}
