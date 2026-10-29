param(
    [Parameter(Mandatory = $true)][int]$ParentPid,
    [Parameter(Mandatory = $true)][string]$PackagePath,
    [Parameter(Mandatory = $true)][string]$InstallDirectory,
    [Parameter(Mandatory = $true)][string]$ExecutablePath,
    [Parameter(Mandatory = $true)][string]$ErrorFile,
    [Parameter(Mandatory = $true)][string]$ReadyFile,
    [Parameter(Mandatory = $true)][string]$ProceedFile,
    [switch]$SkipRestart
)

$ErrorActionPreference = "Stop"
$LogFile = "$PackagePath.install.log"
$Utf8NoBom = [System.Text.UTF8Encoding]::new($false)

function Write-UpdateLog([string]$Message) {
    [System.IO.File]::AppendAllText(
        $LogFile,
        "$([System.DateTimeOffset]::UtcNow.ToString('o', [System.Globalization.CultureInfo]::InvariantCulture)) $Message`r`n",
        $Utf8NoBom
    )
}

function Restart-ExistingPorto {
    if (
        -not $SkipRestart `
        -and -not (Get-Process -Id $ParentPid -ErrorAction SilentlyContinue) `
        -and (Test-Path -LiteralPath $ExecutablePath)
    ) {
        Start-Process -FilePath $ExecutablePath
    }
}

function Get-InstalledPortoProcesses(
    [string]$InstallPrefix,
    [System.Collections.Generic.HashSet[string]]$ExecutableNames
) {
    try {
        foreach ($Target in [System.Diagnostics.Process]::GetProcesses()) {
            try {
                try {
                    if (-not $ExecutableNames.Contains($Target.ProcessName)) {
                        continue
                    }
                    $Start = $Target.StartTime.ToUniversalTime().Ticks
                    $Executable = $Target.MainModule.FileName
                    if ($Target.HasExited) { continue }
                    if (-not $Executable) { throw "Native executable metadata is unavailable." }
                }
                catch {
                    if ($Target.HasExited) { continue }
                    throw
                }
                if ($Executable.StartsWith($InstallPrefix, [System.StringComparison]::OrdinalIgnoreCase)) {
                    [PSCustomObject]@{
                        Name = [System.IO.Path]::GetFileName($Executable)
                        ProcessId = $Target.Id
                        ExecutablePath = $Executable
                        StartTimeTicks = $Start
                    }
                }
            }
            finally {
                $Target.Dispose()
            }
        }
    }
    catch {
        throw "Unable to inspect running Porto processes. $($_.Exception.Message)"
    }
}

function Stop-InstalledPortoProcess([object]$Expected) {
    try {
        $Target = [System.Diagnostics.Process]::GetProcessById($Expected.ProcessId)
    }
    catch [System.ArgumentException] {
        return
    }
    try {
        try {
            if ($Target.HasExited) { return }
            $Start = $Target.StartTime.ToUniversalTime().Ticks
            $Executable = $Target.MainModule.FileName
            if ($Target.HasExited) { return }
        }
        catch {
            if ($Target.HasExited) { return }
            throw
        }
        if (
            $Start -ne $Expected.StartTimeTicks `
            -or -not [string]::Equals($Executable, $Expected.ExecutablePath, [System.StringComparison]::OrdinalIgnoreCase)
        ) {
            Write-UpdateLog "Process $($Expected.ProcessId) changed ownership before stopping; it was not signaled."
            return
        }
        Write-UpdateLog "Stopping installed Porto process $($Expected.ProcessId) $Executable"
        $Target.Kill()
    }
    catch {
        if ($Target.HasExited) { return }
        throw "Unable to stop owned Porto process $($Expected.ProcessId). $($_.Exception.Message)"
    }
    finally {
        $Target.Dispose()
    }
}

function Format-PortoProcesses([object[]]$Processes) {
    return ($Processes | ForEach-Object {
        "$($_.Name) (PID $($_.ProcessId))"
    }) -join ", "
}

try {
    if (-not [System.IO.Path]::IsPathRooted($InstallDirectory)) {
        throw "Refusing to replace a relative Porto installation path."
    }
    if (-not (Test-Path -LiteralPath $PackagePath -PathType Leaf)) {
        throw "Downloaded Porto installer is missing: $PackagePath"
    }

    $InstallPrefix = [System.IO.Path]::GetFullPath($InstallDirectory).TrimEnd("\") + "\"
    Write-UpdateLog "Inspecting installed Porto executable ownership."
    $ExecutableNames = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    foreach ($File in (Get-ChildItem -LiteralPath $InstallDirectory -Filter '*.exe' -Recurse -File)) {
        [void]$ExecutableNames.Add([System.IO.Path]::GetFileNameWithoutExtension($File.Name))
    }
    if ($ExecutableNames.Count -eq 0) {
        throw "Installed Porto executable metadata is unavailable."
    }
    $BlockingProcesses = @(
        Get-InstalledPortoProcesses $InstallPrefix $ExecutableNames | Where-Object {
            $_.Name -like "qemu-system-*.exe"
        }
    )
    if ($BlockingProcesses) {
        throw "Stop running Porto VMs and Kubernetes clusters before updating: $(Format-PortoProcesses $BlockingProcesses)."
    }

    Remove-Item -LiteralPath $ProceedFile -Force -ErrorAction SilentlyContinue
    Write-UpdateLog "Updater helper is ready and waiting for Porto to exit."
    [System.IO.File]::WriteAllText($ReadyFile, "ready`r`n", $Utf8NoBom)

    for ($Attempt = 0; $Attempt -lt 200; $Attempt++) {
        if (Test-Path -LiteralPath $ProceedFile -PathType Leaf) {
            break
        }
        Start-Sleep -Milliseconds 50
    }
    if (-not (Test-Path -LiteralPath $ProceedFile -PathType Leaf)) {
        throw "Porto did not confirm the downloaded update."
    }
    Remove-Item -LiteralPath $ReadyFile -Force -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $ProceedFile -Force -ErrorAction SilentlyContinue

    for ($Attempt = 0; $Attempt -lt 120; $Attempt++) {
        if (-not (Get-Process -Id $ParentPid -ErrorAction SilentlyContinue)) {
            break
        }
        Start-Sleep -Seconds 1
    }
    if (Get-Process -Id $ParentPid -ErrorAction SilentlyContinue) {
        throw "Porto did not exit before the update timeout."
    }

    $InstalledProcesses = @()
    for ($Attempt = 0; $Attempt -lt 50; $Attempt++) {
        $InstalledProcesses = @(Get-InstalledPortoProcesses $InstallPrefix $ExecutableNames)
        if (-not $InstalledProcesses) {
            break
        }
        foreach ($Process in $InstalledProcesses) {
            if ($Process.Name -like "qemu-system-*.exe") {
                continue
            }
            Stop-InstalledPortoProcess $Process
        }
        Start-Sleep -Milliseconds 100
    }
    $InstalledProcesses = @(Get-InstalledPortoProcesses $InstallPrefix $ExecutableNames)
    if ($InstalledProcesses) {
        throw "Porto runtime processes are still using the installation: $(Format-PortoProcesses $InstalledProcesses). Stop running Porto VMs and Kubernetes clusters, then retry the update."
    }

    Write-UpdateLog "Installing $PackagePath into $InstallDirectory"
    $Installer = Start-Process `
        -FilePath $PackagePath `
        -ArgumentList "--updated /S /D=$InstallDirectory" `
        -Wait `
        -PassThru
    if ($Installer.ExitCode -ne 0) {
        throw "Porto installer exited with code $($Installer.ExitCode)."
    }
    if (-not (Test-Path -LiteralPath $ExecutablePath -PathType Leaf)) {
        throw "The updated Porto executable is missing: $ExecutablePath"
    }

    Remove-Item -LiteralPath $ErrorFile -Force -ErrorAction SilentlyContinue
    Restart-ExistingPorto
    Remove-Item -LiteralPath $PackagePath -Force -ErrorAction SilentlyContinue
    Write-UpdateLog "Porto update installed successfully."
}
catch {
    Remove-Item -LiteralPath $ReadyFile -Force -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $ProceedFile -Force -ErrorAction SilentlyContinue
    $Message = "Porto could not install the downloaded update. $($_.Exception.Message) See $LogFile for details."
    Write-UpdateLog $Message
    [System.IO.File]::WriteAllText($ErrorFile, "$Message`r`n", $Utf8NoBom)
    Restart-ExistingPorto
    exit 1
}
