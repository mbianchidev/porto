# Installation

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

Windows packages bundle Lima `v2.2.0+porto.3`: the stable 2.2.0 source with
[upstream's Windows PID fix](https://github.com/lima-vm/lima/commit/28285d6e58dc38a75b912c76f5b5f0cad534d435)
backported and a consoleless force-stop fix. After an interrupted shutdown,
Lima removes stale host-agent and QEMU PID files when Windows reports that
those processes no longer exist, instead of refusing to start with
`OpenProcess: The parameter is incorrect`. Forced stops terminate the selected
process tree instead of relying on a console event, allowing its log handles
to close before the next startup. If a target exits while the stop command is
running, Lima confirms its process handle has exited before accepting the stop.
Live process IDs are not treated as stale;
configuration errors and VM disks are preserved. macOS and Linux retain the
unmodified upstream Lima binaries.

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

macOS DMGs still need Developer ID signing/notarization for warning-free
launches, and Windows installers may show SmartScreen until releases are
signed. Porto provides its own Docker-compatible API, containerd backend,
BuildKit bridge, and Compose backend.

If macOS quarantine blocks an unsigned release, clear the attribute without
following bundled symbolic links:

```sh
xattr -drs com.apple.quarantine "$HOME/Applications/Porto.app"
```

### Manual DMG installation

Open the DMG, drag `Porto.app` to `/Applications`, and launch it from there.
Then enable the CLI with:

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

On an affected macOS 27 host, `IORegisterForSystemPower` can return a null
notification port and Electron can crash in `IONotificationPortGetRunLoopSource`
before Porto starts or creates its diagnostic log. This was reproduced in the
bundled v1.2.12 release, and Electron 44.4.5 still crashes when registration
fails. Removing quarantine does not fix this native crash.

Until the bundled Electron includes the
[upstream Chromium fix](https://github.com/chromium/chromium/commit/69403d85b78bef2370cc9f8206dce84c5ff63ea4)
for [issue 562777834](https://issues.chromium.org/issues/562777834), new macOS
DMGs and desktop archives include a temporary native guard. Install the
package and launch Porto normally; no source checkout, compiler, or recovery
command is required. The guard is linked only to Porto's macOS main executable,
not to Windows/Linux builds or Electron helper processes. It does not set
`DYLD_INSERT_LIBRARIES` or change system security settings.

Valid notification ports pass through unchanged. Only a null port receives an
inert run-loop source. If this fallback is needed, sleep/wake notifications
are unavailable for that Porto process, and the guard reports this to stderr.
Battery and thermal monitoring remain independent.

For **older releases without the bundled guard**, an opt-in recovery launcher
remains available. From a source checkout, with Xcode Command Line Tools
installed and the Porto desktop closed, run:

```sh
bash hacks/open-porto-macos-27.sh /Applications/Porto.app
```

Pass `$HOME/Applications/Porto.app` instead for a per-user installation. The
launcher builds a universal Intel/Apple Silicon library in
`~/Library/Caches/Porto/macos-power-notification`, then loads it only into that
Porto desktop process. It preserves valid notification ports and uses an inert
run-loop source only for a null port. If that fallback is needed, sleep/wake
notifications are unavailable and the helper reports this to stderr.
The loader environment is cleared before Porto launches child processes.

The recovery launcher does not modify the installed application, daemon, VM
disks, or macOS security settings. The script itself is not bundled and refuses
Windows, Linux, and other macOS versions. Do not disable SIP, Gatekeeper, or
library validation if a signed application refuses the helper.

**Removal requirement:** whenever Electron is upgraded, run
`node --test hacks/macos-power-notification-compat.test.cjs` on macOS. The test
forces power registration to fail without touching real app data. Once the
locked Electron starts without the guard, the test fails with removal
instructions: verify the upstream fix, delete the temporary helpers and
packaged native bootstrap, remove their packaging hooks and these recovery
instructions, and keep the injected-failure case as an unguarded regression
test. Preserve the packaged-app rendering and clean-shutdown checks.
`AGENTS.md` records the same requirement for future agents.

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

- Go 1.26.3 or newer
- Node.js 22.12 or newer (Node.js 20.19 is also supported) and npm

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

The resulting `porto` binary contains the daemon and CLI. It looks for dashboard assets in this order:

1. the directory set by `PORTO_UI_DIR`;
2. `ui/dist` in the working directory;
3. `ui/dist` next to the executable;
4. `dist` next to the executable.

For dashboard development, run `npm --prefix ui run dev`. See [continuous integration and releases](ci-cd.md) for all local validation commands and release details.

To place Porto on your `PATH` and keep the daemon running across daily sessions, continue with the [daily-use guide](daily-use.md).
