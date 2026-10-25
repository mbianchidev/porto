# Porto

[![CI](https://github.com/mbianchidev/porto/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/mbianchidev/porto/actions/workflows/ci.yml)
[![CodeQL](https://github.com/mbianchidev/porto/actions/workflows/codeql.yml/badge.svg?branch=main)](https://github.com/mbianchidev/porto/actions/workflows/codeql.yml)

> [!WARNING]
> **Porto is not production-ready.** Use it for local development and experimentation only, not production or critical workloads.
>
> **Releases are not code-signed**, and macOS builds are not notarized. Your operating system may display security warnings or block installation or launch.

Porto is an open-source local development control plane for native applications, Linux containers, Compose stacks, Kubernetes clusters, and Linux virtual machines. A Go daemon and CLI share state with an Electron desktop app or a browser dashboard, so you can discover repositories, start workloads, inspect their data, and manage local runtimes in one place.

Porto runs its own containerd and BuildKit backend rather than proxying another Docker engine. Supported Docker clients and Compose workflows connect through the named `porto` context.

![Porto dashboard showing containers, runtime controls](https://github.com/user-attachments/assets/ed553cf3-1714-431b-83b7-f9443d8a3dab)

## What you can do

- **Run mixed-stack projects.** Discover Make, Compose, Node.js, Python, Go, and Rust applications; set up dependencies; and manage processes, stable ports, HTTP readiness, and persistent stdout/stderr logs.
- **Use Docker tooling and inspect runtime data.** Manage containers, images, builds, networks, and volumes with Docker, Compose, and Buildx. Inspect image layers with Dive, browse runtime files and stats, create local volume backups, and preview protected cleanup.
- **Manage local Kubernetes.** Create k3s, k0s, or kind clusters with scoped kubeconfigs and k9s terminals. Perform [cluster upgrades](docs/kubernetes-upgrades.md), provision bounded k3s/k0s workers with Karpenter, and scale replicas with KEDA on all three distributions. See [autoscaling](docs/kubernetes-autoscaling.md).
- **Create Linux VMs.** Use Lima-backed Ubuntu, CentOS Stream, openSUSE, NixOS, Arch, or Alpine machines, with snapshots where the provider supports them. Kali images are available only for compatible architectures. See [local runtimes](docs/runtimes.md).
- **Keep branches and local URLs organized.** Switch branches with automatic restarts or run concurrent branches in managed worktrees. Open projects and managed Kubernetes Services through local HTTP hostnames, with optional trusted portless HTTPS on macOS.
- **Source-preserving migration and native files.** Dry-run selected local Docker runtime transfers and open capability-supported read-only or explicitly writable native filesystem bridges.
  Per-object migration checkpoints survive daemon interruptions; retries require a fresh dry run and never retarget recreated source names.
  Native capabilities and write consent are resource-identity-bound, with visible disconnected/failed-detach states. Windows native folders use WinFsp (explicit driver installation); unattached source volumes can use separately consented stopped, read-only helpers.
  macOS native folders use the built-in WebDAV filesystem over confined SFTP, without a third-party driver.
  Linux bridges require FUSE3 and SSHFS 3.7.6+ or an equivalent maintained build with symlink containment.
  CLI `--resume` retains exact migration identities and destinations.
  Required CI covers native Linux backends on both architectures, KVM-backed x64 Lima, and real desktop filesystem drivers on both architectures; software-emulated Lima integration is [explicit opt-in acceptance](docs/data-management.md#real-runtime-acceptance).

## Platforms and prerequisites

Releases target **macOS, Linux, and Windows** on **x86-64 (`amd64`) and ARM64 (`arm64`)**. Choose the desktop package for a graphical session, or the CLI/web archive for a headless machine.

| Platform | Desktop package | Runtime requirements and boundaries |
| --- | --- | --- |
| macOS | DMG | Lima is bundled. Install QEMU with `brew install qemu` for snapshot-capable VMs, then restart Porto. |
| Linux | Portable archive | The installer attempts to install missing QEMU through `apt`, `dnf`, `pacman`, or `zypper`. KVM access enables hardware acceleration. |
| Windows | EXE installer | Lima and QEMU are bundled. kind is not bundled on Windows ARM64 because upstream has no native binary. |

Desktop packages include the daemon, dashboard, Docker CLI, Compose, Buildx, Dive, `kubectl`, `k9s`, Lima, and kind where supported. They do **not** require Go, Node.js, or a separate Docker Desktop installation.

They also include Porto's Linux runtime helper for direct container I/O, Windows
containerd connections, CNI, and checkpoint capability discovery. New Windows
container engines use Ubuntu 24.04 LTS. Bundled Lima includes stale-process
recovery and consoleless graceful/forced shutdown fixes, without requiring
manual PID-file deletion or VM recreation. See
[Windows guest recovery](docs/installation.md#windows-guest-recovery).

Native applications still need Git and their own language runtimes, package managers, and build tools in the daemon's `PATH`. Missing optional runtime providers do not prevent native project orchestration. Windows Explorer access to runtime files additionally requires an explicitly installed [WinFsp 2.1+ driver](https://winfsp.dev); in-app file browsing and volume archives do not.

See [installation](docs/installation.md) for platform details and [runtime requirements](docs/runtimes.md#requirements) for source and CLI-only installations.

## Quickstart

### 1. Install Porto

Install and launch the latest desktop release on **macOS or Linux**:

```sh
curl -fsSL https://raw.githubusercontent.com/mbianchidev/porto/main/scripts/install-desktop.sh | sh
```

On **Windows PowerShell**:

```powershell
irm https://raw.githubusercontent.com/mbianchidev/porto/main/scripts/install-desktop.ps1 | iex
```

The installers select your OS and architecture, verify the release's `SHA256SUMS`, install for the current user, expose the CLI, and launch Porto. On macOS/Linux, ensure `~/.local/bin` is on your `PATH`:

```sh
export PATH="$HOME/.local/bin:$PATH"
```

On Windows, open a new terminal after installation so it picks up the updated user `Path`.

If macOS Gatekeeper blocks launch, only unblock a trusted download from the [official releases page](https://github.com/mbianchidev/porto/releases) after checksum verification. For the one-line installer's default location:

```sh
xattr -drs com.apple.quarantine "$HOME/Applications/Porto.app"
```

Reopen that app afterward. For a manual DMG installation, copy `Porto.app` into `/Applications` first and use that path instead. Do not disable Gatekeeper globally. See [macOS launch troubleshooting](docs/installation.md#macos-gatekeeper-quarantine).

For a **headless installation**, download and verify the CLI/web archive from the [releases page](https://github.com/mbianchidev/porto/releases). Keep the `porto` binary (`porto.exe` on Windows) and `ui/dist` directory together, then add the binary's directory to `PATH`. See [manual archive installation](docs/installation.md#manual-archive-installation).

### 2. Check the daemon

**The desktop app starts the daemon automatically** and prepares the container backend on first launch when Docker support is enabled. Do not start a second daemon.

For a CLI/web installation, start it yourself:

```sh
porto daemon start
```

This command runs in the foreground. Leave it open and use a second terminal for the commands below. `Ctrl+C` gracefully stops the daemon and its managed projects.

```sh
porto version
porto daemon status
```

`porto daemon status` should print `running`. Open the dashboard at **http://127.0.0.1:37623**. For startup problems, run `porto diagnose` and consult [diagnostics](docs/diagnostics.md) or [Windows guest recovery](docs/installation.md#windows-guest-recovery).

### 3. Discover projects

```sh
porto scan --depth 3 ~/code ~/work
porto list
```

Replace the roots with existing workspace directories. Put scan flags before the roots. Porto detects the first runnable strategy in each subtree; it does not treat every repository or nested package as a separate application. See [discovery rules](docs/project-management.md#discovery).

### 4. Start a project

```sh
porto start api
porto logs api --stream stdout -n 100
porto stop api
```

Replace `api` with a project name shown by `porto list`. Startup normally runs `git pull --ff-only`; use `porto start --no-pull api` to skip it. Install project dependencies yourself or use the dashboard's **Setup dependencies** action before starting.

Automatic port assignment starts at `41000`; Compose-published ports are also supported. Porto marks an application ready when its HTTP endpoint returns 200. Pin a port with `porto port api 8080`. Running projects are also available at `http://<project>.porto.localhost:37680` without DNS setup.

On macOS, install the trusted portless HTTPS helper once to use `https://<project>.porto.localhost/`:

```sh
porto https install
```

This requests administrator authorization for the loopback forwarder and trusts Porto's local certificate authority. See [local networking](docs/networking.md) for certificate, DNS, dotted-hostname, and custom-forwarding details.

For an always-available setup, follow the [daily-use guide](docs/daily-use.md) to install Porto in a stable location and start it at login or boot.

## Use Docker and optional runtimes

Once the execution backend is ready, select Porto's Docker context and use Docker normally:

```sh
docker context use porto
docker run --rm alpine:latest echo hello
docker ps --all
```

The container example should print `hello` and remove itself. From a Compose project directory, use `docker compose up --detach`. If `DOCKER_HOST` or `DOCKER_CONTEXT` is set, unset it so it does not override the selected context. CLI/web installations can provision the backend with `porto docker engine-install`; desktop packages do this automatically.

`porto docker <command>` is also available: it uses Docker's flags and defaults to Porto's endpoint. Non-conflicting shortcuts include `porto ps`, `porto exec`, and `porto compose`. **`porto start`, `porto stop`, and `porto logs` operate on projects**; use `docker stop` and `docker logs` for containers. `porto docker cli` is unrestricted passthrough and honors explicit or inherited Docker connection settings.

Inspect optional providers with `porto runtime providers` and enable features independently:

```sh
porto runtime enable kubernetes
porto runtime enable vms
porto runtime status
```

See [local runtimes](docs/runtimes.md) for container, Kubernetes, VM, and provider-install commands, and [runtime data management](docs/data-management.md) for storage, backups, migration, and native file access.

## Configuration and local state

Set environment variables in the environment that launches Porto. Keep the same user and `PORTO_HOME` for the desktop, daemon, and CLI.

| Variable | Purpose |
| --- | --- |
| `PORTO_HOME` | Override the directory containing Porto's SQLite database, logs, certificates, and managed runtime metadata. |
| `PORTO_UI_DIR` | Select compiled dashboard assets when `ui/dist` is not beside the binary or in the working directory. |
| `PORTO_LOG_LEVEL` | Set application diagnostics to `debug` (default), `info`, `warn`, or `error`. Restart the desktop and daemon after changing it. |

The default state directory is `~/Library/Application Support/porto` on macOS, `${XDG_CONFIG_HOME:-~/.config}/porto` on Linux, and `%APPDATA%\porto` on Windows. Project state and output live in `porto.db`; application diagnostics live separately in `logs/porto.log`. Completed diagnostic days are compressed, with seven calendar days retained by default. See [diagnostic logs](docs/daily-use.md#application-diagnostic-logs).

Use **Settings** for runtime and desktop preferences, log retention, and optional cleanup schedules. Installed desktop builds offer **Help > Check for updates**, plus update actions in Settings and the menu bar/system tray. See [desktop updates](docs/installation.md#desktop-updates).

## Important limitations

- **Local development only.** The dashboard/API bind to loopback, and Docker clients use a private Unix socket or Windows named pipe. Do not expose these control interfaces through an unauthenticated network proxy.
- **Docker compatibility is bounded.** Porto supports Docker API versions **1.41 through 1.47**, not every Docker Engine feature. Swarm, generic engine plugins, and Windows containers are unsupported; mounts, checkpoints, SDKs, Dev Containers, and Testcontainers have documented capability limits. Check the [compatibility matrix](docs/docker-engine.md#docker-api-compatibility-matrix) before relying on a workflow.
- **Discovery and readiness are opinionated.** Detection follows file and entry-point conventions, and readiness expects HTTP 200 on the selected port. See [project management](docs/project-management.md) before using non-HTTP services or unusual monorepo layouts.
- **Backups are not database-aware.** Local volume copies are crash-consistent, not application-consistent database backups. Native filesystem access and migration support depend on the host/backend. See [data-management safety boundaries](docs/data-management.md).
- **Layer inspection is not vulnerability scanning.** Dive works today; local SBOM and Grype-based CVE analysis remain a [separate roadmap](docs/image-inspection.md).

## Build from source

Build with a security-patched **Go 1.27.3 or newer** release and **Node.js 26 with npm** for the dashboard. Node.js 22.12 remains the compatibility minimum. Release packages do not need these build tools.

```sh
git clone https://github.com/mbianchidev/porto.git
cd porto
npm --prefix ui ci
npm --prefix ui run build
go build ./cmd/porto
```

Run `./porto daemon start` from the repository root, then follow the quickstart in another terminal using `./porto` instead of `porto`. On Windows PowerShell, use `.\porto.exe`.

For the Electron desktop shell, after building the daemon and dashboard:

```sh
npm --prefix ui run desktop:install
npm --prefix ui run desktop
```

On macOS this prepares a cached **Porto Dev.app** tied to the source checkout, without modifying the npm-installed Electron runtime. Packaging macOS desktop archives/DMGs and running startup regression tests require macOS 26+ and full Xcode 26+, not just Command Line Tools. See [source installation](docs/installation.md#build-from-source) and [packaging setup](docs/ci-cd.md#ci-details).
The packaging guide also documents parallel target builds and the optional
checksum-verified download cache.

## Documentation

Start with the [documentation index](docs/README.md), or go directly to a guide:

| Guide | Topics |
| --- | --- |
| [Installation](docs/installation.md) and [daily use](docs/daily-use.md) | Releases, source builds, startup at login/boot, and updates |
| [Project management](docs/project-management.md) and [branches](docs/branch-management.md) | Discovery, setup, CLI commands, ports, readiness, logs, and worktrees |
| [Local runtimes](docs/runtimes.md) and [Docker Engine](docs/docker-engine.md) | Containers, Compose, Kubernetes, VMs, and Docker API compatibility |
| [Kubernetes upgrades](docs/kubernetes-upgrades.md) and [autoscaling](docs/kubernetes-autoscaling.md) | Upgrade/recovery boundaries, Karpenter workers, and KEDA |
| [Runtime data management](docs/data-management.md) and [image inspection](docs/image-inspection.md) | Files, storage, backups, migration, native mounts, and Dive |
| [Networking](docs/networking.md) and [integrations](docs/integrations.md) | Local HTTP/HTTPS, DNS, sql-not-so-lite, KillSwitch, and Sendbox |
| [Diagnostics](docs/diagnostics.md) | Health checks, redacted bundles, and confirmed repairs |
| [Product overview](docs/product.md) and [design system](docs/design.md) | Intended users, product principles, and interface conventions |
| [CI and releases](docs/ci-cd.md) | Local checks, automated tests, packaging, and release artifacts |

## Contributing

Report bugs or discuss changes in [GitHub issues](https://github.com/mbianchidev/porto/issues), or submit a focused [pull request](https://github.com/mbianchidev/porto/pulls). Include your OS, Porto version, reproduction steps, and redacted diagnostics when reporting a problem.

For code changes, build from source as above, then run `go test ./...` and `npm --prefix ui run lint`. The [local validation guide](docs/ci-cd.md#running-the-checks-locally) covers formatting, vet, desktop tests, and optional integration checks.

## License

[MIT](LICENSE)
