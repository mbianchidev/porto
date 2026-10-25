//go:build linux

package localhttps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mbianchidev/porto/internal/process"
)

func linuxIntegration() (systemdIntegration, error) {
	integration, err := linuxTrustIntegration()
	if err != nil {
		return integration, err
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return integration, errors.New("portless HTTPS requires a running systemd host; use HTTPS on port 37681 on other Linux installations")
	}
	for _, path := range []string{"/usr/lib/systemd/systemd-socket-proxyd", "/lib/systemd/systemd-socket-proxyd"} {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			integration.proxyExecutable = path
			break
		}
	}
	if integration.proxyExecutable == "" {
		return integration, errors.New("install the distribution's systemd-socket-proxyd package, then retry")
	}
	if !commandAvailable("certutil") {
		return integration, errors.New("install libnss3-tools (Debian/Ubuntu) or nss-tools (Fedora) for Chromium user trust, then retry 'porto https install'")
	}
	return integration, nil
}

func linuxTrustIntegration() (systemdIntegration, error) {
	integration := systemdIntegration{unitDirectory: "/etc/systemd/system", run: linuxCommand}
	switch {
	case commandAvailable("update-ca-certificates"):
		integration.trustDirectory = "/usr/local/share/ca-certificates"
		integration.refreshTrust = []string{"update-ca-certificates"}
	case commandAvailable("update-ca-trust"):
		integration.trustDirectory = "/etc/pki/ca-trust/source/anchors"
		integration.refreshTrust = []string{"update-ca-trust", "extract"}
	default:
		return integration, errors.New("install ca-certificates with update-ca-certificates or update-ca-trust; custom trust stores require manual HTTPS forwarding")
	}
	return integration, nil
}

func linuxNSSStore() (nssTrust, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nssTrust{}, err
	}
	directory := filepath.Join(home, ".local", "share", "pki", "nssdb")
	legacy := filepath.Join(home, ".pki", "nssdb")
	for _, name := range []string{"cert9.db", "cert8.db"} {
		if _, err := os.Stat(filepath.Join(legacy, name)); err == nil {
			directory = legacy
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return nssTrust{}, err
		}
	}
	if !commandAvailable("certutil") {
		return nssTrust{}, errors.New("Chromium trust requires libnss3-tools or nss-tools")
	}
	return nssTrust{directory: directory, run: func(args ...string) ([]byte, error) {
		command, err := linuxNativeCommand("certutil", args...)
		if err != nil {
			return nil, err
		}
		return command.CombinedOutput()
	}}, nil
}

func commandAvailable(name string) bool {
	_, err := linuxSystemTool(name)
	return err == nil
}

func linuxSystemTool(name string) (string, error) {
	for _, directory := range []string{"/usr/sbin", "/usr/bin", "/sbin", "/bin"} {
		path, err := filepath.EvalSymlinks(filepath.Join(directory, name))
		if err != nil {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if ok && info.Mode().IsRegular() && info.Mode()&0o022 == 0 && owner.Uid == 0 {
			return path, nil
		}
	}
	return "", fmt.Errorf("trusted system tool %s is unavailable; install it with the distribution package manager", name)
}

func linuxNativeCommand(name string, args ...string) (*exec.Cmd, error) {
	path, err := linuxSystemTool(name)
	if err != nil {
		return nil, err
	}
	command := process.NewCommand(context.Background(), "", path, args...)
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	return command, nil
}

func linuxCommand(name string, args ...string) error {
	command, err := linuxNativeCommand(name, args...)
	if err != nil {
		return err
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s failed: %w: %s", name, err, output)
	}
	return nil
}

func Install(certificatePath, authorityPath string) (err error) {
	if _, _, err := readAuthority(authorityPath); err != nil {
		return err
	}
	before := Snapshot(certificatePath)
	if !before.Supported {
		return errors.New(before.Message)
	}
	store, err := linuxNSSStore()
	if err != nil {
		return err
	}
	_, hadBrowserCertificate, err := store.lookup(authorityPath)
	if err != nil {
		return err
	}
	hadSystemTrust := authoritySystemTrusted(authorityPath)
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if err := privilegedAction("install-root", executable, authorityPath); err != nil {
		return err
	}
	defer func() {
		if err == nil {
			return
		}
		if !hadBrowserCertificate {
			err = errors.Join(err, store.set(authorityPath, true))
		}
		if !before.Installed {
			err = errors.Join(err, privilegedAction("uninstall-root"))
		}
		if !hadSystemTrust {
			err = errors.Join(err, privilegedAction("untrust-root", authorityPath))
		}
	}()
	if !listenersReady() {
		return errors.New("owned HTTPS socket did not become ready on both loopbacks; inspect 'systemctl status porto-https.socket' for port conflicts")
	}
	if err := store.set(authorityPath, false); err != nil {
		return err
	}
	if !Trusted(certificatePath) {
		return errors.New("system or Chromium certificate verification failed; renew expired certificates and retry")
	}
	return markInstalled()
}

func Uninstall() error {
	if err := privilegedAction("uninstall-root"); err != nil {
		return err
	}
	return removeMarker()
}

func Snapshot(certificatePath string) Status {
	integration, integrationErr := linuxIntegration()
	status := Status{Supported: integrationErr == nil}
	if integrationErr != nil {
		status.Message = integrationErr.Error()
	}
	authorityPath := filepath.Join(filepath.Dir(certificatePath), "porto-root-ca.pem")
	if store, err := linuxNSSStore(); err != nil {
		status.Message = strings.TrimSpace(status.Message + " " + err.Error())
	} else if trusted, err := store.trusted(authorityPath); err != nil {
		status.Message = strings.TrimSpace(status.Message + " " + err.Error())
	} else {
		status.BrowserTrusted = trusted
	}
	status.Trusted = systemTrusted(certificatePath) && status.BrowserTrusted
	for name := range integration.units() {
		data, err := ownedUnit(filepath.Join(integration.unitDirectory, name))
		if err != nil {
			status.Message = err.Error()
			return status
		}
		if data == nil {
			return status
		}
	}
	status.Installed = true
	command, err := linuxNativeCommand("systemctl", "is-active", "--quiet", httpsSocket)
	status.Listening = err == nil && command.Run() == nil && listenersReady()
	if !status.Listening {
		status.Message = strings.TrimSpace(status.Message + " Run 'porto https install' to repair the owned socket; inspect 'systemctl status porto-https.socket porto-https.service' for port conflicts or helper failures")
	} else if status.Trusted {
		status.Message = "System clients and Chromium NSS trust are ready. Restart an already-open browser; Firefox, Snap, and Flatpak profiles may require explicit CA import"
	}
	return status
}

func Trust(path string) (err error) {
	if _, _, err := readAuthority(path); err != nil {
		return err
	}
	store, err := linuxNSSStore()
	if err != nil {
		return err
	}
	_, hadBrowserCertificate, err := store.lookup(path)
	if err != nil {
		return err
	}
	hadSystemTrust := authoritySystemTrusted(path)
	if err := privilegedAction("trust-root", path); err != nil {
		return err
	}
	if err := store.set(path, false); err != nil {
		if !hadBrowserCertificate {
			err = errors.Join(err, store.set(path, true))
		}
		if !hadSystemTrust {
			err = errors.Join(err, privilegedAction("untrust-root", path))
		}
		return err
	}
	return nil
}

func Untrust(path string) error {
	if _, _, err := readAuthorityIdentity(path); err != nil {
		return err
	}
	store, err := linuxNSSStore()
	if err != nil {
		return err
	}
	if err := privilegedAction("untrust-root", path); err != nil {
		return err
	}
	return store.set(path, true)
}

func Trusted(path string) bool {
	if !systemTrusted(path) {
		return false
	}
	store, err := linuxNSSStore()
	if err != nil {
		return false
	}
	trusted, err := store.trusted(filepath.Join(filepath.Dir(path), "porto-root-ca.pem"))
	return err == nil && trusted
}

func systemTrusted(path string) bool {
	command, err := linuxNativeCommand("openssl", "verify", "-purpose", "sslserver", "-verify_hostname", "porto.localhost", path)
	return err == nil && command.Run() == nil
}

func authoritySystemTrusted(path string) bool {
	command, err := linuxNativeCommand("openssl", "verify", path)
	return err == nil && command.Run() == nil
}

func RootInstall(_ string, authorityPaths ...string) error {
	if os.Geteuid() != 0 {
		return errors.New("HTTPS socket installation requires root; use 'porto https install', not a privileged daemon")
	}
	if len(authorityPaths) != 1 {
		return errors.New("HTTPS installation requires the public Porto certificate authority")
	}
	integration, err := linuxIntegration()
	if err != nil {
		return err
	}
	previous, err := ownedUnit(filepath.Join(integration.unitDirectory, httpsSocket))
	if err != nil {
		return err
	}
	active := false
	if previous != nil {
		command, err := linuxNativeCommand("systemctl", "is-active", "--quiet", httpsSocket)
		if err != nil {
			return err
		}
		active = command.Run() == nil
	}
	if !active {
		if err := checkPortsAvailable(); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(integration.trustDirectory, 0o755); err != nil {
		return err
	}
	return integration.install(authorityPaths[0])
}

func RootUninstall() error {
	if os.Geteuid() != 0 {
		return errors.New("HTTPS socket removal requires root")
	}
	integration := systemdIntegration{unitDirectory: "/etc/systemd/system", run: linuxCommand}
	return integration.uninstall()
}

func RootTrust(path string, remove bool) error {
	if os.Geteuid() != 0 {
		return errors.New("system certificate trust changes require root")
	}
	integration, err := linuxTrustIntegration()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(integration.trustDirectory, 0o755); err != nil {
		return err
	}
	return integration.trust(path, remove)
}

func privilegedAction(action string, args ...string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	commandArgs := append([]string{executable, "https", action}, args...)
	var command *exec.Cmd
	if os.Geteuid() == 0 {
		command = exec.Command(commandArgs[0], commandArgs[1:]...)
	} else if commandAvailable("pkexec") {
		path, err := linuxSystemTool("pkexec")
		if err != nil {
			return err
		}
		command = exec.Command(path, commandArgs...)
	} else {
		path, err := linuxSystemTool("sudo")
		if err != nil {
			return err
		}
		command = exec.Command(path, append([]string{"--"}, commandArgs...)...)
	}
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("authorize HTTPS integration (no daemon or workload is elevated): %w", err)
	}
	return nil
}

func Run(ctx context.Context) error {
	return RunForwarders(ctx, []string{ListenAddress, ListenAddressIPv6}, TargetAddress)
}
