package config

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	AppName                  = "porto"
	DaemonAddr               = "127.0.0.1:37623"
	RouterAddr               = "127.0.0.1:37680"
	RouterTLSAddr            = "127.0.0.1:37681"
	RouterTLSAddrEnv         = "PORTO_TLS_ADDR"
	RouterTLSPublicPortEnv   = "PORTO_TLS_PUBLIC_PORT"
	PortlessHTTPSMarker      = "portless-https"
	LocalDomain              = "porto.local"
	LocalhostDomain          = "porto.localhost"
	BasePort                 = 41000
	DefaultScanDepth         = 3
	BranchCleanupInterval    = 10 * time.Second
	CertificateCheckInterval = 24 * time.Hour
)

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
