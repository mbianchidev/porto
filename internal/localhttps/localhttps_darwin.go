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

type Status struct {
	Installed bool `json:"installed"`
	Listening bool `json:"listening"`
	Trusted   bool `json:"trusted"`
}

func Install(certificatePath, authorityPath string) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve Porto executable: %w", err)
	}
	if err := privilegedAction("install-root", executable); err != nil {
		return err
	}
	if err := Trust(authorityPath); err != nil {
		return fmt.Errorf("portless HTTPS installed, but trust the certificate authority: %w", err)
	}
	markerPath, err := config.PortlessHTTPSMarkerPath()
	if err != nil {
		return err
	}
	if err := os.WriteFile(markerPath, nil, 0o644); err != nil {
		return fmt.Errorf("write portless HTTPS marker: %w", err)
	}
	if !Trusted(certificatePath) {
		return errors.New("certificate authority was added, but macOS does not trust the Porto certificate yet")
	}
	return nil
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
	return Status{
		Installed: statErr == nil,
		Listening: listening,
		Trusted:   Trusted(certificatePath),
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

func RootInstall(sourceExecutable string) error {
	if os.Geteuid() != 0 {
		return errors.New("HTTPS forwarder installation must run as root")
	}
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
	return nil
}

func RootUninstall() error {
	if os.Geteuid() != 0 {
		return errors.New("HTTPS forwarder removal must run as root")
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
