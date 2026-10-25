//go:build windows

package windowscontainers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/process"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

func Probe(ctx context.Context) (Host, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	host := Host{OS: "windows", Build: int(windows.RtlGetVersion().BuildNumber)}
	var processMachine, nativeMachine uint16
	if err := windows.IsWow64Process2(windows.CurrentProcess(), &processMachine, &nativeMachine); err != nil {
		return host, fmt.Errorf("verify native Windows host architecture without emulation fallback: %w", err)
	}
	host.Architecture = architectureForMachine(nativeMachine)
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		return host, fmt.Errorf("inspect Windows container host edition: %w", err)
	}
	host.Edition, _, err = key.GetStringValue("EditionID")
	installation, _, installationErr := key.GetStringValue("InstallationType")
	closeErr := key.Close()
	if err != nil || installationErr != nil || closeErr != nil {
		return host, fmt.Errorf("inspect Windows host identity: %v, %v, %v", err, installationErr, closeErr)
	}
	host.Server = strings.HasPrefix(strings.ToLower(installation), "server")
	if host.Architecture != "amd64" {
		return host, nil
	}
	if !windows.GetCurrentProcessToken().IsElevated() {
		return host, fmt.Errorf("administrator inspection is required to verify Windows Containers and Hyper-V features; use the explicitly elevated native runtime setup")
	}
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return host, err
	}
	// Use the native servicing executable rather than the PowerShell DISM
	// module, whose initialization can block on headless Windows hosts.
	command := process.NewCommand(ctx, "", filepath.Join(system, "dism.exe"),
		"/Online", "/Get-FeatureInfo", "/FeatureName:Containers", "/English")
	command.Env = []string{"SystemRoot=" + filepath.Dir(system), "WINDIR=" + filepath.Dir(system),
		"SystemDrive=" + filepath.VolumeName(system), "ComSpec=" + filepath.Join(system, "cmd.exe"),
		"PATH=" + system + ";" + filepath.Dir(system)}
	for _, folder := range []struct {
		name string
		id   *windows.KNOWNFOLDERID
	}{
		{"USERPROFILE", windows.FOLDERID_Profile},
		{"LOCALAPPDATA", windows.FOLDERID_LocalAppData},
		{"APPDATA", windows.FOLDERID_RoamingAppData},
		{"ProgramData", windows.FOLDERID_ProgramData},
	} {
		value, err := windows.KnownFolderPath(folder.id, 0)
		if err != nil {
			return host, fmt.Errorf("resolve native probe environment %s: %w", folder.name, err)
		}
		command.Env = append(command.Env, folder.name+"="+value)
		if folder.name == "LOCALAPPDATA" {
			command.Env = append(command.Env, "TEMP="+filepath.Join(value, "Temp"), "TMP="+filepath.Join(value, "Temp"))
		} else if folder.name == "ProgramData" {
			command.Env = append(command.Env, "ALLUSERSPROFILE="+value)
		}
	}
	var diagnostics bytes.Buffer
	command.Stderr = &diagnostics
	output, err := command.Output()
	if err != nil {
		return host, fmt.Errorf("inspect native Windows Containers feature without changing it: %w: %s %s",
			errors.Join(err, ctx.Err()), diagnostics.String(), output)
	}
	host.Containers, err = windowsFeatureEnabled(output)
	if err != nil {
		return host, err
	}
	host.HyperV, err = hyperVManagementRunning()
	if err != nil {
		return host, err
	}
	host.Virtualization = windows.IsProcessorFeaturePresent(windows.PF_VIRT_FIRMWARE_ENABLED) &&
		windows.IsProcessorFeaturePresent(windows.PF_SECOND_LEVEL_ADDRESS_TRANSLATION)
	return host, nil
}

func hyperVManagementRunning() (running bool, err error) {
	manager, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return false, fmt.Errorf("inspect native Hyper-V management: %w", err)
	}
	defer func() { err = errors.Join(err, windows.CloseServiceHandle(manager)) }()
	name, err := windows.UTF16PtrFromString("vmms")
	if err != nil {
		return false, err
	}
	handle, err := windows.OpenService(manager, name, windows.SERVICE_QUERY_STATUS)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect Hyper-V management service: %w", err)
	}
	service := &mgr.Service{Name: "vmms", Handle: handle}
	defer func() { err = errors.Join(err, service.Close()) }()
	status, err := service.Query()
	if err != nil {
		return false, fmt.Errorf("inspect Hyper-V management state: %w", err)
	}
	return status.State == svc.Running, nil
}
