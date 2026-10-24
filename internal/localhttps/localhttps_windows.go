//go:build windows

package localhttps

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mbianchidev/porto/internal/process"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	windowsServiceName  = "PortoHTTPS"
	windowsServiceOwner = "Porto loopback-only HTTPS forwarder v1"
)

func windowsBinary() (string, error) {
	root, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	if err != nil {
		return "", fmt.Errorf("resolve protected HTTPS installation directory: %w", err)
	}
	return filepath.Join(root, "PortoHTTPS", "porto-https-forwarder.exe"), nil
}

func openHTTPSService(readOnly bool) (*mgr.Service, error) {
	var manager *mgr.Mgr
	if readOnly {
		handle, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
		if err != nil {
			return nil, err
		}
		manager = &mgr.Mgr{Handle: handle}
	} else {
		var err error
		manager, err = mgr.Connect()
		if err != nil {
			return nil, err
		}
	}
	defer manager.Disconnect()
	name, err := windows.UTF16PtrFromString(windowsServiceName)
	if err != nil {
		return nil, err
	}
	access := uint32(windows.SERVICE_ALL_ACCESS)
	if readOnly {
		access = windows.SERVICE_QUERY_CONFIG | windows.SERVICE_QUERY_STATUS
	}
	handle, err := windows.OpenService(manager.Handle, name, access)
	if err != nil {
		return nil, err
	}
	return &mgr.Service{Name: windowsServiceName, Handle: handle}, nil
}

func requireOwnedService(service *mgr.Service) error {
	configuration, err := service.Config()
	if err != nil {
		return err
	}
	binary, err := windowsBinary()
	if err != nil {
		return err
	}
	if configuration.Description != windowsServiceOwner ||
		!strings.EqualFold(configuration.BinaryPathName, syscall.EscapeArg(binary)+" https-forwarder") ||
		!strings.EqualFold(configuration.ServiceStartName, `NT AUTHORITY\LocalService`) {
		return errors.New("PortoHTTPS service is not owned by this integration; refusing to change it")
	}
	return nil
}

func Install(certificatePath, authorityPath string) error {
	if _, _, err := readAuthority(authorityPath); err != nil {
		return err
	}
	before := Snapshot(certificatePath)
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	return installWithRollback(installSteps{
		installed: before.Installed, trusted: before.Trusted,
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
	return removeMarker()
}

func Snapshot(certificatePath string) Status {
	status := Status{Supported: true, Trusted: Trusted(certificatePath)}
	service, err := openHTTPSService(true)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return status
	}
	if err != nil {
		status.Message = fmt.Sprintf("inspect HTTPS service: %v", err)
		return status
	}
	defer service.Close()
	if err := requireOwnedService(service); err != nil {
		status.Message = err.Error()
		return status
	}
	status.Installed = true
	state, err := service.Query()
	if err != nil {
		status.Message = fmt.Sprintf("inspect HTTPS helper state: %v", err)
		return status
	}
	status.Listening = state.State == svc.Running && listenersReady()
	if !status.Listening {
		status.Message = fmt.Sprintf("HTTPS helper is not listening (state %d, exit %d); check port 443 and rerun 'porto https install'", state.State, state.Win32ExitCode)
	}
	return status
}

func powershell(script string) error {
	command := process.NewCommand(context.Background(), "", "powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", "$ErrorActionPreference='Stop'; "+script)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("Windows HTTPS integration failed: %w: %s", err, output)
	}
	return nil
}

func quotePowerShell(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func Trust(path string) error {
	if _, _, err := readAuthority(path); err != nil {
		return err
	}
	return powershell("Import-Certificate -FilePath " + quotePowerShell(path) + " -CertStoreLocation Cert:\\CurrentUser\\Root | Out-Null")
}

func Untrust(path string) error {
	data, _, err := readAuthority(path)
	if err != nil {
		return err
	}
	block, _ := pem.Decode(data)
	thumbprint := sha1.Sum(block.Bytes)
	// Windows indexes certificate stores by the native SHA-1 thumbprint, not by subject name.
	storePath := `Cert:\CurrentUser\Root\` + strings.ToUpper(hex.EncodeToString(thumbprint[:]))
	return powershell("if (Test-Path -LiteralPath " + quotePowerShell(storePath) + ") { Remove-Item -LiteralPath " + quotePowerShell(storePath) + " }")
}

func Trusted(path string) bool { return verifySystemTrust(path) }

func privilegedAction(action string, args ...string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	arguments := append([]string{"https", action}, args...)
	if windows.GetCurrentProcessToken().IsElevated() {
		command := process.NewCommand(context.Background(), "", executable, arguments...)
		output, err := command.CombinedOutput()
		if err != nil {
			return fmt.Errorf("HTTPS helper %s: %w: %s", action, err, output)
		}
		return nil
	}
	quoted := make([]string, len(arguments))
	for index, argument := range arguments {
		quoted[index] = syscall.EscapeArg(argument)
	}
	return powershell("$p = Start-Process -FilePath " + quotePowerShell(executable) +
		" -ArgumentList " + quotePowerShell(strings.Join(quoted, " ")) +
		" -Verb RunAs -Wait -PassThru; if ($p.ExitCode -ne 0) { throw ('HTTPS authorization or helper failed with exit ' + $p.ExitCode) }")
}

func stopHTTPSService(service *mgr.Service) error {
	state, err := service.Query()
	if err != nil {
		return err
	}
	if state.State == svc.Stopped {
		return nil
	}
	if _, err := service.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
		return fmt.Errorf("stop HTTPS helper: %w", err)
	}
	for range 100 {
		state, err := service.Query()
		if err != nil {
			return err
		}
		if state.State == svc.Stopped {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("HTTPS helper did not stop within 10 seconds; no files were removed")
}

func RootInstall(source string, _ ...string) (err error) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("install the HTTPS service through 'porto https install' and approve its administrator prompt")
	}
	binary, err := windowsBinary()
	if err != nil {
		return err
	}
	service, err := openHTTPSService(false)
	existing := err == nil
	if err != nil && !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return err
	}
	defer func() {
		if service != nil {
			_ = service.Close()
		}
	}()
	var previous []byte
	if existing {
		if err := requireOwnedService(service); err != nil {
			_ = service.Close()
			return err
		}
		previous, err = os.ReadFile(binary)
		if err != nil {
			return fmt.Errorf("read installed HTTPS helper before repair: %w", err)
		}
		if err := stopHTTPSService(service); err != nil {
			return err
		}
	}
	if err := checkPortsAvailable(); err != nil {
		if existing {
			return errors.Join(err, service.Start())
		}
		return err
	}
	directory := filepath.Dir(binary)
	if info, err := os.Lstat(directory); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("HTTPS helper directory is not a regular directory")
		}
		if !existing {
			return errors.New("HTTPS helper directory exists without an owned service; inspect it before retrying")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else if err := os.Mkdir(directory, 0o755); err != nil {
		return err
	}
	acl := process.NewCommand(context.Background(), "", "icacls.exe", directory, "/inheritance:r",
		"/grant:r", "*S-1-5-18:(OI)(CI)F", "*S-1-5-32-544:(OI)(CI)F", "*S-1-5-19:(OI)(CI)RX",
		"/setowner", "*S-1-5-32-544")
	if output, err := acl.CombinedOutput(); err != nil {
		return fmt.Errorf("protect HTTPS helper directory: %w: %s", err, output)
	}
	defer func() {
		if err == nil {
			return
		}
		if service != nil {
			err = errors.Join(err, stopHTTPSService(service))
		}
		if existing {
			err = errors.Join(err, writeIntegrationFile(binary, previous, 0o755), service.Start())
		} else {
			if service != nil {
				err = errors.Join(err, service.Delete())
			}
			if removeErr := os.Remove(binary); !errors.Is(removeErr, os.ErrNotExist) {
				err = errors.Join(err, removeErr)
			}
			if removeErr := os.Remove(directory); !errors.Is(removeErr, os.ErrNotExist) {
				err = errors.Join(err, removeErr)
			}
		}
	}()
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(input)
	closeErr := input.Close()
	if err := errors.Join(err, closeErr); err != nil {
		return err
	}
	if err := writeIntegrationFile(binary, data, 0o755); err != nil {
		return err
	}
	if !existing {
		manager, err := mgr.Connect()
		if err != nil {
			return err
		}
		defer manager.Disconnect()
		service, err = manager.CreateService(windowsServiceName, binary, mgr.Config{
			StartType: mgr.StartAutomatic, ServiceStartName: `NT AUTHORITY\LocalService`,
			DisplayName: "Porto loopback HTTPS", Description: windowsServiceOwner,
		}, "https-forwarder")
		if err != nil {
			return err
		}
	}
	if err := service.SetRecoveryActions([]mgr.RecoveryAction{{Type: mgr.ServiceRestart, Delay: 2 * time.Second}}, 60); err != nil {
		return err
	}
	if err := service.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return err
	}
	if err := service.Start(); err != nil {
		return err
	}
	for range 100 {
		state, err := service.Query()
		if err != nil {
			return err
		}
		if state.State == svc.Running && listenersReady() {
			return nil
		}
		if state.State == svc.Stopped {
			return fmt.Errorf("HTTPS helper exited (%d); check port 443 and IPv6 loopback", state.Win32ExitCode)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("HTTPS helper did not start within 10 seconds")
}

func RootUninstall() error {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("HTTPS service removal requires administrator authorization")
	}
	service, err := openHTTPSService(false)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil
	}
	if err != nil {
		return err
	}
	defer service.Close()
	if err := requireOwnedService(service); err != nil {
		return err
	}
	if err := stopHTTPSService(service); err != nil {
		return err
	}
	if err := service.Delete(); err != nil {
		return err
	}
	binary, err := windowsBinary()
	if err != nil {
		return err
	}
	if err := os.Remove(binary); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Remove(filepath.Dir(binary))
}

type windowsHTTPSService struct{}

func (windowsHTTPSService) Execute(_ []string, requests <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changes <- svc.Status{State: svc.StartPending}
	done := make(chan error, 1)
	go func() { done <- RunForwarders(ctx, []string{ListenAddress, ListenAddressIPv6}, TargetAddress) }()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	running := false
	for {
		select {
		case err := <-done:
			if err != nil {
				slog.Error("HTTPS helper stopped", "error", err)
				return true, 1
			}
			return false, 0
		case <-ticker.C:
			if !running && listenersReady() {
				running = true
				changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
			}
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				changes <- request.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				cancel()
			}
		}
	}
}

func Run(ctx context.Context) error {
	isService, err := svc.IsWindowsService()
	if err != nil {
		return err
	}
	if isService {
		return svc.Run(windowsServiceName, windowsHTTPSService{})
	}
	return RunForwarders(ctx, []string{ListenAddress, ListenAddressIPv6}, TargetAddress)
}
