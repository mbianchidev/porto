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
Windows uses an NSIS EXE installer, and Linux uses the portable desktop archive. Set
`PORTO_VERSION=v1.0.0` to install a specific release or `PORTO_NO_LAUNCH=1` to
install without opening it.

The native-engine release advances Porto's daemon compatibility version. The
installer and desktop app replace an older Porto daemon before launching the
new version, so commands cannot accidentally continue through the removed
upstream Docker proxy.

Desktop archives contain Porto, its dashboard, the Docker CLI, `kubectl`, `k9s`,
Lima, and the supported `kind` binary for that platform. Windows ARM64 excludes
the Docker CLI and KinD because upstream standalone binaries are unavailable.
Linux installation installs QEMU
through `apt`, `dnf`, `pacman`, or `zypper` when it is missing; Windows uses
`winget` when available. Set `PORTO_SKIP_PREREQS=1` to skip that step.

On macOS and Linux, the packaged app automatically provisions and starts its
containerd and BuildKit backend on first launch when Docker support is enabled.
It combines the bundled runtime tools with the user's login-shell `PATH`, so
project commands can find package managers and language toolchains installed
outside the system paths available to graphical applications.
Installing and opening the desktop package requires no follow-up runtime setup
command.

macOS DMGs still need Developer ID signing/notarization for warning-free
launches, and Windows installers may show SmartScreen until releases are
signed. Porto provides its own Docker-compatible API, containerd backend,
BuildKit bridge, and Compose backend.

If macOS quarantine blocks an unsigned release, clear the attribute without
following bundled symbolic links:

```sh
xattr -drs com.apple.quarantine "$HOME/Applications/Porto.app"
```

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

Release desktop apps bundle these provider clients. On macOS source builds can
install them explicitly from Porto with `porto runtime install lima|kind|k9s|k0s`.

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
