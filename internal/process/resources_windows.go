//go:build windows

package process

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

const windowsResourceScript = `$parents = @{}
Get-CimInstance Win32_Process | ForEach-Object { $parents[[int]$_.ProcessId] = [int]$_.ParentProcessId }
$rows = @(Get-CimInstance Win32_PerfFormattedData_PerfProc_Process | ForEach-Object {
  [pscustomobject]@{
    pid = [int]$_.IDProcess
    parentPid = [int]($parents[[int]$_.IDProcess])
    cpuPercent = [double]$_.PercentProcessorTime
    memoryBytes = [int64]$_.WorkingSetPrivate
  }
})
ConvertTo-Json -InputObject $rows -Compress`

func readResourceRows(ctx context.Context) ([]resourceRow, error) {
	output, err := exec.CommandContext(
		ctx,
		"powershell.exe",
		"-NoProfile",
		"-NonInteractive",
		"-Command",
		windowsResourceScript,
	).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("inspect process resources: %w: %s", err, strings.TrimSpace(string(output)))
	}
	var rows []resourceRow
	if err := json.Unmarshal(output, &rows); err != nil {
		return nil, fmt.Errorf("decode process resources: %w", err)
	}
	return rows, nil
}
