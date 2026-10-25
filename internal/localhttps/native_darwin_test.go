//go:build darwin

package localhttps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/process"
)

func nativeBrowserArguments() []string { return nil }

func nativeCertificateConsent(*testing.T, context.Context, int, string) func() error {
	return func() error { return nil }
}

func nativeHeadlessHTTPSCommand(t *testing.T, ctx context.Context, binary, home string, args ...string) ([]byte, bool, error) {
	t.Helper()
	if len(args) != 2 || args[0] != "https" {
		return nil, false, nil
	}
	run := func(elevated bool, arguments ...string) error {
		command := process.NewCommand(ctx, "", binary, arguments...)
		if elevated {
			command = process.NewCommand(ctx, "", "/usr/bin/sudo", append([]string{"-n", binary}, arguments...)...)
		}
		command.Env = process.WithEnvironment(os.Environ(), "PORTO_HOME="+home)
		output, err := command.CombinedOutput()
		if err != nil {
			return fmt.Errorf("headless native macOS HTTPS %v: %w: %s", arguments, err, output)
		}
		return nil
	}
	certificate := filepath.Join(home, "certificates", "porto.local.pem")
	switch args[1] {
	case "install":
		prior := Snapshot(certificate)
		err := installWithRollback(installSteps{
			installed: prior.Installed, trusted: prior.Trusted,
			install: func() error { return run(true, "https", "install-root", binary) },
			remove:  func() error { return run(true, "https", "uninstall-root") },
			trust:   func() error { return run(false, "cert", "trust") },
			untrust: func() error { return run(false, "cert", "untrust") },
			verify:  func() bool { return Trusted(certificate) },
			mark:    func() error { return writeIntegrationFile(filepath.Join(home, config.PortlessHTTPSMarker), nil, 0o600) },
		})
		return nil, true, err
	case "uninstall":
		err := run(true, "https", "uninstall-root")
		if err == nil {
			if removeErr := os.Remove(filepath.Join(home, config.PortlessHTTPSMarker)); !errors.Is(removeErr, os.ErrNotExist) {
				err = removeErr
			}
		}
		return nil, true, err
	default:
		return nil, false, nil
	}
}

func assertNativeHelperLeastPrivilege() error {
	output, err := process.NewCommand(context.Background(), "", "/bin/launchctl", "print", "system/"+launchDaemonLabel).CombinedOutput()
	if err != nil || !strings.Contains(string(output), installedBinary) {
		return fmt.Errorf("inspect owned launchd HTTPS helper: %w: %s", err, output)
	}
	info, err := os.Stat(installedBinary)
	if err != nil || info.Mode()&0o022 != 0 {
		return errors.New("macOS HTTPS helper is missing or writable outside its root owner")
	}
	return nil
}

func crashNativeHelper() error {
	output, err := process.NewCommand(context.Background(), "", "/usr/bin/sudo", "-n", "/bin/launchctl", "kill", "SIGKILL", "system/"+launchDaemonLabel).CombinedOutput()
	if err != nil {
		return fmt.Errorf("crash only the owned launchd helper: %w: %s", err, output)
	}
	return nil
}
