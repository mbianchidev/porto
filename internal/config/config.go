package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const (
	AppName                  = "porto"
	Version                  = "1.0.0"
	APIVersion               = 24
	DaemonAddr               = "127.0.0.1:37623"
	RouterAddr               = "127.0.0.1:37680"
	RouterTLSAddr            = "127.0.0.1:37681"
	RouterTLSAddrEnv         = "PORTO_TLS_ADDR"
	RouterTLSPublicPortEnv   = "PORTO_TLS_PUBLIC_PORT"
	DockerCanonicalSocketEnv = "PORTO_DOCKER_CANONICAL_SOCKET"
	PortlessHTTPSMarker      = "portless-https"
	LocalDomain              = "porto.local"
	LocalhostDomain          = "porto.localhost"
	BasePort                 = 41000
	DefaultScanDepth         = 3
	BranchCleanupInterval    = 10 * time.Second
	CertificateCheckInterval = 24 * time.Hour
)

var branchTokenSeparators = regexp.MustCompile(`[^a-z0-9]+`)

func RouterTLSAddress() string {
	if address := strings.TrimSpace(os.Getenv(RouterTLSAddrEnv)); address != "" {
		return address
	}
	return RouterTLSAddr
}

func ProjectHTTPSURL(hostname string) string {
	host := hostname + "." + LocalhostDomain
	port := strings.TrimSpace(os.Getenv(RouterTLSPublicPortEnv))
	if port == "" {
		if PortlessHTTPSInstalled() {
			port = "443"
		} else {
			_, port, _ = net.SplitHostPort(RouterTLSAddress())
		}
	}
	if port == "" || port == "443" {
		return "https://" + host + "/"
	}
	return "https://" + net.JoinHostPort(host, port) + "/"
}

func ProjectHostname(base, branch, defaultBranch string) string {
	if branch == "" || branch == defaultBranch {
		return base
	}
	return appendHostnameSuffix(base, compactBranchToken(branch))
}

func DisambiguateProjectHostname(candidate, branch string) string {
	sum := sha256.Sum256([]byte(branch))
	return appendHostnameSuffix(candidate, hex.EncodeToString(sum[:3]))
}

func ManagedWorktreeRoot() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "worktrees"), nil
}

func compactBranchToken(branch string) string {
	parts := branchTokenSeparators.Split(strings.ToLower(branch), -1)
	tokens := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		if len(part) > 3 {
			part = part[:3]
		}
		tokens = append(tokens, part)
	}
	if len(tokens) == 0 {
		return "branch"
	}
	return strings.Join(tokens, "-")
}

func appendHostnameSuffix(base, suffix string) string {
	labels := strings.Split(base, ".")
	last := labels[len(labels)-1]
	maxSuffix := 63 - len(last) - 1
	if maxSuffix < 1 {
		last = last[:min(len(last), 55)]
		maxSuffix = 63 - len(last) - 1
	}
	if len(suffix) > maxSuffix {
		sum := sha256.Sum256([]byte(suffix))
		hash := hex.EncodeToString(sum[:3])
		keep := maxSuffix - len(hash) - 1
		if keep < 1 {
			suffix = hash
			if len(suffix) > maxSuffix {
				suffix = suffix[:maxSuffix]
			}
		} else {
			suffix = strings.Trim(suffix[:keep], "-") + "-" + hash
		}
	}
	labels[len(labels)-1] = last + "-" + suffix
	return strings.Join(labels, ".")
}

func Dir() (string, error) {
	if custom := os.Getenv("PORTO_HOME"); custom != "" {
		return custom, os.MkdirAll(custom, 0o755)
	}
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil || home == "" {
			return "", errors.New("cannot determine config directory")
		}
		base = filepath.Join(home, ".config")
	}
	dir := filepath.Join(base, AppName)
	return dir, os.MkdirAll(dir, 0o755)
}

func DBPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "porto.db"), nil
}

func RuntimeDir() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	runtimeDir := filepath.Join(dir, "run")
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(runtimeDir, 0o700); err != nil {
		return "", err
	}
	return runtimeDir, nil
}

func DockerSocketPath() (string, error) {
	if runtime.GOOS == "windows" {
		return `\\.\pipe\porto_docker_engine`, nil
	}
	dir, err := RuntimeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "docker.sock"), nil
}

func DockerEndpoint() (string, error) {
	socketPath, err := DockerSocketPath()
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(socketPath, `\\.\pipe\`) {
		return "npipe:////./pipe/" + strings.TrimPrefix(socketPath, `\\.\pipe\`), nil
	}
	return "unix://" + socketPath, nil
}

func DockerEndpointStatePath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "docker-endpoint.json"), nil
}

func DockerEngineDir() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	engineDir := filepath.Join(dir, "docker")
	if err := os.MkdirAll(engineDir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(engineDir, 0o700); err != nil {
		return "", err
	}
	return engineDir, nil
}

func CanonicalDockerSocketPath() string {
	if path := strings.TrimSpace(os.Getenv(DockerCanonicalSocketEnv)); path != "" {
		return path
	}
	if runtime.GOOS == "windows" {
		return `\\.\pipe\docker_engine`
	}
	return "/var/run/docker.sock"
}

func KubernetesConfigDir() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	kubeconfigDir := filepath.Join(dir, "kubernetes")
	if err := os.MkdirAll(kubeconfigDir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(kubeconfigDir, 0o700); err != nil {
		return "", err
	}
	return kubeconfigDir, nil
}

func KubernetesClusterFileToken(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:12])
}

func VMStateDir() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	vmDir := filepath.Join(dir, "vms")
	if err := os.MkdirAll(vmDir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(vmDir, 0o700); err != nil {
		return "", err
	}
	return vmDir, nil
}

func CertificatePaths() (string, string, error) {
	dir, err := Dir()
	if err != nil {
		return "", "", err
	}
	certDir := filepath.Join(dir, "certificates")
	return filepath.Join(certDir, LocalDomain+".pem"), filepath.Join(certDir, LocalDomain+"-key.pem"), nil
}

func CertificateAuthorityPaths() (string, string, error) {
	dir, err := Dir()
	if err != nil {
		return "", "", err
	}
	certDir := filepath.Join(dir, "certificates")
	return filepath.Join(certDir, "porto-root-ca.pem"), filepath.Join(certDir, "porto-root-ca-key.pem"), nil
}

func PortlessHTTPSMarkerPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, PortlessHTTPSMarker), nil
}

func PortlessHTTPSInstalled() bool {
	path, err := PortlessHTTPSMarkerPath()
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
