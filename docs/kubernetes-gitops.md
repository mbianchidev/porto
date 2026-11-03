# Kubernetes-native GitOps delivery

Open **Kubernetes > GitOps applications**. Select the Kubernetes context,
application namespace, controller and controller namespace before preparing an
operation. Flux and Argo CD are alternative application owners. Discovery never
installs either controller, and using one does not require the other.

This is separate from [Compose application delivery](project-management.md#application-delivery)
and [direct Kubernetes/Helm administration](runtimes.md#manifest-workload-and-permission-administration).
Compose Git polling and webhooks are not Kubernetes reconciliation controllers.
Argo CD here means application reconciliation, not Workflows, Rollouts or Events.

## Supported contracts

| Controller | Bootstrap pin | Application APIs |
| --- | --- | --- |
| Flux | 2.10.0 | GitRepository/HelmRepository `source.toolkit.fluxcd.io/v1`, Kustomization `kustomize.toolkit.fluxcd.io/v1`, HelmRelease `helm.toolkit.fluxcd.io/v2` |
| Argo CD | 3.5.7 | Application/AppProject `argoproj.io/v1alpha1` |

Flux uses source-controller 1.10.1, kustomize-controller 1.10.1 and
helm-controller 1.7.1. Bootstrap omits optional Flux image, notification and
source-watcher components. Argo CD uses the pinned upstream installation,
including its Redis 8.2.3 dependency. Discovery checks the controller images,
complete component rollout and required fields in the installed, served CRD
schemas. An unsupported image/schema, unavailable controller, authentication
failure or denied API read is not treated as an empty successful installation.

The initial workflows support Flux Git/Kustomization and
Helm-repository/HelmRelease applications, and single-source Argo CD Git directory,
Kustomize and Helm applications. Executable renderer plugins, cross-namespace
sources/dependencies and external Argo destinations are not enabled. Application
access follows the selected service account/project; do not grant cluster-wide
privileges to an application source. Existing externally owned application roots remain
visible but are not taken over implicitly.

## Controller installation and recovery

Expand **Controller installation** separately from application management.
Bootstrap uses `flux-system` or `argocd` in the selected managed context.
Existing compatible installations can be discovered in another explicitly
selected controller namespace. Source installations need no Flux/Argo CLI.
Direct Helm handoff and rendered Helm uninstall inspection require the existing
verified [Helm toolchain](runtimes.md#helm-repositories-and-releases).

The selected Kubernetes identity must be able to inspect the served APIs and
CRDs and create the installation's namespace, CRDs, RBAC and workload resources.
Read-only identities cannot bootstrap. Native RBAC, admission, binding and
escalation checks remain authoritative; Porto never substitutes an administrator
kubeconfig, global context, insecure connection or force-overwrite.

The installer downloads reviewed versioned upstream manifests over verified
HTTPS using Porto networking settings. Cached manifests are rechecked against
these SHA-256 pins:

```text
Flux 2.10.0
a8c838ccd9be5d30eba9df87cd229b7c4eb80c0049fba1bc3b9840829f267f28

Argo CD 3.5.7
09bfbb9041b4a8503c6e4fa22cf41c403fa62be17526de353648a4551f5beb77
```

Preview shows namespace/CRD/RBAC consequences and requires extra acknowledgement.
When the controller namespace is absent, its creation and cluster resources can
be dry-run, but namespaced admission is explicitly deferred until activation.
Admission, quota or rollout failure can leave a partial installation.
Controller updates use non-forcing server-side apply so generated Secret values
and separately managed settings are not claimed or replaced.

An encrypted installation inventory records resource UIDs as work progresses.
Recover an interrupted bootstrap by inspecting operation history and previewing
the same pinned bootstrap again. Upgrade/removal uses installation ownership,
not resource names alone. Keep Porto's credential-backed state and recovery
access outside the failed cluster's only dependency chain.

Removing a controller is blocked while any application of that controller
exists, including unrelated namespaces. It removes recorded owned components,
not namespaces, CRDs or applications. Missing credentials, replaced resource
UIDs and pending finalizers require inspection rather than forced cleanup.

## Namespace-scoped ownership protection

Application mutation requires the reviewed native ValidatingAdmissionPolicy and
a deny binding scoped to the selected application namespace. Explicit controller
bootstrap includes this protection. For an existing supported installation,
choose **Enable selected namespace ownership guard** under controller lifecycle;
this requires separate infrastructure authority but does not reinstall or claim
the controller.

The policy rejects changes between Flux, Argo CD and native Helm owners,
multiple native owners, adoption of an existing controller-generated child and
application adoption of Porto control/credential resources. Its namespace
selector does not enable bindings in unrelated namespaces. Evaluation errors
fail closed. Existing conflicting ownership can require deliberate repair before
reconciliation will be admitted.

Native API support, policy type-checking, the exact policy/binding contract and
read permission are checked before application mutation. No automatic exception
or force ownership is used. Status/scale subresources remain delegated to their
native owners. Controller removal preserves the shared policy and namespace
binding, just as it preserves namespaces and CRDs. Infrastructure administrators
must retain their normal native recovery access; do not give application service
accounts cluster-scoped policy or namespace-label administration.

## Scoped sources and application prerequisites

Expand **Connect a source** to register an HTTPS Git or Helm source in this exact
environment/context/controller/namespace. URLs cannot contain credentials, query
tokens or fragments. Choose public access explicitly or supply a username and
password/token. Existing private access can be retained only for the same source
URL and kind. URL changes require an explicit credential/public-access decision.

Private access and pending previews use encrypted files backed by the system
credential store. Approved activation provisions only the application's native
source Secret; plaintext credentials do not enter Git, diffs, logs or operation
history. Controller diagnostic messages, inline sensitive values and Secret
contents are withheld from public GitOps views. Unlock/recover lost credentials
and reconnect the exact source; there is no public/global credential fallback.
Configure additional controller trust through its installation owner before
using a private CA; TLS verification is never disabled.

Flux applications require an existing reconciliation service account in the
selected application namespace, appropriate native Role/RoleBinding grants and
permission to delegate that identity. Grant only the resource kinds the source
needs. Do not give an application repository namespace/CRD/PV/RBAC administration
by default. Bootstrap disables cross-namespace references and Kustomize remote
bases and configures a default service account rather than an implicit elevated
fallback. Explicit same-namespace dependencies must already exist.

Argo CD applications require an explicit AppProject in the controller namespace
that permits the exact source URL and
`https://kubernetes.default.svc` destination/application namespace, with no
cluster-resource allowlist. Wildcard-only repository/destination grants are not
accepted. Create this project through an authorized native administration flow;
bootstrap does not grant tenant repositories authority over their own project.

**Remove local registration** removes local metadata/access only. Installed native
sources, source Secrets, applications, controllers and storage remain. Rotate
native access deliberately when retiring a credential that is still in use.

## Reconciliation, policy and observed health

Register a suspended application using an immutable 40/64-digit Git commit or
exact semantic chart version. Branches, moving tags, `HEAD` and version ranges
are not accepted as desired application revisions. Registration/configuration
updates do not apply workloads; they require a separate reviewed sync/resume.

- **Reconcile / sync** requests the intended revision through the native owner.
  A suspended application runs one attempt and is suspended again after
  observation when the API remains reachable. Source acquisition and dependency
  progress are allowed to complete instead of treating the first missing
  artifact as a permanent failure.
- **Suspend** pauses future reconciliation, not existing workload execution or
  data changes. It is not a rollback or a health guarantee.
- **Resume** explicitly enables the selected continuous policy. Running Flux
  Kustomizations inherently correct drift. Native Helm upgrades inherently prune
  removed chart resources; this cannot be represented as a disabled policy.
  Argo CD automatic sync, self-heal and pruning are separately selected.
- **Update** first suspends the application and replaces its reviewed scoped
  source/configuration. It remains suspended for explicit progression.
- **Recover** selects a compatible immutable desired revision, reconciles it
  once and suspends again. It changes the desired-state pointer, not Git history,
  database schemas, deleted volumes or application data.

The inventory and inspector distinguish desired, fetched and applied revisions,
source/render-admission/reconciliation/workload-health failures, native
conditions, drift, dependencies and the controller's last reported success.
Unknown/unreported values remain unknown. A synchronized revision does not prove
workload readiness, and native workload health does not prove an application SLO.
While suspended, observations are historical because reconciliation is paused.

Preview tokens expire after ten minutes, are single-use, and bind the selected
scope, Kubernetes identity, served schemas, source access and resource identities.
Activation rechecks those boundaries. Kubernetes administration, GitOps, direct
Helm and managed-cluster lifecycle share operation exclusion/history.
Cancellation or daemon restart can leave accepted native work; inspect exact
state before retrying. No automatic full rollback or success is implied.

## Ownership handoff and deletion

Use **Stage ownership handoff** for direct-managed resources. Supply 1-50 exact
top-level resource targets with context, namespace, group, version, resource,
name, UID and resourceVersion. Porto locks those identities against direct
manifest/workload mutation and creates a suspended native root. Competing
controllers, changed identities and native Helm-owned manifest targets are
rejected. Reconcile deliberately, inspect actual ownership/inventory/readiness,
then use **Verify ownership handoff**.

Direct Helm handoff uses a suspended Flux HelmRelease with the existing release's
exact name and storage namespace. It locks the native release store, creates no
parallel release, and requires native release-owner/history proof before
verification. Direct Helm operations remain blocked while a HelmRelease owns the
release, even if it is suspended.

Choose **Orphan / preserve** or **Controller-native prune / uninstall** explicitly.
Pruning, Helm hooks, namespace/CRD/storage consequences and unknown rendered
inventory require extra acknowledgement. Native finalizer failures remain
partial operations. Orphaning a HelmRelease explicitly removes only its Flux
finalizer after suspension; unrelated finalizers are preserved. Argo orphaning
removes only its native application-resource finalizer. Namespace and controller
deletion are never hidden behind application/source removal.

## API and verification

The target-scoped API is `/api/kubernetes/gitops`. Reads expose `controller`,
`applications`, `application` and `sources`; application changes use `previews`
then `apply`. Controller changes use separate `controller/previews` and
`controller/apply` endpoints. An application token cannot authorize controller
bootstrap. Environment routing reuses the existing explicit identity adapter;
application operations need workload authority and controller lifecycle needs
separate infrastructure authority plus native Kubernetes permissions.

Focused checks:

```sh
go test ./internal/kubeadmin ./internal/daemon ./internal/environments
npm --prefix ui test -- gitops.spec.ts
```

Native acceptance bootstraps real pinned controllers and serves a generated,
authenticated synthetic Git/Helm source over verified HTTPS. It requires an
explicitly disposable `porto-gitops-acceptance-*` managed cluster, reachable
source hostname and its exact owned kubeconfig/context:

```sh
PORTO_TEST_GITOPS_CONFIRM=disposable \
PORTO_TEST_GITOPS_KUBECONFIG=/absolute/path/to/disposable-kubeconfig.yaml \
PORTO_TEST_GITOPS_CONTEXT=porto-porto-gitops-acceptance-example \
PORTO_TEST_GITOPS_SOURCE_HOST=host.lima.internal \
go test ./internal/kubeadmin -run '^TestNativeGitOpsDelivery$' -count=1 -timeout=30m
```

This opt-in test installs controllers only in that disposable cluster, tests
Git updates/recovery and native Helm delivery, and verifies orphan preservation.
It must never target a personal/shared workload cluster. Delete the dedicated
managed cluster after acceptance; source-server and local test state are cleaned
up by the test.
