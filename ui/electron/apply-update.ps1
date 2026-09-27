param(
    [Parameter(Mandatory = $true)][int]$ParentPid,
    [Parameter(Mandatory = $true)][string]$PackagePath,
    [Parameter(Mandatory = $true)][string]$InstallDirectory,
    [Parameter(Mandatory = $true)][string]$ExecutablePath,
    [Parameter(Mandatory = $true)][string]$ErrorFile,
    [switch]$SkipRestart
)

$ErrorActionPreference = "Stop"
$LogFile = "$PackagePath.install.log"

function Write-UpdateLog([string]$Message) {
    Add-Content -LiteralPath $LogFile -Value "$(Get-Date -Format o) $Message"
}

function Restart-ExistingPorto {
    if (-not $SkipRestart -and (Test-Path -LiteralPath $ExecutablePath)) {
        Start-Process -FilePath $ExecutablePath
    }
}

function Get-InstalledPortoProcesses([string]$InstallPrefix) {
    try {
        return @(
            Get-CimInstance Win32_Process | Where-Object {
                $_.ExecutablePath -and $_.ExecutablePath.StartsWith(
                    $InstallPrefix,
                    [System.StringComparison]::OrdinalIgnoreCase
                )
            }
        )
    }
    catch {
        throw "Unable to inspect running Porto processes. $($_.Exception.Message)"
    }
}

try {
    if (-not [System.IO.Path]::IsPathRooted($InstallDirectory)) {
        throw "Refusing to replace a relative Porto installation path."
    }
    if (-not (Test-Path -LiteralPath $PackagePath -PathType Leaf)) {
        throw "Downloaded Porto installer is missing: $PackagePath"
    }

    for ($Attempt = 0; $Attempt -lt 120; $Attempt++) {
        if (-not (Get-Process -Id $ParentPid -ErrorAction SilentlyContinue)) {
            break
        }
        Start-Sleep -Seconds 1
    }
    if (Get-Process -Id $ParentPid -ErrorAction SilentlyContinue) {
        throw "Porto did not exit before the update timeout."
    }

    $InstallPrefix = [System.IO.Path]::GetFullPath($InstallDirectory).TrimEnd("\") + "\"
    $InstalledProcesses = @()
    for ($Attempt = 0; $Attempt -lt 50; $Attempt++) {
        $InstalledProcesses = @(Get-InstalledPortoProcesses $InstallPrefix)
        if (-not $InstalledProcesses) {
            break
        }
        foreach ($Process in $InstalledProcesses) {
            if ($Process.Name -like "qemu-system-*.exe") {
                continue
            }
            Write-UpdateLog "Stopping installed Porto process $($Process.ProcessId) $($Process.ExecutablePath)"
            Stop-Process -Id $Process.ProcessId -Force -ErrorAction SilentlyContinue
        }
        Start-Sleep -Milliseconds 100
    }
    $InstalledProcesses = @(Get-InstalledPortoProcesses $InstallPrefix)
    if ($InstalledProcesses) {
        $BlockingProcesses = ($InstalledProcesses | ForEach-Object {
            "$($_.Name) (PID $($_.ProcessId))"
        }) -join ", "
        throw "Porto runtime processes are still using the installation: $BlockingProcesses. Stop running Porto VMs and Kubernetes clusters, then retry the update."
    }

    Write-UpdateLog "Installing $PackagePath into $InstallDirectory"
    $Installer = Start-Process `
        -FilePath $PackagePath `
        -ArgumentList "/S /D=$InstallDirectory" `
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
    $Message = "Porto could not install the downloaded update. $($_.Exception.Message) See $LogFile for details."
    Write-UpdateLog $Message
    Set-Content -LiteralPath $ErrorFile -Value $Message
    Restart-ExistingPorto
    exit 1
}
