# Optional integrations

Porto's optional integrations are disabled until explicitly enabled. Project
integrations additionally require their compatible managed-project inputs.
Etcqlite has a separate, explicit datastore lifecycle and never scans projects.

Separately installed tooling uses the [permission-scoped extension system](extensions.md).
Those packages have independent verification, explicit grants and isolated
execution; they do not replace or automatically activate the built-in
integrations below. Local image analysis remains its own
[Syft/Grype workflow](image-inspection.md), not a duplicate scanner extension.

## sql-not-so-lite

Enable [sql-not-so-lite](https://github.com/mbianchidev/sql-not-so-lite) from the dashboard's **Optional integration** panel. Porto checks managed project roots for files with SQLite extensions and validates the SQLite file header before doing external work.

If no orchestrated project contains a valid SQLite database, Porto does not install or run anything. When an eligible project exists, Porto uses an existing `sqnsl` binary from `PATH`, or installs the pinned integration revision with Go, then runs:

```sh
sqnsl scan <project-path>...
```

Daemon activation and rescans run in the background and expose their current state in the dashboard. Offline `porto scan` commands run the integration synchronously. Output and failures are recorded in eligible project logs under the `sqnsl` stream.

Removed project roots and database candidates that disappear during discovery
are skipped and logged, so a deleted worktree or build directory does not block
other projects. Missing paths do not trigger installation or an error state
when no databases remain. Permission failures and other I/O errors are still
reported; Porto does not delete or recreate stale project directories.

## Etcqlite

[Etcqlite](https://github.com/mbianchidev/etcqlite) is an opt-in native datastore
integration on **macOS and Linux**. It supports a standalone member or three
host-local quorum members, plus the external-etcd endpoint contract for
**new k3s clusters only**. Three members on one host do not provide independent
failure domains. This is a local-development profile, not a production HA claim.
Windows native lifecycle, kind/k0s datastore integration, live migration,
membership changes and automatic engine/schema upgrades are not advertised.

Install matching `etcqlite` and `etcqlite-snapshot` binaries separately. Keep the
snapshot helper beside the resolved native executable, including when the main
binary is reached through a symlink. Porto does not download, build or patch them.
Expose the main binary in the **daemon's** `PATH`, or set
`PORTO_ETCQLITE_BINARY` to its absolute path before launching Porto. The optional
`PORTO_ETCQLITE_SNAPSHOT_BINARY` override must resolve to that same sibling helper,
because the native CLI's offline snapshot commands use the sibling too.

Open **Settings > Optional Etcqlite datastore**, or run:

```sh
porto etcqlite enable
porto etcqlite create local-db --members 1
porto etcqlite start local-db
porto etcqlite status
porto etcqlite logs local-db --stream stderr --tail 100
porto etcqlite backup local-db
porto etcqlite backups
```

Enabling only saves the gate. Initialization does not start a member, and
selecting a datastore for k3s does not enable, install or start it. While disabled,
inventory is an empty disabled response and native actions fail before binary
lookup, filesystem discovery or execution. No cluster is scanned or retargeted.
Other integrations, existing clusters and provider datastore defaults remain
unchanged.

### Identity, storage and restart behavior

Each instance has a persistent random nonzero 64-bit cluster ID and individual
member IDs. Identity-bearing control requests use decimal **strings**, preserving
all 64 bits in browsers. Three-member genesis uses a shared configuration with
distinct member/PID assignments and separate private `members/<member>/data`
directories. The runtime never shares a live SQLite/WAL directory between nodes.

Use an absolute, node-local `PORTO_HOME`. State, certificates and stdout/stderr
files live under `PORTO_HOME/etcqlite/instances/<opaque-name-token>`. Directory
components use Porto's SHA-256-derived file tokens rather than user-provided path
text; metadata preserves the display name and immutable IDs. macOS requires a local
filesystem; Linux accepts ext, XFS, Btrfs, ZFS, overlay, tmpfs and ramfs and rejects
other/shared/network/FUSE filesystems. tmpfs/ramfs are suitable only for disposable
fixtures, not persistent datastores. Missing or symlinked node data is not
reinitialized. Interrupted initialization stays failed with its existing data.

Porto generates a separate per-instance CA, private client credentials and
member certificates, without adding trust to the OS or changing Porto's web CA.
All managed client connections use mTLS, including loopback; three-member peer
traffic uses loopback mTLS and
`spiffe://etcqlite/<16-digit-hex-cluster-id>/<16-digit-hex-member-id>` identities.
Certificates remain persistent. Expired or invalid credentials produce visible
readiness errors; they are not replaced behind a pinned k3s binding.

Native members outlive a daemon/desktop exit. Direct private file stdio prevents
broken pipes, and enabled restart reconciliation checks PID **plus kernel birth
identity and executable path** before reusing or signaling a process. It never
signals an unrelated reused PID or clears occupied ports. Only already requested
running instances are reconciled; existing data is never passed through `init`
again. Readiness requires authenticated discovery with the expected cluster and
member identities and a successful quorum/read `/readyz` barrier. A `started`
log line is not readiness. Member failures, bounded log tails and process resource
usage are visible through the integration API.

### New k3s clusters

Loopback client advertisements stay local. For a guest-reachable Lima endpoint,
explicitly opt into a reachable host listener and advertisement:

```sh
porto etcqlite create k3s-db --members 3 \
  --advertise-host host.lima.internal --listen-host 0.0.0.0
porto etcqlite start k3s-db
porto kubernetes cluster create new-dev --provider k3s --etcqlite k3s-db
```

Host firewall and Lima DNS/routing must permit the connection. Porto does not
rewrite a loopback endpoint, proxy the datastore or move data into a VM mount.
Only the new k3s server receives private CA/cert/key files and
`--datastore-endpoint`, `--datastore-cafile`, `--datastore-certfile` and
`--datastore-keyfile`; worker setup is unchanged. A bounded guest-side mTLS
readiness check runs before the k3s installer.

The selected cluster ID, endpoints and CA fingerprint are pinned in the existing
cluster metadata. Restart refuses changed bindings rather than changing a running
cluster's datastore. There is no existing-cluster migration or datastore-edit
operation. A datastore referenced by any managed k3s cluster cannot be stopped,
disabled or removed through the integration, even while that cluster is stopped.
Explicitly removing the k3s cluster releases the binding but leaves Etcqlite running
and its data intact.

### Native recovery and reversible removal

Backups are **native recovery images**, not inspector projections or volume tar
archives. A running member must advertise `native-snapshot-v1` through
`/etcqlite/v1/discovery`. Porto pins the exact quoted identity ETag as `If-Match`
when posting `{}` to `/etcqlite/v1/snapshots`; missing/stale preconditions,
authorization, identity or readiness failures never fall back to projections.
If native etcd authentication has been enabled separately, launch the daemon with
`PORTO_ETCQLITE_TOKEN` containing its raw opaque authorization token, not a Bearer
prefix. Tokens and private keys are not returned in integration inventory.

Each private, no-overwrite image is checked by `etcqlite snapshot status`,
including native integrity/protected-record checks. Porto also checks expected
IDs, revision/schema agreement, byte count and the SHA-256 payload/trailer.
Native format 1 ends in a 32-byte SHA-256 trailer; status `sha256` excludes that
trailer and `bytes` includes it. Compaction revision is signed: `-1` means not
compacted; values below `-1` or above the current revision are rejected.
The supported image limit is 8 GiB per member and actions have a two-minute budget.
Only completed `verified.json` records appear as backups. Failed/unverified
private staging never replaces or advertises an earlier verified image.

Cluster deletion, upgrades and worker removal for selected k3s datastores require
verified native images before destructive VM changes. Completed k3s clusters also
require preserving their private server recovery token. Incomplete new bootstrap
may have no token yet; that limitation is logged and recorded as
`tokenPreserved: false` in its recovery metadata before fresh VM cleanup.
These images do not back up application volumes, and there is no automatic k3s
disaster restore or live cutover. Keep the native images, credentials, k3s token
and recovery metadata privately together.

For an unbound instance:

```sh
porto etcqlite stop local-db
porto etcqlite delete local-db --confirm
# Use the original decimal cluster ID from inventory or verified backup metadata:
porto etcqlite restore local-db --cluster-id ORIGINAL_ID --confirm
porto etcqlite start local-db
porto etcqlite stop local-db
porto etcqlite disable
```

Removal first verifies native images, then stops only ownership-verified members
and moves the original directory to
`PORTO_HOME/etcqlite/retained/<opaque-name-and-ID-token>`.
It never recursively purges data or backup history. It opens the **existing**
private `data/node.lock` without creating/truncating/replacing it, holds the same
nonblocking exclusive `flock` through retention/restoration and refuses a current
writer. Restoration moves the same retained identity/data back without overwrite
or startup. Stop the restored instance before disabling; disabling does not stop
running members. Native recovery-image import/rebootstrap remains an explicit
Etcqlite operation into isolated node-local directories, not this reversible
directory restoration.

### Local API

The routes use Porto's existing loopback-only control API and strict JSON decoder;
they are not granted to remote environments or extensions by this integration.

| Method and path | Request / result |
| --- | --- |
| `GET /api/integrations/etcqlite` | Disabled empty inventory, or explicit instance/member state. |
| `POST /api/integrations/etcqlite/enable` or `/disable` | Persist gate only; disabling refuses active or bound instances. |
| `POST /api/integrations/etcqlite/instances` | `{name, members: 1|3, advertiseHost, listenHost}`; returns initialized, stopped instance. |
| `POST /api/integrations/etcqlite/instances/{name}/start` or `/stop` | `{clusterID: "decimal ID"}`; identity-bound lifecycle. |
| `POST /api/integrations/etcqlite/instances/{name}/backup` | `{clusterID}`; verified native recovery artifacts. |
| `GET /api/integrations/etcqlite/backups` | Completed verified backup records only. |
| `GET /api/integrations/etcqlite/instances/{name}/logs?stream=stderr&tail=100` | Bounded stdout/stderr tails, maximum 1,000 lines / last 1 MiB per file. |
| `DELETE /api/integrations/etcqlite/instances/{name}` | `{clusterID, confirm: true}`; verified backup and reversible retention. |
| `POST /api/integrations/etcqlite/instances/{name}/restore` | Original `{clusterID, confirm: true}`; no overwrite, startup or identity rewrite. |
| `POST /api/kubernetes/clusters` | Optional `etcqlite: {name, clusterID?}` on a **new k3s** request; server pins the verified identity/fingerprint. |

Errors retain the existing `{message}` shape: invalid requests `400`, missing
owned records `404`, stale identities/bindings/occupied listeners `409`,
disabled/unavailable integration `503` and unsupported native platform `501`.
Native verification or storage failures are explicit, not successful empty results.

## KillSwitch

On macOS, enable [KillSwitch](https://github.com/mbianchidev/kill-switch) to share the active ports owned by processes managed by the current Porto daemon. KillSwitch keeps those source-owned ports separate from its own configured ports.

Installation is always explicit. Use **Install KillSwitch** in the dashboard or run:

```sh
porto kill-switch install
```

Porto can then sync active ports automatically and delegate cleanup to KillSwitch. Cleanup follows KillSwitch's auto-kill, age, runtime, indicator, and exclusion settings.

See the [KillSwitch integration guide](kill-switch.md) for requirements, commands, port ownership, and troubleshooting.

## Sendbox

Install [Sendbox](https://github.com/mbianchidev/sendbox) on a compatible macOS 26 Apple Silicon machine, then initialize each project that should expose the integration:

```sh
sendbox init --project /path/to/project
```

This creates `.sendbox.yaml`. Enable **Sendbox** in Porto's dashboard, then use **Run in Sendbox** and **Stop Sendbox**, or:

```sh
porto sendbox start <project>
porto sendbox stop <project>
```

Porto runs `sendbox run --config <project>/.sendbox.yaml --project <project>` and captures its output in the project's existing logs under the `sendbox` and `sendbox-stderr` streams. Porto does not install, require, or run Sendbox when no managed project contains `.sendbox.yaml`.

Sendbox sessions are independent from normal Porto processes. They do not receive Porto's automatic port assignment and are not routed through Porto's local hostnames. Avoid running both modes simultaneously when they would bind the same host port.
