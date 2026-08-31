# Project management

Porto discovers runnable projects, chooses a setup and run strategy, and keeps their runtime state and logs in one place.

## CLI reference

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
porto docker status|containers|images|builds|networks|volumes
porto docker context-install|activate|deactivate
porto kubernetes status|contexts|pods|services|nodes
porto kubernetes cluster create|start|stop|scale|delete
porto runtime status|enable|disable <docker|kubernetes|vms>
porto vm status|images|list|create|start|stop|delete|exec|snapshot|restore
```

`porto daemon start` runs in the foreground. Stopping it with `Ctrl+C` or `SIGTERM` gracefully stops all projects and Sendbox sessions managed by that daemon.

## Discovery

`porto scan` walks each selected root to the requested depth. It always ignores `node_modules` and also honors the comma-separated `--ignore` list. Detection stops for a subtree when Porto finds a runnable project. Paths are canonicalized so overlapping roots and symlink aliases do not create duplicates.

The daemon also scans `~/.copilot/copilot-worktrees` at startup when that directory exists.

Porto selects the first matching run strategy:

1. `Makefile` or `makefile`, preferring `dev`, `run`, or `start` targets.
2. `docker-compose.yml`, `docker-compose.yaml`, `compose.yml`, or `compose.yaml`.
3. `package.json`, preferring `scripts.start` and then `scripts.dev`, while honoring pnpm, Yarn, Bun, or npm lockfiles.
4. Python projects with `requirements.txt` or `pyproject.toml` and `manage.py`, `main.py`, or `app.py`.
5. Go modules with a root `main.go`.
6. Rust crates with `src/main.rs`.

## Dependency setup

The dashboard's **Setup dependencies** action runs one setup at a time per project, writes output to the process console, and continues if the browser disconnects. Stop a project before setting it up.

The setup strategy follows the detected project type:

- Make projects use the first available `install`, `setup`, `bootstrap`, `deps`, or `dependencies` target.
- Compose projects run `docker compose build --no-cache`.
- Node projects use pnpm, Yarn, Bun, or npm according to their lockfile, and run the build script when Porto detected a production `start` script.
- Python projects use uv, Poetry, Pipenv, or a project-local `.venv` with pip.
- Go and Rust projects run `go mod download` or `cargo fetch`.

Porto checks its native Docker and BuildKit backend before Compose setup and
startup, and points Compose at the Porto socket automatically.

## Starting and stopping projects

Before startup, Porto runs `git pull --ff-only` on the active branch by default. If a GitHub SSH key fails, it can retry over authenticated HTTPS. Pass `--no-pull` to skip the update.

Porto runs Git in its own process group. If a Porto-started Git operation times
out or fails, Porto terminates the complete process tree and removes an
`index.lock` only when it observed that exact lock appear during its own
operation. Locks that existed before the operation, were replaced afterward,
or are reported as belonging to another Git process are preserved.

If a repository already contains a lock from an older crash, close any active
Git/editor process and remove that lock manually once. Current Porto operations
do not leave new pending index locks behind.

Automatic port assignment begins at `41000` and avoids collisions. Use `porto port <project> <port>` to pin a port. Compose projects that publish fixed host ports use the responsive published port instead, including projects that wrap Compose commands in Make targets.

After launch, a project remains `starting` until `http://127.0.0.1:<assigned-port>/` returns HTTP 200. A live process stays in that state while its frontend is unavailable and becomes `crashed` if it exits before readiness succeeds.

For Compose projects, `porto kill` runs `docker compose down --remove-orphans`. Compose first applies each service's graceful shutdown behavior, then Porto removes running, created, exited, and orphaned containers before reaping the local launcher.

## State and logs

Porto stores project metadata, runtime state, pinned ports, and output in `porto/porto.db` under the platform's user configuration directory. Common defaults are:

```text
Linux:   ~/.config/porto/porto.db
macOS:   ~/Library/Application Support/porto/porto.db
Windows: %AppData%\porto\porto.db
```

Set `PORTO_HOME=/path/to/dir` to choose another location for self-hosted or portable setups.

`porto logs` and the dashboard process console can show all entries or only stdout or stderr. Clearing is scoped to the selected project and stream; `--stream all --clear` removes every stored log entry for that project.

Dashboard cards also show each project's process ID, readiness status, assigned port, branch, and dirty state. The process console supports filtering and one-click copying and clearing.

For Git branch workflows, see [branch management](branch-management.md). For project URLs and TLS, see [local networking](networking.md).

For containers, the Docker-compatible endpoint, local Kubernetes clusters, pod inspection, and Linux virtual machines, see [local runtimes](runtimes.md).
