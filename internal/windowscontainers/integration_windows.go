//go:build windows

package windowscontainers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"debug/pe"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"github.com/mbianchidev/porto/internal/process"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const serviceOwner = "Porto private native Windows containerd service v1"

var runtimeFiles = []string{"containerd.exe", "containerd-shim-runhcs-v1.exe", "nerdctl.exe",
	"nat.exe", "sdnbridge.exe", "sdnoverlay.exe"}

func currentSID() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return user.User.Sid.String(), nil
}

func installationFor(sid string) (Installation, error) {
	if _, err := windows.StringToSid(sid); err != nil || !strings.HasPrefix(sid, "S-1-5-21-") {
		return Installation{}, fmt.Errorf("invalid native runtime owner SID %q", sid)
	}
	root, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	if err != nil {
		return Installation{}, err
	}
	return Installation{Version: 1, OwnerSID: sid,
		Root: filepath.Join(root, "PortoWindowsContainers-"+OwnerKey(sid))}, nil
}

func Current() (Installation, error) {
	sid, err := currentSID()
	if err != nil {
		return Installation{}, err
	}
	return installationFor(sid)
}

func readInstallation(expected Installation) (Installation, error) {
	if err := protectedTree(expected.Root); err != nil {
		return Installation{}, err
	}
	data, err := os.ReadFile(filepath.Join(expected.Root, "installation.json"))
	if err != nil {
		return Installation{}, err
	}
	var installed Installation
	if err := json.Unmarshal(data, &installed); err != nil {
		return Installation{}, fmt.Errorf("%w: decode installation: %v", ErrOwnership, err)
	}
	if installed.Version != 1 || installed.OwnerSID != expected.OwnerSID ||
		!strings.EqualFold(installed.Root, expected.Root) || len(installed.Generation) != 32 ||
		strings.Trim(installed.Generation, "0123456789abcdef") != "" {
		return Installation{}, fmt.Errorf("%w: unexpected installation identity", ErrOwnership)
	}
	return installed, nil
}

func openService(i Installation, readOnly bool) (*mgr.Service, error) {
	handle, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return nil, err
	}
	defer windows.CloseServiceHandle(handle)
	name, err := windows.UTF16PtrFromString(i.ServiceName())
	if err != nil {
		return nil, err
	}
	access := uint32(windows.SERVICE_ALL_ACCESS)
	if readOnly {
		access = windows.SERVICE_QUERY_CONFIG | windows.SERVICE_QUERY_STATUS | windows.READ_CONTROL
	}
	service, err := windows.OpenService(handle, name, access)
	if err != nil {
		return nil, err
	}
	return &mgr.Service{Name: i.ServiceName(), Handle: service}, nil
}

func serviceCommand(i Installation) string {
	return syscall.EscapeArg(i.Binary()) + " windows-containers-worker " + syscall.EscapeArg(i.OwnerSID)
}

func requireOwnedService(service *mgr.Service, i Installation) error {
	configuration, err := service.Config()
	if err != nil {
		return err
	}
	if configuration.Description != serviceOwner ||
		!strings.EqualFold(configuration.BinaryPathName, serviceCommand(i)) ||
		!strings.EqualFold(configuration.ServiceStartName, "LocalSystem") {
		return fmt.Errorf("%w: service identity, executable, arguments, or account differs; refusing to adopt or stop it", ErrOwnership)
	}
	if err := requireServiceACL(service); err != nil {
		return err
	}
	return verifyDeployment(i)
}

func protectService(service *mgr.Service, i Installation) error {
	descriptor, err := windows.SecurityDescriptorFromString("O:BAG:BAD:P(A;;0xF01FF;;;SY)(A;;0xF01FF;;;BA)(A;;0x2008D;;;" + i.OwnerSID + ")")
	if err != nil {
		return err
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return err
	}
	group, _, err := descriptor.Group()
	if err != nil {
		return err
	}
	acl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetSecurityInfo(service.Handle, windows.SE_SERVICE,
		windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		owner, group, acl, nil)
}

func requireServiceACL(service *mgr.Service) error {
	descriptor, err := windows.GetSecurityInfo(service.Handle, windows.SE_SERVICE, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || (owner.String() != "S-1-5-18" && owner.String() != "S-1-5-32-544") {
		return fmt.Errorf("%w: native service is not administrator-owned", ErrOwnership)
	}
	acl, _, err := descriptor.DACL()
	if err != nil || acl == nil {
		return fmt.Errorf("%w: native service has no protected ACL", ErrOwnership)
	}
	const unsafeRights = windows.SERVICE_CHANGE_CONFIG | windows.SERVICE_START | windows.SERVICE_STOP | windows.SERVICE_PAUSE_CONTINUE |
		windows.SERVICE_USER_DEFINED_CONTROL | windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER | windows.GENERIC_WRITE | windows.GENERIC_ALL
	for n := uint16(0); n < acl.AceCount; n++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, uint32(n), &ace); err != nil {
			return err
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return fmt.Errorf("%w: unexpected native service ACL entry", ErrOwnership)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		if sid != "S-1-5-18" && sid != "S-1-5-32-544" && ace.Mask&unsafeRights != 0 {
			return fmt.Errorf("%w: native service is writable or controllable by %s without elevation", ErrOwnership, sid)
		}
	}
	return nil
}

func Snapshot(ctx context.Context) Status {
	host, probeErr := Probe(ctx)
	if probeErr != nil {
		host.Problem = probeErr.Error()
	}
	status := statusForHost(host)
	expected, err := Current()
	if err != nil {
		status.Message = err.Error()
		return status
	}
	status.Endpoint = expected.Endpoint()
	service, err := openService(expected, true)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return status
	}
	status.Installed = true
	if err != nil {
		status.Message = "Inspect native Windows service: " + err.Error()
		return status
	}
	defer service.Close()
	installed, err := readInstallation(expected)
	if err == nil {
		err = requireOwnedService(service, installed)
	}
	if err != nil {
		status.Message = err.Error()
		return status
	}
	status.Owned = true
	state, err := service.Query()
	if err != nil {
		status.Message = err.Error()
		return status
	}
	if state.State == svc.Running {
		probeContext, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		request, _ := http.NewRequestWithContext(probeContext, http.MethodGet, "http://localhost/api/worker/status", nil)
		client := ClientFor(installed)
		defer client.CloseIdleConnections()
		response, err := client.Do(request)
		if err == nil {
			defer response.Body.Close()
			var live Status
			if response.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&live) == nil &&
				live.Owned && live.Context == ContextName && live.Endpoint == installed.Endpoint() {
				return live
			}
		}
		status.Message = "Native Windows service is starting or unavailable; inspect its protected service.log and retry start"
	} else {
		status.Message = fmt.Sprintf("Native Windows service is stopped (state %d, exit %d); use 'porto docker windows start'", state.State, state.Win32ExitCode)
	}
	return status
}

func Action(ctx context.Context, action string) error {
	if action == "context" {
		return installContext(ctx)
	}
	switch action {
	case "install", "start", "stop", "remove":
	default:
		return errors.New("native Windows action must be install, start, stop, remove, or context")
	}
	i, err := Current()
	if err != nil {
		return err
	}
	host, probeErr := Probe(ctx)
	if !host.PotentiallySupported() && (action == "install" || action == "start") {
		return errors.Join(ErrUnsupported, probeErr, host.Validate())
	}
	source, err := os.Executable()
	if err != nil {
		return err
	}
	bundle := os.Getenv(RuntimeDirectoryEnv)
	if bundle == "" {
		bundle = filepath.Join(filepath.Dir(source), "runtime", "windows-containers", "amd64", "bin")
	}
	if bundle, err = filepath.Abs(bundle); err != nil {
		return err
	}
	arguments := []string{"windows-containers-elevated", action, i.OwnerSID, source, bundle}
	if windows.GetCurrentProcessToken().IsElevated() {
		return RootAction(ctx, action, i.OwnerSID, source, bundle)
	}
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return err
	}
	quoted := make([]string, len(arguments))
	for n, argument := range arguments {
		quoted[n] = syscall.EscapeArg(argument)
	}
	psQuote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
	script := "$ErrorActionPreference='Stop'; $p=Start-Process -FilePath " + psQuote(source) +
		" -ArgumentList " + psQuote(strings.Join(quoted, " ")) +
		" -Verb RunAs -Wait -PassThru; if($p.ExitCode -ne 0){throw ('Native Windows authorization/setup failed, exit '+$p.ExitCode+'; inspect the Windows host capabilities and Porto service.log')}"
	command := process.NewCommand(ctx, "", filepath.Join(system, "WindowsPowerShell", "v1.0", "powershell.exe"),
		"-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script)
	command.Env = systemEnvironment(i)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("explicitly elevated native Windows %s: %w: %s", action, err, output)
	}
	return nil
}

func RootAction(ctx context.Context, action, sid, source, bundle string) error {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("native Windows service changes require explicit administrator authorization")
	}
	i, err := installationFor(sid)
	if err != nil {
		return err
	}
	if action == "install" || action == "start" {
		host, err := Probe(ctx)
		if err != nil {
			return err
		}
		if err := host.Validate(); err != nil {
			return err
		}
		i.Host = host
	}
	unlock, err := installationLock(i)
	if err != nil {
		return err
	}
	defer unlock()
	if action == "install" {
		return install(ctx, i, source, bundle)
	}
	installed, err := readInstallation(i)
	if err != nil {
		return err
	}
	service, err := openService(i, false)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) && action == "remove" {
		return nil
	}
	if err != nil {
		return err
	}
	defer service.Close()
	if err := requireOwnedService(service, installed); err != nil {
		return err
	}
	switch action {
	case "start":
		installed.Host = i.Host
		if err := writeJSON(filepath.Join(i.Root, "installation.json"), installed); err != nil {
			return err
		}
		return startService(ctx, service, installed)
	case "stop", "remove":
		if err := stopService(ctx, service, installed); err != nil {
			return err
		}
		if action == "remove" {
			// Only unregister the owned supervisor. All images, containers,
			// network receipts and installed generations remain recoverable.
			return service.Delete()
		}
		return nil
	default:
		return errors.New("invalid elevated native Windows action")
	}
}

func installationLock(i Installation) (func(), error) {
	runtime.LockOSThread()
	locked := false
	defer func() {
		if !locked {
			runtime.UnlockOSThread()
		}
	}()
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;SY)(A;;GA;;;BA)")
	if err != nil {
		return nil, err
	}
	name, _ := windows.UTF16PtrFromString(`Global\` + i.ServiceName() + "-setup")
	handle, err := windows.CreateMutex(&windows.SecurityAttributes{
		Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd,
	}, false, name)
	if err != nil {
		return nil, fmt.Errorf("lock native Windows setup: %w", err)
	}
	state, err := windows.WaitForSingleObject(handle, 30_000)
	if err != nil || (state != windows.WAIT_OBJECT_0 && state != windows.WAIT_ABANDONED) {
		windows.CloseHandle(handle)
		return nil, fmt.Errorf("another native Windows setup is active: wait=%d: %v", state, err)
	}
	locked = true
	return func() { _ = windows.ReleaseMutex(handle); _ = windows.CloseHandle(handle); runtime.UnlockOSThread() }, nil
}

func install(ctx context.Context, i Installation, source, bundle string) (err error) {
	for _, path := range append([]string{source}, runtimeSourcePaths(bundle)...) {
		if err := validateAMD64Executable(path); err != nil {
			return err
		}
	}
	service, serviceErr := openService(i, false)
	existing := serviceErr == nil
	if serviceErr != nil && !errors.Is(serviceErr, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return serviceErr
	}
	var previous Installation
	wasRunning := false
	createdRoot := false
	defer func() {
		if err == nil {
			return
		}
		if createdRoot {
			if _, statErr := os.Stat(filepath.Join(i.Root, "installation.json")); errors.Is(statErr, os.ErrNotExist) {
				if protectErr := protectedTree(i.Root); protectErr == nil {
					err = errors.Join(err, os.RemoveAll(i.Root))
				}
				return
			}
		}
		if i.Generation != "" {
			live, readErr := readInstallation(i)
			if readErr == nil && live.Generation != i.Generation {
				inUse := false
				check, serviceErr := openService(i, true)
				if serviceErr == nil {
					configuration, configErr := check.Config()
					inUse = configErr != nil || strings.EqualFold(configuration.BinaryPathName, serviceCommand(i))
					check.Close()
				} else if !errors.Is(serviceErr, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
					inUse = true
				}
				if !inUse {
					err = errors.Join(err, os.RemoveAll(i.Deployment()))
				}
			}
		}
	}()
	if existing {
		defer service.Close()
		previous, err = readInstallation(i)
		if err != nil {
			return err
		}
		if err := requireOwnedService(service, previous); err != nil {
			return err
		}
		state, err := service.Query()
		if err != nil {
			return err
		}
		wasRunning = state.State == svc.Running
		if err := assertIdle(ctx, previous, state.State == svc.Stopped); err != nil {
			return err
		}
	}
	if !existing {
		if err := requireAbsentPipe(i.ContainerdPipe()); err != nil {
			return err
		}
		if err := CheckNetworks(i); err != nil {
			return err
		}
		if err := requireAbsentPipe(i.Pipe()); err != nil {
			return err
		}
	}
	if _, statErr := os.Lstat(i.Root); statErr == nil {
		saved, err := readInstallation(i)
		if err != nil {
			return fmt.Errorf("%w: existing native runtime directory cannot be adopted: %v", ErrOwnership, err)
		}
		if !existing {
			if err := assertIdle(ctx, saved, true); err != nil {
				return err
			}
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	} else if err := createProtectedRoot(i); err != nil {
		return err
	} else {
		createdRoot = true
	}
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return err
	}
	i.Generation = hex.EncodeToString(bytes)
	for _, directory := range []string{filepath.Join(i.Root, "versions"), i.Deployment(), i.Bin(),
		i.Data(), filepath.Join(i.Data(), "containerd"), filepath.Join(i.Data(), "state"),
		filepath.Join(i.Data(), "nerdctl"), filepath.Join(i.Data(), "cni"), filepath.Join(i.Data(), "certs.d")} {
		if err := makeProtectedDirectory(directory); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		if err := protectedTree(directory); err != nil {
			return err
		}
	}
	i.Hashes = make(map[string]string)
	for _, pair := range append([][2]string{{source, i.Binary()}}, runtimeCopyPaths(bundle, i.Bin())...) {
		hash, err := copyExecutable(pair[0], pair[1])
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(i.Deployment(), pair[1])
		i.Hashes[relative] = hash
	}
	if err := writeRuntimeConfiguration(i); err != nil {
		return err
	}
	for _, name := range []string{"containerd.toml", "nerdctl.toml"} {
		data, err := os.ReadFile(filepath.Join(i.Deployment(), name))
		if err != nil {
			return err
		}
		hash := sha256.Sum256(data)
		i.Hashes[name] = hex.EncodeToString(hash[:])
	}
	changed := false
	stoppedExisting := false
	defer func() {
		if err == nil {
			return
		}
		rollbackContext, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if stoppedExisting && !changed {
			err = errors.Join(err, writeJSON(filepath.Join(i.Root, "installation.json"), previous),
				writeJSON(filepath.Join(i.Root, "clean-stop.json"), previous.Generation))
			if wasRunning {
				err = errors.Join(err, startService(rollbackContext, service, previous))
			}
			return
		}
		if changed && service != nil {
			rollbackErr := stopService(rollbackContext, service, i)
			if rollbackErr != nil {
				state, queryErr := service.Query()
				_, admissionErr := os.Stat(filepath.Join(i.Deployment(), "admitted.json"))
				// Setup proved the old engine idle before this attempt, and
				// the worker serves no mutations before writing admission.
				if queryErr == nil && state.State == svc.Stopped && errors.Is(admissionErr, os.ErrNotExist) {
					if markErr := writeJSON(filepath.Join(i.Root, "clean-stop.json"), i.Generation); markErr == nil {
						rollbackErr = stopService(rollbackContext, service, i)
					}
				}
			}
			if rollbackErr != nil {
				err = errors.Join(err, rollbackErr)
				return
			}
			if existing {
				err = errors.Join(err, writeJSON(filepath.Join(i.Root, "installation.json"), previous))
				err = errors.Join(err, writeJSON(filepath.Join(i.Root, "clean-stop.json"), previous.Generation))
				err = errors.Join(err, setServiceCommand(service, previous))
				if wasRunning {
					err = errors.Join(err, startService(rollbackContext, service, previous))
				}
			} else {
				err = errors.Join(err, service.Delete())
			}
		}
	}()
	if existing {
		if err := stopService(ctx, service, previous); err != nil {
			return err
		}
		stoppedExisting = true
	}
	if err := writeJSON(filepath.Join(i.Root, "installation.json"), i); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(i.Root, "clean-stop.json"), i.Generation); err != nil {
		return err
	}
	if existing {
		if err := setServiceCommand(service, i); err != nil {
			_ = writeJSON(filepath.Join(i.Root, "installation.json"), previous)
			return err
		}
	} else {
		manager, err := mgr.Connect()
		if err != nil {
			return err
		}
		defer manager.Disconnect()
		service, err = manager.CreateService(i.ServiceName(), i.Binary(), mgr.Config{
			StartType: mgr.StartManual, ServiceStartName: "LocalSystem",
			DisplayName: "Porto native Windows containers (" + OwnerKey(i.OwnerSID) + ")",
			Description: serviceOwner,
		}, "windows-containers-worker", i.OwnerSID)
		if err != nil {
			return err
		}
		defer service.Close()
	}
	changed = true
	if err := protectService(service, i); err != nil {
		return err
	}
	return startService(ctx, service, i)
}

func setServiceCommand(service *mgr.Service, i Installation) error {
	configuration, err := service.Config()
	if err != nil {
		return err
	}
	configuration.BinaryPathName = serviceCommand(i)
	return service.UpdateConfig(configuration)
}

func runtimeSourcePaths(bundle string) []string {
	paths := make([]string, len(runtimeFiles))
	for n, file := range runtimeFiles {
		paths[n] = filepath.Join(bundle, file)
	}
	return paths
}

func runtimeCopyPaths(source, target string) [][2]string {
	paths := make([][2]string, len(runtimeFiles))
	for n, file := range runtimeFiles {
		paths[n] = [2]string{filepath.Join(source, file), filepath.Join(target, file)}
	}
	return paths
}

func validateAMD64Executable(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("native Windows runtime bundle is incomplete (%s): %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("native Windows runtime binary is not a regular file: %s", path)
	}
	binary, err := pe.Open(path)
	if err != nil {
		return fmt.Errorf("inspect native runtime executable %s: %w", path, err)
	}
	defer binary.Close()
	if binary.Machine != pe.IMAGE_FILE_MACHINE_AMD64 {
		return fmt.Errorf("native Windows runtime requires AMD64 PE binaries, not machine %#x: %s", binary.Machine, path)
	}
	return nil
}

func copyExecutable(source, target string) (string, error) {
	input, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer input.Close()
	output, err := newProtectedFile(target)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(output, hash), input)
	err = errors.Join(copyErr, output.Sync(), output.Close())
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func verifyDeployment(i Installation) error {
	for _, relative := range append([]string{"porto.exe", "containerd.toml", "nerdctl.toml"}, func() []string {
		out := make([]string, len(runtimeFiles))
		for n, file := range runtimeFiles {
			out[n] = filepath.Join("bin", file)
		}
		return out
	}()...) {
		path := filepath.Join(i.Deployment(), relative)
		if err := protectedTree(path); err != nil {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, readErr := io.Copy(hash, file)
		err = errors.Join(readErr, file.Close())
		if err != nil {
			return err
		}
		if i.Hashes[relative] == "" || hex.EncodeToString(hash.Sum(nil)) != i.Hashes[relative] {
			return fmt.Errorf("%w: installed binary hash differs: %s", ErrOwnership, relative)
		}
	}
	return nil
}

func createProtectedRoot(i Installation) error {
	sd, err := windows.SecurityDescriptorFromString("O:BAG:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FRFX;;;" + i.OwnerSID + ")")
	if err != nil {
		return err
	}
	path, _ := windows.UTF16PtrFromString(i.Root)
	return windows.CreateDirectory(path, &windows.SecurityAttributes{
		Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd,
	})
}

func protectedParentDescriptor(path string) (*windows.SECURITY_DESCRIPTOR, error) {
	parent := filepath.Dir(path)
	if err := protectedTree(parent); err != nil {
		return nil, err
	}
	return windows.GetNamedSecurityInfo(parent, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
}

func newProtectedFile(path string) (*os.File, error) {
	descriptor, err := protectedParentDescriptor(path)
	if err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, &windows.SecurityAttributes{
		Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor,
	}, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}

func makeProtectedDirectory(path string) error {
	descriptor, err := protectedParentDescriptor(path)
	if err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.CreateDirectory(name, &windows.SecurityAttributes{
		Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor,
	})
}

func protectedTree(path string) error {
	// Do not follow junctions, including those in an ancestor of a binary.
	for current := path; ; current = filepath.Dir(current) {
		pointer, err := windows.UTF16PtrFromString(current)
		if err != nil {
			return err
		}
		attributes, err := windows.GetFileAttributes(pointer)
		if err != nil {
			return err
		}
		if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return fmt.Errorf("%w: refusing reparse point %s", ErrOwnership, current)
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || (owner.String() != "S-1-5-18" && owner.String() != "S-1-5-32-544") {
		return fmt.Errorf("%w: installation path is not administrator-owned: %s", ErrOwnership, path)
	}
	acl, _, err := descriptor.DACL()
	if err != nil || acl == nil {
		return fmt.Errorf("%w: installation has no protected DACL: %s", ErrOwnership, path)
	}
	const writeMask = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.FILE_WRITE_EA |
		windows.FILE_WRITE_ATTRIBUTES | windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER |
		windows.GENERIC_WRITE | windows.GENERIC_ALL | 0x40 // FILE_DELETE_CHILD
	for n := uint16(0); n < acl.AceCount; n++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, uint32(n), &ace); err != nil {
			return err
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return fmt.Errorf("%w: unexpected installation ACL entry type %d on %s", ErrOwnership, ace.Header.AceType, path)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		if sid != "S-1-5-18" && sid != "S-1-5-32-544" && ace.Mask&writeMask != 0 {
			return fmt.Errorf("%w: installation path is writable by %s: %s", ErrOwnership, sid, path)
		}
	}
	return nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	stage := path + ".new"
	file, err := newProtectedFile(stage)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	err = errors.Join(writeErr, file.Sync(), file.Close())
	if err != nil {
		_ = os.Remove(stage)
		return err
	}
	defer os.Remove(stage)
	source, _ := windows.UTF16PtrFromString(stage)
	target, _ := windows.UTF16PtrFromString(path)
	return windows.MoveFileEx(source, target, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

func startService(ctx context.Context, service *mgr.Service, i Installation) error {
	state, err := service.Query()
	if err != nil {
		return err
	}
	if state.State == svc.Stopped {
		if err := requireAbsentPipe(i.ContainerdPipe()); err != nil {
			return err
		}
		if err := requireAbsentPipe(i.Pipe()); err != nil {
			return err
		}
		if err := service.Start(); err != nil {
			return err
		}
	}
	return waitService(ctx, service, i, true)
}

func stopService(ctx context.Context, service *mgr.Service, i Installation) error {
	state, err := service.Query()
	if err != nil {
		return err
	}
	if err := assertIdle(ctx, i, state.State == svc.Stopped); err != nil {
		return err
	}
	if state.State == svc.Stopped {
		return nil
	}
	if _, err := service.Control(svc.Stop); err != nil {
		return err
	}
	if err := waitService(ctx, service, i, false); err != nil {
		return err
	}
	return assertIdle(ctx, i, true)
}

func waitService(ctx context.Context, service *mgr.Service, i Installation, running bool) error {
	waitContext, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := service.Query()
		if err != nil {
			return err
		}
		if !running && state.State == svc.Stopped {
			return nil
		}
		if running && state.State == svc.Stopped {
			return fmt.Errorf("native Windows service exited (%d); inspect %s", state.Win32ExitCode, filepath.Join(i.Root, "service.log"))
		}
		if running && state.State == svc.Running {
			return nil
		}
		select {
		case <-waitContext.Done():
			return fmt.Errorf("wait for native Windows service: %w", waitContext.Err())
		case <-ticker.C:
		}
	}
}

func assertIdle(ctx context.Context, i Installation, stopped bool) error {
	if stopped {
		data, err := os.ReadFile(filepath.Join(i.Root, "clean-stop.json"))
		var generation string
		if err == nil {
			err = json.Unmarshal(data, &generation)
		}
		if err != nil || generation != i.Generation {
			return errors.New("native runtime did not record a verified idle shutdown; start it to recover and inspect workloads before stopping, updating, or removing")
		}
		return nil
	}
	output, err := runNerdctl(ctx, i, "ps", "--quiet")
	if err != nil {
		return fmt.Errorf("cannot prove native runtime is idle; no service was changed: %w", err)
	}
	if strings.TrimSpace(string(output)) != "" {
		return ErrActive
	}
	// Include paused and restarting workloads, not just nerdctl's running filter.
	output, err = runNerdctl(ctx, i, "ps", "-a", "--format", "{{json .}}")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		var item struct{ State, Status string }
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			return err
		}
		state := strings.ToLower(strings.TrimSpace(item.State))
		status := strings.ToLower(strings.TrimSpace(item.Status))
		idle := state == "exited" || state == "created" || state == "dead"
		if state == "" {
			idle = strings.HasPrefix(status, "exited") || strings.HasPrefix(status, "created") || strings.HasPrefix(status, "dead")
		}
		if !idle {
			return ErrActive
		}
	}
	return nil
}

func systemEnvironment(i Installation) []string {
	system, _ := windows.GetSystemDirectory()
	root := filepath.Dir(system)
	return []string{"SystemRoot=" + root, "WINDIR=" + root, "PATH=" + i.Bin() + ";" + system + ";" + root,
		"SystemDrive=" + filepath.VolumeName(system), "COMSPEC=" + filepath.Join(system, "cmd.exe"), "PATHEXT=.COM;.EXE;.BAT;.CMD",
		"ProgramData=" + filepath.Dir(i.Root), "ProgramFiles=" + filepath.Join(filepath.VolumeName(system)+`\`, "Program Files"),
		"USERPROFILE=" + i.Data(), "APPDATA=" + i.Data(), "LOCALAPPDATA=" + i.Data(),
		"TEMP=" + i.Data(), "TMP=" + i.Data(), "PSModulePath=" + filepath.Join(system, "WindowsPowerShell", "v1.0", "Modules")}
}

func runNerdctl(ctx context.Context, i Installation, args ...string) ([]byte, error) {
	commandContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := process.NewCommand(commandContext, "", filepath.Join(i.Bin(), "nerdctl.exe"), append(i.NerdctlArgs(), args...)...)
	command.Env = systemEnvironment(i)
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("private nerdctl: %w: %s", err, output)
	}
	return output, nil
}

func Dial(ctx context.Context, pipe string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, pipe)
}

func requireAbsentPipe(path string) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED, 0)
	if err == nil {
		_ = windows.CloseHandle(handle)
	}
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return nil
	}
	return fmt.Errorf("%w: private pipe %s already exists or cannot be inspected; no process was adopted or stopped (%v)", ErrOwnership, path, err)
}

func pipeProcess(connection net.Conn, expected uint32) error {
	file, ok := connection.(interface{ Fd() uintptr })
	if !ok {
		return fmt.Errorf("%w: named pipe does not expose its server identity", ErrOwnership)
	}
	var actual uint32
	if err := windows.GetNamedPipeServerProcessId(windows.Handle(file.Fd()), &actual); err != nil {
		return err
	}
	if actual != expected || expected == 0 {
		return fmt.Errorf("%w: pipe server PID %d does not match owned service PID %d", ErrOwnership, actual, expected)
	}
	return nil
}

func dialOwned(ctx context.Context, expected Installation) (net.Conn, error) {
	i, err := readInstallation(expected)
	if err != nil {
		return nil, err
	}
	service, err := openService(i, true)
	if err != nil {
		return nil, err
	}
	defer service.Close()
	if err := requireOwnedService(service, i); err != nil {
		return nil, err
	}
	state, err := service.Query()
	if err != nil {
		return nil, err
	}
	if state.State != svc.Running {
		return nil, errors.New("the separately owned native Windows service is not running")
	}
	connection, err := Dial(ctx, i.Pipe())
	if err != nil {
		return nil, err
	}
	if err := pipeProcess(connection, state.ProcessId); err != nil {
		connection.Close()
		return nil, err
	}
	return connection, nil
}

func Listen(i Installation) (net.Listener, error) {
	return winio.ListenPipe(i.Pipe(), &winio.PipeConfig{
		SecurityDescriptor: "O:SYG:SYD:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" + i.OwnerSID + ")",
		InputBufferSize:    64 * 1024, OutputBufferSize: 64 * 1024,
	})
}

func installContext(ctx context.Context) error {
	status := Snapshot(ctx)
	if !status.Owned || !status.Installed {
		return errors.New("install an owned native Windows runtime before selecting its context")
	}
	run := func(args ...string) ([]byte, error) {
		command := process.NewCommand(ctx, "", "docker", args...)
		command.Env = process.WithEnvironment(os.Environ(), "DOCKER_HOST=", "DOCKER_CONTEXT=")
		output, err := command.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("native Windows Docker context: %w: %s", err, output)
		}
		return output, nil
	}
	output, err := run("context", "inspect", ContextName)
	if err == nil {
		var documents []struct {
			Endpoints struct{ Docker struct{ Host string } }
		}
		if json.Unmarshal(output, &documents) != nil || len(documents) != 1 || documents[0].Endpoints.Docker.Host != status.Endpoint {
			return errors.New("porto-windows context exists with a different endpoint; refusing to overwrite an unrelated context")
		}
	} else {
		// A failed inspect is not evidence that the name is free.
		list, listErr := run("context", "ls", "--format", "{{.Name}}")
		if listErr != nil {
			return listErr
		}
		for _, name := range strings.Fields(string(list)) {
			if name == ContextName {
				return err
			}
		}
		if _, err := run("context", "create", ContextName, "--docker", "host="+status.Endpoint); err != nil {
			return err
		}
	}
	_, err = run("context", "use", ContextName)
	return err
}
