# Porto - Local Development Orchestrator

[![CI](https://github.com/mbianchidev/porto/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/mbianchidev/porto/actions/workflows/ci.yml)
[![CodeQL](https://github.com/mbianchidev/porto/actions/workflows/codeql.yml/badge.svg?branch=main)](https://github.com/mbianchidev/porto/actions/workflows/codeql.yml)

Porto is an open-source CLI, daemon, and lightweight React dashboard for managing runnable projects on a development machine. It discovers local repos, tracks their process IDs and ports in a small SQLite database, prevents port collisions, can pull the active Git branch before startup, captures logs, and exposes friendly local hostnames.

## Features

- Go CLI and daemon with a small SQLite database under `~/.config/porto/porto.db` (override with `PORTO_HOME`).
- React dashboard served by the daemon for one-click start, stop, restart, and kill actions.
- Project discovery across user-selected roots plus automatic daemon-start scanning of `~/.copilot/copilot-worktrees`.
- Detection priority: `Makefile`, Compose files, `package.json` scripts, Python entry points, Go mains, then Rust binaries.
- One-click dependency setup using Make setup targets, no-cache Compose builds, Node lockfiles, Python virtual environments, Go modules, or Cargo.
- Stable automatic port assignment starting at `41000`, with pinned port overrides.
- PID, status, port, branch, dirty state, and persistent stdout/stderr tracking with dashboard filtering and clearing.
- Dashboard branch switching with automatic restart, plus concurrent branch instances backed by managed Git worktrees.
- Pre-start `git pull --ff-only` by default, with `--no-pull` when needed.
- Optional automatic cleanup of fully merged local and remote branches, with pruning and protected branch patterns.
- Optional [sql-not-so-lite](https://github.com/mbianchidev/sql-not-so-lite) database discovery for orchestrated projects that contain SQLite files.
- Optional macOS [KillSwitch](https://github.com/mbianchidev/kill-switch) integration for active port visibility and stale dev-server cleanup.
- Optional [Sendbox](https://github.com/mbianchidev/sendbox) sessions for projects with a `.sendbox.yaml` configuration.
- Trusted HTTPS hostname routing for simple and dotted project names, with one-time macOS setup for portless `https://<project>.porto.localhost/` links.
- Zero-configuration HTTP compatibility via `http://<project>.porto.localhost:37680`.
- Multiplatform design using Go and a pure-Go SQLite driver for Linux, macOS, and Windows.

## Install a release

Download the archive for your platform from the [releases page](https://github.com/mbianchidev/porto/releases), verify it, and unpack it:

```sh
sha256sum --check --ignore-missing SHA256SUMS
tar -xzf porto_<version>_<os>_<arch>.tar.gz
```

macOS ships `shasum` instead of GNU `sha256sum`, so verify with `shasum -a 256 --check --ignore-missing SHA256SUMS` unless coreutils is installed.

Each archive contains the `porto` binary next to the dashboard assets in `ui/dist`. Keep that layout, or point `PORTO_UI_DIR` at the dashboard directory.

## Install from source

Requirements:

- Go 1.25+
- Node.js 22.12+ (or 20.19+) and npm for building the dashboard

```sh
npm --prefix ui ci
npm --prefix ui run build
go build -o porto ./cmd/porto
```

The binary carries the whole daemon and CLI, and loads the dashboard from `PORTO_UI_DIR`, `ui/dist` in the working directory, or `ui/dist` or `dist` next to the executable. The React UI is intentionally simple to self-host: run `npm --prefix ui run dev` during UI development, or build static assets with `npm --prefix ui run build`.

Each project card includes **Setup dependencies**. Porto runs one setup at a time per project, writes its output to the process console, and keeps the setup running if the browser disconnects. Stop a project before setting it up.

## Quickstart

```sh
porto scan ~/code ~/work --depth 3
porto list
porto daemon start
porto start api
porto logs api --stream stdout -n 100
porto stop api
```

Open the dashboard at:

```text
http://127.0.0.1:37623
```

On macOS, install the trusted portless HTTPS helper once:

```sh
porto https install
```

This opens the native macOS administrator authorization dialog only to install a dedicated TCP forwarder on port 443. The Porto daemon and managed projects remain unprivileged. If a project has hostname `api`, access it at:

```text
https://api.porto.localhost/
```

The existing zero-configuration HTTP route remains available at `http://api.porto.localhost:37680`.

Project names keep valid dots in their generated hostnames. For example, `devoidofbeauty.com` is routed as `devoidofbeauty.com.porto.localhost`, not `devoidofbeauty-com.porto.localhost`.

## CLI

```text
porto scan [roots...] --depth 3 [--ignore .git,vendor,dist,target]
porto list
porto daemon start|status
porto start|stop|restart|kill <project> [--no-pull]
porto logs <project> [-n 200] [--stream all|stdout|stderr] [--clear]
porto cert path|generate|trust|untrust|status
porto https install|status|uninstall
porto branch <project> <branch>
porto port <project> <port>
porto kill-switch status|install|sync|cleanup
porto sendbox start|stop <project>
```

## Discovery rules

Porto walks each selected root up to the requested depth. It always ignores `node_modules` and also honors the comma-separated `--ignore` list. When a runnable project is found, detection stops for that subtree. Paths are canonicalized so overlapping roots or symlink aliases do not create duplicate projects.

The daemon also scans the current user's `~/.copilot/copilot-worktrees` directory at startup when it exists. This uses the current home directory reported by the operating system and does not hardcode a username.

Run strategy priority:

1. `Makefile` / `makefile`, preferring `dev`, `run`, or `start` targets.
2. `docker-compose.yml`, `docker-compose.yaml`, `compose.yml`, or `compose.yaml`.
3. `package.json`, preferring `scripts.start` then `scripts.dev` and honoring pnpm, Yarn, Bun, or npm lockfiles.
4. Python projects with `requirements.txt` or `pyproject.toml` and `manage.py`, `main.py`, or `app.py`.
5. Go modules with a root `main.go`.
6. Rust crates with `src/main.rs`.

For Compose projects, `porto kill` runs `docker compose down --remove-orphans`. Compose first applies each service's graceful shutdown behavior, then Porto removes running, created, exited, and orphaned containers before reaping the local launcher.

Dependency setup follows the project strategy and manifests:

- Make projects use the first available `install`, `setup`, `bootstrap`, `deps`, or `dependencies` target.
- Compose projects run `docker compose build --no-cache`.
- Node projects use pnpm, Yarn, Bun, or npm based on their lockfile, and run the build script when Porto detected a production `start` script.
- Python projects use uv, Poetry, Pipenv, or a project-local `.venv` with pip.
- Go and Rust projects run `go mod download` or `cargo fetch`.

## Persistence

Porto stores project metadata, runtime state, pinned ports, and logs in SQLite:

```text
~/.config/porto/porto.db
```

Set `PORTO_HOME=/path/to/dir` to choose another location, which makes self-hosted or portable setups easy.

Project output is stored in the same database. `porto logs` and the dashboard process console can show all entries or only stdout/stderr. Clearing is scoped to the selected project and stream; `--stream all --clear` removes every stored log entry for that project.

## Branch switching and instances

Each project card lists the repository's local and remote-tracking branches. Selecting a different running branch stops the process, switches the worktree, updates its HTTPS hostname, and restarts it. Porto refuses the switch when the worktree is dirty or the target branch is already checked out elsewhere.

Use **New instance** to run another branch without disturbing the original checkout. Porto creates a managed Git worktree under `~/.config/porto/worktrees` (or `$PORTO_HOME/worktrees`), gives it an independent process, port, logs, and controls, and keeps the default branch on the base hostname. Other branches use compact labels: `copilot/improve-elemental-resistances-system` becomes `cop-imp-ele-res-sys`, so a project named `2dnd` receives `https://2dnd-cop-imp-ele-res-sys.porto.localhost/`. Porto shortens long labels and adds a deterministic suffix when needed to keep every hostname valid and unique.

Managed instances can be removed from their project card after their worktree is clean. Porto stops the process, removes the Git worktree, and deletes only that instance's runtime metadata and logs.

## Branch cleanup

Open the dashboard's **Branch hygiene** panel to enable automatic local or remote cleanup. Porto checks every 10 seconds and only removes branches whose complete Git history is already reachable from the repository's default branch. The current branch, default branch, unmerged branches, and configured protected names or glob patterns are never removed.

Remote cleanup is disabled by default and requires confirmation in the dashboard because it permanently deletes branches from the primary Git remote. Optional pruning runs `git fetch --prune` with interactive credential prompts disabled. Squash-merged and rebase-merged branches are intentionally left alone unless Git can prove their complete history is merged.

## sql-not-so-lite integration

Enable **sql-not-so-lite** from the dashboard's **Optional integration** panel. Porto checks managed project roots for files with SQLite extensions and validates the SQLite file header before doing any external work.

If no orchestrated project contains a valid SQLite database, Porto does not install or run anything. When an eligible project exists, Porto uses an existing `sqnsl` binary from `PATH`, or installs the pinned integration revision with Go, then runs:

```sh
sqnsl scan <project-path>...
```

Daemon activation and rescans run in the background and expose their current state in the dashboard. Offline `porto scan` commands run the integration synchronously. Integration output and failures are recorded in eligible project logs under the `sqnsl` stream.

## KillSwitch integration

On macOS, enable **KillSwitch** from the dashboard's **Optional integration** panel. Porto syncs only ports belonging to processes the current daemon is actively managing. KillSwitch stores those source-owned ports separately, so it does not replace the ports configured in KillSwitch itself.

Installation is always explicit. Use the dashboard's **Install KillSwitch** action or run:

```sh
porto kill-switch install
```

After installation, Porto can sync active ports automatically and delegate a cleanup pass to KillSwitch. Cleanup follows KillSwitch's own auto-kill, age, runtime, indicator, and exclusion settings. See [KillSwitch integration details](docs/kill-switch.md) for platform requirements, command behavior, and failure handling.

## Sendbox integration

Install [Sendbox](https://github.com/mbianchidev/sendbox) on a compatible macOS 26 Apple Silicon machine, then initialize each project that should expose the integration:

```sh
sendbox init --project /path/to/project
```

This creates `.sendbox.yaml`. Enable **Sendbox** in Porto's dashboard, then use **Run in Sendbox** / **Stop Sendbox**, or:

```sh
porto sendbox start <project>
porto sendbox stop <project>
```

Porto runs `sendbox run --config <project>/.sendbox.yaml --project <project>` and captures its output in the project's existing logs under the `sendbox` and `sendbox-stderr` streams. Porto does not install, require, or run Sendbox when no managed project contains `.sendbox.yaml`.

Sendbox sessions are independent from normal Porto processes. They do not receive Porto's automatic port assignment and are not routed through Porto's local hostnames; avoid running both modes simultaneously when they would bind the same host port.

## Local HTTPS, certificates, and DNS

When the daemon starts, Porto creates a persistent ECDSA local certificate authority and a renewable server certificate under:

```text
~/.config/porto/certificates/porto.local.pem
~/.config/porto/certificates/porto.local-key.pem
~/.config/porto/certificates/porto-root-ca.pem
~/.config/porto/certificates/porto-root-ca-key.pem
```

Use `porto cert path` to print the active paths or `porto cert generate` to replace the base server certificate. Renewal updates a running daemon immediately, and the daemon checks daily for certificates within 30 days of expiry. The base certificate covers only `porto.local`, `*.porto.local`, `porto.localhost`, `*.porto.localhost`, `localhost`, and loopback IP addresses. Dotted project names receive isolated in-memory certificates containing only their exact hostname, selected through TLS SNI. Server certificate renewal keeps the same trusted local authority, so browser trust persists. Private keys are written with owner-only permissions on Unix.

`porto https install` is supported on macOS. It trusts `porto-root-ca.pem` for SSL in the current user's login keychain and installs a root-owned launchd helper that forwards raw TCP from `127.0.0.1:443` to Porto's unprivileged TLS router on `127.0.0.1:37681`. The helper does not read certificate keys, open Porto's database, or launch projects. Re-run the command after upgrading the Porto binary to refresh the helper. Use `porto https status` to inspect installation, listening, and trust state; `porto https uninstall` removes the port forwarder, while `porto cert untrust` removes certificate trust.

Use `<project>.porto.localhost`, which normally resolves to loopback without configuration. Porto also accepts `.porto.local`, but because `.local` is reserved for mDNS, those aliases require exact names in your local DNS or hosts file, for example:

```text
127.0.0.1 porto.local api.porto.local devoidofbeauty.com.porto.local
```

Hosts files do not support wildcard entries, so add each project hostname separately. For a one-off request without changing DNS:

```sh
curl --resolve api.porto.local:37681:127.0.0.1 https://api.porto.local:37681
```

The HTTP router continues listening on `127.0.0.1:37680` and accepts both `<project>.porto.localhost` and `<project>.porto.local`. Without the portless helper, HTTPS remains available on `:37681`. `PORTO_TLS_ADDR` and `PORTO_TLS_PUBLIC_PORT` remain available for custom forwarding setups. Never run the Porto daemon with `sudo`, because managed projects inherit the daemon's privileges.

## Development

```sh
go test ./...
go build ./cmd/porto
npm --prefix ui run build
npm --prefix ui run lint
```

## Continuous integration and releases

GitHub Actions runs formatting, `go vet`, the Go test suite on Linux, macOS, and Windows, a cross-compilation pass for every release target, and the dashboard lint and build on Node 22.12, 24, and 26. CodeQL and a weekly `govulncheck` plus `npm audit` job cover security, and Dependabot keeps Go modules, dashboard packages, and actions current.

Pushing a `vX.Y.Z` tag builds and publishes archives for Linux, macOS, and Windows on `amd64` and `arm64`, together with a `SHA256SUMS` file and signed build provenance attestations. See [CI/CD details](docs/ci-cd.md).

## License

MIT
