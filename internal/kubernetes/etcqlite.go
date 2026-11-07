package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mbianchidev/porto/internal/etcqlite"
)

type EtcqliteDatastore struct {
	Name        string `json:"name"`
	ClusterID   string `json:"clusterID,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

type EtcqliteProvider interface {
	Connection(context.Context, string, string) (etcqlite.Connection, error)
	Backup(context.Context, string, string) (etcqlite.Backup, error)
}

var datastoreFingerprintPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func (p *ClusterProvisioner) SetEtcqliteProvider(provider EtcqliteProvider) {
	p.etcqlite = provider
}

func (p *ClusterProvisioner) resolveEtcqlite(ctx context.Context, request *ClusterRequest, creating bool) (*etcqlite.Connection, error) {
	if request.Etcqlite == nil {
		return nil, nil
	}
	if request.Provider != "k3s" {
		return nil, fmt.Errorf("%w: datastore selection is supported only for explicitly created k3s clusters", etcqlite.ErrInvalid)
	}
	if !clusterNamePattern.MatchString(request.Etcqlite.Name) {
		return nil, fmt.Errorf("%w: invalid datastore name", etcqlite.ErrInvalid)
	}
	if p.etcqlite == nil {
		return nil, fmt.Errorf("%w: enable and start the named datastore explicitly", etcqlite.ErrUnavailable)
	}
	expected := request.Etcqlite.ClusterID
	if creating {
		if request.Etcqlite.Fingerprint != "" && !datastoreFingerprintPattern.MatchString(request.Etcqlite.Fingerprint) {
			return nil, fmt.Errorf("%w: invalid Etcqlite datastore fingerprint", etcqlite.ErrInvalid)
		}
	} else if expected == "" || !datastoreFingerprintPattern.MatchString(request.Etcqlite.Fingerprint) {
		return nil, errors.New("Etcqlite datastore binding lacks a pinned identity; existing clusters are never migrated or retargeted")
	}
	connection, err := p.etcqlite.Connection(ctx, request.Etcqlite.Name, expected)
	if err != nil {
		return nil, err
	}
	if connection.ClusterID == "" || !datastoreFingerprintPattern.MatchString(connection.Fingerprint) || len(connection.Endpoints) == 0 ||
		len(connection.CA) == 0 || len(connection.Certificate) == 0 || len(connection.Key) == 0 {
		return nil, errors.New("Etcqlite connection lacks verified identity, endpoints or mTLS material")
	}
	for _, endpoint := range connection.Endpoints {
		address, err := url.Parse(endpoint)
		if err != nil || address.Scheme != "https" || address.User != nil || address.Path != "" ||
			address.RawQuery != "" || address.Fragment != "" || address.Port() == "" {
			return nil, errors.New("Etcqlite k3s datastore must use explicit HTTPS client endpoints")
		}
		if ip := net.ParseIP(address.Hostname()); (ip != nil && (ip.IsLoopback() || ip.IsUnspecified())) ||
			strings.EqualFold(address.Hostname(), "localhost") || strings.HasSuffix(strings.ToLower(address.Hostname()), ".localhost") {
			return nil, errors.New("Etcqlite datastore endpoint is not guest-reachable; Porto does not rewrite loopback addresses")
		}
	}
	if creating {
		if (expected != "" && connection.ClusterID != expected) ||
			(request.Etcqlite.Fingerprint != "" && request.Etcqlite.Fingerprint != connection.Fingerprint) {
			return nil, fmt.Errorf("%w: selected Etcqlite identity changed before k3s creation", etcqlite.ErrConflict)
		}
		request.Etcqlite = &EtcqliteDatastore{Name: request.Etcqlite.Name, ClusterID: connection.ClusterID, Fingerprint: connection.Fingerprint}
	} else if connection.ClusterID != expected || connection.Fingerprint != request.Etcqlite.Fingerprint {
		return nil, fmt.Errorf("%w: Etcqlite endpoint/CA identity changed; the existing k3s cluster was not retargeted", etcqlite.ErrConflict)
	}
	return &connection, nil
}

func (p *ClusterProvisioner) configureK3sEtcqlite(ctx context.Context, machine string, connection *etcqlite.Connection) ([]string, error) {
	const directory = "/etc/rancher/k3s/porto-etcqlite"
	if _, err := p.vms.Exec(ctx, machine, []string{"sudo", "install", "-d", "-m", "700", directory}, nil); err != nil {
		return nil, fmt.Errorf("prepare private k3s Etcqlite credentials: %w", err)
	}
	for _, file := range []struct {
		name string
		data []byte
	}{
		{"ca.pem", connection.CA}, {"client.pem", connection.Certificate}, {"client-key.pem", connection.Key},
	} {
		if _, err := p.vms.Exec(ctx, machine, []string{
			"sudo", "sh", "-c", "umask 077; cat > " + directory + "/" + file.name,
		}, file.data); err != nil {
			return nil, fmt.Errorf("write private k3s Etcqlite %s: %w", file.name, err)
		}
	}
	var failures []error
	ready := 0
	for _, endpoint := range connection.Endpoints {
		_, err := p.vms.Exec(ctx, machine, []string{
			"sudo", "curl", "--fail-with-body", "--silent", "--show-error", "--connect-timeout", "5", "--max-time", "10",
			"--cacert", directory + "/ca.pem", "--cert", directory + "/client.pem", "--key", directory + "/client-key.pem",
			endpoint + "/readyz",
		}, nil)
		if err != nil {
			failures = append(failures, err)
		} else {
			ready++
		}
	}
	if ready < len(connection.Endpoints)/2+1 {
		return nil, fmt.Errorf("Etcqlite datastore is not mTLS-ready from the new k3s VM; no installer was run: %w", errors.Join(failures...))
	}
	return []string{
		"--datastore-endpoint", strings.Join(connection.Endpoints, ","),
		"--datastore-cafile", directory + "/ca.pem",
		"--datastore-certfile", directory + "/client.pem",
		"--datastore-keyfile", directory + "/client-key.pem",
	}, nil
}

func (p *ClusterProvisioner) backupEtcqliteDatastore(ctx context.Context, request ClusterRequest) (string, error) {
	if request.Etcqlite == nil {
		return "", nil
	}
	if _, err := p.resolveEtcqlite(ctx, &request, false); err != nil {
		return "", err
	}
	backup, err := p.etcqlite.Backup(ctx, request.Etcqlite.Name, request.Etcqlite.ClusterID)
	if err != nil {
		return "", fmt.Errorf("verify native Etcqlite recovery image before changing k3s: %w", err)
	}
	if !filepath.IsAbs(backup.Path) || backup.ClusterID != request.Etcqlite.ClusterID || len(backup.Snapshots) == 0 {
		return "", errors.New("Etcqlite backup did not return a verified, identity-bound recovery artifact")
	}
	server := clusterMachineName(clusterRuntimeName(request), "server", 1)
	token, err := p.vms.Exec(ctx, server, []string{"sudo", "cat", "/var/lib/rancher/k3s/server/token"}, nil)
	tokenPreserved := err == nil && strings.TrimSpace(string(token)) != ""
	incompleteCreation := request.CreatedAt != "" && request.RunningAt == "" && (request.Phase == "creating" || request.Phase == "error")
	if !tokenPreserved && !incompleteCreation {
		return "", errors.Join(errors.New("k3s recovery token could not be preserved; native backup remains, destructive operation refused"), err)
	}
	if tokenPreserved {
		if err := writePrivateExclusive(filepath.Join(backup.Path, "k3s-server-token"), token); err != nil {
			return "", fmt.Errorf("preserve private k3s recovery token: %w", err)
		}
	} else {
		slog.Warn("Incomplete new k3s bootstrap has no recovery token; native datastore images are verified and retained", "cluster", request.Name, "error", err)
	}
	data, err := json.Marshal(struct {
		ClusterRequest
		TokenPreserved bool `json:"tokenPreserved"`
	}{request, tokenPreserved})
	if err != nil {
		return "", err
	}
	if err := writePrivateExclusive(filepath.Join(backup.Path, "k3s-cluster.json"), data); err != nil {
		return "", err
	}
	return backup.Path, nil
}

func (p *ClusterProvisioner) ProtectEtcqliteLifecycle(_ context.Context, name, clusterID string) error {
	names, err := p.ManagedNames()
	if err != nil {
		return fmt.Errorf("inspect k3s datastore ownership before Etcqlite lifecycle: %w", err)
	}

	for _, cluster := range names {
		spec, err := p.readClusterMetadata(cluster)
		if err != nil {
			return err
		}
		if spec.Etcqlite != nil && spec.Etcqlite.Name == name {
			if spec.Etcqlite.ClusterID != clusterID {
				return fmt.Errorf("%w: k3s %s references a different identity under this datastore name", etcqlite.ErrConflict, cluster)
			}
			return fmt.Errorf("%w: k3s %s still uses Etcqlite %s; the datastore will not be stopped, disabled or removed", etcqlite.ErrConflict, cluster, name)
		}
	}
	return nil
}

func writePrivateExclusive(path string, contents []byte) (err error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	if _, err := file.Write(contents); err != nil {
		return err
	}
	return file.Sync()
}
