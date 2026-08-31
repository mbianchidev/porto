# Porto - Self-hosted App Orchestrator

[![CI](https://github.com/mbianchidev/porto/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/mbianchidev/porto/actions/workflows/ci.yml)
[![CodeQL](https://github.com/mbianchidev/porto/actions/workflows/codeql.yml/badge.svg?branch=main)](https://github.com/mbianchidev/porto/actions/workflows/codeql.yml)

Porto is an open-source desktop and web control plane for development workloads running on your machine, NAS, or home system. It discovers mixed-stack repositories and gives you one daemon, CLI, and dashboard to manage native applications, containers, Compose stacks, local Kubernetes clusters, and Linux virtual machines.

![Porto dashboard showing discovered projects and runtime controls](https://github.com/user-attachments/assets/03f957c8-33ba-4fb8-be9c-a41edd4e85cf)


## Why Porto

- **One place for every project.** Scan roots once, then control processes, ports, readiness, and persistent logs from the CLI or dashboard.
- **Mixed stacks, one workflow.** Porto recognizes Make, Compose, Node.js, Python, Go, and Rust projects and chooses the appropriate setup and start commands.
- **Containers without a separate dashboard.** Inspect and operate containers, images, builds, volumes, and networks through Porto or standard Docker clients.
- **Local Kubernetes.** Create managed k3s, k0s, or kind clusters without inheriting an unrelated global kube context.
- **Cluster terminal included.** Open a cluster-scoped k9s session from the dashboard or with `porto kubernetes terminal <cluster>`.
- **Disposable Linux machines.** Create standalone Ubuntu, CentOS Stream, openSUSE, NixOS, Arch, and Alpine environments; Kali is catalogued where an official compatible cloud image exists.
- **No more port bookkeeping.** Stable automatic assignments avoid collisions, while pinned and Compose-published ports remain supported.
- **Branch-aware workflows.** Switch branches with automatic restarts or run concurrent branches in isolated managed worktrees.
- **Friendly local URLs.** Open projects through zero-configuration HTTP hostnames or trusted portless HTTPS on macOS.
- **Local and portable.** Runtime state stays in a small SQLite database, and the Go daemon runs on Linux, macOS, and Windows.
- **Docker and Compose native.** Use the `porto` Docker context for Compose projects and BuildKit multi-platform image builds without proxying another Docker engine.

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
`kubectl`, `k9s`, Lima, and the supported `kind` binary.

For a headless installation, download the CLI/web archive from the
[releases page](https://github.com/mbianchidev/porto/releases). Keep the
`porto` binary and `ui/dist` directory together, then add the binary to `PATH`.

See the [installation guide](docs/installation.md) for checksum verification, source builds, and custom dashboard paths.

CLI/web archives remain available for headless machines and servers.

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
- [Local runtimes](docs/runtimes.md) — Docker socket, containers, Compose, Kubernetes, pod inspection, and Linux VMs
- [Porto Docker Engine](docs/docker-engine.md) — native containerd backend, Docker context compatibility, supported API, and limitations
- [Branch management](docs/branch-management.md) — switching, concurrent instances, and merged-branch cleanup
- [Local networking](docs/networking.md) — HTTP, HTTPS, certificates, DNS, and forwarding
- [Optional integrations](docs/integrations.md) — sql-not-so-lite, KillSwitch, and Sendbox
- [Product overview](docs/product.md) — users, purpose, positioning, and product principles
- [Design system](docs/design.md) — visual language, components, and interaction rules
- [Continuous integration and releases](docs/ci-cd.md) — local checks, automation, and release artifacts
- [Documentation index](docs/README.md) — all guides

## License

[MIT](LICENSE)
