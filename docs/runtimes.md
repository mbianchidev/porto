# Local runtimes

Porto can manage native development applications, Docker resources, local Kubernetes clusters, and standalone Linux virtual machines from the same daemon, CLI, and dashboard.

Native project orchestration remains the default. Docker, Kubernetes, and VM providers are optional and report clear unavailable states when their required tools are not installed or running.

Enable providers independently:

```sh
porto runtime enable docker
porto runtime enable kubernetes
porto runtime enable vms
porto runtime status
```

Disable a provider with `porto runtime disable <provider>`. Disable Docker only after `porto docker deactivate` when Porto owns the canonical socket.

## Architecture

Porto separates the control plane from runtime providers:

- The Porto daemon owns the HTTP API, dashboard state, project lifecycle, local routes, and Docker API proxy.
- Native projects continue to run directly on the host.
- Docker resource operations use the standard `docker` CLI and selected Docker context.
- The Porto Docker socket proxies the selected upstream Docker Engine endpoint.
- Kubernetes inspection uses the selected `kubectl` context.
- Porto-created Kubernetes clusters use Lima VMs running k3s.
- Standalone Linux VMs use Lima and remain separate from Kubernetes node groups.

This design keeps existing tools and scripts compatible while giving Porto one operational view.

## Requirements

Core Porto only requires the dependencies listed in the [installation guide](installation.md).

Additional runtime features require:

| Capability | Requirement |
| --- | --- |
| Containers, images, builds, networks, volumes, Compose | Docker CLI and a reachable Docker-compatible Engine |
| Kubernetes inspection | `kubectl` and an authorized kubeconfig context |
| Porto-created Kubernetes clusters | `kubectl`; Porto can install `kind`, `limactl`, and `k0sctl` on macOS |
| Standalone virtual machines | `limactl` and host virtualization support |

Missing optional tools do not prevent native projects or the Porto daemon from running.

Inspect or install provider tools:

```sh
porto runtime providers
porto runtime install lima
porto runtime install kind
porto runtime install k0s
```

The k0s and k3s providers both use Lima on the host and install their
distribution inside Porto-managed guests. Porto uses Homebrew for explicit
provider installation on macOS. Other
platforms receive an actionable manual-install error instead of a silent
fallback.

## Docker-compatible endpoint

The daemon creates a user-owned socket:

```text
<PORTO_HOME>/run/docker.sock
```

On Windows, Porto exposes the equivalent named pipe:

```text
\\.\pipe\porto_docker_engine
```

It proxies Docker Engine API requests to the endpoint selected when the daemon starts. Override that upstream explicitly:

```sh
PORTO_DOCKER_UPSTREAM=unix:///path/to/docker.sock porto daemon start
```

If the selected upstream becomes available after the daemon starts, disable and re-enable the Docker provider or restart the daemon so Porto can resolve it again.

Install a named Docker context:

```sh
porto docker context-install
docker --context porto info
```

To make standard tools use Porto with `DOCKER_HOST` unset, activate the canonical Unix socket:

```sh
porto docker activate
```

Porto never overwrites an existing non-symlink socket. Replacing another runtime's symbolic link requires explicit intent:

```sh
porto docker activate --replace
```

Writing `/var/run/docker.sock` normally requires administrator privileges. When the operating system rejects the operation, Porto prints a command that preserves the correct `PORTO_HOME`. Deactivation removes only the link Porto owns and restores the previous symbolic link:

```sh
porto docker deactivate
```

The Porto-owned proxy socket uses mode `0600`. Treat access to any Docker socket as host-administrator access.

Windows clients use the named `porto` Docker context. Canonical
`\\.\pipe\docker_engine` takeover is intentionally not automatic because
Windows named pipes cannot be replaced with a reversible symbolic link.

### Docker resources

```sh
porto docker status
porto docker containers
porto docker images
porto docker builds
porto docker networks
porto docker volumes

porto docker container restart <container>
porto docker pull alpine:latest
porto docker build . --tag example:dev
```

Top-level inventory aliases are also available:

```sh
porto containers
porto images
porto builds
porto networks
porto volumes
```

Compose projects continue to use their standard Compose files. Porto assigns a
stable Compose project name per Porto project and generates a temporary
`!override` file that remaps published TCP ports onto free localhost ports.
This keeps concurrent projects and branch worktrees isolated without editing
the source Compose file. Porto exposes Compose project and service labels from
Docker so the dashboard can group related containers.

## Kubernetes

Porto operates only contexts it created and stores under `PORTO_HOME`.
It does not implicitly inherit the current global `kubectl` context. External
contexts remain untouched.

```sh
porto kubernetes status
porto kubernetes contexts
porto kubernetes pods --namespace all
porto kubernetes services --namespace default
porto kubernetes nodes
```

Pass `--context` when a command should not use the current context.

### Create a local cluster

Porto supports three explicit providers:

- **k3s** (default): lightweight Kubernetes on Porto-managed Lima VMs
- **k0s**: conformant Kubernetes on Porto-managed Lima VMs
- **kind**: Kubernetes nodes in containers through the Porto Docker endpoint

For k3s and k0s, Porto provisions one Lima VM for the controller and one VM
for each requested worker. CPU, RAM, and disk values are per VM. Nodes share
Lima's `user-v2` network, the Kubernetes API is forwarded to an allocated
loopback port, and Porto maintains localhost `kubectl port-forward` listeners
for LoadBalancer and NodePort services. The Services screen shows the assigned
localhost port and links directly to it.

```sh
porto kubernetes cluster create dev \
  --provider k3s \
  --cpus 2 \
  --memory 2048 \
  --disk 20 \
  --workers 2 \
  --worker-cpus 4 \
  --worker-memory 4096 \
  --worker-disk 30
```

Pin a k3s version when reproducibility is required:

```sh
porto kubernetes cluster create dev --version v1.33.4+k3s1
```

Create with another provider:

```sh
porto kubernetes cluster create kind-dev --provider kind --workers 2
porto kubernetes cluster create k0s-dev --provider k0s --workers 2
```

kind node topology is fixed at creation time. Recreate a kind cluster to
change its node count; k3s and k0s node groups can be scaled in place.

Porto stores each generated kubeconfig under an opaque, fixed-length filename
inside `<PORTO_HOME>/kubernetes`. Use `porto kubernetes kubeconfig <cluster>`
to resolve its path. The context, cluster, and user entries are all named
`porto-<cluster>` so multiple generated kubeconfigs can be merged safely.

Inspect or install the generated context into the default kubeconfig:

```sh
porto kubernetes kubeconfig dev
porto kubernetes context-install dev
```

Context installation creates a one-time `.porto-backup` before replacing an existing kubeconfig.

Scale a worker group:

```sh
porto kubernetes cluster scale dev workers --nodes 3 --cpus 4 --memory 4096 --disk 30
```

Import an image from the active Docker runtime into every k3s node without pushing to an external registry:

```sh
porto kubernetes cluster image-import dev example:dev
```

Lifecycle commands preserve the VM disks unless the cluster is deleted:

```sh
porto kubernetes cluster stop dev
porto kubernetes cluster start dev
porto kubernetes cluster delete dev
```

Cluster deletion requires explicit confirmation through the daemon API and removes the matching Porto-managed node VMs and kubeconfig.

### Pod inspection

List pods and read logs:

```sh
porto kubernetes pods --namespace all
porto kubernetes logs default api-7d9f --container api
porto kubernetes logs default api-7d9f --container api --previous
```

Run a command:

```sh
porto kubernetes exec default api-7d9f --container api -- sh -lc 'id && pwd'
```

List or read files:

```sh
porto kubernetes files default api-7d9f --container api --path /app
porto kubernetes files default api-7d9f --container api --path /app/config.json --read
```

The dashboard adds pod overview, streaming logs, a WebSocket-backed interactive terminal, bounded text-file editing, resource statistics, events, and effective manifest views. Pod operations use the selected kubeconfig identity and never bypass Kubernetes RBAC or container filesystem permissions.

File reads and writes are limited to 1 MiB per request. Changes inside an ephemeral container filesystem may disappear when Kubernetes replaces the pod.

## Virtual machines

Porto exposes a versioned VM image catalog:

- Ubuntu 24.04 LTS
- CentOS Stream 10
- openSUSE Tumbleweed
- NixOS unstable
- Arch Linux current cloud snapshot (x86_64; emulated on ARM hosts)
- Alpine Linux 3.23
- Kali Linux 2026.2 installer metadata. Creation remains blocked until a trusted
  cloud-init disk is available; Porto does not substitute unverified community
  security images.

List images and instances:

```sh
porto vm status
porto vm images
porto vm list
```

Create and start a machine:

```sh
porto vm create test-ubuntu \
  --image ubuntu-24.04 \
  --cpus 4 \
  --memory 4096 \
  --disk 30
```

Lifecycle and access:

```sh
porto vm start test-ubuntu
porto vm shell test-ubuntu
porto vm exec test-ubuntu uname -a
porto vm copy ./fixture.txt test-ubuntu:/tmp/fixture.txt
porto vm snapshot test-ubuntu before-upgrade
porto vm restore test-ubuntu before-upgrade
porto vm stop test-ubuntu
porto vm delete test-ubuntu
```

Porto-created Kubernetes node VMs use the `porto-<cluster>-<group>-<index>` naming scheme. Standalone machines are not attached to Kubernetes automatically. Porto records ownership metadata and hides unrelated Lima instances and Kubernetes node VMs from the standalone machine inventory.

The image catalog maps to Lima templates. Template availability depends on the installed Lima version and host architecture; Porto surfaces provider failures without changing native project state.

## API

Runtime APIs are served from the existing local daemon:

```text
GET    /api/runtime
GET    /api/docker/status
GET    /api/docker/containers
GET    /api/docker/images
GET    /api/docker/builds
GET    /api/docker/networks
GET    /api/docker/volumes

GET    /api/kubernetes/status
GET    /api/kubernetes/contexts
GET    /api/kubernetes/clusters
GET    /api/kubernetes/pods
GET    /api/kubernetes/services
GET    /api/kubernetes/nodes

GET    /api/vms/status
GET    /api/vms/images
GET    /api/vms/instances
```

Mutation routes validate JSON input, use argument arrays rather than host-shell concatenation, and require explicit confirmation for deleting clusters, VMs, or pod filesystem paths.

## Failure behavior

- Optional runtime failures do not stop native project orchestration.
- Missing CLIs, unreachable daemons, unavailable Kubernetes APIs, and permission failures return actionable errors.
- Porto refuses destructive canonical Docker socket changes it cannot safely reverse.
- Failed cluster creation removes VMs created during that attempt and reports cleanup errors.
- Runtime command timeouts are bounded; image builds and provisioning receive longer explicit limits.

## Desktop and web

The same frontend is served in the browser and loaded by Porto Desktop. `localhost-ing` remains the native-project control surface, while the resource sections expose Docker, Kubernetes, and VM inventories.

Closing the browser or desktop window does not stop the daemon. Stopping the daemon still performs the existing graceful shutdown of native projects and Sendbox sessions managed by that daemon.
