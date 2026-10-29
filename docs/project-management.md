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

Index-lock discovery has its own five-second deadline and honors caller
cancellation. The Git command's execution timeout starts after this preflight.

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

`porto logs` and the dashboard process console can show all entries or only stdout or stderr. Clearing is scoped to the selected project and stream; `--stream all --clear` removes every stored log entry for that project. The dashboard console can expand to the full window and returns to its inline position when minimized or when `Escape` is pressed.

Dashboard project rows also show each project's process ID, readiness status, assigned port, branch, and dirty state. Select a row to expand its service channel directly underneath it; select the row again to collapse it. The log quick action opens the same inline channel with the process console active.

For Git branch workflows, see [branch management](branch-management.md). For project URLs and TLS, see [local networking](networking.md).

For containers, the Docker-compatible endpoint, local Kubernetes clusters, pod inspection, and Linux virtual machines, see [local runtimes](runtimes.md).

## Application delivery

Open **Local development > Applications** to create a Compose application or
adopt an existing Compose project. This is delivery metadata for Porto's
existing project, not another application runtime: project IDs, Compose project
labels (`porto-ID`), process logs, stable loopback port overrides, start/stop
controls, branch/worktree isolation and registry handling are shared.
Native-process projects and managed Kubernetes containers cannot be adopted.
Only the local Linux/WSL Compose target is supported; other targets are rejected
until Porto's environment abstraction provides them.

Applications are whole Compose stacks, not single-container shortcuts. Multiple
services, dependency health/completion conditions, shared networks, named
volumes, builds and published ports remain in the effective definition.
Readiness checks every enabled service and every configured replica, including
successful completion jobs; intentionally zero-scaled services do not block
deployment. Services behind inactive Compose profiles are not started or
assigned ports. The delivery UI currently uses the default profile selection.
Updates remove obsolete containers only inside that project's Compose ownership;
they never delete retained volumes. Git-based relative build, bind-mount and
configuration paths resolve from the selected Compose file's directory, including
files in repository subdirectories.
Machine-readable runtime reads keep stdout separate from shell/tool diagnostics,
including container state, inventory enrichment, network/image/volume lists,
metrics, helper probes and owned-engine discovery. Advisory stderr does not
break health-gated dependencies, lifecycle decisions or identity checks.
Failed commands remain failures with bounded diagnostics; partial JSON is never
accepted as successful inspection.
Docker-compatible summaries and inspection resolve full network IDs from actual
backend network metadata. This lets Compose retain an unchanged service during
stack reconciliation rather than recreating it because CNI inspection omitted
an opaque ID. Network lists expose the same full identities, not display-shortened
IDs. Recorded native mount annotations retain named-volume identity in Docker
summaries while preserving the actual OCI source and read-only flags; ordinary
bind mounts are not guessed to be volumes. Runtime helper teardown stops its
owned process group, including
SSH descendants, so repeated health checks and lifecycle operations release
their transport sessions.
Noninteractive Lima FIFO bridges also watch a private lifetime channel: closing
unused application stdin cannot terminate output, while completion or failed
setup releases guest reader jobs and their FIFO directory instead of consuming
SSH multiplexing slots.

Choose **Compose editor**, upload a YAML/JSON file (at most 1 MiB), select an
immutable template version, or provide an explicit Git repository/ref/file.
Built-in HTTP and worker templates declare required image/port/secret variables
and resource limits. Editable copies remain standard Compose; custom templates
use a new name/version rather than replacing an existing version. Keep secret
literals out of reusable templates and supply them through secret variables.

Supply non-secret variables as a JSON object and sensitive variables through
the password-protected secret JSON input. Omitted secret keys retain their
stored values. Host-control environment variables such as `PATH`, `DOCKER_*`,
`COMPOSE_*`, `GIT_*` and loader settings are not application variables.
**Validate and preview** runs maintained Compose configuration validation without
starting services. The preview shows image/resource/port/storage changes and an
effective-definition hash, never environment values. Porto preserves an
existing service/target/protocol's loopback port assignment where available;
new or occupied ports are allocated during activation. The project's runtime
port and recorded effective configuration show the actual result.

**Apply preview** requires explicit confirmation, rejects expired/reused or
concurrently changed previews, and queues one project-owned operation. Direct
project lifecycle actions and delivery actions share the same exclusion gate;
other projects remain independent. Progress, cancellation, health failures,
partial deployment and interrupted-daemon states appear in the application and
**Activity > Operation history**. Failed health checks are not successful
deployments. Failure does not delete the previous successful revision or any
persistent volume.

Recorded definitions also drive the existing project **Start**, **Stop** and
**Setup dependencies** actions. Startup does not implicitly pull a recorded
application's Git source. Delivery builds selected Compose build services with
`--no-cache` before starting them; build errors are visible in redacted project
logs. Canceling an in-progress activation stops its owned launcher and selected
services within a bounded cleanup budget without deleting volumes.

Definitions and rollback snapshots are AES-256-GCM encrypted with an
installation-specific key in the native system credential store. This avoids
native credential-store blob-size limits while retaining credential-store-backed
Git tokens, webhook keys and secret variables. Unlock the system credential
store before editing/deploying; an unavailable or missing key never falls back
to plaintext. Compose's active effective definition must exist as a private
`0600` runtime file. Raw definitions require an explicit reveal action and may
contain user-written inline values; stored secret-variable values are never
returned by that action. Previews, operation history and diagnostic exports
exclude the encrypted payload and runtime definition, and project log capture
redacts known stored secret values. Applications can still emit transformed or
unrecognized sensitive data; treat workload output accordingly.

### Git ownership and automatic updates

Git sources support credential-free HTTPS URLs and explicitly selected local
repositories, an exact branch/tag/commit ref, and a confined relative Compose
path. Private HTTPS access uses a token scoped to that project and exact
repository, stored behind the system credential store. Tokens are passed only
to Git's scoped HTTPS header configuration, never URL/command arguments or
repository files. Cross-host redirects, external Git transports, inherited
credential helpers and checkout hooks are disabled. Immutable commit checkouts
are separate from the user's repository/worktree. Submodule/LFS initialization
and SSH credential onboarding are not provided by this workflow.

Repositories and Compose definitions are executable workload configuration, not
an untrusted-code sandbox. Review trusted source, bind mounts, capabilities,
build contexts and image references before opting into updates.

Polling is **off by default**. An opted-in interval is 60-86400 seconds; failed
attempts record actionable history and exponential backoff up to one hour.
Manual update checks and authenticated webhook notifications fetch only the
approved ref. An already-deployed commit does not redeploy again. Concurrent
actions are rejected, and daemon restarts never turn interrupted work into
success.

Direct edits to a Git-owned definition require **Detach Git ownership**, which
turns polling/webhooks off. Out-of-band changes to the active definition block
reconciliation instead of being overwritten. Review a new preview with explicit
drift-replacement consent or choose a recorded rollback.

Webhooks use a **separate loopback-only, webhook-only listener** whose current
URL is shown on the application. It has no project/control API routes. The
port can change after restarting Porto. Never expose Porto's unauthenticated
local control API; an optional trusted HTTPS relay must forward only the
authenticated webhook listener.

The JSON body contains the exact approved `repository` and `ref`. Send:

```text
X-Porto-Timestamp: Unix timestamp in seconds
X-Porto-Delivery: unique 8-128 character delivery ID
X-Porto-Signature: sha256=<hex HMAC-SHA256>
```

Sign the exact UTF-8 bytes `timestamp + "\n" + deliveryID + "\n" + body` using the
configured 32-256 byte secret. Timestamps outside five minutes, invalid HMACs,
unapproved sources, repeated delivery IDs and concurrent admissions are rejected.
Accepted replay identities are persisted across restarts. Native provider
webhook formats need a trusted relay that verifies the provider request and
creates this local signed envelope.

### Recorded rollback and recovery

Each successful deployment records its exact Git commit, base/effective
configuration hashes and available immutable service image references.
**Preview rollback** compares a selected revision with the current definition;
applying it creates another revision and pauses automatic updates. Recorded
image digests use local-only pull policy; unavailable original images fail
visibly rather than silently pulling a mutable replacement tag. Definitions
without complete image attribution carry an explicit rollback warning.

Rollback is **not database/schema or persistent-data rollback**. It never uses
`down --volumes`, deletes volumes, or promises reversal of application migrations,
external side effects or storage changes. A failed rollback remains partial
with the previous successful revision available for investigation. Keep the
credential-store key and Porto state together when backing up deployment
history; losing the key makes encrypted historical definitions unavailable.

Synthetic delivery tests cover editor/upload/template/Git sources, immutable
commits, ownership, drift, cancellation, failed health/rollback, webhook
authentication/replay and secret redaction. Native Compose validation can be
exercised without an engine or network with
`PORTO_TEST_COMPOSE=1 go test ./internal/compose`.
