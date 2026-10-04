$ErrorActionPreference = 'Stop'

$version = '2.1.25156'
$expected = '073a70e00f77423e34bed98b86e600def93393ba5822204fac57a29324db9f7a'
$installer = Join-Path $env:RUNNER_TEMP "winfsp-$version.msi"
Invoke-WebRequest `
  -Uri "https://github.com/winfsp/winfsp/releases/download/v2.1/winfsp-$version.msi" `
  -OutFile $installer
$actual = (Get-FileHash $installer -Algorithm SHA256).Hash.ToLowerInvariant()
if ($actual -ne $expected) {
  throw 'WinFsp CI installer checksum did not match the pinned official release.'
}
$process = Start-Process `
  -FilePath 'msiexec.exe' `
  -ArgumentList @('/i', "`"$installer`"", '/qn', '/norestart', 'REBOOT=ReallySuppress') `
  -Wait `
  -PassThru
if ($process.ExitCode -notin @(0, 3010)) {
  throw "WinFsp CI installer exited with code $($process.ExitCode)."
}
Remove-Item -LiteralPath $installer -Force
