# Local runtimes

Porto can manage native development applications, Docker resources, local Kubernetes clusters, and standalone Linux virtual machines from the same daemon, CLI, and dashboard.

Native project orchestration remains the default. The Docker API server is enabled by default. Its containerd execution backend, Kubernetes, and VM providers report clear unavailable states when required tools are not installed or running.

Enable providers independently:

```sh
porto runtime enable docker
porto runtime enable kubernetes
porto runtime enable vms
porto runtime status
```

Disable a provider with `porto runtime disable <provider>`. Disabling Docker closes its API socket; deactivate the canonical Docker endpoint first when Porto owns it.

## Architecture

Porto separates the control plane from runtime providers:

- The Porto daemon owns the HTTP API, dashboard state, project lifecycle, local routes, and Docker Engine-compatible API server.
- Native projects continue to run directly on the host.
- Docker clients connect to Porto through the named `porto` context.
- Porto translates supported Docker API operations to nerdctl and a Porto-owned containerd backend, either local or in a persistent Lima VM.
- Kubernetes inspection uses the selected `kubectl` context.
- Porto-created Kubernetes clusters use Lima VMs running k3s.
- Standalone Linux VMs use Lima and remain separate from Kubernetes node groups.

This design keeps supported Docker clients compatible without forwarding requests to another Docker daemon.

## Requirements

Core Porto only requires the dependencies listed in the [installation guide](installation.md).

Additional runtime features require:

| Capability | Requirement |
| --- | --- |
| Docker client compatibility | Docker CLI or another Docker Engine API client |
| Containers, images, networks, volumes | `nerdctl` with containerd and BuildKit, or `limactl` for Porto-managed containerd and BuildKit |
| Compose project orchestration | Docker CLI with Compose, using Porto's native Docker endpoint |
| Kubernetes inspection | `kubectl` and an authorized kubeconfig context |
| Porto-created Kubernetes clusters | `kubectl`; Porto can install `kind` and `limactl` on macOS |
| Standalone virtual machines | `limactl` and host virtualization support |

Missing optional tools do not prevent native projects or the Porto daemon from running.

Inspect or install provider tools:

```sh
porto runtime providers
porto runtime install lima
porto runtime install kind
porto runtime install k9s
porto runtime install k0s
```

The k0s and k3s providers both use Lima on the host and install their
distribution inside Porto-managed guests. Porto uses Homebrew for explicit
provider installation on macOS. Other
platforms receive an actionable manual-install error instead of a silent
fallback.

## Docker Engine-compatible endpoint

The daemon creates a user-owned socket:

```text
<PORTO_HOME>/run/docker.sock
```

On Windows, Porto exposes the equivalent named pipe:

```text
\\.\pipe\porto_docker_engine
```

The endpoint is served by Porto itself whenever the Docker runtime is enabled. It can answer health and system information requests even when the containerd execution backend still needs installation.

```sh
porto docker context-install
docker --context porto info
```

Install the execution backend before creating containers:

```sh
porto docker engine-install
porto docker status
```

See [Porto Docker Engine](docker-engine.md) for architecture, supported Docker API routes, persistence, cleanup, and limitations.

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

The Porto-owned API socket uses mode `0600`. Treat access to any Docker socket as host-administrator access.

Windows clients use the named `porto` Docker context. Canonical
`\\.\pipe\docker_engine` takeover is intentionally not automatic because
Windows named pipes cannot be replaced with a reversible symbolic link.

### Docker resources

```sh
porto docker status
porto docker engine-install
porto docker containers
porto docker images
porto docker networks
porto docker volumes

porto docker container restart <container>
porto docker pull alpine:latest
```

Top-level inventory aliases are also available:

```sh
porto containers
porto images
porto builds
porto networks
porto volumes
```

Compose projects use their standard Compose files against Porto's native Docker
endpoint. Porto assigns a stable Compose project name per Porto project and generates a temporary
`!override` file that remaps published TCP ports onto free localhost ports.
This keeps concurrent projects and branch worktrees isolated without editing
the source Compose file. Porto exposes Compose project and service labels from
Docker so the dashboard can group related containers.

Compose builds use Porto's BuildKit bridge, including multi-platform Bake
targets when the worker supports the requested architectures.
The Builds screen reads BuildKit history directly and reports active, successful,
and failed records with their creation time, duration, image name, and platform.

## Kubernetes

Porto operates only contexts it created and stores under `PORTO_HOME`.
It does not implicitly inherit the current global `kubectl` context. External
contexts remain untouched.

```sh
porto kubernetes status
porto kubernetes contexts
porto kubernetes pods --namespace all
porto kubernetes services --namespace default
porto kubernetes configmaps --namespace default
porto kubernetes secrets --namespace default
porto kubernetes nodes
```

Pass `--context` when a command should not use the current context.
`porto kubernetes configs` is a shorter alias for `configmaps`.
Stopped managed clusters keep their kubeconfig and context, but the dashboard
does not poll resource APIs until the selected context is reachable. On first
load it prefers a fully running managed cluster; start a stopped cluster or
select another context before opening its resources.

### Create a local cluster

Porto supports three native-engine providers:

- **k3s** (default): lightweight Kubernetes on Porto-managed Lima VMs
- **k0s**: conformant Kubernetes on Porto-managed Lima VMs
- **kind**: Kubernetes nodes in privileged containers through the Porto Docker endpoint

Porto-managed clusters include a default local-path storage class and Envoy
Gateway. kind clusters also include metrics-server v0.9.0 for pod, container,
and node CPU/memory stats. Porto installs or repairs these add-ons during
cluster creation and startup.

Porto names k3s contexts `porto-k3s-<cluster>` and migrates older
`porto-<cluster>` kubeconfigs automatically. New k3s clusters disable the
packaged Traefik chart; all three providers use the same Gateway API data
plane instead.

For k3s and k0s, Porto provisions one Lima VM for the controller and one VM
for each requested worker. CPU, RAM, and disk values are per VM. Nodes share
Lima's `user-v2` network, the Kubernetes API is forwarded to an allocated
loopback port, and persistent volumes remain on the cluster nodes across stop
and start operations.

Porto continuously reconciles HTTP-capable Service ports in running managed
clusters into Gateway API `HTTPRoute` resources. ClusterIP, NodePort, and
LoadBalancer Services receive a stable hostname such as
`api-8080.default.dev.porto.localhost`. One internal Envoy data-plane forward
per cluster feeds Porto's existing HTTP/HTTPS router, so service URLs use the
same DNS behavior and trusted certificates as Compose and native projects.
The hostname mapping is stored in Porto's SQLite database and survives daemon
and cluster restarts. Non-HTTP TCP ports, including database Services, retain
direct localhost forwarding and now support ClusterIP Services too. Deleting
the cluster removes its mappings and forwards.

Porto recognizes HTTP routes from `appProtocol: http`, conventional `http` or
`web` port names, and common development HTTP ports. Give uncommon web ports
an explicit `appProtocol` or port name; other TCP ports appear as raw
`localhost:<port>` endpoints.

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

### Open a cluster terminal

Desktop releases bundle k9s. Select a managed cluster in the dashboard and
open the **k9s terminal** tab; Porto launches k9s with that cluster's private
kubeconfig, context, and all namespaces already selected.

The embedded PTY is available on macOS and Linux. On Windows, use the bundled
CLI command below from PowerShell or Windows Terminal.

The CLI opens the same scoped TUI in the current terminal:

```sh
porto kubernetes terminal dev
porto kubernetes terminal dev --namespace platform --command deployments
porto kubernetes terminal dev --readonly
```

Inside k9s, press `?` for help, type `:ctx` to inspect contexts, `:ns` to
switch namespaces, `:pods` to return to pods, `l` for logs, `s` for a pod
shell, `Esc` to leave a view, and `Ctrl+C` to exit. Porto does not merge or
replace the user's global kubeconfig for this terminal.

Source installations can install k9s on macOS with:

```sh
porto runtime install k9s
```

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

The Activity screen samples current CPU and memory for Porto itself, native
projects, containers, Kubernetes nodes and pod containers, and standalone
Lima VMs. Kubernetes pod rows are detail-only because their usage is already
included in the node totals.

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

The dashboard adds pod overview, streaming logs, an expandable xterm terminal backed by a real PTY, bounded text-file editing, resource statistics, events, readable and copyable effective manifest views, ConfigMap inspection, and Secret inventory. ConfigMap text values are visible to authorized users. Secret values are never returned by Porto; only names, types, and data keys are exposed. All operations use the selected kubeconfig identity and never bypass Kubernetes RBAC or container filesystem permissions.
ConfigMap inspectors provide copy actions for every text or base64-encoded binary value and for the complete Kubernetes resource as formatted JSON.

File reads and writes are limited to 1 MiB per request. Changes inside an ephemeral container filesystem may disappear when Kubernetes replaces the pod.
File inspection requires `sh` and basic POSIX utilities inside the selected container. Shellless `scratch` and distroless images report that file inspection is unavailable instead of exposing the underlying `kubectl exec` command.

The pod terminal can start a one-hour ephemeral debug toolbox when the
application image has no shell or troubleshooting utilities. Porto uses the
multi-platform image
`docker.io/alpine/k8s:1.36.1@sha256:692239d739589247c4a791205ed9619c28ae85a21286e19a6211c04a62c56668`
so the toolbox contents are immutable. It includes `kubectl`, Bash, BusyBox
utilities, `curl`, `jq`, `yq`, Git, Helm, and Kustomize. The debug container
shares the pod network, targets the selected container's process namespace,
and mirrors its volume mounts so in-cluster credentials and mounted data remain
available. It uses a non-root, restricted-compatible security context; tools
that require elevated Linux capabilities remain subject to the pod's security
policy. Subpath mounts are exposed at their full volume root because Kubernetes
does not permit `subPath` or `subPathExpr` on ephemeral containers. Creating it
requires the selected Kubernetes identity to update the
pod's `ephemeralcontainers` subresource. Kubernetes retains the terminated
ephemeral-container record until the pod is replaced.

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
  --vm-type qemu \
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

Snapshot names follow the same 1-63 character lowercase letter, number, dot, and hyphen format as VM names. Lima snapshots are experimental and currently available only for QEMU-backed VMs. On macOS, install QEMU with `brew install qemu`, restart Porto, then create the VM with `--vm-type qemu` or choose the QEMU driver in the dashboard. Porto temporarily stops running VMs for snapshot creation and restoration, then restarts them even when the snapshot command fails. Existing `vz` VMs cannot be converted and must be recreated to use snapshots.

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

- Containerd, Kubernetes, and VM runtime failures do not stop native project orchestration or the Docker API health endpoint.
- Missing CLIs, unreachable daemons, unavailable Kubernetes APIs, and permission failures return actionable errors.
- Unsupported Docker Engine API operations return HTTP 501 with a Docker-compatible JSON error.
- Porto refuses destructive canonical Docker socket changes it cannot safely reverse.
- Failed cluster creation removes VMs created during that attempt and reports cleanup errors.
- Runtime command timeouts are bounded; image builds and provisioning receive longer explicit limits.

## Desktop and web

The same frontend is served in the browser and loaded by Porto Desktop. `localhost-ing` remains the native-project control surface, while the resource sections expose Docker, Kubernetes, and VM inventories.

Closing the browser or desktop window does not stop the daemon. Stopping the daemon still performs the existing graceful shutdown of native projects and Sendbox sessions managed by that daemon.
