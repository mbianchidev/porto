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
OS/architecture archive, verify it against `SHA256SUMS`, install it for the
current user, add a `porto` CLI entry, and launch the app. Set
`PORTO_VERSION=v1.0.0` to install a specific release or `PORTO_NO_LAUNCH=1` to
install without opening it.

Desktop archives contain Porto, its dashboard, `kubectl`, Lima, and the
supported `kind` binary for that platform. Linux installation installs QEMU
through `apt`, `dnf`, `pacman`, or `zypper` when it is missing; Windows uses
`winget` when available. Set `PORTO_SKIP_PREREQS=1` to skip that step.

macOS releases still need Developer ID signing/notarization for warning-free
launches, and Windows may show SmartScreen until releases are signed. Docker
and Compose currently require a reachable Docker-compatible engine; the
desktop app, native project orchestration, VM management, and k3s/k0s cluster
management do not require a separate Porto installation.

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

Windows releases use `.zip` archives.

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

## Build from source

Requirements:

- Go 1.25 or newer
- Node.js 22.12 or newer (Node.js 20.19 is also supported) and npm

Source builds use standard host tools:

- Docker CLI for the named Porto context and existing Compose project orchestration
- `nerdctl` with containerd, or `limactl` for Porto's managed containerd backend
- `kubectl` for Kubernetes inspection
- `limactl` for k3s/k0s clusters and standalone Linux VMs
- `kind` for Kubernetes-in-Docker clusters

Release desktop apps bundle these provider clients. On macOS source builds can
install them explicitly from Porto with
`porto runtime install lima|kind|k0s`.

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
