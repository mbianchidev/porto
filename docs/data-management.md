# Inspecting, protecting and moving runtime data

Porto's container inspector has Overview, Logs, Files, Stats, Inspect, Terminal,
and Layers views. Compose project/service ownership stays visible when opening
an inspector. Closing a view cancels its requests and log subscription.

## Container files, logs and metrics

Files are inspected by the bundled Linux runtime helper rather than by a shell
inside the workload. Running containers use their current mount namespace;
stopped containers use their existing snapshot without starting a task. Image
files are immutable. Mounted volume/bind boundaries and read-only mounts are
shown separately.

Paths must remain inside the selected resource. Absolute/escaping symlinks,
device files and unsupported special entries cannot be traversed or edited.
Listings are bounded; enter a narrower directory when a listing is truncated.
Text previews and edits are limited to 256 KiB, individual uploads/downloads to
64 MiB. Upload creates a new file. An edit or deletion requires the preview's
checksum; a concurrent change is rejected. Atomic replacement can break a
file's hardlink relationship, so use an archive when preserving links matters.

New Porto-created containers retain timestamped stdout/stderr journals alongside
their existing raw logs. Older containers retain combined, untimed history;
Porto does not invent timestamps or rewrite it. The log viewer supports search,
stream filtering and copying, retains at most 5,000 records / 4 MiB, and reports
connection loss explicitly.

Stats retain 180 recent samples per inspected container, with a bounded
128-container daemon cache. Missing CPU, memory, network, process or block-I/O
counters are unavailable, not zero. Inspect masks sensitive environment/label
values until an explicit reveal; raw data is the data already accessible through
the local Docker socket.

## Storage and scoped cleanup

**Containers → Storage and cleanup** shows references, Compose ownership,
packed image sizes, shared layers, writable snapshots, volume allocation, and
BuildKit cache records. Select individual unused objects before previewing.
No broad cleanup is selected by default.

Content blobs and snapshots are counted once; snapshot usage excludes parents.
Per-image logical sizes overlap and cannot be summed. Shared BuildKit records
and containerd's cross-namespace metadata database are not presented as a
fabricated grand total. Cleanup upper bounds and unavailable reclaimed-byte
totals are labelled explicitly.

Referenced volumes/images/networks, managed Kubernetes node containers, native
attachments and active builds are protected at the backend. A changed identity
invalidates a preview. Cleanup outcomes remain in Activity, including partial
failures. Existing opt-in weekly image/cache cleanup remains separate from
explicit category/object cleanup.

```sh
porto docker storage usage
porto docker storage prune --category image          # dry run
porto docker storage prune --category image --confirm
porto docker operations
porto docker operations cancel 12
```

Docker clients can use `/system/df` and category prune APIs. Porto adds
accounting/warning fields, and returns an unavailable reclaimed-byte value
rather than inventing zero or summing shared layers.

## Volume archives and backups

Open a volume's **Files** or **Data and backups** tab. Clone/import creates a new
volume. Restore and Empty require an exact preview and explicit confirmation;
active writers must be stopped first. Restore validates the archive in staging
and atomically publishes it only after validation and a final ownership check.
Cancellation, corrupt archives or insufficient space before publish preserve
the original directory.

Archives are tar files containing `data/` members and a final
`porto-manifest.json` (format version 1). The manifest records resource identity,
capture time, logical byte count, permissions, UID/GID, relative contained
symlinks, hardlinks, per-file SHA-256 and manifest integrity. Runtime exports use
logical containerd-namespace ownership and translate rootless UID/GID ranges
when restoring. Unsupported identity ranges, absolute links, devices, sockets,
FIFOs and special permission bits fail explicitly rather than being dropped.

The daemon reads/writes archives only inside Porto's managed `transfers/` and
`backups/` roots, using filesystem-confined handles that reject symlink escapes.
Dashboard imports upload an archive, and exports are downloaded after
verification. The CLI reads a user-selected local import file and uploads it;
for export it downloads the verified result into the user-selected local path
without overwriting an existing file. HTTP requests cannot select arbitrary
host files or destination directories. Backup folder choices are relative to
Porto's `backups/` directory.

**Copies and backups are crash-consistent, not application-consistent database
backups.** Porto does not stop or quiesce writers implicitly. Quiesce your
application or stop its containers yourself before exporting when required.

```sh
porto docker volume export fixture ./fixture.tar     # preview
porto docker volume export fixture ./fixture.tar --confirm
porto docker volume clone fixture fixture-copy --confirm
porto docker volume import ./fixture.tar restored-fixture --confirm
porto docker volume restore restored-fixture ./fixture.tar --confirm
porto docker volume empty restored-fixture --confirm
porto docker backups schedule fixture --hours 24 --retain 7 --enabled
porto docker backups run 1
```

Local backup schedules, deadlines, progress and outcomes survive a daemon
restart in SQLite. Missed deadlines run once and move forward from the current
time, without overlap or replay storms. Retention removes only identity- and
checksum-proven older archives after a replacement is verified; it never removes
the last verified recovery point before replacement. Pausing/removing a schedule
does not delete its archives. Failures and interrupted jobs remain visible.

## Migration

**Migrate runtime data** discovers Docker Desktop and other local Docker
contexts without switching the active context. Select images, named volumes,
custom networks and compatible stopped containers, then review the dry run.
Source sockets, original resources and registry credentials are not changed.

```sh
porto docker migrate contexts
porto docker migrate --context desktop-linux
porto docker migrate --context desktop-linux \
  --objects image:fixture:latest,volume:fixture-data,container:fixture \
  --confirm
```

Container dependencies must be selected too. Unsupported host paths,
namespace/device/DNS/capability/runtime options are rejected before creating
that container. Sensitive environment transfer requires separate explicit
local consent; registry credentials and source context authentication material
are never imported. Selected resources are transferred through local Unix
sockets or Windows named pipes, not unauthenticated remote endpoints.

Image content is loaded and its config digest checked. Volume data uses the
source's read-only container archive API or an already accessible local
mountpoint. An unreferenced Docker Desktop volume without such access requires
you to attach it to an existing source container or export a local archive
yourself; Porto does not create a source helper or start a workload implicitly.
Completed destination objects remain on partial failure, with per-object
results and deterministic conflicts. A persisted volume identity ledger permits
resuming a verified transfer without silently duplicating a recreated name.

## Native file-manager access

**Open files in native file manager** is opt-in. Images are always read-only;
container, volume and standalone VM writes require an explicit write selection.
These are real filesystem paths, not copies with implicit copy-back.

```sh
porto docker files volume fixture                    # capability report
porto docker files volume fixture --confirm
porto docker files container fixture --writable --confirm
porto docker files image fixture:latest --confirm
porto docker files vm fixture-machine --confirm
porto docker files                                  # attachment inventory
porto docker files detach <attachment-id>
```

| Host / backend | Native integration | Requirements / limitations |
| --- | --- | --- |
| Linux / local containerd | Owned Linux bind mount | Linux mount permission, kernel 5.12+ recursive safe attributes; stopped snapshots do not start workloads |
| macOS / Porto Lima | Foreground SSHFS over Lima SSH | macFUSE and a maintained SSHFS build proving `contain_symlinks`; sudo SFTP access inside the Porto guest |
| Linux / Porto Lima | Foreground SSHFS over Lima SSH | FUSE3 and containment-capable SSHFS |
| Windows / Porto Lima | Explicitly unavailable | Current folder bridges cannot prove the required containment/readiness contract; use Files, archives or `porto vm copy` |
| Standalone managed VM | SSHFS of its guest home | Running, Porto-owned standalone VM; no implicit boot or access to unrelated VM resources |

Rootless local kernels may forbid binding a running container's locked mount
subtree. In-app Files reads its live `/proc` root safely without a bind; native
local access reports that limitation and requires an explicit stop before using
the snapshot. Lima bridges can use the live root over contained SSHFS without
starting another workload. Stopped/image snapshot mounts enter the actual
containerd user/mount namespaces with maintained `nsenter`, never guess host
snapshotter paths.

Host locations include the complete resource fingerprint and read/write mode
under Porto's private `files/` directory (local Linux bind mounts use the
helper-owned `/run/porto-native/` location). Reused names never reuse an old
identity. Native permission checks retain backend boundaries; UID/GID spaces and
case sensitivity follow the backend/driver, not a silent ownership rewrite.
Contained symlinks are enforced, escaping links rejected. Remote filesystem
notifications are not guaranteed; refresh/poll for concurrent changes.

Detach before destructive storage changes. The daemon monitors resource
identity/connection state, detaches on shutdown/update, and recovers its exact
owned attachment records after an interrupted daemon. Failed detach operations
remain visible; cleanup never removes source data. If a driver or backend is
unavailable, use the in-app inspector, verified archives, or VM copy instead.
