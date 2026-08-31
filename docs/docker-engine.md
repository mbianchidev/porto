# Porto Docker Engine

Porto owns a Docker Engine-compatible API endpoint. It does not forward requests to Docker Desktop, Podman, or another Docker daemon.

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
        +-- local nerdctl -> local containerd
        |
        `-- limactl shell porto-engine -> nerdctl -> containerd

Buildx / Compose Bake
        |
        v
Docker `/grpc` and `/session` upgrades
        |
        v
BuildKit socket or Lima `buildctl dial-stdio`
```

The API server is part of the Porto daemon and starts whenever the Docker runtime is enabled. Docker is enabled by default for new Porto installations and can be toggled with `porto runtime enable docker` or `porto runtime disable docker`. The execution backend is independent:

- If `nerdctl` and BuildKit are available in Porto's `PATH`, Porto uses the local containerd installation.
- Otherwise, `porto docker engine-install` creates a persistent Lima VM named `porto-engine` with rootless containerd, BuildKit, and writable default host mounts.
- Windows exposes the Docker-compatible named pipe, but automatic backend installation is not implemented. Install nerdctl, containerd, and BuildKit manually.

Porto stores backend ownership metadata in `<PORTO_HOME>/docker/engine.json` and a matching protected marker inside the Lima VM. An unrelated VM named `porto-engine` is never adopted or deleted. Container images, writable layers, networks, and volumes remain in containerd's persistent storage. Stopping Porto does not delete them.

## Install the Docker context

Start Porto and install the named context:

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

## Supported Docker API

Porto accepts versioned and unversioned Docker Engine paths. It currently advertises API 1.47 with minimum API 1.41.

| Resource | Supported operations |
| --- | --- |
| System | `/_ping`, `/version`, `/info` |
| Containers | list, create, inspect, start, stop, restart, pause, unpause, rename, wait, followed logs, attach, exec, archive copy, resource update, remove |
| Images | list, inspect, pull, save, remove |
| Networks | list, create, inspect, connect, disconnect, remove |
| Volumes | list, create, inspect, remove |
| Builds | Docker build API plus BuildKit `/grpc` and `/session` upgrades |

The common detached lifecycle works through standard Docker clients:

```sh
docker --context porto pull alpine:latest
docker --context porto create --name demo alpine:latest sleep 30
docker --context porto start demo
docker --context porto ps -a
docker --context porto rm --force demo
```

Container creation supports image, command, entrypoint, environment, labels, working directory, user, hostname, `--volume` bind/volume mappings, published ports, network mode, restart policy, TTY, stdin, and automatic removal.

The Porto socket is also a Compose backend. Compose uses normal project labels,
networks, named volumes, builds, container startup, and cleanup through Porto:

```sh
docker --context porto compose up --detach --build
docker --context porto compose down --volumes
```

Porto's own Compose project orchestration points Docker Compose at the native
Porto endpoint automatically.

BuildKit clients connect through Docker-compatible h2c upgrades. Multi-platform
builds work with Buildx and Compose Bake:

```sh
docker --context porto buildx build \
  --platform linux/amd64,linux/arm64 \
  --tag registry.example.com/team/app:latest \
  --push .
```

The Porto CLI and legacy Docker build API also pass comma-separated platforms
to nerdctl:

```sh
porto docker build . \
  --tag example/app:latest \
  --platform linux/amd64,linux/arm64
```

Cross-architecture builds require a BuildKit worker with the required native
worker or binfmt/QEMU emulation.

## Explicit limitations

Porto returns HTTP `501 Not Implemented` with a Docker JSON error for unsupported API operations. It does not silently ignore requested isolation or security settings.

Not implemented:

- selecting only one log output stream and structured `--mount` requests; these return 501 instead of changing semantics
- log output streams incrementally and preserve order within stdout and stderr; exact ordering between the two streams is best effort
- TTY Docker exec, detached exec, attach with historical logs, and streaming stats through Docker clients
- build history, commit, import, export, load, and save through the legacy Docker API
- legacy build contexts larger than 2 GiB; Buildx sessions use normal BuildKit file synchronization
- swarm, services, tasks, secrets, configs, plugins, and node management
- events, system prune, system disk-usage details, and registry authentication on the legacy image-pull endpoint
- capability changes, namespace overrides beyond KinD's host/private modes, and resource limits beyond CPU/memory container updates
- remote TCP/TLS exposure and Windows containers

The Porto dashboard's existing container exec endpoint is separate from the Docker exec protocol and continues to invoke nerdctl directly.

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

To expose Porto at the canonical Unix path:

```sh
porto docker activate
```

Porto never replaces a non-symlink `/var/run/docker.sock`. Replacing another runtime's symlink requires `--replace`; Porto records the previous target and restores it on:

```sh
porto docker deactivate
```

Access to the Porto Docker socket grants control over containers and host-mounted files. Treat it as host-administrator access.
