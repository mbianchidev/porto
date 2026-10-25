//go:build windows

package windowscontainers

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/process"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
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
	powershell := filepath.Join(system, "WindowsPowerShell", "v1.0", "powershell.exe")
	modules := filepath.Join(system, "WindowsPowerShell", "v1.0", "Modules")
	script := `$ErrorActionPreference='Stop'
$features=@(Get-WindowsOptionalFeature -Online)
$containers=@($features | Where-Object {$_.FeatureName -eq 'Containers' -and $_.State -eq 'Enabled'}).Count -gt 0
$hyperv=@($features | Where-Object {$_.FeatureName -in @('Microsoft-Hyper-V-All','Microsoft-Hyper-V') -and $_.State -eq 'Enabled'}).Count -gt 0
$processors=@(Get-CimInstance -ClassName Win32_Processor)
$ready=$processors.Count -gt 0 -and @($processors | Where-Object {
  -not $_.VirtualizationFirmwareEnabled -or -not $_.SecondLevelAddressTranslationExtensions
}).Count -eq 0
@{Containers=$containers; HyperV=$hyperv; Virtualization=$ready} | ConvertTo-Json -Compress`
	command := process.NewCommand(ctx, "", powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script)
	command.Env = []string{"SystemRoot=" + filepath.Dir(system), "WINDIR=" + filepath.Dir(system),
		"PATH=" + system, "PSModulePath=" + modules}
	output, err := command.CombinedOutput()
	if err != nil {
		return host, fmt.Errorf("inspect native Windows features without changing them: %w: %s", err, output)
	}
	var features struct {
		Containers, HyperV, Virtualization bool
	}
	if err := json.Unmarshal(output, &features); err != nil {
		return host, fmt.Errorf("decode Windows host feature inspection: %w", err)
	}
	host.Containers, host.HyperV, host.Virtualization = features.Containers, features.HyperV, features.Virtualization
	return host, nil
}
