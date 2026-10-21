# Porto - Docker Desktop but better

[![CI](https://github.com/mbianchidev/porto/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/mbianchidev/porto/actions/workflows/ci.yml)
[![CodeQL](https://github.com/mbianchidev/porto/actions/workflows/codeql.yml/badge.svg?branch=main)](https://github.com/mbianchidev/porto/actions/workflows/codeql.yml)

> [!WARNING]
> **Porto is not production-ready.** Use it for local development and experimentation only, not production or critical workloads.
>
> **Releases are not code-signed**, and macOS builds are not notarized. Your operating system may display security warnings or block installation or launch.

Porto is an open-source desktop (and optionally web) control plane for development workloads running on your machine, NAS, or home system. It discovers mixed-stack repositories and gives you one daemon, CLI, and dashboard to manage native applications, containers, Compose stacks, local Kubernetes clusters, and Linux virtual machines.

It provides a portable, open-source alternative to proprietary desktop container managers while keeping standard Docker clients and Compose workflows.

![Porto dashboard showing containers, runtime controls](https://github.com/user-attachments/assets/ed553cf3-1714-431b-83b7-f9443d8a3dab)

## Why Porto

- **One place for every project.** Scan your workspace(s) root once, then control processes, ports, readiness, and persistent logs from the CLI or dashboard.
- **Mixed stacks, one workflow.** Porto recognizes Make, Compose, Node.js, Python, Go, and Rust projects and chooses the appropriate setup and start commands.
- **Containers without a separate dashboard.** Inspect and operate containers, images, builds, volumes, and networks through Porto or standard Docker clients.
- **Local Kubernetes.** Create managed k3s, k0s, or kind clusters without inheriting an unrelated global kube context.
- **Kubernetes upgrades.** Upgrade k3s/k0s nodes in place, or replace kind while preserving supported persistent volumes, application images, and manifests. Recovery keeps the original kind nodes available. See [cluster upgrades](docs/kubernetes-upgrades.md).
- **Local autoscaling.** Karpenter provisions bounded k3s/k0s worker VMs for pending pods; KEDA scales workload replicas from event triggers on all three distributions. See [Kubernetes autoscaling](docs/kubernetes-autoscaling.md).
- **Cluster terminal included.** Open a cluster-scoped k9s session from the dashboard or with `porto kubernetes terminal <cluster-name>`.
- **Disposable Linux machines.** Create standalone Ubuntu, CentOS Stream, openSUSE, NixOS, Arch, and Alpine environments; Kali is catalogued where an official compatible cloud image exists.
- **No more port bookkeeping.** Stable automatic assignments avoid collisions, while pinned and Compose-published ports remain supported.
- **Branch-aware workflows.** Switch branches with automatic restarts or run concurrent branches in isolated managed worktrees.
- **Friendly local URLs.** Open projects and managed Kubernetes Services through stable zero-configuration HTTP hostnames or trusted portless HTTPS on macOS.
- **Local and portable.** Runtime state stays in a small SQLite database, and the Go daemon runs on Linux, macOS, and Windows.
- **Docker, Compose, and Buildx included.** Desktop packages ship a pinned Docker CLI plus Compose and Buildx for the `porto` context. Porto's managed engine sets up CPU emulation automatically, without proxying another Docker engine.
- **Docker shortcuts without a second command syntax.** Use `porto docker ps`, `porto docker logs`, `porto docker exec`, or `porto docker compose` with Docker's flags. Non-conflicting commands also work as `porto ps`, `porto exec`, and `porto compose`; project commands such as `porto start`, `porto stop`, and `porto logs` keep their existing meaning.
- **Run containers directly.** Use `porto docker run --rm alpine:latest echo hello`, or pass Docker arguments through with `porto docker cli --context porto run --rm alpine:latest echo hello`. See [local runtimes](docs/runtimes.md).
- **Image layers in the terminal.** Run `porto docker dive IMAGE` with the bundled Dive client to inspect layer contents, file changes, and wasted space.
- **Local diagnostics and repair.** Run `porto diagnose` or use the Diagnostics page to inspect runtime health, preview a redacted local bundle, and apply confirmed Porto-owned repairs.
- **Optional runtime cleanup.** Prune unused images and build cache weekly or with **Run now**, with retained results for both manual and scheduled runs.
- **Inspect and protect runtime data.** Browse shell-independent files with cancellable downloads, stream bounded logs, inspect live/recent counters and redacted metadata, and use **Storage** for per-store usage and exact object/category previews. Clone, empty, restore, or remove volumes with ownership checks; make verified local archives and edit local backup schedules/retention without an account. See [runtime data management](docs/data-management.md).
- **Source-preserving migration and native files.** Dry-run selected local Docker runtime transfers and open capability-supported read-only or explicitly writable native filesystem bridges.
  Per-object migration checkpoints survive daemon interruptions; retries require a fresh dry run and never retarget recreated source names.
  Native capabilities and write consent are resource-identity-bound, with visible disconnected/failed-detach states. Windows native folders use WinFsp (explicit driver installation); unattached source volumes can use separately consented stopped, read-only helpers.
  macOS native folders use the built-in WebDAV filesystem over confined SFTP, without a third-party driver.
  Linux bridges require FUSE3 and SSHFS 3.7.6+ or an equivalent maintained build with symlink containment.
  CLI `--resume` retains exact migration identities and destinations; real-runtime CI exercises both release architectures on Linux, macOS, and Windows.

See the [Docker API compatibility matrix](docs/docker-engine.md#docker-api-compatibility-matrix)
for supported, partial, capability-gated, and explicitly unsupported operations.
Porto enforces Docker API 1.41–1.47, preserves historical event attributes, and
uses the Linux backend's CPU counters for Docker stats. The same matrix documents
tested Docker SDK, Dev Containers, and Testcontainers workflows and their
configuration/capability limits.
See [image layer inspection](docs/image-inspection.md) for Dive usage and the
separate local Grype/SBOM roadmap.

## Quickstart

### 1. Install Porto

Install and launch the latest desktop release on macOS or Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/mbianchidev/porto/main/scripts/install-desktop.sh | sh
```

On Windows PowerShell:

```powershell
irm https://raw.githubusercontent.com/mbianchidev/porto/main/scripts/install-desktop.ps1 | iex
```

The one-liners detect the OS and architecture, verify the published SHA-256
checksum, install the native macOS DMG or Windows EXE package for the current
user, expose the `porto` CLI, and launch the desktop app. Linux uses the
portable desktop archive. Every desktop package bundles the daemon, dashboard,
`kubectl`, `k9s`, Lima, the supported `kind` binary, and Porto's Linux runtime
helper for direct container I/O, Windows containerd connections, CNI, and
checkpoint capability discovery.

New Windows container engines use Ubuntu 24.04 LTS instead of Lima's moving
default image. Windows packages include Lima's stale-process recovery and
consoleless graceful/forced shutdown fixes; normal shutdown recovery does not require
deleting PID files or recreating the runtime VM. See
[Windows guest recovery](docs/installation.md#windows-guest-recovery) for an
existing crashed engine.

For a headless installation, download the CLI/web archive from the
[releases page](https://github.com/mbianchidev/porto/releases). Keep the
`porto` binary and `ui/dist` directory together, then add the binary to `PATH`.

See the [installation guide](docs/installation.md) for checksum verification, source builds, and custom dashboard paths.

Source builds require Go 1.27.3 or newer, using a security-patched release.

macOS desktop packaging and startup regression tests require full Xcode 26 or
newer, not just Command Line Tools. See [packaging setup](docs/ci-cd.md#ci-details).
The same guide documents CI's parallel target builds and optional
checksum-verified download cache for local packaging.

CLI/web archives remain available for headless machines and servers.

Open **Settings** to see the running Porto release version, including prerelease
and build metadata. Installed desktop builds offer **Help > Check for updates**
and a **Check for updates** action in Settings and the menu bar/system tray.

### 2. Discover projects

```sh
porto scan ~/code ~/work --depth 3
porto list
```

### 3. Start the daemon

```sh
porto daemon start
```

The daemon runs in the foreground. Leave it open and use a second terminal for project commands.

Desktop and daemon diagnostics are saved to `logs/porto.log` under Porto's
OS-specific data directory, with **debug** logging enabled by default. Porto
compresses completed days and keeps seven calendar days unless System settings
selects a different retention period. See
[application diagnostic logs](docs/daily-use.md#application-diagnostic-logs)
for exact paths and the `PORTO_LOG_LEVEL` setting.

Electron 44.7.1 fixes the macOS 27 power-notification startup crash without
Porto's former native workaround. See
[macOS compatibility](docs/installation.md#macos-27-power-notification-crash).

### 4. Start a project

```sh
porto start api
porto logs api --stream stdout -n 100
porto stop api
```

Replace `api` with a project name shown by `porto list`. Open the dashboard at:

```text
http://127.0.0.1:37623
```

Running projects are also available without DNS setup at `http://<project>.porto.localhost:37680`. On macOS, install the trusted portless HTTPS helper once to use `https://<project>.porto.localhost/`:

```sh
porto https install
```

See [local networking](docs/networking.md) for certificate, DNS, dotted-hostname, and custom-forwarding details.

For an always-available setup, follow the [daily-use guide](docs/daily-use.md) to install Porto in a stable location and start it at login or boot.

## Documentation

- [Installation](docs/installation.md) — release archives, source builds, and dashboard assets
- [Daily use](docs/daily-use.md) — durable installation, login or boot startup, upgrades, and home systems
- [Project management](docs/project-management.md) — CLI reference, discovery, setup, ports, readiness, and logs
- [Local runtimes](docs/runtimes.md) — Docker socket, containers, Compose, Kubernetes resources, pod inspection, and Linux VMs
- [Porto Docker Engine](docs/docker-engine.md) — native containerd backend, Docker context compatibility, supported API, and limitations
- [Runtime data management](docs/data-management.md) — container inspection, storage accounting, volume archives/backups, migration, and native file-manager capabilities
- [Branch management](docs/branch-management.md) — switching, concurrent instances, and merged-branch cleanup
- [Local networking](docs/networking.md) — HTTP, HTTPS, certificates, DNS, and forwarding
- [Optional integrations](docs/integrations.md) — sql-not-so-lite, KillSwitch, and Sendbox
- [Product overview](docs/product.md) — users, purpose, positioning, and product principles
- [Design system](docs/design.md) — visual language, components, and interaction rules
- [Continuous integration and releases](docs/ci-cd.md) — local checks, automation, and release artifacts
- [Documentation index](docs/README.md) — all guides

## License

[MIT](LICENSE)
