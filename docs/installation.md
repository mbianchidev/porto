# Installation

Optional trusted HTTPS is available through **Settings > Trusted local HTTPS**
or `porto https install` on macOS, Windows and supported systemd Linux hosts.
Only platform setup is elevated. Linux requires distribution CA tooling and
Chromium NSS tools; see [networking prerequisites and trust boundaries](networking.md).

Windows installations also bundle architecture-matched Linux WSL client tools.
Enable individual WSL 2 distributions explicitly; the app never converts a
distribution or replaces its existing Lima/QEMU Linux engine. See the
[Windows integration and validation matrix](windows-integration.md).

For live Windows Explorer/editor access to runtime files, install the maintained
[WinFsp 2.1+ driver](https://winfsp.dev) explicitly, then use **Open files** in
Porto. Desktop packages bundle the matching native mount helper; no SSHFS-Win
installation or cached sync folder is needed. In-app Files and volume archives
do not require WinFsp. See [native runtime access](data-management.md#native-file-manager-access)
for write boundaries, guest support and cleanup.

Porto needs both the `porto` binary and the compiled dashboard assets. Release archives include both.

## Install a release

### Desktop one-liner

macOS and Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/mbianchidev/porto/main/scripts/install-desktop.sh | sh
```

Windows PowerShell:

```powershell
irm https://raw.githubusercontent.com/mbianchidev/porto/main/scripts/install-desktop.ps1 | iex
```

Both installers resolve the latest GitHub release, select the matching
OS/architecture package, verify it against `SHA256SUMS`, install it for the
current user, add a `porto` CLI entry, and launch the app. macOS uses a DMG,
Windows uses an NSIS EXE installer, and Linux uses the portable desktop archive.
The interactive Windows installer offers an unchecked desktop-shortcut option;
silent installs do not create one. Set
`PORTO_VERSION=v1.0.0` to install a specific release or `PORTO_NO_LAUNCH=1` to
install without opening it.

The desktop app compares the SHA-256 identity cached by the running daemon with
the daemon bundled in the installed app. It gracefully replaces a different
daemon before opening the dashboard, even when the executable path, product
version, and API compatibility version are unchanged. This prevents a newly
installed Porto app from continuing to use code left running by an older build.

Desktop startup failures include the path to the persistent diagnostic log:
`logs/porto.log` inside the platform's Porto data directory, or under
`PORTO_HOME` when configured. Logging defaults to debug and includes the
detached daemon's output. Completed days are compressed as dated ZIP archives;
Porto keeps seven days by default and exposes retention in System settings. See
[application diagnostic logs](daily-use.md#application-diagnostic-logs) for
platform paths, rotation, retention, and verbosity settings.

Desktop archives contain Porto, its dashboard, the Docker CLI with Compose and
Buildx, Dive for terminal image-layer inspection, `kubectl`, `k9s`, Lima, and
the supported `kind` binary for that platform. Windows packages also
contain architecture-matched QEMU and `qemu-img`, so Lima needs no separate
system installation. Windows ARM64 excludes KinD because upstream does not
publish a native binary; Porto builds its Docker CLI from Docker's pinned
official source and bundles upstream Windows ARM64 Compose and Buildx plugins.
Linux installation installs QEMU
through `apt`, `dnf`, `pacman`, or `zypper` when it is missing. Set
`PORTO_SKIP_PREREQS=1` to skip that Linux prerequisite step.

On macOS, Linux, and Windows, the packaged app automatically provisions and starts its
containerd and BuildKit backend on first launch when Docker support is enabled.
On macOS and Linux, it combines the bundled runtime tools with the user's
login-shell `PATH`, so project commands can find package managers and language
toolchains installed outside the system paths available to graphical applications.
Installing and opening the desktop package requires no follow-up runtime setup
command.

The install scripts add a `docker` command beside `porto` only when that name is
unused or already points to Porto. Existing user-managed commands are preserved.
The always-available explicit form is:

```sh
porto docker cli --version
porto docker cli compose version
porto docker cli buildx version
porto docker dive alpine:latest
porto docker dive --container running-api
```

Porto's Docker CLI discovers the plugins shipped beside it before user and
system plugin directories. It still reads the user's original Docker config,
contexts, credential helpers, and Buildx state directly; Porto does not rewrite
`~/.docker/config.json` or delete existing plugins.

On Windows, Porto connects to the guest containerd socket through its bundled
Linux helper over Lima's standard-input/output transport, without forwarding a
Windows filesystem path as an SSH Unix socket. After installation or engine
startup, the daemon immediately retries the container inventory connection.
The desktop waits for that fresh inventory rather than treating an older
unavailable snapshot as a failed installation.

New Windows container engines use the explicit Ubuntu 24.04 LTS template,
also used by Porto's managed Kubernetes nodes. A Lima default-image update
therefore cannot silently change the Windows engine's guest OS. Existing
engines retain their disks and guest OS; an app upgrade does not recreate them.

Windows packages bundle Lima `v2.2.1+porto.2`: the stable 2.2.1 source with
[upstream's Windows PID fix](https://github.com/lima-vm/lima/commit/28285d6e58dc38a75b912c76f5b5f0cad534d435)
backported and consoleless shutdown fixes. Graceful QEMU stops request ACPI
powerdown through that instance's existing QMP socket and retain Lima's
host-agent event and stopped-state checks. No console window or implicit
forced termination is required. After an interrupted shutdown,
Lima removes stale host-agent and QEMU PID files when Windows reports that
those processes no longer exist, instead of refusing to start with
`OpenProcess: The parameter is incorrect`. Forced stops terminate the selected
process tree instead of relying on a console event, allowing its log handles
to close before the next startup. If a target exits while the stop command is
running, Lima confirms its process handle has exited before accepting the stop,
including Windows' "no running instance of the task" exit status.
Live process IDs are not treated as stale;
configuration errors and VM disks are preserved. macOS and Linux retain the
unmodified upstream Lima binaries.
ARM64 packages also use the actual Windows CPU feature bits when selecting SSH
ciphers, instead of emitting a spurious initialization warning into every
runtime response.
An offline/reconnecting WSL filesystem reporting `ERROR_INVALID_FUNCTION` does
not invalidate the running engine's saved mount configuration. This exception
is confined to WSL UNC paths; permissions, ordinary UNC failures and actual
filesystem I/O errors remain visible.

New engines use nerdctl 2.4.1 from Lima 2.2.1. It fixes an upstream `save`
race that could return a truncated archive under load. Existing engines retain
their installed guest tools; upgrading Porto does not replace their disks.
Porto validates saved archive structure before returning success. If an older
engine reports an incomplete export, update nerdctl inside that owned guest to
2.4.0 or newer using its upstream checksum-verified release, then retry the
export. Do not delete the original image after a failed save.

### Windows guest recovery

An ownership-check timeout can mean that QEMU is still running but the Linux
guest has crashed. When an ownership probe fails, Porto checks a bounded tail
of the guest's `serial.log` and includes a detected kernel panic and log path
in the error. It does not bypass ownership verification or automatically
delete or replace the engine. Increasing the SSH timeout cannot recover a
panicked guest.

For an existing guest, first try stopping and starting the engine with the
updated bundled Lima. If an older force-stop left `ha.stdout.log` locked,
restart Windows to release the orphaned processes before retrying.

**Only for a disposable engine, or after backing up its data:** quit Porto,
install the updated desktop package without launching it, then remove only
the container-engine VM:

```powershell
$lima = "$env:LOCALAPPDATA\Programs\Porto\resources\runtime\lima\bin\limactl.exe"
& $lima delete --force porto-engine
if ($LASTEXITCODE -ne 0) {
    throw "Engine removal failed. Do not manually delete PID files or VM directories."
}
```

Adjust the executable path for a custom installation location. This deletes
the engine's containers, images, and volumes. Reopen Porto to create the new
Ubuntu 24.04 engine and install its bundled runtime helper automatically.
Other Lima VMs and Porto's project database are not removed. Do not delete
the entire `.lima` or Porto state directory.

### Desktop updates

Installed Porto desktop builds check the repository's stable GitHub Releases
shortly after launch and every 12 hours. When a newer version is available,
Porto prompts before downloading the matching macOS DMG, Windows NSIS installer,
or Linux desktop archive. Every download is verified against the release's
`SHA256SUMS` file.

Choose **Help > Check for updates** in the desktop menu, **Check for updates**
in the menu bar/system tray, or the same button under **software updates** in
**Settings** to check immediately. Menu checks bring Porto to the foreground and
show the result or a clear error. They respect the automatic-download preference
and can reopen an update prompt previously dismissed with **Later**.
Checking/downloading/installing disables repeated checks; source builds and
browser dashboards show updates as unavailable.

Enable **Automatically download new Porto versions** in the desktop preferences
on **Settings** to skip the download prompt. Porto never restarts itself: after
verification it waits for **Restart and update**. That restart replaces the
desktop package and reconnects the bundled daemon without deleting or
recreating existing VMs, Kubernetes clusters, containers, images, or volumes.
On Windows, Porto first stops its owned container engine cleanly and starts it
again after the updated desktop launches. If another Porto VM or Kubernetes
cluster still has the bundled QEMU runtime open, the update aborts instead of
force-killing that workload; stop it and retry the update.

In-app installation requires the current application directory to be writable
by the user. The one-line installers use writable per-user locations by default.
If Porto was copied into an administrator-owned directory, install the
downloaded update manually or move Porto to a user-writable application
directory.

Windows Porto 1.2.13 and earlier can exit before their detached update helper is
ready, so those builds cannot reliably repair themselves in place. Stop active
Porto VMs and Kubernetes clusters, then run the verified installer once:

```powershell
irm https://raw.githubusercontent.com/mbianchidev/porto/main/scripts/install-desktop.ps1 | iex
```

Porto 1.2.14 and later wait for the helper to signal readiness before exiting.
The current updater also completes its locked-runtime preflight first. If a
bundled QEMU process still has the installation open, Porto keeps the desktop
running and reports the blocking workload instead of disappearing before the
installer starts.

macOS DMGs still need Developer ID signing/notarization for warning-free
launches, and Windows installers may show SmartScreen until releases are
signed. Porto provides its own Docker-compatible API, containerd backend,
BuildKit bridge, and Compose backend.

### macOS Gatekeeper quarantine

If macOS Gatekeeper blocks Porto's first launch, only remove quarantine for a
download you trust from the
[official releases page](https://github.com/mbianchidev/porto/releases).
First verify the DMG or desktop archive against that release's `SHA256SUMS`;
see [checksum verification](#manual-archive-installation). If verification
fails, do not unblock or open the app.

For a manual installation, copy `Porto.app` from the DMG or extracted desktop
archive into `/Applications` first, rather than launching it from the mounted
DMG or Downloads. Then run this in Terminal:

```sh
xattr -dr com.apple.quarantine "/Applications/Porto.app"
```

Reopen `/Applications/Porto.app` afterward.

The one-line installer instead uses `~/Applications/Porto.app` by default.
For that installation, use the matching path, without following bundled
symbolic links:

```sh
xattr -drs com.apple.quarantine "$HOME/Applications/Porto.app"
```

Reopen `~/Applications/Porto.app` afterward. If you set `PORTO_INSTALL_DIR`,
substitute the actual installed app path. Both commands apply only to the
selected Porto bundle; do not disable Gatekeeper globally.

### Manual DMG installation

Open the DMG, drag `Porto.app` to `/Applications`, and launch it from there.
If macOS blocks launch, follow
[macOS Gatekeeper quarantine](#macos-gatekeeper-quarantine), then enable the
CLI with:

#### Enable the CLI

```sh
mkdir -p "$HOME/.local/bin"
ln -sfn "/Applications/Porto.app/Contents/Resources/porto" "$HOME/.local/bin/porto"
if [ ! -e "$HOME/.local/bin/docker" ] && [ ! -L "$HOME/.local/bin/docker" ]; then
  ln -s "/Applications/Porto.app/Contents/Resources/runtime/bin/docker" "$HOME/.local/bin/docker"
fi
if [ ! -e "$HOME/.local/bin/dive" ] && [ ! -L "$HOME/.local/bin/dive" ]; then
  ln -s "/Applications/Porto.app/Contents/Resources/runtime/bin/dive" "$HOME/.local/bin/dive"
fi
grep -qxF 'export PATH="$HOME/.local/bin:$PATH"' "$HOME/.zprofile" ||
  echo 'export PATH="$HOME/.local/bin:$PATH"' >> "$HOME/.zprofile"
exec zsh -l
```

### Make Porto the default Docker engine

Porto automatically creates and repairs the named `porto` Docker context. Make
that context the default for the current user without administrator privileges:

```sh
docker context use porto
docker context show
docker info --format 'Porto server {{.ServerVersion}}'
```

Some tools ignore Docker contexts and connect directly to
`/var/run/docker.sock`. On macOS and Linux, expose Porto through that canonical
path with:

```sh
porto docker activate
```

If another runtime already owns a symbolic link there, replacement requires
explicit intent:

```sh
porto docker activate --replace
```

Writing `/var/run/docker.sock` usually requires administrator privileges. Porto
prints an exact retry command that preserves the current user's Porto state.
For the default macOS installation, the equivalent command is:

```sh
sudo env PORTO_HOME="$HOME/Library/Application Support/porto" \
  "$(command -v porto)" docker activate --replace
```

Verify the canonical endpoint independently of the selected Docker context:

```sh
readlink /var/run/docker.sock
docker --host unix:///var/run/docker.sock info \
  --format 'Porto server {{.ServerVersion}}'
```

Porto records the previous symbolic-link target. Restore it with
`porto docker deactivate`; if required, rerun the administrator command printed
by Porto with `docker deactivate` as the final arguments.

Canonical activation is optional: Porto Desktop and
`docker --context porto ...` work without it. It is intended for tools that
hardcode the canonical Unix socket. The Porto socket remains mode `0600`, so
this does not grant other operating-system users access. Windows uses only the
named `porto` context because named-pipe takeover cannot be reversed safely.

### macOS 27 power-notification crash

Electron 44.7.1 handles failed `IORegisterForSystemPower` registration without
dereferencing a null notification port. Its upstream fix is tracked in
[Chromium 69403d85](https://github.com/chromium/chromium/commit/69403d85b78bef2370cc9f8206dce84c5ff63ea4)
and [issue 562777834](https://issues.chromium.org/issues/562777834).
Porto no longer includes a power-notification library, replacement native entry
point, or recovery launcher. Install the current package and launch it normally;
no loader environment or macOS security-setting change is needed.

`node --test scripts/macos-startup.test.cjs` verifies the locked runtime,
cached development app, and packaged app with an isolated synthetic registration
failure. Each must render and exit cleanly without the former workaround.
The test no longer expects or deliberately produces an Electron crash.

### Manual archive installation

Download the archive for your platform and `SHA256SUMS` from the [releases page](https://github.com/mbianchidev/porto/releases). Verify the download, then unpack it:

```sh
sha256sum --check --ignore-missing SHA256SUMS
tar -xzf porto_<version>_<os>_<arch>.tar.gz
```

macOS ships `shasum` instead of GNU `sha256sum`:

```sh
shasum -a 256 --check --ignore-missing SHA256SUMS
```

Windows CLI/web and portable desktop releases use `.zip` archives. The
recommended desktop download is the architecture-specific `.exe` installer.

Each archive has this layout:

```text
porto_<version>_<os>_<arch>/
  porto            # porto.exe on Windows
  ui/dist/         # dashboard assets
  README.md
  LICENSE
```

Keep `ui/dist` beside the binary when moving the installation, or set `PORTO_UI_DIR` to the dashboard directory.

Desktop archives use the `porto-desktop_<version>_<os>_<arch>` prefix and
contain the Porto desktop application with the matching Porto binary and compiled
dashboard assets plus portable runtime tools bundled in its resources
directory. CLI/web archives keep the `porto_<version>_<os>_<arch>` prefix.
Native installers use the same desktop prefix with `.dmg` on macOS and `.exe`
on Windows.

## Build from source

Requirements:

- Go 1.27.3 or newer, using a security-patched release
- Node.js 26.12.1 or a newer 26.x release and npm (recommended); Node.js 22.12 or newer and 20.19 remain supported

The minimum Go version follows the `go` directive in `go.mod`.

Building macOS desktop packages or running their startup regression tests also
requires macOS 26 or newer and full Xcode 26 or newer, not just Command Line Tools.
See [macOS packaging setup](ci-cd.md#ci-details) for scoped Xcode selection.
Running the unpackaged desktop from source does not require the icon compiler.

Source builds use standard host tools:

- Docker CLI with Compose for the named Porto context and Compose project orchestration
- `nerdctl` with containerd and BuildKit, or `limactl` for Porto's managed containerd and BuildKit backend
- `kubectl` for Kubernetes inspection
- `k9s` for interactive cluster terminals
- `limactl` for k3s/k0s clusters and standalone Linux VMs
- `qemu-system-*` for snapshot-capable Lima VMs
- `kind` for Kubernetes-in-Porto clusters

Release desktop apps bundle Docker, Compose, Buildx, Dive, kubectl, k9s, and Lima.
Kind is bundled except on Windows ARM64. Windows packages bundle QEMU; macOS packages do
not because upstream does not publish relocatable binaries and package-manager
builds have large architecture-specific dynamic library closures. On macOS, run
`brew install qemu` and restart Porto. Source builds can install the other
providers with `porto runtime install lima|kind|k9s|k0s`.

Porto keeps native project orchestration and Docker API health checks available when optional execution backends are missing.

From the repository root:

```sh
npm --prefix ui ci
npm --prefix ui run build
go build -o porto ./cmd/porto
```

To run the desktop shell from source:

```sh
npm --prefix ui run desktop:install
npm --prefix ui run desktop
```

On macOS, `desktop` (or `npm --prefix ui/electron start`) prepares a cached
**Porto Dev.app** using Electron's original native executable. It copies
Electron into a checkout/runtime-specific directory under
`~/Library/Caches/Porto/development/`; it never edits the npm-installed
`Electron.app`, installed Porto application, or helper processes. The copied
bundle embeds the source entry and is ad-hoc signed for local development;
preparation no longer compiles or links a native workaround.

The launcher prints the exact app path. Reopen **Porto Dev.app** from that path
or its Dock icon: the bundle remembers this checkout and loads its current
source even without the original command-line arguments. Opening
`node_modules/electron/dist/Electron.app` directly is not equivalent; it is the
generic Electron runtime and can show its welcome screen without an app path.
Keep the source checkout in place, and rerun the npm command after moving it or
changing the Electron runtime. Cached dev bundles remain immutable while
running, and source edits are loaded without repackaging.

This remains development mode: packaged-only updates, login registration, and
bundled-daemon replacement are not enabled by the cached wrapper. Windows and
Linux keep using their installed Electron runtime with an absolute source app
path.

The resulting `porto` binary contains the daemon and CLI. It looks for dashboard assets in this order:

1. the directory set by `PORTO_UI_DIR`;
2. `ui/dist` in the working directory;
3. `ui/dist` next to the executable;
4. `dist` next to the executable.

For dashboard development, run `npm --prefix ui run dev`. See [continuous integration and releases](ci-cd.md) for all local validation commands and release details.

To place Porto on your `PATH` and keep the daemon running across daily sessions, continue with the [daily-use guide](daily-use.md).
