package networking

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/process"
)

type HostSubnet struct {
	CIDR      string `json:"cidr"`
	Interface string `json:"interface"`
	Source    string `json:"source"`
}

type SubnetConflict struct {
	Subnet    string `json:"subnet"`
	HostCIDR  string `json:"hostCIDR"`
	Interface string `json:"interface"`
}

type HostStatus struct {
	Subnets            []HostSubnet `json:"subnets"`
	LocalhostAddresses []string     `json:"localhostAddresses"`
	Warnings           []string     `json:"warnings"`
}

type Capability struct {
	Supported bool   `json:"supported"`
	Message   string `json:"message"`
}

func DirectIPCapability(goos, backend string, rootless bool) Capability {
	if goos == "linux" && backend == "local" && !rootless {
		return Capability{Supported: true, Message: "Rootful local Linux bridge addresses use existing host routes; firewall and VPN policy still apply"}
	}
	if rootless {
		return Capability{Message: "Rootless container addresses are private to their network namespace; use published loopback ports or an opted-in HTTP route"}
	}
	return Capability{Message: "Lima/QEMU guest container addresses are not routed onto the host; use owned loopback forwards. Porto does not install LAN or VPN routes"}
}

func SubnetConflicts(subnets []string, host []HostSubnet) ([]SubnetConflict, error) {
	conflicts := []SubnetConflict{}
	for _, value := range subnets {
		subnet, err := netip.ParsePrefix(value)
		if err != nil {
			return nil, fmt.Errorf("invalid workload subnet %q: %w", value, err)
		}
		for _, candidate := range host {
			prefix, err := netip.ParsePrefix(candidate.CIDR)
			if err != nil {
				return nil, fmt.Errorf("invalid host subnet %q: %w", candidate.CIDR, err)
			}
			if prefix.Bits() == 0 || prefix.Addr().BitLen() != subnet.Addr().BitLen() {
				continue
			}
			if prefix.Overlaps(subnet) {
				conflicts = append(conflicts, SubnetConflict{Subnet: value, HostCIDR: candidate.CIDR, Interface: candidate.Interface})
			}
		}
	}
	return conflicts, nil
}

func InspectHost(ctx context.Context) HostStatus {
	status := HostStatus{Subnets: []HostSubnet{}, LocalhostAddresses: []string{}, Warnings: []string{}}
	interfaces, err := net.Interfaces()
	if err != nil {
		status.Warnings = append(status.Warnings, "Read host interfaces: "+err.Error())
	}
	seen := map[string]bool{}
	add := func(subnet HostSubnet) {
		key := subnet.CIDR + "\x00" + subnet.Interface
		if !seen[key] {
			status.Subnets = append(status.Subnets, subnet)
			seen[key] = true
		}
	}
	for _, device := range interfaces {
		if device.Flags&net.FlagUp == 0 || device.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, err := device.Addrs()
		if err != nil {
			status.Warnings = append(status.Warnings, "Read interface "+device.Name+": "+err.Error())
			continue
		}
		for _, address := range addresses {
			prefix, err := netip.ParsePrefix(address.String())
			if err != nil {
				status.Warnings = append(status.Warnings, "Unrecognized interface address "+address.String())
				continue
			}
			add(HostSubnet{CIDR: prefix.Masked().String(), Interface: device.Name, Source: "interface"})
		}
	}
	routes, err := hostRoutes(ctx)
	if err != nil {
		status.Warnings = append(status.Warnings, "Full routing/VPN inspection is unavailable: "+err.Error())
	} else {
		for _, route := range routes {
			add(route)
		}
	}
	resolveContext, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	addresses, err := net.DefaultResolver.LookupIPAddr(resolveContext, "probe.porto.localhost")
	if err != nil {
		status.Warnings = append(status.Warnings, "The host resolver did not resolve *.porto.localhost. Browsers may resolve .localhost internally; use curl --resolve for command-line clients or exact local DNS records. Split DNS settings are not overwritten")
	} else {
		for _, address := range addresses {
			if !address.IP.IsLoopback() {
				status.Warnings = append(status.Warnings, "System DNS returned a non-loopback .localhost address; check VPN/split-DNS policy before using local routes")
			}
			status.LocalhostAddresses = append(status.LocalhostAddresses, address.String())
		}
	}
	sort.Slice(status.Subnets, func(i, j int) bool {
		return status.Subnets[i].Interface+status.Subnets[i].CIDR < status.Subnets[j].Interface+status.Subnets[j].CIDR
	})
	return status
}

func hostRoutes(ctx context.Context) ([]HostSubnet, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	switch runtime.GOOS {
	case "linux":
		routes := []HostSubnet{}
		for _, family := range []string{"-4", "-6"} {
			output, err := process.NewCommand(ctx, "", "ip", "-json", family, "route", "show", "table", "all").CombinedOutput()
			if err != nil {
				return nil, fmt.Errorf("ip route: %w", err)
			}
			var entries []struct {
				Destination string `json:"dst"`
				Device      string `json:"dev"`
			}
			if err := json.Unmarshal(output, &entries); err != nil {
				return nil, err
			}
			for _, entry := range entries {
				if prefix, ok := routePrefix(entry.Destination); ok && prefix.Bits() != 0 {
					routes = append(routes, HostSubnet{CIDR: prefix.String(), Interface: entry.Device, Source: "route"})
				}
			}
		}
		return routes, nil
	case "darwin":
		output, err := process.NewCommand(ctx, "", "/usr/sbin/netstat", "-rn").CombinedOutput()
		if err != nil {
			return nil, err
		}
		routes := []HostSubnet{}
		for _, line := range strings.Split(string(output), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 4 {
				continue
			}
			if prefix, ok := routePrefix(fields[0]); ok && prefix.Bits() != 0 && fields[3] != "lo0" {
				routes = append(routes, HostSubnet{CIDR: prefix.String(), Interface: fields[3], Source: "route"})
			}
		}
		return routes, nil
	case "windows":
		output, err := process.NewCommand(ctx, "", "powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command",
			"$ErrorActionPreference='Stop'; @(Get-NetRoute | Select-Object DestinationPrefix,InterfaceAlias) | ConvertTo-Json -Compress").CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("Get-NetRoute: %w", err)
		}
		var entries []struct {
			Prefix    string `json:"DestinationPrefix"`
			Interface string `json:"InterfaceAlias"`
		}
		if err := json.Unmarshal(output, &entries); err != nil {
			return nil, err
		}
		routes := []HostSubnet{}
		for _, entry := range entries {
			if prefix, ok := routePrefix(entry.Prefix); ok && prefix.Bits() != 0 {
				routes = append(routes, HostSubnet{CIDR: prefix.String(), Interface: entry.Interface, Source: "route"})
			}
		}
		return routes, nil
	default:
		return nil, fmt.Errorf("route inspection is unsupported on %s", runtime.GOOS)
	}
}

func routePrefix(value string) (netip.Prefix, bool) {
	value = strings.TrimSpace(value)
	if value == "" || value == "default" || strings.Contains(value, "%") {
		return netip.Prefix{}, false
	}
	if prefix, err := netip.ParsePrefix(value); err == nil {
		return prefix.Masked(), true
	}
	host, suffix, hasMask := strings.Cut(value, "/")
	if strings.Contains(host, ":") {
		if address, err := netip.ParseAddr(host); err == nil && !hasMask {
			return netip.PrefixFrom(address, 128), true
		}
		return netip.Prefix{}, false
	}
	parts := strings.Split(host, ".")
	bits := len(parts) * 8
	if hasMask {
		var err error
		bits, err = strconv.Atoi(suffix)
		if err != nil {
			return netip.Prefix{}, false
		}
	}
	for len(parts) < 4 {
		parts = append(parts, "0")
	}
	address, err := netip.ParseAddr(strings.Join(parts, "."))
	if err != nil || bits < 0 || bits > 32 {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(address, bits).Masked(), true
}
