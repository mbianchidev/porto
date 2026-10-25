//go:build darwin

package localhttps

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mbianchidev/porto/internal/config"
)

const (
	launchDaemonLabel = "dev.porto.https-forwarder"
	installDirectory  = "/Library/Application Support/Porto"
	installedBinary   = installDirectory + "/porto-https-forwarder"
	launchDaemonPath  = "/Library/LaunchDaemons/" + launchDaemonLabel + ".plist"
)

func Install(certificatePath, authorityPath string) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve Porto executable: %w", err)
	}
	prior := Snapshot(certificatePath)
	return installWithRollback(installSteps{
		installed: prior.Installed, trusted: prior.Trusted,
		install: func() error { return privilegedAction("install-root", executable) },
		remove:  func() error { return privilegedAction("uninstall-root") },
		trust:   func() error { return Trust(authorityPath) },
		untrust: func() error { return Untrust(authorityPath) },
		verify:  func() bool { return Trusted(certificatePath) },
		mark:    markInstalled,
	})
}

func Uninstall() error {
	if err := privilegedAction("uninstall-root"); err != nil {
		return err
	}
	markerPath, err := config.PortlessHTTPSMarkerPath()
	if err != nil {
		return err
	}
	if err := os.Remove(markerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove portless HTTPS marker: %w", err)
	}
	return nil
}

func Snapshot(certificatePath string) Status {
	listening := true
	for _, address := range []string{ListenAddress, ListenAddressIPv6} {
		connection, err := net.DialTimeout("tcp", address, 300*time.Millisecond)
		if err != nil {
			listening = false
			continue
		}
		_ = connection.Close()
	}
	_, statErr := os.Stat(launchDaemonPath)
	trusted := Trusted(certificatePath)
	return Status{
		Installed:      statErr == nil,
		Listening:      listening,
		Trusted:        trusted,
		Supported:      true,
		BrowserTrusted: trusted,
	}
}

func Trust(authorityPath string) error {
	keychain, err := loginKeychain()
	if err != nil {
		return err
	}
	return interactiveCommand(
		"/usr/bin/security",
		"add-trusted-cert",
		"-r", "trustRoot",
		"-p", "ssl",
		"-k", keychain,
		authorityPath,
	)
}

func Untrust(authorityPath string) error {
	return interactiveCommand("/usr/bin/security", "remove-trusted-cert", authorityPath)
}

func Trusted(certificatePath string) bool {
	command := exec.Command(
		"/usr/bin/security",
		"verify-cert",
		"-c", certificatePath,
		"-p", "ssl",
		"-s", config.LocalhostDomain,
	)
	return command.Run() == nil
}

func RootInstall(sourceExecutable string, _ ...string) (err error) {
	if os.Geteuid() != 0 {
		return errors.New("HTTPS forwarder installation must run as root")
	}
	fresh := false
	if _, err := os.Lstat(launchDaemonPath); errors.Is(err, os.ErrNotExist) {
		fresh = true
		if err := checkPortsAvailable(); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if data, err := os.ReadFile(launchDaemonPath); err != nil || string(data) != launchDaemonPlist() {
		return errors.New("the existing HTTPS launchd service is not the Porto-owned configuration; no service was replaced")
	}
	defer func() {
		if fresh && err != nil {
			err = errors.Join(err, RootUninstall())
		}
	}()
	if err := ensureSecureInstallDirectory(); err != nil {
		return err
	}
	if err := copyExecutable(sourceExecutable, installedBinary); err != nil {
		return err
	}
	if err := writeRootFile(launchDaemonPath, []byte(launchDaemonPlist()), 0o644); err != nil {
		return err
	}
	if err := stopLaunchDaemon(); err != nil {
		return err
	}
	if output, err := exec.Command("/bin/launchctl", "bootstrap", "system", launchDaemonPath).CombinedOutput(); err != nil {
		return fmt.Errorf("start HTTPS forwarder: %w: %s", err, output)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !listenersReady() {
		if time.Now().After(deadline) {
			return errors.New("Porto launchd helper did not bind both loopbacks")
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

func RootUninstall() error {
	if os.Geteuid() != 0 {
		return errors.New("HTTPS forwarder removal must run as root")
	}
	if data, err := os.ReadFile(launchDaemonPath); err == nil && string(data) != launchDaemonPlist() {
		return errors.New("the HTTPS launchd configuration is not owned by Porto; no service was removed")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := stopLaunchDaemon(); err != nil {
		return err
	}
	for _, path := range []string{launchDaemonPath, installedBinary} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}
	return nil
}

func stopLaunchDaemon() error {
	domainTarget := "system/" + launchDaemonLabel
	if output, err := exec.Command("/bin/launchctl", "print", domainTarget).CombinedOutput(); err != nil {
		if strings.Contains(string(output), "Could not find service") || strings.Contains(string(output), "Bad request") {
			return nil
		}
		return fmt.Errorf("inspect HTTPS forwarder: %w: %s", err, output)
	}
	if output, err := exec.Command("/bin/launchctl", "bootout", domainTarget).CombinedOutput(); err != nil {
		return fmt.Errorf("stop HTTPS forwarder: %w: %s", err, output)
	}
	for range 20 {
		if err := exec.Command("/bin/launchctl", "print", domainTarget).Run(); err != nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("HTTPS forwarder is still running after launchd bootout")
}

func privilegedAction(action string, args ...string) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve Porto executable: %w", err)
	}
	commandArgs := append([]string{executable, "https", action}, args...)
	script := `on run argv
set commandText to quoted form of item 1 of argv
repeat with argumentIndex from 2 to count argv
set commandText to commandText & " " & quoted form of item argumentIndex of argv
end repeat
do shell script commandText with administrator privileges
end run`
	osascriptArgs := append([]string{"-e", script}, commandArgs...)
	return interactiveCommand("/usr/bin/osascript", osascriptArgs...)
}

func interactiveCommand(name string, args ...string) error {
	command := exec.Command(name, args...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("%s failed: %w", filepath.Base(name), err)
	}
	return nil
}

func loginKeychain() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, "Library", "Keychains", "login.keychain-db"), nil
}

func ensureSecureInstallDirectory() error {
	if err := os.MkdirAll(installDirectory, 0o755); err != nil {
		return fmt.Errorf("create HTTPS forwarder directory: %w", err)
	}
	if err := os.Chown(installDirectory, 0, 0); err != nil {
		return fmt.Errorf("own HTTPS forwarder directory: %w", err)
	}
	if err := os.Chmod(installDirectory, 0o755); err != nil {
		return fmt.Errorf("protect HTTPS forwarder directory: %w", err)
	}
	info, err := os.Lstat(installDirectory)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || info.Mode()&0o022 != 0 || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("HTTPS forwarder directory must be root-owned and not group/world writable")
	}
	return nil
}

func copyExecutable(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open Porto executable: %w", err)
	}
	defer input.Close()
	temp, err := os.CreateTemp(filepath.Dir(destination), ".porto-https-forwarder.*")
	if err != nil {
		return fmt.Errorf("create HTTPS forwarder: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err := io.Copy(temp, input); err != nil {
		_ = temp.Close()
		return fmt.Errorf("copy HTTPS forwarder: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync HTTPS forwarder: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close HTTPS forwarder: %w", err)
	}
	if err := os.Chown(tempPath, 0, 0); err != nil {
		return fmt.Errorf("own HTTPS forwarder: %w", err)
	}
	if err := os.Chmod(tempPath, 0o755); err != nil {
		return fmt.Errorf("protect HTTPS forwarder: %w", err)
	}
	if err := os.Rename(tempPath, destination); err != nil {
		return fmt.Errorf("install HTTPS forwarder: %w", err)
	}
	return nil
}

func writeRootFile(path string, contents []byte, mode os.FileMode) error {
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err := temp.Write(contents); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Chown(tempPath, 0, 0); err != nil {
		return err
	}
	if err := os.Chmod(tempPath, mode); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func launchDaemonPlist() string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>` + launchDaemonLabel + `</string>
  <key>ProgramArguments</key>
  <array>
    <string>` + installedBinary + `</string>
    <string>https-forwarder</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ProcessType</key>
  <string>Background</string>
  <key>ThrottleInterval</key>
  <integer>5</integer>
</dict>
</plist>
`
}

func Run(ctx context.Context) error {
	return RunForwarders(ctx, []string{ListenAddress, ListenAddressIPv6}, TargetAddress)
}
