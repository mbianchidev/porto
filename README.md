# Porto — Local Project Orchestrator

Porto is an open-source CLI, daemon, and lightweight React dashboard for managing runnable projects on a development machine. It discovers local repos, tracks their process IDs and ports in a small SQLite database, prevents port collisions, can pull the active Git branch before startup, captures logs, and exposes friendly local hostnames.

## Features

- Go CLI and daemon with a small SQLite database under `~/.config/porto/porto.db` (override with `PORTO_HOME`).
- React dashboard served by the daemon for one-click start, stop, restart, and kill actions.
- Project discovery across user-selected roots with `--depth` and ignore lists.
- Detection priority: `Makefile`, then Compose files, then `package.json` scripts.
- Stable automatic port assignment starting at `41000`, with pinned port overrides.
- PID, status, port, branch, dirty state, and log tracking.
- Pre-start `git pull --ff-only` by default, with `--no-pull` when needed.
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
