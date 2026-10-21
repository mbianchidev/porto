# Local Kubernetes autoscaling

Open a running managed cluster's **Autoscaling** tab. Node supply and workload
replicas are independent options:

| Controller | Purpose | Supported distributions |
| --- | --- | --- |
| Karpenter 1.14.2 with Porto's provider | Create real local Lima worker VMs for unschedulable pods | k3s, k0s |
| KEDA 2.21.0 | Scale workload replicas from event triggers | k3s, k0s, kind |

KEDA does not create nodes. Karpenter does not choose replica counts. Combining
them lets KEDA increase demand and Karpenter add capacity when fixed nodes cannot
schedule the resulting pods. kind topology remains immutable; its node option is
explicitly unavailable rather than backed by simulated nodes.

## Enable controllers

```sh
# Enable both with up to three additional worker VMs.
porto kubernetes cluster autoscaling dev --nodes --max-nodes 3 \
  --cpus 2 --memory 2048 --disk 20 --keda

# Inspect desired configuration, controller readiness and reserved capacity.
porto kubernetes cluster autoscaling dev

# Change only KEDA; omitted options preserve the existing configuration.
porto kubernetes cluster autoscaling kind-dev --keda

# Stop node provisioning without removing its existing VMs.
porto kubernetes cluster autoscaling dev --nodes=false
```

Saving configuration installs the pinned upstream CRDs/controller manifests and
waits for readiness. Porto refuses to take over foreign autoscaler installations
or resources. It uses a private kubeconfig and explicit context; neither
`KUBECONFIG` nor the user's current global context can redirect the controller.

The node controller runs in the Porto daemon on the host, using upstream
Karpenter scheduling and lifecycle controllers with a real Porto CloudProvider.
Its `PortoNodeClass` maps a machine shape to a managed Lima worker. It does not
expose a privileged host API inside the cluster and does not use KWOK or synthetic
Node objects. KEDA runs in the cluster's `keda` namespace.

The daemon must remain running for new node provisioning and consolidation.
KEDA continues operating in the cluster when the host node controller is paused.
Settings and claim reservations persist under the private Kubernetes state
directory's `autoscaling/<cluster>` subdirectory, separate from cluster metadata.

## Capacity and lifecycle safety

The minimum autoscaled worker count is zero. The control plane and fixed worker
groups are not adopted or removed by Karpenter. Existing fixed capacity is used
first when it satisfies scheduling constraints; enabling node autoscaling does
not immediately allocate the maximum worker count.

One managed `NodePool` and `PortoNodeClass`, both named `porto-local`, define
the automatic workers. To require an autoscaled worker for a workload, use:

```yaml
spec:
  template:
    spec:
      nodeSelector:
        karpenter.sh/nodepool: porto-local
```

Set resource requests and normal Kubernetes scheduling constraints accurately.
Impossible requests, incompatible labels/taints, storage affinity, or exhausted
budgets cannot be repaired by increasing replicas.

Bounds apply to automatic workers independently of fixed nodes:

- At most 32 workers, each with 1-32 CPUs, 1024-131072 MiB memory and 10-1024 GiB disk.
- Aggregate automatic-worker ceilings of 128 CPUs, 262144 MiB memory and 4096 GiB disk.
- The configured maximum counts persisted reservations, including uncertain failed
  creates, so retries and concurrent claims cannot bypass it.

These are safeguards, not a guarantee that the host has that much free capacity.
Choose a budget that leaves headroom for Porto, the engine, fixed clusters and
other applications. A maximum cannot be reduced below already reserved workers.

Worker installation pins the controller's actual full k3s/k0s release, including
legacy clusters created without a version pin. It does not follow a moving
installation channel or allow workers to silently jump ahead of the control plane.

Consolidation is conservative (`WhenEmpty`), not active-workload packing.
Persistent-volume and ephemeral-local-data protections prevent disposal of nodes
whose data would be lost. Disruption respects the controller's Kubernetes
eviction/PDB handling. Missing ownership or uncertain creation/deletion fails
closed and retains reservations rather than deleting a potentially foreign VM.
Check the Autoscaling status and daemon diagnostics when provisioning or cleanup
is blocked.

Start, stop, upgrade, recovery, rename and delete first pause and join the host
controller, then use the same mutation gate as normal cluster operations.
Successful start/upgrade/recovery resumes it. Stop leaves it paused.
Rename moves its host state; delete forgets host settings only after cluster
removal. Disabling the Kubernetes runtime pauses its node controllers without
removing cluster resources.

## Configure event-driven workloads

Use **Event-driven workload** in the inspector to create or update a Porto-owned
KEDA ScaledObject. Select a Deployment or StatefulSet, set minimum/maximum
replicas, and supply a JSON trigger array using the schema for KEDA 2.21.0.

Alternatively, save this synthetic request as `workload-scaling.json`:

```json
{
  "namespace": "default",
  "name": "daytime-workers",
  "targetName": "workers",
  "targetKind": "Deployment",
  "targetAPIVersion": "apps/v1",
  "minReplicas": 0,
  "maxReplicas": 5,
  "pollingInterval": 30,
  "cooldownPeriod": 300,
  "triggers": [
    {
      "type": "cron",
      "metadata": {
        "timezone": "Etc/UTC",
        "start": "0 8 * * *",
        "end": "0 18 * * *",
        "desiredReplicas": "2"
      }
    }
  ]
}
```

```sh
porto kubernetes cluster workload-scale dev --file workload-scaling.json
porto kubernetes cluster workload-unscale dev default daytime-workers --confirm
```

The target must already exist. Use a single replica-management owner for each
workload. Configure authentication through existing `TriggerAuthentication` or
`ClusterTriggerAuthentication` resources, referenced in each trigger's
`authenticationRef`; do not embed credentials in trigger metadata.

Removing a scaler stops event-driven replica control; it does not remove the
workload. Disabling KEDA refuses to proceed while ScaledObjects or ScaledJobs
remain, including foreign ones. It does not silently delete their configuration.
Remove or transfer workload scaling deliberately before disabling the controller.

## API

`GET /api/kubernetes/clusters/{name}/autoscaling` returns desired `nodes`/`keda`
configuration plus controller versions, `nodeSupported`, `running`, `paused`,
`managedNodes`, `kedaReady`, and an optional diagnostic message.

`POST` to the same URL accepts the complete configuration:

```json
{
  "nodes": {
    "enabled": true,
    "maxNodes": 3,
    "machine": {"cpus": 2, "memoryMiB": 2048, "diskGiB": 20}
  },
  "keda": true
}
```

`POST /api/kubernetes/clusters/{name}/autoscaling/workloads` accepts the
ScaledObject request shown above. `DELETE` its
`/autoscaling/workloads/{namespace}/{scaler}?confirm=true` URL to remove a
Porto-owned scaler.

## Development checks

Mocked tests cover ownership, idempotent claims, reservation bounds,
controller cancellation, failure recovery, installation conflicts, workload
scalers, and actual-controller version pinning:

```sh
go test -race ./internal/autoscaling ./internal/kubernetes ./internal/daemon
```

The isolated kind preservation smoke uses only synthetic workload data and a
uniquely named cluster against an explicitly selected local Porto Docker endpoint:

```sh
PORTO_KUBERNETES_TEST_DOCKER_HOST="$(docker context inspect porto --format '{{.Endpoints.docker.Host}}')" \
PORTO_KUBERNETES_UPGRADE_INTEGRATION=1 \
  go test ./internal/kubernetes -run '^TestKindUpgradeVolumeIntegration$' -count=1 -timeout=70m -v
```

Set `PORTO_KUBERNETES_UPGRADE_SOURCE_ENGINE=1` instead of providing a Docker host
to create a separate, invocation-private Lima engine and exercise the current
source Docker API. This costs an additional VM but never replaces or stops the
installed engine. Both modes verify stdin forwarding before attempting kind
creation, so an older daemon cannot consume the full node-creation timeout on
an input-bridge failure.

It creates Kubernetes 1.36.4, writes a marker only in the source PVC, upgrades to
1.37.0, and checks that the redeployed application reads the original marker.
Its cleanup targets only the exact source and replacement runtime names recorded
by that test; it never stops or removes another cluster.
