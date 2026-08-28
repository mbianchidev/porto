# Porto - Self-hosted app Orchestrator

[![CI](https://github.com/mbianchidev/porto/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/mbianchidev/porto/actions/workflows/ci.yml)
[![CodeQL](https://github.com/mbianchidev/porto/actions/workflows/codeql.yml/badge.svg?branch=main)](https://github.com/mbianchidev/porto/actions/workflows/codeql.yml)

Porto is an open-source control plane for the projects running on your development machine, NAS, or home system. It discovers mixed-stack repositories and gives you one CLI and dashboard to set them up, start and stop them, inspect logs, manage branches, and open them at predictable local URLs.

![Porto dashboard showing discovered projects and runtime controls](https://github.com/user-attachments/assets/51024275-79dd-46ad-9bfb-72d4ad1f79a1)

## Why Porto

- **One place for every project.** Scan roots once, then control processes, ports, readiness, and persistent logs from the CLI or dashboard.
- **Automatic setup and startup.** Porto recognizes Make, Compose, Node.js, Python, Go, and Rust projects and chooses the appropriate commands.
- **No more port bookkeeping.** Stable automatic assignments avoid collisions, while pinned and Compose-published ports remain supported.
- **Branch-aware workflows.** Switch branches with automatic restarts or run concurrent branches in isolated managed worktrees.
- **Friendly local URLs.** Open projects through zero-configuration HTTP hostnames or trusted portless HTTPS on macOS.
- **Local and portable.** Runtime state stays in a small SQLite database, and the Go daemon runs on Linux, macOS, and Windows.

## Quickstart

### 1. Install Porto

Download the archive for your platform from the [releases page](https://github.com/mbianchidev/porto/releases). Keep the `porto` binary and `ui/dist` directory together, then run the binary from the extracted directory or add it to your `PATH`.

See the [installation guide](docs/installation.md) for checksum verification, source builds, and custom dashboard paths.

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

```sh
porto start api
porto logs api --stream stdout -n 100
porto stop api
```

Replace `api` with a project name shown by `porto list`. Open the dashboard at:

```text
http://127.0.0.1:37623
```

Running projects are also available without setup at `http://<project>.porto.localhost:37680`. On macOS, install the trusted portless HTTPS helper once to use `https://<project>.porto.localhost/`:

```sh
porto https install
```

See [local networking](docs/networking.md) for certificate, DNS, dotted-hostname, and custom-forwarding details.

For an always-available setup, follow the [daily-use guide](docs/daily-use.md) to install Porto in a stable location and start it at login or boot.

## Documentation

- [Installation](docs/installation.md) — release archives, source builds, and dashboard assets
- [Daily use](docs/daily-use.md) — durable installation, login or boot startup, upgrades, and home systems
- [Project management](docs/project-management.md) — CLI reference, discovery, setup, ports, readiness, and logs
- [Branch management](docs/branch-management.md) — switching, concurrent instances, and merged-branch cleanup
- [Local networking](docs/networking.md) — HTTP, HTTPS, certificates, DNS, and forwarding
- [Optional integrations](docs/integrations.md) — sql-not-so-lite, KillSwitch, and Sendbox
- [Continuous integration and releases](docs/ci-cd.md) — local checks, automation, and release artifacts
- [Documentation index](docs/README.md) — all guides

## License

[MIT](LICENSE)
