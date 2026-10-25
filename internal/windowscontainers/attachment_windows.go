//go:build windows

package windowscontainers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Microsoft/hcsshim/hcn"
	"github.com/containerd/containerd/v2/pkg/netns"
	cni "github.com/containerd/go-cni"
)

func nativeCNI(i Installation, config []byte) (cni.CNI, error) {
	return cni.New(cni.WithPluginDir([]string{i.Bin()}), cni.WithConfListBytes(config))
}

func ownedCNIConfiguration(i Installation, name string) ([]byte, error) {
	return readOwnedCNIConfiguration(i, name, protectedTree)
}

func AttachNetwork(ctx context.Context, i Installation, id, name string, published []string) (attachment Attachment, err error) {
	attachment = Attachment{Container: id, Network: name}
	if name == "none" {
		return attachment, nil
	}
	if len(id) != 64 || strings.Trim(id, "0123456789abcdef") != "" {
		return attachment, errors.New("invalid native network container identity")
	}
	if err := validateNetworkName(i, name); err != nil {
		return attachment, err
	}
	ports, err := ParsePublishedPorts(published)
	if err != nil {
		return attachment, err
	}
	networkMu.Lock()
	defer networkMu.Unlock()
	ledger, err := networkLedger(i)
	if err != nil {
		return attachment, err
	}
	record, ok := ledger[name]
	if !ok {
		return attachment, fmt.Errorf("%w: native NAT network has no receipt", ErrOwnership)
	}
	network, err := hcn.GetNetworkByID(record.ID)
	if err != nil {
		return attachment, err
	}
	if err := checkNetwork(record, *network); err != nil {
		return attachment, err
	}
	config, err := ownedCNIConfiguration(i, name)
	if err != nil {
		return attachment, err
	}
	plugin, err := nativeCNI(i, config)
	if err != nil {
		return attachment, err
	}
	namespace, err := netns.NewNetNS("")
	if err != nil {
		return attachment, err
	}
	attachment.Namespace, attachment.NetworkID, attachment.Config, attachment.Ports = namespace.GetPath(), record.ID, config, ports
	directory := filepath.Join(i.Root, "attachments")
	if err := makeProtectedDirectory(directory); err != nil && !errors.Is(err, os.ErrExist) {
		_ = namespace.Remove()
		return attachment, err
	}
	receipt := filepath.Join(directory, id+".json")
	if _, err := os.Lstat(receipt); err == nil {
		_ = namespace.Remove()
		return attachment, fmt.Errorf("%w: native endpoint receipt already exists", ErrOwnership)
	}
	if err := writeJSON(receipt, attachment); err != nil {
		_ = namespace.Remove()
		return attachment, err
	}
	defer func() {
		if err == nil {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		endpoints, listErr := hcn.GetNamespaceEndpointIds(attachment.Namespace)
		if listErr != nil && !hcn.IsNotFoundError(listErr) {
			err = errors.Join(err, listErr)
			return
		}
		for _, id := range endpoints {
			endpoint, queryErr := hcn.GetEndpointByID(id)
			if queryErr != nil || !strings.EqualFold(endpoint.HostComputeNetwork, record.ID) {
				err = errors.Join(err, fmt.Errorf("%w: refusing cleanup of changed native endpoint: %v", ErrOwnership, queryErr))
				return
			}
		}
		removeErr := plugin.Remove(cleanup, attachment.Container, attachment.Namespace, cni.WithCapabilityPortMap(ports))
		namespaceErr := namespace.Remove()
		err = errors.Join(err, removeErr, namespaceErr)
		if removeErr == nil && namespaceErr == nil {
			err = errors.Join(err, os.Remove(receipt))
		}
	}()
	result, setupErr := plugin.Setup(ctx, id, attachment.Namespace, cni.WithCapabilityPortMap(ports))
	endpoints, listErr := hcn.GetNamespaceEndpointIds(attachment.Namespace)
	if listErr == nil {
		attachment.Endpoints = endpoints
	}
	if setupErr != nil || listErr != nil {
		return attachment, errors.Join(setupErr, listErr)
	}
	for _, endpointID := range attachment.Endpoints {
		endpoint, err := hcn.GetEndpointByID(endpointID)
		if err != nil || !strings.EqualFold(endpoint.HostComputeNetwork, record.ID) {
			return attachment, fmt.Errorf("%w: native endpoint does not belong to its recorded NAT network: %v", ErrOwnership, err)
		}
	}
	for _, config := range result.Interfaces {
		for _, ip := range config.IPConfigs {
			if ip.IP.To4() != nil {
				attachment.Address, attachment.Gateway = ip.IP.String(), ip.Gateway.String()
				break
			}
		}
	}
	if err := writeJSON(receipt, attachment); err != nil {
		return attachment, err
	}
	return attachment, nil
}

func DetachNetwork(ctx context.Context, i Installation, attachment Attachment) error {
	if attachment.Network == "none" || attachment.Namespace == "" {
		return nil
	}
	if len(attachment.Container) != 64 || strings.Trim(attachment.Container, "0123456789abcdef") != "" {
		return ErrOwnership
	}
	if err := validateNetworkName(i, attachment.Network); err != nil {
		return err
	}
	networkMu.Lock()
	defer networkMu.Unlock()
	receipt := filepath.Join(i.Root, "attachments", attachment.Container+".json")
	data, err := os.ReadFile(receipt)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var stored Attachment
	if json.Unmarshal(data, &stored) != nil || stored.Namespace != attachment.Namespace || stored.NetworkID != attachment.NetworkID {
		return ErrOwnership
	}
	endpoints, err := hcn.GetNamespaceEndpointIds(stored.Namespace)
	if err != nil && !hcn.IsNotFoundError(err) {
		return err
	}
	owned := make(map[string]bool, len(stored.Endpoints))
	attached := make(map[string]bool, len(endpoints))
	for _, id := range stored.Endpoints {
		owned[strings.ToLower(id)] = true
	}
	for _, id := range endpoints {
		attached[strings.ToLower(id)] = true
		if !owned[strings.ToLower(id)] {
			return fmt.Errorf("%w: native namespace contains an unrecorded endpoint", ErrOwnership)
		}
	}
	for _, id := range stored.Endpoints {
		endpoint, err := hcn.GetEndpointByID(id)
		if hcn.IsNotFoundError(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !strings.EqualFold(endpoint.HostComputeNetwork, stored.NetworkID) {
			return ErrOwnership
		}
		if endpoint.HostComputeNamespace != "" && !strings.EqualFold(endpoint.HostComputeNamespace, stored.Namespace) {
			return ErrOwnership
		}
		if attached[strings.ToLower(id)] {
			if endpoint.HostComputeNamespace != "" && !strings.EqualFold(endpoint.HostComputeNamespace, stored.Namespace) {
				return ErrOwnership
			}
			if err := endpoint.NamespaceDetach(stored.Namespace); err != nil && !hcn.IsNotFoundError(err) {
				return err
			}
		}
		if err := endpoint.Delete(); err != nil && !hcn.IsNotFoundError(err) {
			return err
		}
	}
	if err := netns.LoadNetNS(stored.Namespace).Remove(); err != nil {
		return err
	}
	return os.Remove(receipt)
}
