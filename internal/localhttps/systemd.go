package localhttps

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	systemdOwner = "# Managed by Porto HTTPS v1\n"
	httpsSocket  = "porto-https.socket"
	httpsService = "porto-https.service"
)

type systemdIntegration struct {
	unitDirectory, trustDirectory, proxyExecutable string
	refreshTrust                                   []string
	run                                            func(string, ...string) error
}

func (s systemdIntegration) units() map[string][]byte {
	return map[string][]byte{
		httpsSocket: []byte(systemdOwner + `[Unit]
Description=Porto loopback HTTPS socket

[Socket]
ListenStream=127.0.0.1:443
ListenStream=[::1]:443
BindIPv6Only=ipv6-only
NoDelay=true

[Install]
WantedBy=sockets.target
`),
		httpsService: []byte(systemdOwner + `[Unit]
Description=Porto loopback HTTPS forwarder
Requires=porto-https.socket
After=porto-https.socket

[Service]
ExecStart=` + s.proxyExecutable + ` 127.0.0.1:37681
DynamicUser=yes
NoNewPrivileges=yes
CapabilityBoundingSet=
PrivateTmp=yes
PrivateDevices=yes
ProtectSystem=strict
ProtectHome=yes
RestrictAddressFamilies=AF_INET AF_INET6
IPAddressDeny=any
IPAddressAllow=localhost
Restart=on-failure
`),
	}
}

func ownedUnit(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("refusing non-regular HTTPS unit %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(data, []byte(systemdOwner)) {
		return nil, fmt.Errorf("%s is not owned by Porto; leave it unchanged and resolve the conflict", path)
	}
	return data, nil
}

func (s systemdIntegration) install(authorityPath string) (err error) {
	authority, fingerprint, err := readAuthority(authorityPath)
	if err != nil {
		return err
	}
	previous := map[string][]byte{}
	for name := range s.units() {
		path := filepath.Join(s.unitDirectory, name)
		data, err := ownedUnit(path)
		if err != nil {
			return err
		}
		previous[path] = data
	}
	trustPath := filepath.Join(s.trustDirectory, "porto-"+fingerprint+".crt")
	oldTrust, err := os.ReadFile(trustPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if oldTrust != nil && !bytes.Equal(oldTrust, authority) {
		return fmt.Errorf("certificate file %s does not match Porto's authority; refusing to replace it", trustPath)
	}
	previous[trustPath] = oldTrust
	wasInstalled := previous[filepath.Join(s.unitDirectory, httpsSocket)] != nil
	defer func() {
		if err == nil {
			return
		}
		if !wasInstalled {
			err = errors.Join(err, s.run("systemctl", "stop", httpsSocket, httpsService))
			err = errors.Join(err, s.run("systemctl", "disable", httpsSocket))
		}
		for path, data := range previous {
			if data == nil {
				removeErr := os.Remove(path)
				if !errors.Is(removeErr, os.ErrNotExist) {
					err = errors.Join(err, removeErr)
				}
			} else {
				err = errors.Join(err, writeIntegrationFile(path, data, 0o644))
			}
		}
		err = errors.Join(err, s.run(s.refreshTrust[0], s.refreshTrust[1:]...))
		err = errors.Join(err, s.run("systemctl", "daemon-reload"))
	}()
	for name, data := range s.units() {
		if err := writeIntegrationFile(filepath.Join(s.unitDirectory, name), data, 0o644); err != nil {
			return err
		}
	}
	if err := writeIntegrationFile(trustPath, authority, 0o644); err != nil {
		return err
	}
	if err := s.run(s.refreshTrust[0], s.refreshTrust[1:]...); err != nil {
		return fmt.Errorf("refresh system certificate trust: %w", err)
	}
	if err := s.run("systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := s.run("systemctl", "enable", "--now", httpsSocket); err != nil {
		return fmt.Errorf("activate loopback HTTPS; check port 443 and 'systemctl status porto-https.socket': %w", err)
	}
	return nil
}

func (s systemdIntegration) uninstall() error {
	found := false
	for name := range s.units() {
		data, err := ownedUnit(filepath.Join(s.unitDirectory, name))
		if err != nil {
			return err
		}
		found = found || data != nil
	}
	if !found {
		return nil
	}
	if err := s.run("systemctl", "disable", "--now", httpsSocket); err != nil {
		return err
	}
	if err := s.run("systemctl", "stop", httpsService); err != nil {
		return err
	}
	for name := range s.units() {
		if err := os.Remove(filepath.Join(s.unitDirectory, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return s.run("systemctl", "daemon-reload")
}

func (s systemdIntegration) trust(path string, remove bool) error {
	data, fingerprint, err := readAuthority(path)
	if err != nil {
		return err
	}
	destination := filepath.Join(s.trustDirectory, "porto-"+fingerprint+".crt")
	if remove {
		existing, err := os.ReadFile(destination)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !bytes.Equal(data, existing) {
			return errors.New("installed authority no longer matches Porto's certificate; refusing removal")
		}
		if info, err := os.Lstat(destination); err != nil || !info.Mode().IsRegular() {
			return errors.New("installed authority is not a regular Porto-owned file")
		}
		if err := os.Remove(destination); err != nil {
			return err
		}
	} else if err := writeIntegrationFile(destination, data, 0o644); err != nil {
		return err
	}
	if err := s.run(s.refreshTrust[0], s.refreshTrust[1:]...); err != nil {
		return fmt.Errorf("refresh certificate trust with %s: %w", strings.Join(s.refreshTrust, " "), err)
	}
	return nil
}
