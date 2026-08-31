$ErrorActionPreference = "Stop"

$Repository = if ($env:PORTO_REPOSITORY) { $env:PORTO_REPOSITORY } else { "mbianchidev/porto" }
$Tag = $env:PORTO_VERSION
if (-not $Tag) {
    $ReleaseApi = if ($env:PORTO_RELEASE_API_URL) { $env:PORTO_RELEASE_API_URL } else { "https://api.github.com/repos/$Repository/releases/latest" }
    $Release = Invoke-RestMethod $ReleaseApi
    $Tag = $Release.tag_name
}
if (-not $Tag -or -not $Tag.StartsWith("v")) {
    throw "Unable to resolve a Porto release tag."
}

$ProcessorArchitecture = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
$Architecture = switch ($ProcessorArchitecture) {
    "ARM64" { "arm64" }
    "AMD64" { "amd64" }
    default { throw "Unsupported architecture: $ProcessorArchitecture" }
}
$Version = $Tag.Substring(1)
$Asset = "porto-desktop_${Version}_windows_${Architecture}.exe"
$BaseUrl = if ($env:PORTO_RELEASE_BASE_URL) { $env:PORTO_RELEASE_BASE_URL } else { "https://github.com/$Repository/releases/download/$Tag" }
$Temporary = Join-Path ([System.IO.Path]::GetTempPath()) "porto-desktop-$([guid]::NewGuid())"
$Installer = Join-Path $Temporary $Asset
$Checksums = Join-Path $Temporary "SHA256SUMS"

try {
    New-Item -ItemType Directory -Path $Temporary | Out-Null
    Invoke-WebRequest "$BaseUrl/$Asset" -OutFile $Installer
    Invoke-WebRequest "$BaseUrl/SHA256SUMS" -OutFile $Checksums
    $ChecksumLine = Get-Content $Checksums | Where-Object { $_ -match [regex]::Escape($Asset) } | Select-Object -First 1
    if (-not $ChecksumLine) {
        throw "No checksum was published for $Asset."
    }
    $Expected = ($ChecksumLine -split "\s+")[0].ToLowerInvariant()
    $Actual = (Get-FileHash -Algorithm SHA256 $Installer).Hash.ToLowerInvariant()
    if ($Expected -ne $Actual) {
        throw "Checksum verification failed for $Asset."
    }

    $InstallRoot = if ($env:PORTO_INSTALL_DIR) { $env:PORTO_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA "Programs\Porto" }
    if (Test-Path $InstallRoot) {
        $InstallPath = [System.IO.Path]::GetFullPath($InstallRoot)
        $Processes = Get-CimInstance Win32_Process | Where-Object {
            $_.ExecutablePath -and $_.ExecutablePath.StartsWith($InstallPath, [System.StringComparison]::OrdinalIgnoreCase)
        }
        foreach ($Process in $Processes) {
            Stop-Process -Id $Process.ProcessId -Force
        }
        for ($Attempt = 0; $Attempt -lt 50; $Attempt++) {
            $Remaining = Get-CimInstance Win32_Process | Where-Object {
                $_.ExecutablePath -and $_.ExecutablePath.StartsWith($InstallPath, [System.StringComparison]::OrdinalIgnoreCase)
            }
            if (-not $Remaining) {
                break
            }
            Start-Sleep -Milliseconds 100
        }
        if ($Remaining) {
            throw "Porto is still running from $InstallRoot. Close it and retry the installation."
        }
    }

    $InstallProcess = Start-Process $Installer -ArgumentList "/S /D=$InstallRoot" -Wait -PassThru
    if ($InstallProcess.ExitCode -ne 0) {
        throw "Porto installer exited with code $($InstallProcess.ExitCode)."
    }
    if (-not (Test-Path (Join-Path $InstallRoot "Porto.exe"))) {
        throw "Porto was not installed at $InstallRoot."
    }

    $StartMenu = Join-Path $env:APPDATA "Microsoft\Windows\Start Menu\Programs"
    $ShortcutPath = Join-Path $StartMenu "Porto.lnk"
    $Shell = New-Object -ComObject WScript.Shell
    $Shortcut = $Shell.CreateShortcut($ShortcutPath)
    $Shortcut.TargetPath = Join-Path $InstallRoot "Porto.exe"
    $Shortcut.WorkingDirectory = $InstallRoot
    $Shortcut.IconLocation = Join-Path $InstallRoot "Porto.exe"
    $Shortcut.Save()

    $BinDirectory = if ($env:PORTO_BIN_DIR) { $env:PORTO_BIN_DIR } else { Join-Path $env:LOCALAPPDATA "Porto\bin" }
    New-Item -ItemType Directory -Force -Path $BinDirectory | Out-Null
    $Command = '"{0}" %*' -f (Join-Path $InstallRoot "resources\porto.exe")
    $Command | Set-Content -Encoding ASCII (Join-Path $BinDirectory "porto.cmd")
    $UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if (($UserPath -split ";") -notcontains $BinDirectory) {
        [Environment]::SetEnvironmentVariable("Path", "$BinDirectory;$UserPath", "User")
    }

    if ($env:PORTO_SKIP_PREREQS -ne "1" -and -not (Get-Command qemu-system-x86_64.exe -ErrorAction SilentlyContinue) -and -not (Get-Command qemu-system-aarch64.exe -ErrorAction SilentlyContinue)) {
        if (Get-Command winget.exe -ErrorAction SilentlyContinue) {
            & winget.exe install --id SoftwareFreedomConservancy.QEMU --exact --silent --accept-package-agreements --accept-source-agreements
            if ($LASTEXITCODE -ne 0) {
                Write-Warning "QEMU installation failed. Install QEMU manually to use Porto virtual machines."
            }
            $env:Path = [Environment]::GetEnvironmentVariable("Path", "Machine") + ";" + [Environment]::GetEnvironmentVariable("Path", "User")
        }
        else {
            Write-Warning "Install QEMU to use Porto virtual machines."
        }
    }

    Write-Host "Installed Porto at $InstallRoot"
    if ($env:PORTO_NO_LAUNCH -ne "1") {
        Start-Process (Join-Path $InstallRoot "Porto.exe")
    }
}
finally {
    if (Test-Path $Temporary) {
        Remove-Item -Recurse -Force $Temporary
    }
}
