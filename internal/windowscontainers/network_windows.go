//go:build windows

package windowscontainers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Microsoft/hcsshim/hcn"
)

var networkMu sync.Mutex

func networkLedger(i Installation) (map[string]Network, error) {
	data, err := os.ReadFile(filepath.Join(i.Root, "networks.json"))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]Network{}, nil
	}
	if err != nil {
		return nil, err
	}
	var networks map[string]Network
	if err := json.Unmarshal(data, &networks); err != nil {
		return nil, err
	}
	return networks, nil
}

func checkNetwork(expected Network, actual hcn.HostComputeNetwork) error {
	if !strings.EqualFold(expected.ID, actual.Id) || expected.Name != actual.Name || actual.Type != hcn.NAT ||
		len(actual.Ipams) != 1 || len(actual.Ipams[0].Subnets) != 1 ||
		actual.Ipams[0].Subnets[0].IpAddressPrefix != expected.Subnet {
		return fmt.Errorf("%w: HNS network %s identity, type, or subnet changed; no network was changed", ErrOwnership, expected.Name)
	}
	return nil
}

func CheckNetworks(i Installation) error {
	networkMu.Lock()
	defer networkMu.Unlock()
	ledger, err := networkLedger(i)
	if err != nil {
		return err
	}
	networks, err := hcn.ListNetworks()
	if err != nil {
		return err
	}
	for _, actual := range networks {
		if validateNetworkName(i, actual.Name) != nil {
			continue
		}
		expected, ok := ledger[actual.Name]
		if !ok {
			return fmt.Errorf("%w: refusing unrecorded HNS network %s", ErrOwnership, actual.Name)
		}
		if err := checkNetwork(expected, actual); err != nil {
			return err
		}
	}
	return nil
}

func EnsureNetwork(i Installation, name, subnet string) (Network, error) {
	networkMu.Lock()
	defer networkMu.Unlock()
	if err := validateNetworkName(i, name); err != nil {
		return Network{}, err
	}
	ip, block, err := net.ParseCIDR(subnet)
	if err != nil || ip.To4() == nil || !ip.Equal(block.IP) {
		return Network{}, errors.New("native NAT networking requires a canonical IPv4 subnet")
	}
	ones, bits := block.Mask.Size()
	if bits != 32 || ones < 16 || ones > 28 {
		return Network{}, errors.New("native NAT subnet prefix must be between /16 and /28")
	}
	ledger, err := networkLedger(i)
	if err != nil {
		return Network{}, err
	}
	networks, err := hcn.ListNetworks()
	if err != nil {
		return Network{}, err
	}
	if previous, ok := ledger[name]; ok {
		if previous.Subnet != subnet {
			return Network{}, fmt.Errorf("%w: native network subnet differs from its receipt", ErrOwnership)
		}
		for _, actual := range networks {
			if strings.EqualFold(actual.Id, previous.ID) {
				return previous, checkNetwork(previous, actual)
			}
			if actual.Name == name {
				return Network{}, fmt.Errorf("%w: HNS name was replaced", ErrOwnership)
			}
		}
		// A missing network is not silently recreated: existing container
		// endpoints and persisted network identities need explicit recovery.
		return Network{}, fmt.Errorf("%w: recorded native HNS network %s is missing", ErrOwnership, name)
	}
	for _, actual := range networks {
		if actual.Name == name {
			return Network{}, fmt.Errorf("%w: HNS network name already belongs to another runtime", ErrOwnership)
		}
		for _, ipam := range actual.Ipams {
			for _, other := range ipam.Subnets {
				address, network, err := net.ParseCIDR(other.IpAddressPrefix)
				if err == nil && (block.Contains(address) || network.Contains(block.IP)) {
					return Network{}, fmt.Errorf("native NAT subnet %s overlaps HNS network %s; no network was changed", subnet, actual.Name)
				}
			}
		}
	}
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return Network{}, err
	}
	for _, address := range addresses {
		host, hostNet, parseErr := net.ParseCIDR(address.String())
		if parseErr == nil && host.To4() != nil && (block.Contains(host) || hostNet.Contains(block.IP)) {
			return Network{}, fmt.Errorf("native NAT subnet %s overlaps host interface %s", subnet, address)
		}
	}
	gateway := append(net.IP(nil), block.IP.To4()...)
	gateway[3]++
	created, err := (&hcn.HostComputeNetwork{
		Name: name, Type: hcn.NAT, SchemaVersion: hcn.SchemaVersion{Major: 2},
		Ipams: []hcn.Ipam{{Type: "Static", Subnets: []hcn.Subnet{{
			IpAddressPrefix: subnet, Routes: []hcn.Route{{NextHop: gateway.String(), DestinationPrefix: "0.0.0.0/0"}},
		}}}},
	}).Create()
	if err != nil {
		return Network{}, fmt.Errorf("create Porto-owned HNS NAT: %w", err)
	}
	receipt := Network{Name: name, ID: created.Id, Subnet: subnet}
	ledger[name] = receipt
	if err := writeJSON(filepath.Join(i.Root, "networks.json"), ledger); err != nil {
		return Network{}, errors.Join(err, created.Delete())
	}
	return receipt, nil
}

func RemoveNetwork(i Installation, name string) error {
	networkMu.Lock()
	defer networkMu.Unlock()
	if err := validateNetworkName(i, name); err != nil {
		return err
	}
	if name == i.DefaultNetwork() {
		return errors.New("the owned default native NAT network is preserved with runtime data")
	}
	ledger, err := networkLedger(i)
	if err != nil {
		return err
	}
	expected, ok := ledger[name]
	if !ok {
		return fmt.Errorf("%w: native network has no receipt", ErrOwnership)
	}
	networks, err := hcn.ListNetworks()
	if err != nil {
		return err
	}
	for _, actual := range networks {
		if actual.Name != name && !strings.EqualFold(actual.Id, expected.ID) {
			continue
		}
		if err := checkNetwork(expected, actual); err != nil {
			return err
		}
		endpoints, err := hcn.ListEndpoints()
		if err != nil {
			return err
		}
		for _, endpoint := range endpoints {
			if strings.EqualFold(endpoint.HostComputeNetwork, actual.Id) {
				return errors.New("native HNS network still has endpoints; remove only its containers first")
			}
		}
		if err := actual.Delete(); err != nil {
			return err
		}
	}
	delete(ledger, name)
	return writeJSON(filepath.Join(i.Root, "networks.json"), ledger)
}
