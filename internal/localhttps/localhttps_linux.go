//go:build linux

package localhttps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/mbianchidev/porto/internal/process"
)

func linuxIntegration() (systemdIntegration, error) {
	integration := systemdIntegration{unitDirectory: "/etc/systemd/system", run: linuxCommand}
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

func commandAvailable(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func linuxCommand(name string, args ...string) error {
	command := process.NewCommand(context.Background(), "", name, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s failed: %w: %s", name, err, output)
	}
	return nil
}

func Install(certificatePath, authorityPath string) error {
	if _, _, err := readAuthority(authorityPath); err != nil {
		return err
	}
	before := Snapshot(certificatePath)
	if !before.Supported {
		return errors.New(before.Message)
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if err := privilegedAction("install-root", executable, authorityPath); err != nil {
		return err
	}
	if !Trusted(certificatePath) {
		return errors.New("HTTPS is installed but system trust verification failed; check 'porto cert status', renew expired certificates and rerun 'porto https install'")
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
	integration, err := linuxIntegration()
	if err != nil {
		return Status{Message: err.Error()}
	}
	status := Status{Supported: true, Trusted: Trusted(certificatePath)}
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
	status.Listening = exec.Command("systemctl", "is-active", "--quiet", httpsSocket).Run() == nil && listenersReady()
	if !status.Listening {
		status.Message = "Run 'porto https install' to repair the owned socket; inspect 'systemctl status porto-https.socket porto-https.service' for port conflicts or helper failures"
	}
	return status
}

func Trust(path string) error   { return privilegedAction("trust-root", path) }
func Untrust(path string) error { return privilegedAction("untrust-root", path) }

func Trusted(path string) bool {
	return exec.Command("openssl", "verify", "-purpose", "sslserver", "-verify_hostname", "porto.localhost", path).Run() == nil
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
	if err := os.MkdirAll(integration.trustDirectory, 0o755); err != nil {
		return err
	}
	return integration.install(authorityPaths[0])
}

func RootUninstall() error {
	if os.Geteuid() != 0 {
		return errors.New("HTTPS socket removal requires root")
	}
	integration, err := linuxIntegration()
	if err != nil {
		return err
	}
	return integration.uninstall()
}

func RootTrust(path string, remove bool) error {
	if os.Geteuid() != 0 {
		return errors.New("system certificate trust changes require root")
	}
	integration, err := linuxIntegration()
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
		command = exec.Command("pkexec", commandArgs...)
	} else {
		command = exec.Command("sudo", append([]string{"--"}, commandArgs...)...)
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
