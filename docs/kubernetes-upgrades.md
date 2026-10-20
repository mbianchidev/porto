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
checks storage support. It saves private application manifests and locally
available application images. After stopping the original nodes, it streams
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
hard-coded old cluster IPs.

### Storage and consistency boundaries

Supported storage is a filesystem PV under `/var/local-path-provisioner/` with
node affinity identifying exactly one Porto kind node. PVC names and data paths
are preserved; the PV/PVC relationship is rebuilt with the new Kubernetes UIDs.

Other PV backends and direct pod `hostPath`, inline CSI, NFS, or other unsupported
volume sources block the replacement during preflight. Migrate these to supported
local-path PVCs or use their storage provider's recovery procedure first.
`emptyDir` remains ephemeral and is not carried into replacement pods.

Each volume or image archive is limited to 2 GiB by Porto's Docker archive API.
A failed export leaves the original node data intact. Ensure sufficient free
disk space for the original nodes, replacement nodes, and recovery archives.
Images must already be available in the source nodes; missing images block
preflight rather than leaving a restored application unable to start.

Copies are crash-consistent after node shutdown, not database-aware backups.
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

## API

`POST /api/kubernetes/clusters/{name}/upgrade` accepts
`{"version":"v1.37.1+k3s1","confirm":true}`. Successful responses contain the
version, message, and an optional recovery backup path.

`POST /api/kubernetes/clusters/{name}/upgrade-recover` accepts
`{"confirm":true}` and is restricted to failed kind upgrades with a matching
Porto-owned recovery record.
