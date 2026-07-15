# Porto - Local Development Orchestrator

Porto is an open-source CLI, daemon, and lightweight React dashboard for managing runnable projects on a development machine. It discovers local repos, tracks their process IDs and ports in a small SQLite database, prevents port collisions, can pull the active Git branch before startup, captures logs, and exposes friendly local hostnames.

## Features

- Go CLI and daemon with a small SQLite database under `~/.config/porto/porto.db` (override with `PORTO_HOME`).
- React dashboard served by the daemon for one-click start, stop, restart, and kill actions.
- Project discovery across user-selected roots with `--depth` and ignore lists.
- Detection priority: `Makefile`, then Compose files, then `package.json` scripts.
- Stable automatic port assignment starting at `41000`, with pinned port overrides.
- PID, status, port, branch, dirty state, and log tracking.
- Pre-start `git pull --ff-only` by default, with `--no-pull` when needed.
- Optional automatic cleanup of fully merged local and remote branches, with pruning and protected branch patterns.
- Optional [sql-not-so-lite](https://github.com/mbianchidev/sql-not-so-lite) database discovery for orchestrated projects that contain SQLite files.
- Optional macOS [KillSwitch](https://github.com/mbianchidev/kill-switch) integration for active port visibility and stale dev-server cleanup.
- Optional [Sendbox](https://github.com/mbianchidev/sendbox) sessions for projects with a `.sendbox.yaml` configuration.
- Local hostname routing via `http://<project>.porto.localhost:37680`.
- Multiplatform design using Go and a pure-Go SQLite driver for Linux, macOS, and Windows.

## Install from source

Requirements:

- Go 1.25+
- Node.js 22+ and npm for building the dashboard

```sh
npm --prefix ui install
npm --prefix ui run build
go build -o porto ./cmd/porto
```

The binary is self-contained for the daemon and CLI. The React UI is intentionally simple to self-host: run `npm --prefix ui run dev` during UI development, or build static assets with `npm --prefix ui run build`.

## Quickstart

```sh
porto scan ~/code ~/work --depth 3
porto list
porto daemon start
porto start api
porto logs api -n 100
porto stop api
```

Open the dashboard at:

```text
http://127.0.0.1:37623
```

If a project has hostname `api`, access it through the local router at:

```text
http://api.porto.localhost:37680
```

## CLI

```text
porto scan [roots...] --depth 3 [--ignore .git,vendor,dist,target]
porto list
porto daemon start|status
porto start|stop|restart|kill <project> [--no-pull]
porto logs <project> [-n 200]
porto branch <project> <branch>
porto port <project> <port>
porto kill-switch status|install|sync|cleanup
porto sendbox start|stop <project>
```

## Discovery rules

Porto walks each selected root up to the requested depth. It always ignores `node_modules` and also honors the comma-separated `--ignore` list. When a runnable project is found, detection stops for that subtree.

Run strategy priority:

1. `Makefile` / `makefile`, preferring `dev`, `run`, or `start` targets.
2. `docker-compose.yml`, `docker-compose.yaml`, `compose.yml`, or `compose.yaml`.
3. `package.json`, preferring `scripts.start` then `scripts.dev`.

## Persistence

Porto stores project metadata, runtime state, pinned ports, and logs in SQLite:

```text
~/.config/porto/porto.db
```

Set `PORTO_HOME=/path/to/dir` to choose another location, which makes self-hosted or portable setups easy.

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

Sendbox sessions are independent from normal Porto processes. They do not receive Porto's automatic port assignment and are not routed through `*.porto.localhost`; avoid running both modes simultaneously when they would bind the same host port.

## Local DNS and routing

Porto does not edit system host files. It serves a reversible opt-in local reverse proxy on `127.0.0.1:37680` and routes hostnames ending in `.porto.localhost` to the assigned project port. This keeps setup simple and works with standard localhost resolution on modern systems.

## Development

```sh
go test ./...
go build ./cmd/porto
npm --prefix ui run build
npm --prefix ui run lint
```

## License

MIT
