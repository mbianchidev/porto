# Porto Docker Engine

See [runtime data management](data-management.md) for storage accounting,
confirmed object/category cleanup, versioned local volume archives, scheduled
crash-consistent backups, safe staged restore, source-preserving migration, and
native filesystem boundaries. `/system/df` reports unique content/snapshot
provenance; category prune APIs protect referenced resources and return warnings
when actual reclaimed bytes are unavailable rather than inventing a total.

Porto owns a Docker Engine-compatible API endpoint. It does not forward requests to Docker Desktop, Podman, or another Docker daemon.

Engine budgets, opt-in Resource Saver, architecture policy and synchronized bind
mapping are described in [resource management](resource-management.md). They use
the same ownership-checked backend and do not adopt manually paused tasks or
retarget existing container mounts.

Image deletion can wait for containerd layer and snapshot garbage collection,
especially with native Windows filesystem layers. It uses the bounded
30-minute image/storage operation budget rather than the 20-second metadata
probe budget. A caller's shorter deadline or cancellation still takes priority;
cleanup errors are returned instead of being retried or ignored.

## Architecture

The data path is:

```text
Docker CLI / Docker SDK
        |
        v
<PORTO_HOME>/run/docker.sock
        |
        v
Porto Docker API server
        |
        v
Porto runtime manager
        |
        +-- persistent containerd gRPC inventory + lifecycle subscription
        |
        +-- containerd v2 client -> images, snapshots, OCI specs, tasks, exec
        |
        +-- backend-local runtime helper -> FIFO/TTY, CNI, CRIU probes
        |
        `-- compatibility CLI for preflight-unsupported or legacy resources

Buildx / Compose Bake
        |
        v
Docker `/grpc` and `/session` upgrades
        |
        v
BuildKit socket or Lima `buildctl dial-stdio`
```

The API server is part of the Porto daemon and starts whenever the Docker runtime is enabled. Docker is enabled by default for new Porto installations and can be toggled with `porto runtime enable docker` or `porto runtime disable docker`. The execution backend is independent:

- If `nerdctl`, containerd, and BuildKit are available in Porto's `PATH`, Porto uses the local containerd installation and connects to containerd directly for supported operations.
- Otherwise, the packaged desktop app automatically creates a persistent Lima VM named `porto-engine` with rootless containerd, BuildKit, and writable default host mounts on first launch, including on Windows. Porto installs its bundled Linux runtime helper and configures QEMU user-mode emulation only in that owned VM. CLI-only installations can run `porto docker engine-install`.

An explicit supported Rosetta policy selects VZ and its amd64 userspace handler
for a new Apple Silicon engine instead. Existing QEMU disks are not converted.

New Windows engines use `template://ubuntu-24.04`; macOS and Linux keep Lima's
default template. This selection applies only when creating an engine and
never changes the disk or image of an existing instance.

Porto stores backend ownership metadata in `<PORTO_HOME>/docker/engine.json` and a matching protected marker inside the Lima VM. An unrelated VM named `porto-engine` is never adopted or deleted. Container images, writable layers, networks, and volumes remain in containerd's persistent storage. Stopping Porto does not delete them.

## Live container inventory

The daemon maintains one authoritative, revisioned container snapshot for both
the Containers and Localhost views. It connects to the active containerd
namespace directly, subscribes before the initial list to avoid a startup race,
and refreshes after container, task, exec, image, namespace, snapshot, and
sandbox events. Bursts are coalesced for 100 ms, while a 30-second
reconciliation repairs missed or reordered events.

The dashboard receives snapshots from
`/api/docker/containers/events` over server-sent events. It reconnects with
bounded exponential backoff and falls back to a 30-second refresh only while
the stream is unavailable. `/api/docker/containers/snapshot` exposes the same
revision, typed task/exit/OOM/restart/health/resource metadata, lifecycle
history, namespace, and stale-state status.

Task list entries are matched by container ID when provided, otherwise by
process ID (an init task's ID is its container's ID). Actual task state takes
precedence over a restart policy's desired state, so running Compose services
are counted as running and stopped tasks retain their exit status.
Restart-policy labels describe intent, not an observed restart: a container
without a task reports `Created`, not `Restarting`, even if a stale desired-state
label says `running`. Observing the inventory never changes its restart policy.
Both created and exited containers can be started or removed without a
running task; Stop and Restart remain disabled until the container is active.

Docker inspect also projects an explicitly empty compatibility-runtime status
from its existing lifecycle flags and timestamps. A never-started container
therefore reports `created` to Compose instead of an empty state that can
prevent startup convergence. Explicit nonempty states, process identity,
exit history, health and other inspection fields are retained.

When the Docker endpoint uses the event-backed inventory, successful creation
waits until that healthy inventory contains the new container ID. Compose can
therefore list and start every member immediately after creation instead of
losing a newly created service to a delayed snapshot. Cancellation or failed
inventory confirmation reports the already-created ID for recovery, rather
than claiming the whole operation completed or deleting the container.

For the Porto Lima engine, the daemon discovers the rootless containerd socket
inside the VM and keeps a helper channel open for the gRPC connection. All host
platforms use `limactl shell` standard input/output and the bundled guest helper's
`dial-stdio` command, just as BuildKit uses its stdio transport. No host Unix
socket or independent `ssh -N -L` connection is created. A healthy existing Lima
SSH session can therefore carry containerd even when new connections to Lima's
forwarded SSH port are closed before authentication.

A completed or cancelled gRPC dial does not terminate an already established
channel. Closing it reaps only its own subprocess/channel, without exiting
Lima's shared SSH master or removing its socket. The connection and subscription
are closed during runtime
disable and daemon shutdown. Normal observation does not repeatedly run `nerdctl ps` or spawn
`limactl shell` processes. Container lifecycle events and inventory failures
also flow into the desktop Activity log.

For a startup error containing `containerd SSH tunnel exited before connecting`
and `Connection closed by 127.0.0.1`, check the read-only shell path with
`limactl shell --workdir=/ porto-engine -- true`. If that works, containerd
must use the helper channel rather than requiring a new SSH forward. Readiness
deadlines are unchanged; helper/socket/ownership failures stay explicit and
never authorize deleting or automatically recreating the guest.

Ownership, socket discovery, and helper probes run from the guest root
directory instead of entering a mounted host home directory. Capability probes
have a bounded deadline so a stalled optional helper cannot prevent the
container inventory from becoming available. Successful engine installation
and startup wake reconnect backoff immediately; timeout errors retain the
underlying command diagnostics. Helper updates are staged and checked before
replacing the previous executable, including while existing tunnels use it.
If ownership verification fails, Porto also checks the last 64 KiB of the
guest serial log for a kernel panic after the latest visible boot marker.
The error includes the panic summary and log path without dumping unrelated
guest output. Missing logs leave the original command error intact. A guest
panic does not weaken ownership checks or authorize automatic recreation; see
[Windows guest recovery](installation.md#windows-guest-recovery).

Lifecycle actions use containerd task APIs. Starting or restarting a stopped
Porto-owned container recreates its task from the stored OCI spec, snapshot
mounts, network endpoints, and I/O metadata. Legacy containers whose task
lifecycle is owned by the compatibility runtime remain on that path.
When the inventory has a healthy connection, lifecycle and removal operations
borrow that same verified backend instead of opening another SSH tunnel and
repeating health probes. Finishing an operation never closes the
inventory-owned connection; the inventory still owns reconnect and shutdown.
This keeps slow emulated guests on native cleanup, including `docker rm -v`,
rather than sending Porto-owned OCI metadata to nerdctl after a redundant
connection probe times out.
Porto-owned containers use persistent log URIs and Porto-created FIFO/TTY
bridges, including attached start and direct exec over a Lima guest connection.
The Lima bridge preserves SSH stdin for its background FIFO writer, waits for
input forwarding to finish, then explicitly closes containerd stdin. This keeps
`docker exec -i`, kind bootstrap configuration, and volume-archive transfers
from dropping input or waiting forever for EOF.
After an attached auto-remove container exits, Porto removes it and its
anonymous volumes before completing Docker's removal wait. The container's exit
code is retained, and cleanup failures are logged and returned to the waiting
client rather than leaving `docker run --rm` waiting indefinitely.

## Private registry credentials

Add Docker Hub, GitHub Container Registry, GitLab Container Registry, or a
custom HTTPS OCI registry from the dashboard Settings page. Porto stores the
credential in the operating system credential store and keeps only the profile
name, registry host, username, test image, and verification status in SQLite.

Saving or re-verifying a profile performs a real image pull. Enabled profiles
are used only after that succeeds. Dashboard pulls and Docker-compatible
`/images/create` requests without an explicit `X-Registry-Auth` header
automatically select the verified profile whose host matches the image
reference. A client-supplied `X-Registry-Auth` header still takes precedence.
For every authenticated pull, Porto creates a narrowly scoped temporary Docker
config in the active backend and removes it after the command finishes.

Docker's daemon-side `POST /auth` login flow verifies HTTPS registry credentials
using the registry's Basic/Bearer challenge. It does not save credentials in
Porto or a backend Docker configuration. The Docker client continues to own its
credential helper/configuration, and supplies credentials on later pulls and
pushes. Plaintext authentication and plaintext token-realm redirects are
rejected. Registry search uses Docker Hub's search API, or a named registry's
legacy search API when that registry implements one; OCI Distribution alone
does not define a search endpoint.

The Docker API is an owner-trusted control interface: Unix sockets are mode
`0600`, Windows named pipes grant access only to their owner, and Porto does
not provide a TCP Docker listener. As in Docker Engine, authorized callers
deliberately choose registry and import destinations, including private-network
hosts. Do not expose this interface through an unauthenticated network proxy.

Container creation uses the containerd v2 client for image resolution and
unpack, writable snapshot allocation, OCI spec generation, metadata creation,
and ordered cleanup. Porto selects this path only after preflight confirms that
the request can be represented directly. Requests that still need legacy-owned
volume, initial CNI, device, init, or security-option behavior use the
compatibility path before any direct resource is mutated.

The Lima backend uses the Linux `overlayfs` snapshotter by default, regardless
of the desktop host OS. In particular, containerd 2.4's macOS client default
(`erofs`) does not apply to the Linux guest. Porto honors the active namespace's
`containerd.io/defaults/snapshotter` label when set, and uses the same selected
snapshotter for pulls, unpack checks, unpacking, writable snapshots, and failed
creation cleanup. Local containerd backends retain their native defaults.
Existing containers keep the snapshotter recorded in their metadata.
Explicitly selecting EROFS requires a working guest snapshotter and the
[upstream prerequisites](https://containerd.io/docs/2.4/snapshotters/erofs/);
Porto does not silently replace an unavailable configured snapshotter or change
containerd's configuration.

Porto-owned healthchecks are scheduled by the daemon and executed through
direct containerd exec. Timing, retries, start periods, start intervals, bounded
result logs, and starting/healthy/unhealthy transitions are persisted in
container metadata so they recover after a daemon restart.

Network connect and disconnect use CNI `ADD` and `DEL` through the backend-local
runtime helper. The inventory exposes endpoint interface, MAC, address, gateway,
aliases, and port mapping state. A missing helper, CNI configuration, plugin, or
reachable network namespace is reported through the runtime capability matrix
instead of being treated as direct support.

Checkpoints are stored as named containerd checkpoint images with source
metadata. Restore is advertised only when the backend helper discovers CRIU;
it creates a new, unused target container ID, restores image/spec/runtime/RW
state, recreates the task with the checkpoint descriptor, and starts it.
Backends without the required runtime support return a typed unsupported error.

## Install the Docker context

Porto Desktop creates or updates the named context automatically. For CLI-only
installations, start Porto and install it explicitly:

```sh
porto daemon start
porto docker context-install
docker --context porto info
```

This health check is suitable for scripts:

```sh
docker --context porto info >/dev/null 2>&1 \
  && echo "Porto is running" \
  || echo "Porto is not running"
```

The context can report Porto's server information before a containerd backend is installed. Resource operations return an explicit unavailable error until the backend is ready:

```sh
porto docker engine-install
porto docker status
```

Manage the persistent backend:

```sh
porto docker engine-start
porto docker engine-stop
porto docker engine-remove --confirm
```

`engine-remove --confirm` deletes the Porto-owned Lima VM and every container resource stored in it. It does not delete a system containerd installation used through local `nerdctl`.

## Docker API compatibility matrix

Porto advertises Docker Engine API **1.47**, minimum **1.41**. Unversioned
`/_ping` and `/version` negotiate normally; versioned requests outside that
inclusive range return HTTP 400 with Docker's too-old/too-new error format,
before any backend operation. Escaped image names remain intact.

| Resource | Status | Operations and boundaries |
| --- | --- | --- |
| System | Supported | `/_ping`, `/version`, `/info`, `/system/df` with explicit shared-store provenance, and container lifecycle `/events` |
| Containers | Supported | list with size/limit/since/before, create, inspect, start, stop, restart, pause, unpause, rename, wait, process lists, filesystem changes, followed logs, historical/live attach, initial TTY dimensions and container resize, TTY and detached exec, exec resize, archive copy, filesystem export, one-shot/streaming stats, combined resource/restart-policy update, remove, category prune |
| Container mounts | Partial | structured bind, local volume, and tmpfs mounts with read-only, propagation, recursive bind controls, `nocopy`, size, mode, and basic tmpfs flags; bind source auto-creation, volume subpath/driver/label and explicit masked/read-only path overrides remain unsupported |
| Checkpoints | Capability-gated | standard Docker checkpoint create/list/remove routes, backed by named containerd checkpoint images; creation needs CRIU, custom checkpoint directories are unsupported |
| Container restore | Capability-gated | restore is advertised only when the owned backend reports CRIU support |
| Images | Supported | list including `--all`, inspect, history, registry search, login, pull, save, archive load, request-body/HTTP(S) rootfs import, container commit, tag, authenticated push, remove including `--no-prune`, and category prune |
| Networks | Supported | list, create, inspect, connect, disconnect, remove and category prune |
| Volumes | Supported | list, create, inspect, reference-protected remove and category prune; Porto API adds local file/transfer/backup tools |
| Builds | Supported | Docker build API plus BuildKit `/grpc` and `/session` upgrades, Buildx `--load`, image export, registry push and selective cache prune |
| Orchestration | Unsupported | Swarm services/tasks/nodes and generic engine plugins return explicit errors; native Windows containers use a separately selected [containerd/runhcs endpoint](windows-integration.md#native-windows-setup), not the Linux backend |

### Endpoint and version boundaries

| Endpoints | Proven contract and boundaries |
| --- | --- |
| `GET/HEAD /_ping`, `GET /version` | SDK/CLI negotiation; API 1.41 through 1.47 |
| `GET /info`, `GET /containers/json`, `GET /containers/{id}/json` | backend-derived counts, capabilities, ports, mounts, and known network endpoints; unknown host/storage/security information is not guessed |
| `POST /containers/create`, `POST /containers/{id}/update` | decoded and preflight-validated options; unsupported non-default fields, including unknown nested fields, fail before mutation |
| `GET /containers/{id}/stats` | streaming, two-sample non-streaming, and immediate one-shot; backend Linux CPU totals/counts and raw cgroup v1/v2 counters |
| `GET /containers/{id}/top`, `GET /containers/{id}/changes` | backend process arguments and typed added/modified/deleted filesystem paths; snapshot comparison uses the recorded snapshot key and excludes external container mounts |
| `POST /containers/{id}/resize` | resizes the container task's terminal; independent of exec resize |
| `POST/GET /containers/{id}/checkpoints`, `DELETE /containers/{id}/checkpoints/{name}` | standard create/list/remove responses; names are scoped to the source container, duplicate names conflict, and deletion verifies source metadata |
| `POST /auth`, `GET /images/search`, `GET /images/{name}/history` | HTTPS challenge-based login, bounded/filterable registry search, and image configuration history with actual image identity |
| `GET /events` | bounded container-lifecycle replay with immutable actor names/images/labels; supports both map and array filter encodings |
| `POST /images/load`, `POST /images/create?fromSrc=-`, `POST /images/{name}/push` | incremental progress/error frames are flushed immediately; interrupted uploads, canceled transports, malformed archives, and partial failures remain errors |
| `GET /images/{name}/json`, `GET /images/json` | `VirtualSize` is omitted starting at API 1.44; image inspect omits `Container`/`ContainerConfig` starting at 1.45 |
| Container health configuration | `StartInterval` requires API 1.44; older inspect responses omit it |
| Structured mount options | recursive read-only controls require API 1.44 and a capable Linux kernel/runtime; volume subpaths require 1.45 but remain unsupported; tmpfs option arrays require 1.46 |

Image archive loads and request-body rootfs imports enable HTTP/1 full-duplex
streaming before starting the backend. Progress may arrive before the upload
finishes without Go's HTTP server draining unread archive bytes. Both
content-length and chunked uploads preserve their binary contents; transport
setup and progress-flush failures are reported rather than retried as image
format errors.

### Create/update option boundaries

| Options | Behavior |
| --- | --- |
| Image, command/entrypoint, environment, labels, user, working directory, hostname, exposed/published ports, health checks, TTY/stdin/`StdinOnce`, restart policy, auto-remove | supported; compatibility-owned requests preflight to `nerdctl` |
| CPU/memory/PID limits, CPU shares/period/quota/sets, memory reservation/swap/swappiness, OOM disabling, block-I/O weight | translated at creation; direct updates persist the OCI resources and update a running task. Combined resource/restart updates preflight policy metadata and roll back both metadata changes if the task update fails |
| Read-only root, capability additions/drops, custom DNS/search/options, extra hosts, supplementary groups, ulimits | supported through the native spec or compatibility runtime |
| `--gpus` | NVIDIA count/device/capability requests are translated; the Linux backend must actually provide the driver, toolkit, and devices. Unsupported request drivers/options fail explicitly |
| `-P` / `PublishAllPorts` | merges image and request exposed ports, preserves explicit bindings, and selects explicit TCP/UDP host ports so rootless nerdctl does not need unsupported automatic allocation |
| Security options and namespaces | custom AppArmor/SELinux profiles, unconfined/no-new-privileges settings, cgroup/IPC/PID/UTS options use backend validation. Docker seccomp profile JSON is staged privately beside the backend and removed after creation; daemon-host filenames are not accepted as profile JSON |
| Structured bind, local volume, tmpfs | basic supported mount options; local/default volume driver declarations are accepted |
| `HostConfig.LogConfig` | default/`json-file` without custom options; other drivers or configuration are rejected |
| `HostConfig.ConsoleSize` | accepted and ignored when `Tty` is false or omitted, including nonzero dimensions sent by Docker for ordinary `run` commands; TTY requests retain initial dimensions and support task resize |
| Windows-only CPU/I/O settings, per-device block-I/O create fields, device-cgroup rules, masked/read-only path overrides, volume inheritance, annotations, and unknown fields | unsupported settings are rejected before creation; supported device mappings and sysctls retain their compatibility path. Docker cannot dynamically update device/GPU/ulimit settings |
| Defaults | omitted, null, false, zero, empty strings/collections, and documented default enum/sentinel values remain compatible with normal client payloads; an explicit empty masked/read-only path list is an override, not a default |

Docker events replay only the latest 200 retained container lifecycle events
when `since` is supplied. The stream supports `type`, `event`, `container`,
`image`, `label`, and `scope` filters, honors `since`/`until`, uses a one-slot
latest-snapshot buffer, and unsubscribes immediately when the client disconnects.
Image, network, namespace, and snapshot events still drive reconciliation but
are not exposed as fabricated Docker resource events.
Actor metadata is copied when an event is captured, including metadata carried
by containerd update events. Deletion and subsequent rename/label changes cannot
rewrite retained attributes. A failed creation-time metadata lookup is logged;
unavailable historical attributes are not invented from later container state.

Container stats use raw containerd cgroup v1/v2 counters plus host CPU totals
from the **Linux backend's** `/proc/stat`, including Lima guests. Guest CPU time
is not double-counted, and cgroup v2 receives a verified online CPU count even
without a per-CPU usage array. `stream=false` collects two samples a second
apart for Docker's CPU percentage calculation; `stream=false&one-shot=true`
returns one immediate sample without fabricated previous counters. One-shot
with streaming is rejected. Active inventory connections are reused for metrics,
instead of opening a new SSH tunnel for every sample.
Lima samples collect cgroup and host CPU counters together in the Linux runtime
helper, so SSH/emulation latency cannot offset the two measurements. The guest
architecture and cgroup driver also determine direct task placement; rootless
systemd guests use a delegated user slice rather than a host-derived path.

CPU, memory, PID,
network, and block-I/O sections are emitted only when the backend reports those
measurements; Porto does not replace unavailable counters with invented zeroes.
Missing backend host CPU inputs return an explicit availability error rather
than a misleading zero percentage.
Legacy compatibility-owned containers return an explicit capability error when
raw task metrics are unavailable.

End-to-end coverage exercises the bundled Docker CLI, Compose project
lifecycle, Buildx build/load, image archive load/save, tag/push through a
disposable registry, exec/TTY, attach, and KinD-required privileged operations.
Real Docker SDK contract tests run on every host, including Windows named-pipe
transport. The opt-in development-client suite and native CI matrix exercise
the SDK, ordinary CLI load/save/tag/push, a disposable registry, Dev Containers
up/post-create/exec, and Testcontainers start/log-wait/exec/cleanup against
local Linux containerd and Lima-backed runtimes. Fixtures are synthetic;
workspaces and resources are individually owned and removed.

Dockerode's plain HTTP exec streams receive Docker-framed HTTP 200 responses;
CLI/SDK connection upgrades still receive HTTP 101. Testcontainers' `tail=-1`
log sentinel means all retained lines. Lima log paths and recorder executables
are resolved in the Linux guest, including when the desktop host is Windows.
For Node Testcontainers on Windows, use backslashes in the pipe path
(`npipe:////.\pipe\porto`): its WHATWG URL parser removes the `.` segment from
the ordinary CLI spelling `npipe:////./pipe/porto`. The native CLI/SDK keep their
normal endpoint spelling.

The tested Dev Containers configuration uses a named workspace volume.
Testcontainers uses explicit harness cleanup with Ryuk disabled: Porto does
not mount the host control socket into its guest or claim a working guest-side
Ryuk reaper. Host bind mounts and other development-container features remain
subject to the mount/capability matrix.

Porto does not claim generic drop-in
compatibility for remote TCP, Swarm, broad system prune, extra custom network
aliases, or other operations listed as unsupported. HTTP(S) rootfs imports
stream to the backend with a 2 GiB archive limit and bounded redirects; download
errors do not become successful imports. Import configuration changes remain
unsupported.

Container size queries measure the recorded snapshot and its ancestors,
including stopped containers. Containerd does not keep Docker's separate
hidden intermediate-image/parent-image records: `images --all` lists the same
stored image records, and `rmi --no-prune` removes only the requested image
reference, just as the existing containerd removal path does.

Docker's experimental feature advertisement allows the CLI to dispatch standard
checkpoint commands. It does not imply CRIU or GPU availability. Standard
in-place `docker start --checkpoint` restore is rejected; Porto's separate
restore API creates a new target container. Custom checkpoint directories are
not redirected to host paths.

Filesystem changes and compatibility TTY attachment require the current bundled
Linux helper. Snapshot comparison runs inside a private mount namespace and
starts its concurrent walker in a child process so all walker threads inherit
the same mounts. It neither assumes that a container ID equals its snapshot key
nor changes the workload's mount namespace. Compatibility TTY starts use a
backend-local PTY, preserve initial dimensions, and retain the container exit
code; container resize goes directly to its task.

To exercise these paths against an **already running, owned** Lima engine, build
the current helper for the guest architecture and run:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -o /tmp/porto-parity-helper ./cmd/porto-runtime-helper
PORTO_DOCKER_PARITY_INTEGRATION=1 \
  PORTO_DOCKER_PARITY_TEST_HELPER=/tmp/porto-parity-helper \
  go test ./internal/docker -run '^TestDockerCommandParityLiveIntegration$' -count=1
```

Use `GOARCH=amd64` for an x86-64 guest. The fixture stages a private guest helper,
does not replace the installed helper or restart the engine, and removes only
its uniquely named synthetic containers, image, and helper directory.

Run the live suite against an **already running, owned** Lima engine without
restarting or recreating it:

```sh
npm --prefix scripts/docker-clients ci
PORTO_DOCKER_CLIENT_INTEGRATION=1 PORTO_DOCKER_CLIENT_BACKEND=lima \
  go test ./internal/docker -run '^TestDockerDevelopmentClientsIntegration$' -count=1 -timeout=25m -v
```

Use `PORTO_DOCKER_CLIENT_BACKEND=local` for a Linux containerd/BuildKit backend.
CI uses `scripts/test-docker-clients.sh` with isolated runtime roots and owned
processes/VMs; it refuses to provision runtimes outside GitHub Actions.
Fresh SSH connections must be available in an existing Lima guest. A guest
at its SSH socket connection limit fails this preflight rather than restarting
or terminating unrelated sessions.

The common detached lifecycle works through standard Docker clients:

```sh
docker --context porto pull alpine:latest
docker --context porto create --name demo alpine:latest sleep 30
docker --context porto start demo
docker --context porto ps -a
docker --context porto rm --force demo
```

Attached runs start with their standard input/output streams connected before
the container task begins, including short-lived automatic-removal workloads:

```sh
docker --context porto run --rm --network none alpine:latest printf 'hello\n'
```

Container waits support Docker's `not-running`, `next-exit`, and `removed`
conditions and preserve the container exit code after automatic removal.

The Containers dashboard can also create and start a container from either a
local image name or a remote image reference. It supports an optional
loopback-only published port and an optional shell health command.

Container creation supports image, command, entrypoint, environment, labels,
working directory, user, hostname, stop behavior, healthchecks, `--volume`
and structured bind/volume/tmpfs mappings, published ports, network mode, restart policy, TTY,
stdin, and automatic removal. Requests that contain only directly supported
resources use containerd without invoking `nerdctl`; other accepted requests
preflight to the compatibility path.

For Porto-owned containers, `CMD` healthchecks remain argument arrays rather
than being converted into shell commands. `CMD-SHELL` explicitly uses the
container shell. Inspect requests do not trigger probes; the daemon scheduler
owns execution and result retention. Legacy containers keep their existing
runtime-owned health behavior.

For example, run nginx with an explicit health check:

```sh
docker --context porto run --detach --name porto-nginx \
  --publish 8080:80 \
  --health-cmd 'wget -q -O /dev/null http://127.0.0.1/' \
  --health-interval 30s \
  nginx:alpine
```

The Porto socket is also a Compose backend. Compose uses normal project labels,
service-name networking, health dependencies, named volumes, builds, container
startup, and cleanup through Porto:

```sh
docker --context porto compose up --detach --build
docker --context porto compose down --volumes
```

Porto's own Compose project orchestration points Docker Compose at the native
Porto endpoint automatically.

BuildKit clients connect through Docker-compatible h2c upgrades. Multi-platform
builds work with Buildx and Compose Bake:

The proxy coordinates gateway and status calls with the matching Solve's storage,
architecture and exporter preflight. A cold guest must not receive gateway
`Ping` before that build can be registered upstream. Admission is keyed by the
original build ID across upgraded connections; other builds stay independent,
caller cancellation remains bounded, and actual preflight refusals are not
replaced with an opaque missing-job error. Verified worker discovery can supply
the existing namespace/exporter compatibility proof without a duplicate probe.

SDK uploads to the ordinary Docker `/build` endpoint are unpacked into a
confined temporary context before invoking nerdctl, which does not accept a
tar context as `-`. Gzip uploads are supported; expanded archives are bounded
to 2 GiB and 100000 entries. Escaping paths/links and special-file entries are
rejected, and local/Lima temporary contexts are removed after completion or
cancellation.

```sh
docker --context porto buildx build \
  --platform linux/amd64,linux/arm64 \
  --tag registry.example.com/team/app:latest \
  --push .
```

Porto accepts Buildx's internal `moby` exporter and delegates it to BuildKit's
standard image exporter. The wrapper normalizes requested tags, stores the
result in Porto Engine's containerd image store, and unpacks it by default.
Builds without a tag retain a dangling image reference. This makes the default
Buildx output, `--load`, `type=image`, registry pushes, `docker compose build`,
and `docker compose up` use the native image store directly without a Docker or
OCI tar export/import step:

```sh
docker --context porto buildx build --load --tag porto-test .
docker --context porto image inspect porto-test
docker --context porto compose up --build
```

Lima BuildKit tunnels, including build-history connections, stay alive until
their connection closes. Completion of gRPC dialing does not stop the tunnel;
finishing or canceling a history request still closes and reaps its subprocess.
This behavior is shared by macOS, Windows, and Linux.

The Porto CLI and legacy Docker build API also pass comma-separated platforms
to nerdctl:

```sh
porto docker build . \
  --tag example/app:latest \
  --platform linux/amd64,linux/arm64
```

The managed Lima engine automatically installs static QEMU user-mode emulators
from its Ubuntu package repositories and registers the packaged `binfmt_misc`
handlers with the `F` (`fix_binary`) flag required inside containers. The
systemd registrations survive VM reboots. Packaged desktop launches reconcile
this setup even when the engine is already running, so existing installations
gain multi-platform support without recreating the VM.
Porto also enables packaged 32-bit ARM/x86 handlers that distributions omit
because they assume native compatibility; Apple Silicon cannot execute
32-bit ARM binaries natively.

`linux/amd64` is supported on ARM64 hosts. Porto builds and runs AMD64
containers through QEMU/binfmt, and `uname -m` inside the container reports
`x86_64`. Image inspect selects the platform stored for the local image so
Compose validates AMD64 images correctly, while attached starts normalize
containerd's empty pre-start status before the Docker hijack handshake. An
image-inspect JSON error or failed stream upgrade is therefore a Docker API
compatibility defect, not evidence that AMD64 emulation is unavailable.

The first setup needs package-repository access; subsequent launches reuse the
installed packages and repair missing or disabled registrations without
downloading them again. Provisioning failures are reported instead of claiming
that the engine is ready.

BuildKit may retain a stale 32-bit platform list after emulation is enabled.
Porto refreshes only the BuildKit user service once when required, then verifies
its reported platforms. The VM, containerd, and running containers stay up.
If BuildKit reports active builds, setup refuses the refresh with an explicit
error; finish those builds and retry setup. Later launches and VM boots do not
restart a builder that already reports the configured platforms.

Inspect the platforms BuildKit can actually execute:

```sh
docker --context porto buildx inspect porto
```

The `PLATFORMS` column in `docker buildx ls` lists executable CPU targets, not
separate build systems. BuildKit discovers native and emulated platforms; Porto
does not hard-code that list. QEMU enables targets such as AMD64, ARM, ARM64,
386, PowerPC, RISC-V, and s390x, subject to the installed emulator and image
support. Emulated compilation can be slower than a native worker.

CLI-only users can apply the same setup to an existing engine with
`porto docker engine-install` or `porto docker engine-start`. Externally managed
local containerd/BuildKit installations are not modified: their administrator
must provide the required native workers or binfmt/QEMU emulation.

## Storage accounting and volume lifecycle

**Containers → Storage** measures actual containerd content and
snapshot usage in the configured Linux snapshotter(s), local volume allocation,
non-shared BuildKit records and shared runtime metadata. Packed image sizes and
shared cache/metadata scopes are not summed into a misleading total.
Partially downloaded foreign-platform image content remains visible and
protected instead of breaking the whole report. Select exact objects or one
category, inspect its frozen preview and confirm. Backend ownership and
active-build checks run again before each removal.

Volumes expose file browsing, container/Compose mount relationships, clone,
empty, local import/export, staged restore and confirmed removal. Scheduled
local backups use SQLite-backed schedules and operation history. Retention
requires a durable verified replacement first, and never removes an unproven
archive. Copies are **crash-consistent**, not application-consistent database
backups: explicitly stop or quiesce writers when needed. Linux helper/backend
capabilities and external volume drivers/options remain explicit limitations.
See [runtime data management](data-management.md) for CLI commands, archive
integrity/ownership, cancellation, restore safety and local backup controls.

## Unused image and build-cache cleanup

The Settings page offers opt-in weekly cleanup and a separate **Run now**
action. Automatic cleanup is disabled for both new and existing installations.
Enabling it requires confirmation and schedules the first run seven days later;
saving unrelated settings does not reset that deadline. **Run now** also works
with automatic cleanup disabled and requires its own confirmation.

Both paths perform the same cleanup against Porto's configured backend:

- Prune all unused BuildKit cache records. The `builder` and `buildx` commands
  address the same Porto BuildKit cache, so it is pruned once, not twice.
- Run namespace-scoped `nerdctl image prune --all --force`, including unused
  tagged image references. Images referenced by running or stopped containers
  are retained.

Containers, volumes, networks, other Docker contexts, and VMs are not removed.
Porto checks for active builds before starting and reports a skipped run rather
than interfering with a build. Concurrent manual and scheduled requests cannot
start overlapping cleanup runs.

Accepted runs continue when the Settings page is closed. The daemon records
the trigger, start/end times, outcome, removed image-reference count, build-cache
record count, reported cache bytes, and diagnostic output in SQLite. Settings
shows the latest ten results, including failures and partial results. Image
layer storage can be shared, so Porto does not invent a reclaimed-byte total for
images. Image-deletion warnings and unknown diagnostics are reported as failures
even when the CLI exits successfully. The known host-platform rootfs warning
(`no match for platform in manifest`) only prevents nerdctl from listing layer
digests for a foreign-platform image; it does not prevent image-reference
deletion. Porto retains that warning in **Image cleanup output** without failing
an otherwise successful run. Diagnostics are checked even after retained output
is truncated, so a later deletion failure cannot be hidden by earlier warnings.

If a result cannot be saved, the running daemon still exposes its observed
counts and storage error instead of leaving the UI stuck on "running".
Further cleanup is blocked until the storage problem is resolved and Porto
restarts; results that could not be persisted cannot survive that restart.

Every completed attempt, including **Run now**, schedules the next automatic
attempt seven days later when enabled. Missed weeks do not cause a burst of
catch-up runs, and failures do not silently turn weekly cleanup into an hourly
retry loop. Disable Docker to pause automatic execution, or disable the cleanup
setting to remove the schedule; retained results remain available.

Cleanup has a ten-minute deadline. Daemon shutdown cancels an active run and
records its partial outcome before closing the runtime. On restart, an
unfinished persisted run is marked interrupted rather than successful. Counts
reflect runtime-reported removals; an interrupted operation can have additional
effects that were not reported before it stopped.

The daemon exposes `GET /api/docker/cleanup` for the schedule and results and
`POST /api/docker/cleanup` for **Run now**. The POST returns `202 Accepted` with
a run identifier; clients must read its eventual outcome from the GET response.

## Explicit limitations

Porto returns HTTP `501 Not Implemented` with a Docker JSON error for unsupported API operations. It does not silently ignore requested isolation or security settings.

Not implemented:

- selecting only one log output stream; unsupported mount propagation,
  recursive bind, volume subpath/driver/label, and advanced tmpfs options return
  501 before resources are created
- log output streams incrementally and preserve order within stdout and stderr; exact ordering between the two streams is best effort, and Porto-owned raw log files cannot synthesize per-line timestamps
- attach with historical logs
- remote-URL rootfs import; request-body import, container commit/export, and image save/load use Docker-compatible endpoints
- legacy build contexts larger than 2 GiB; Buildx sessions use normal BuildKit file synchronization
- swarm, services, tasks, secrets, configs, plugins, and node management
- broad `/system/prune`; `/system/df` and category prune APIs are supported,
  but exact released bytes across shared stores remain unavailable
- namespace overrides beyond KinD's host/private modes, combined resource and restart-policy updates, and resource limits beyond CPU/memory container updates
- more than the standard Compose container-name/service-name network alias pair;
  extra custom aliases fail before container creation
- remote TCP/TLS exposure; native Windows containers require the separately selected [Windows backend](windows-integration.md#native-windows-setup)

The Porto dashboard's existing container exec endpoint is separate from the
Docker exec protocol and remains a compatibility path. Standard Docker exec,
including TTY, detach, and resize, uses direct containerd process APIs when the
capability matrix reports support.

The native API supports the privileged containers, exec stdin/stdout hijacking,
followed logs, archive transfer, image save, IPv6 network, and resource-update
operations used by KinD.

## Upgrading from the proxy-era daemon

The native engine advances Porto's internal daemon API version. If output still
mentions a Docker upstream request, an older installed daemon is running. Run
the current one-line installer again, or stop the older desktop daemon before
starting the new build. Current clients refuse to send commands to the older
daemon.

## Socket ownership and cleanup

The Unix socket is created with mode `0600` and removed during graceful shutdown. A stale socket is replaced only when it is a Unix socket; Porto refuses to replace a regular file or directory.

Porto installs a named `porto` Docker context automatically. Select it for the
current user with:

```sh
docker context use porto
```

To expose Porto at the canonical Unix path for tools that ignore contexts, use
`porto docker activate`. Porto never replaces a non-symlink
`/var/run/docker.sock`. Replacing another runtime's symlink requires
`--replace`; Porto records the previous target and restores it on:

```sh
porto docker deactivate
```

See [Make Porto the default Docker engine](installation.md#make-porto-the-default-docker-engine)
for administrator retries, verification commands, restoration, and Windows
behavior.

Access to the Porto Docker socket grants control over containers and host-mounted files. Treat it as host-administrator access.
