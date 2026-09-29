$ErrorActionPreference = "Stop"

function Get-InstalledPortoProcesses([string]$InstallPrefix) {
    return @(
        Get-CimInstance Win32_Process | Where-Object {
            $_.ExecutablePath -and $_.ExecutablePath.StartsWith(
                $InstallPrefix,
                [System.StringComparison]::OrdinalIgnoreCase
            )
        }
    )
}

function Format-PortoProcesses([object[]]$Processes) {
    return ($Processes | ForEach-Object {
        "$($_.Name) (PID $($_.ProcessId))"
    }) -join ", "
}

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
    $IsUpdate = Test-Path $InstallRoot
    if ($IsUpdate) {
        $PortoCli = Join-Path $InstallRoot "resources\porto.exe"
        if (Test-Path -LiteralPath $PortoCli -PathType Leaf) {
            & $PortoCli docker engine-stop
            if ($LASTEXITCODE -ne 0) {
                Write-Warning "Porto could not stop its container engine cleanly; checking for locked runtime processes."
            }
        }

        $InstallPrefix = [System.IO.Path]::GetFullPath($InstallRoot).TrimEnd("\") + "\"
        $Processes = @(Get-InstalledPortoProcesses $InstallPrefix)
        $BlockingProcesses = @($Processes | Where-Object {
            $_.Name -like "qemu-system-*.exe"
        })
        if ($BlockingProcesses) {
            throw "Stop running Porto VMs and Kubernetes clusters before updating: $(Format-PortoProcesses $BlockingProcesses)."
        }

        foreach ($Process in $Processes) {
            if ($Process.Name -like "qemu-system-*.exe") {
                continue
            }
            Stop-Process -Id $Process.ProcessId -Force -ErrorAction SilentlyContinue
        }
        for ($Attempt = 0; $Attempt -lt 50; $Attempt++) {
            $Remaining = @(Get-InstalledPortoProcesses $InstallPrefix)
            if (-not $Remaining) {
                break
            }
            Start-Sleep -Milliseconds 100
        }
        if ($Remaining) {
            throw "Porto is still running from ${InstallRoot}: $(Format-PortoProcesses $Remaining). Close it and retry the installation."
        }
    }

    $InstallerArguments = if ($IsUpdate) { "--updated /S /D=$InstallRoot" } else { "/S /D=$InstallRoot" }
    $InstallProcess = Start-Process $Installer -ArgumentList $InstallerArguments -Wait -PassThru
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
    $DockerCommandPath = Join-Path $BinDirectory "docker.cmd"
    $DockerCommand = '"{0}" %*' -f (Join-Path $InstallRoot "resources\runtime\bin\docker.exe")
    $ExistingDocker = Get-Command docker -ErrorAction SilentlyContinue
    if ($ExistingDocker -and $ExistingDocker.Source -ne $DockerCommandPath) {
        Write-Warning "Preserving existing Docker command at $($ExistingDocker.Source); use 'porto docker cli' for Porto's bundled toolchain."
    }
    elseif (-not (Test-Path $DockerCommandPath)) {
        $DockerCommand | Set-Content -Encoding ASCII $DockerCommandPath
    }
    elseif ((Get-Content -Raw $DockerCommandPath) -match [regex]::Escape("\resources\runtime\bin\docker.exe")) {
        $DockerCommand | Set-Content -Encoding ASCII $DockerCommandPath
    }
    else {
        Write-Warning "Preserving existing Docker command at $DockerCommandPath; use 'porto docker cli' for Porto's bundled toolchain."
    }
    $DiveCommandPath = Join-Path $BinDirectory "dive.cmd"
    $DiveCommand = '"{0}" %*' -f (Join-Path $InstallRoot "resources\runtime\bin\dive.exe")
    $ExistingDive = Get-Command dive -ErrorAction SilentlyContinue
    if ($ExistingDive -and $ExistingDive.Source -ne $DiveCommandPath) {
        Write-Warning "Preserving existing Dive command at $($ExistingDive.Source); use 'porto docker dive' for Porto's bundled image inspector."
    }
    elseif (-not (Test-Path $DiveCommandPath)) {
        $DiveCommand | Set-Content -Encoding ASCII $DiveCommandPath
    }
    elseif ((Get-Content -Raw $DiveCommandPath) -match [regex]::Escape("\resources\runtime\bin\dive.exe")) {
        $DiveCommand | Set-Content -Encoding ASCII $DiveCommandPath
    }
    else {
        Write-Warning "Preserving existing Dive command at $DiveCommandPath; use 'porto docker dive' for Porto's bundled image inspector."
    }
    $UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if (($UserPath -split ";") -notcontains $BinDirectory) {
        [Environment]::SetEnvironmentVariable("Path", "$BinDirectory;$UserPath", "User")
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
