package networking

import "testing"

func TestSubnetConflictsIncludeSplitVPNRoutesAndIPv6(t *testing.T) {
	conflicts, err := SubnetConflicts([]string{"10.42.0.0/16", "fd42::/64"}, []HostSubnet{
		{CIDR: "10.0.0.0/8", Interface: "synthetic-vpn", Source: "route"},
		{CIDR: "fd42::/48", Interface: "synthetic-vpn6", Source: "route"},
		{CIDR: "192.0.2.0/24", Interface: "synthetic-lan", Source: "interface"},
	})
	if err != nil || len(conflicts) != 2 {
		t.Fatalf("subnet conflicts = %+v, %v", conflicts, err)
	}
	if _, err := SubnetConflicts([]string{"not-a-subnet"}, nil); err == nil {
		t.Fatal("invalid subnet was silently accepted")
	}
}
