# Kubernetes cluster upgrades

Select a managed cluster in **Kubernetes**, open **Upgrade**, and enter a full
distribution release. Porto requires explicit confirmation because upgrades
interrupt cluster access and workloads. The API accepts upgrades only for
Porto-owned clusters, never an unrelated global kube context.

```sh
porto kubernetes cluster upgrade dev --version v1.37.1+k3s1 --confirm
porto kubernetes cluster upgrade k0s-dev --version v1.36.4+k0s.1 --confirm
porto kubernetes cluster upgrade kind-dev --version v1.37.0 --confirm
```

Choose a release compatible with the currently installed version. Downgrades,
prereleases, moving channels such as `latest`, and skipped Kubernetes minor
versions are rejected. Repeat the operation one minor version at a time when
crossing several minor releases. Porto checks the actual node inventory and
kubelet versions before changing anything; every node must be owned and Ready.

The cluster inventory reports **upgrading** and then the configured version
only after completion. Operations have a two-hour bound and continue in the
daemon if the inspector or requesting browser closes. Other mutations of the
same cluster are locked. Explicit port forwards are stopped before upgrade;
reopen them afterward. A daemon interruption is recorded as **upgrade-failed**.

## k3s and k0s

Porto upgrades the controller first, then one worker at a time. Each replacement
binary is downloaded from the selected upstream release and verified against its
published SHA-256 checksum before installation. Existing systemd arguments,
join configuration, disks, labels, and taints remain intact.

The single controller restarts in place, causing a brief API outage. Its
workloads are not evicted. Worker drains respect PodDisruptionBudgets and do not
force deletion of `emptyDir` data. A blocked drain fails visibly; adjust the
workload or disruption budget before retrying. Workers return to Ready at the
target Kubernetes version before the next node starts. Previously cordoned
workers stay cordoned.

If an upgrade fails, the old configured version and target/error remain in
metadata, even if some nodes already upgraded. Restore node health, then choose
**Retry upgrade** or repeat the same command. Porto does not attempt an unsafe
automatic downgrade of an already upgraded Kubernetes datastore.

## kind: replacement with preservation

kind does not offer an in-place Kubernetes version upgrade. Porto creates a
replacement with a distinct runtime name and the same topology, leaving the
original nodes stopped rather than deleting them.

Before stopping the original cluster, Porto discovers application resources and
checks storage support and intact Docker stdin forwarding. An older running
daemon with a broken guest stdin bridge is rejected before replacement starts;
restart with the updated Porto build first. It saves private application manifests
and exportable application images. When kind has discarded an image's compressed
cache layers, Porto records its immutable registry digest and restores that exact
image using the workload's restored pull credentials instead of following a moving
tag. After stopping the original nodes, it streams
local persistent-volume data into checksummed archives. The original containers
and their data remain available throughout replacement.

Porto then:

1. Creates the replacement at the requested version and imports application images.
2. Copies volume data to the corresponding replacement nodes before workloads start.
3. Restores namespaces, CRDs, configuration, credentials, PVs, PVCs, and application resources.
4. Rewrites node-name constraints and owner references for the new cluster identities.
5. Verifies node versions/readiness, PVC bindings, workload rollouts, and standalone pod readiness.

Replica counts and suspended CronJob configuration are preserved. Controller-owned
pods and ReplicaSets are recreated by their owners, not replayed as independent
applications. Completed Jobs are not rerun. Services keep their names and desired
ports but receive new cluster IPs; applications must use service DNS rather than
hard-coded old cluster IPs. Service-account token Secret names are preserved,
but their old token/CA/UID are cleared so the new cluster's controller can issue
valid replacement credentials.

### Storage and consistency boundaries

Supported storage is a filesystem PV under `/var/local-path-provisioner/` with
node affinity identifying exactly one Porto kind node. PVC names and data paths
are preserved; the PV/PVC relationship is rebuilt with the new Kubernetes UIDs.

Other PV backends and direct pod `hostPath`, inline CSI, NFS, or other unsupported
volume sources block the replacement during preflight. Migrate these to supported
local-path PVCs or use their storage provider's recovery procedure first.
`emptyDir` remains ephemeral and is not carried into replacement pods.

Each upgrade volume or image archive has a 2 GiB safety limit.
A failed export leaves the original node data intact. Ensure sufficient free
disk space for the original nodes, replacement nodes, and recovery archives.
Images must already be available in the source nodes; missing image identities
block preflight. Re-pulling discarded layers requires the original registry to
remain reachable with valid workload credentials. `imagePullPolicy: Never` is
never changed implicitly: if such an image is not exportable, preload it with
`porto kubernetes cluster image-import` before retrying. The source cluster is
not stopped for that preflight failure.

Copies are crash-consistent after node shutdown, not database-aware backups.
Short-lived, network-disabled copy helpers mount the node's persistent Docker
volume directly, read-only for backup, so a stopped node does not need to run
an exec process. Helpers use the already cached node image, preserve ownership,
permissions and extended metadata, and are removed by exact newly created
container ID without deleting the attached persistent volume.
Quiesce applications and take application-specific backups when transactional
consistency is required. Persistent volumes do not preserve external systems,
in-flight requests, or process memory.

## Recover a failed kind replacement

Use **Recover original cluster** in the Upgrade tab, or:

```sh
porto kubernetes cluster upgrade-recover kind-dev --confirm
```

Recovery first stops a failed replacement, then restores the original kubeconfig
and starts the original nodes. It never merges writes made inside the replacement
back into the original cluster. The global kubeconfig entry is refreshed without
changing unrelated entries or the user's current context.

Successful upgrades retain both the original stopped kind nodes and a private
`.upgrade-<cluster>-...` recovery directory under Porto's Kubernetes state
directory. The Upgrade tab and API result show its exact path. The directory
contains application Secrets and kubeconfig credentials: do not publish it or
include it in an unredacted diagnostic bundle.

No automatic retention deletes recovery material. After independently verifying
the replacement and retaining any desired backup, the original runtime name is
recorded in the directory's `recovery.json`; use kind against the Porto Docker
endpoint to delete only that exact old runtime. Do not delete the replacement
runtime named in the current cluster ownership metadata.

## Newer Kubernetes releases

Porto has no maximum Kubernetes version. A new minor release, such as 1.38,
works as soon as the provider publishes a stable build for it. Use the
provider's release format: `v1.38.0+k3s1` for k3s, `v1.38.0+k0s.0` for k0s,
and `v1.38.0` for kind and Apple. Upgrades reject prereleases (`-alpha`,
`-beta`, `-rc`) and moving channels. Cluster creation passes `--version` to the
provider unchanged, so give it a published stable release.

### New clusters

| Provider | Without `--version` | Select a newer release |
| --- | --- | --- |
| k3s | k3s `stable` channel at creation time | `--version v1.38.0+k3s1` |
| k0s | k0s stable release at creation time | `--version v1.38.0+k0s.0` |
| kind | Default node image of the bundled kind binary | `--version v1.38.0` (a `kindest/node` tag) |
| Apple | Porto's pinned `kindest/node` image | `--version v1.38.0` |

k3s and k0s pick up a new minor version once their stable channel moves to it,
which can take several weeks after the upstream Kubernetes release. Pass an
explicit `--version` to use it before then. kind node images are built for a
specific kind release, so a newer minor version can need a Porto release that
bundles a newer kind.

```sh
porto kubernetes cluster create dev --provider k3s --version v1.38.0+k3s1
```

### Existing clusters

Upgrade one minor version at a time. A 1.36 cluster reaches 1.38 in two steps:

```sh
porto kubernetes cluster upgrade dev --version v1.37.1+k3s1 --confirm
porto kubernetes cluster upgrade dev --version v1.38.0+k3s1 --confirm
```

Apple clusters can't be upgraded. Create a new cluster at the target version
and move workloads to it.

### Maintainer checklist for a new minor version

Make these changes once the upstream release and the provider builds are
published. Pinned components must still support the older cluster versions
people run, not only the new one.

1. **Bundled kubectl**: `KUBECTL_VERSION` in
   `scripts/bundle-desktop-runtime.sh`. Porto runs kubectl for cluster
   operations, and kubectl supports servers only one minor version older or
   newer. Use the minor version before the newest one: kubectl 1.37 covers 1.36
   to 1.38 clusters. Update the bundled version in
   [remote environments](remote-environments.md).
2. **Debug toolbox**: `DebugToolboxImage` in `internal/kubernetes/debug.go`,
   pinned by digest. Its kubectl follows the same rule. Update the image
   reference in [runtimes](runtimes.md).
3. **kind**: `KIND_VERSION` in `scripts/bundle-desktop-runtime.sh`, once a kind
   release lists `kindest/node` images for the new minor version in its release
   notes.
4. **Apple default image**: `appleNodeImage` in
   `internal/kubernetes/apple_native.go`, pinned by digest. Update its version in
   [runtimes](runtimes.md) after verifying a native cluster.
5. **Go Kubernetes libraries**: the `k8s.io/*` modules in `go.mod`, plus
   `sigs.k8s.io/controller-runtime` and `sigs.k8s.io/karpenter` when they need to
   match. Update the client-go version in
   [remote environments](remote-environments.md).
6. **Cluster add-ons**: check each project's Kubernetes compatibility matrix and
   bump if needed. Envoy Gateway is `envoyGatewayManifestURL` in
   `internal/kubernetes/provision.go`; Karpenter and KEDA are in
   `internal/autoscaling/types.go`; Flux, Argo CD and Helm are in
   `internal/kubeadmin/gitops_types.go` and `internal/kubeadmin/helm_types.go`.
7. **Examples and tests**: the version placeholders in
   `ui/src/pages/KubernetesOverview.tsx` and `ui/src/pages/ClusterUpgrade.tsx`,
   the examples in this document, and the source and target versions of the
   kind upgrade integration test (`internal/kubernetes/upgrade_integration_test.go`,
   described in [Kubernetes autoscaling](kubernetes-autoscaling.md)). Move that
   test to upgrade from the previous minor version to the new one.

k3s and k0s need no Porto change: Porto downloads the selected release from
upstream when it installs or upgrades a node.

## API

`POST /api/kubernetes/clusters/{name}/upgrade` accepts
`{"version":"v1.37.1+k3s1","confirm":true}`. Successful responses contain the
version, message, and an optional recovery backup path.

`POST /api/kubernetes/clusters/{name}/upgrade-recover` accepts
`{"confirm":true}` and is restricted to failed kind upgrades with a matching
Porto-owned recovery record.
