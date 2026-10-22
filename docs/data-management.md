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
Text previews and edits are limited to 256 KiB of UTF-8 text, individual
uploads/downloads to 64 MiB. **Download** in each file row works for binary files
and files too large for a text preview. Downloads, uploads, and previews expose
**Cancel file operation** and stop when the view closes; connection failures
remain visible and retryable. Upload creates a new file and never implicitly
overwrites one. Editing/deletion requires a bounded text preview. Selecting
another file or directory asks before discarding an unsaved edit. An edit or
deletion requires the preview's checksum; a concurrent change is rejected.
Atomic replacement can break a
file's hardlink relationship, so use an archive when preserving links matters.

New Porto-created containers retain timestamped stdout/stderr journals alongside
their existing raw logs. Older containers retain combined, untimed history;
Porto does not invent timestamps or rewrite it. The log viewer supports search,
stream filtering and copying, retains at most 5,000 records / 4 MiB, and reports
connection loss explicitly. **Clear buffer** discards the visible history
without reconnecting or replaying older logs. Pause closes the subscription;
Resume/Reconnect reload the retained tail, so output outside that tail is not
guaranteed to be recovered.

Stats retain 180 recent samples per inspected container, with a bounded
128-container daemon cache. **History metric** selects CPU, memory, process
count, network RX/TX, or block read/write history; **Recent samples** shows
the recorded times and values. Network and block-I/O counters are cumulative
bytes, not transfer rates. Missing counters are unavailable, not zero, and
produce gaps in the history rather than fabricated continuity. A stopped task
or lost connection leaves retained samples visible with its limitation.
Inspect masks sensitive environment/label
values until an explicit reveal; raw data is the data already accessible through
the local Docker socket. Missing structured metadata is explicitly marked as
not supplied by the backend.

## Storage and scoped cleanup

**Containers → Storage** shows references, Compose ownership,
packed image sizes, shared layers, writable snapshots, volume allocation, and
BuildKit cache records, their descriptions, usage counts and last-use times.
Search resource names, owning containers or Compose applications and filter by
resource type, in-use, unused or dangling state. Select individual unused objects
or choose one explicit cleanup category. No broad cleanup is selected by default.

Content blobs and snapshots are counted once; snapshot usage excludes parents.
Per-image logical sizes are locally stored packed blobs, overlap and cannot be
summed. Partially pulled multi-platform images retain their missing-content
warning and are protected from cleanup. Non-shared BuildKit records are counted
separately from shared records. Containerd's metadata plugin supplies the
authoritative database location; the Linux helper measures it in the backend's
filesystem namespace. Metadata covers **all** containerd namespaces, not just
Porto, and excludes content/snapshot stores. Missing measurements are unavailable,
not zero. These overlapping scopes are not presented as a fabricated grand
total. Cleanup upper bounds and unavailable reclaimed-byte totals are explicit.

Referenced volumes/images/networks, managed Kubernetes node containers, native
attachments and active builds are protected at the backend. A changed identity
invalidates a preview. A category preview freezes the exact candidate identities;
new unused objects never join a confirmed run. Ownership, protections and active
builds are checked again before each removal. BuildKit must report the selected
record actually removed before Porto records success. Cleanup outcomes remain
in Activity, including partial failures. Existing opt-in weekly image/cache
cleanup remains separate from explicit category/object cleanup.

```sh
porto docker storage usage
porto docker storage prune --category image          # dry run
porto docker storage prune --category image --confirm
porto docker storage prune --objects volume:fixture-data,cache:cache-record-id
porto docker operations
porto docker operations watch 12
porto docker operations cancel 12
```

Docker clients can use `/system/df` and category prune APIs. Porto adds
accounting/warning fields, and returns an unavailable reclaimed-byte value
rather than inventing zero or summing shared layers.

## Volume archives and backups

Open a volume's **Files** or **Data and backups** tab. Overview and transfer
previews show owning containers, Compose project/service, mount destinations,
read-only/writable relationships and managed-cluster protections. **Import local
archive** works even before any volume exists. Clone/import creates a new
volume. Restore, Empty and Remove require an exact preview and explicit
confirmation; active mounts must be stopped before replacement and all
container references detached before removal. Force is not a bypass.
Stopped managed-cluster data stays protected. Restore validates the archive in staging
and atomically publishes it only after validation and a final ownership check.
Cancellation, corrupt archives or insufficient space before publish preserve
the original directory, and pins the previewed whole-archive checksum in the Linux
helper before extraction. Temporary destination ownership is reserved in SQLite
before creation. After interruption, recovery deletes only exact
label-proven unfinished volumes; foreign or unverifiable objects are retained
with an explicit failure instead of guessed cleanup.
Owned cleanup reuses its fresh ownership inspection for guarded removal within
the same data operation, rather than repeating a remote inspection. Native mounts
and current container references still block removal. Failed cleanup retains its
reservation for recovery.

Archives are tar files containing `data/` members and a final
`porto-manifest.json` (format version 1). The manifest records resource identity,
capture time, logical byte count, permissions, UID/GID, relative contained
symlinks, hardlinks, per-file SHA-256 and manifest integrity. Runtime exports use
logical containerd-namespace ownership and translate rootless UID/GID ranges
when restoring. Unsupported identity ranges, absolute links, devices, sockets,
FIFOs and special permission bits fail explicitly rather than being dropped.
Regular-file/directory permissions, supported ownership and link timestamps are
preserved. Symbolic-link mode bits follow the destination filesystem (Linux
links are always `0777`); they are never applied by following the link's target.
Native volume identity comes from the guest's stable `volume.json` inode and
birth time, not Docker-only creation fields absent from nerdctl. Replacing
`_data` does not retarget a backup schedule, while recreating a volume does.

The daemon reads/writes archives only inside Porto's managed `transfers/` and
`backups/` roots, using filesystem-confined handles that reject symlink escapes.
Dashboard imports upload an archive, and exports are downloaded after
verification. The CLI reads a user-selected local import file and uploads it;
for export it downloads the verified result into the user-selected local path
without overwriting an existing file, checking available space and the
operation's archive checksum before publication. An existing local destination
is rejected before starting an export. HTTP requests cannot select arbitrary
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
porto docker data volume remove restored-fixture --confirm
porto docker backups schedule fixture --hours 24 --retain 7 --enabled
porto docker backups update 1 --hours 12 --retain 3
porto docker backups pause 1
porto docker backups resume 1
porto docker backups run 1
porto docker backups remove 1 --confirm
```

Local backup schedules, deadlines, progress and outcomes survive a daemon
restart in SQLite. Missed deadlines run once and move forward from the current
time, without overlap or replay storms. Retention removes only identity- and
checksum-proven older archives after a replacement is verified; it never removes
the last verified recovery point before replacement. The verified replacement
is durably recorded before retention begins; interrupted backups with such a
record remain downloadable recovery points. The dashboard provides **Edit
backup policy**, **Pause/Resume**, **Back up now**, and **Remove backup schedule**.
Pausing/removing a schedule does not delete its archives, and a failed schedule
can be paused while its source is unavailable. Failures and interrupted jobs
remain visible in operation history and Activity. **Cancel** stops the job;
closing a view or stopping the CLI watcher does not silently cancel it.

Filesystem transfer and metadata measurement require the bundled Linux helper:
local Linux containerd, or Porto's Linux Lima backend on macOS/Windows. External
volume drivers/options, unsupported ownership ranges and unavailable native
filesystem capabilities fail explicitly. Archives have bounded 64 GiB logical
data / 100,000-entry limits. Native host file access is optional, not required
for in-app browsing, archives or backups.

## Migration

**Migrate runtime data** discovers Docker Desktop and other local Docker
contexts without switching the active context. Select images, named volumes,
custom networks and compatible stopped containers, then review the dry run.
Source sockets, original resources and registry credentials are not changed.
Unattached volumes can be read through explicitly consented temporary helpers;
their exact source names and consequences appear in the dry run.

```sh
porto docker migrate contexts
porto docker migrate --context desktop-linux
porto docker migrate --context desktop-linux \
  --objects image:fixture:latest,volume:fixture-data,container:fixture \
  --confirm
porto docker migrate --context desktop-linux \
  --objects volume:unattached-fixture --allow-source-helper --confirm
porto docker migrate --context desktop-linux --objects volume:fixture-data \
  --destinations volume:fixture-data=restored-data --confirm
porto docker migrate --resume 42 --allow-source-helper --confirm
```

Container dependencies must be selected too. Unsupported host paths,
namespace/device/DNS/capability/runtime options are rejected before creating
that container. CPU scheduling/cpuset, PID/ulimit, additional host/group, custom
logging, static endpoint and unlimited-swap settings that cannot be preserved
are also rejected rather than dropped. Exposed ports, IPv6 host bindings and
network aliases retain their inspected values. Container recreation requires a
selected named reference for its exact original image digest; it never pulls a
replacement for a missing or retagged image. Sensitive environment transfer
requires separate explicit local consent; registry credentials and source context authentication material
are never imported. Selected resources are transferred through local Unix
sockets or Windows named pipes, not unauthenticated remote endpoints.
Unallocated host ports and rootless internal-network/published-port combinations
are reported before creation because their source semantics cannot be retained.
Original sources are never reconfigured to work around those limitations.
Supported named-volume mounts are recreated with `nocopy`, preserving empty
migrated volumes rather than populating them from the destination image.

Named image aliases are preserved and missing aliases are restored on retry.
Untagged images require an explicit source tag, rather than importing under a
generic name that could replace unrelated content. Legacy Docker config IDs and
newer OCI manifest/index IDs are resolved to checksum-verified configuration
bytes before load; destination config content is verified afterward.
Native containerd migration compares configuration digests pinned by the
original source OCI manifest, not an inspector's potentially rewritten unpacked
layer presentation. Changed execution settings or rootfs diff IDs still conflict.
Volume data always uses
the selected source's read-only container archive API. A Unix socket can forward
to another machine or VM; a source-reported mountpoint is never treated as host
data merely because a path with that name exists locally.
For an unattached volume, select **Allow temporary
read-only source access** (CLI: `--allow-source-helper`). Porto loads a uniquely
labelled empty scratch image and creates a stopped helper container. Its volume
mount is **read-only with NoCopy**, its network disabled, and it is never started.
There is no registry pull, executable workload, volume population, or source
workload stop. The archive is read through Docker's stopped-container API.
Only identity/label-proven helper containers/images are removed afterward;
original resources and data remain intact. Helper reservations persist before
source mutations, so a daemon interruption can recover exact owned resources.
An offline or altered source retains its cleanup reservation and reports the
failure instead of guessing ownership. Without that explicit consent, the dry
run rejects volumes needing a helper; local archive import remains available.
Completed destination objects remain on partial failure, with per-object
results and deterministic conflicts. A persisted volume identity ledger permits
resuming a verified transfer without silently duplicating a recreated name.
Source volume labels are retained. A verified destination identity is recorded
before the temporary publication reservation is cleared, so an interrupted
publication cannot silently become an unrelated existing volume.

Each selected object's identity and destination are checkpointed as **pending**
before transfer, **running** before mutation, and **succeeded**, **failed** or
**cancelled** afterward. A daemon restart preserves recorded successes and marks
the active object's unrecorded outcome **interrupted**; it may already exist, so
its completion is never guessed. A failed checkpoint stops further transfers.
Source metadata is rechecked against the confirmed dry run immediately before
each object is handled. JSON member order, image-alias order and Docker's
unordered observed mount list are canonicalized without discarding source
configuration or exact numeric values.

Use **Review migration** in the operation history to restore exact selections
and destination names, then run a new dry run. Sensitive/helper consent is
cleared and must be granted again. Missing, unsupported or recreated source
identities are shown explicitly and are not silently replaced by matching names.
The CLI uses `--resume <operation-id>` to retain the original identities and
destinations, rather than resolving saved names against a new inventory.
Sensitive/helper consent must be selected again; `--resume` obtains a new
preview before `--confirm`. Use `--destinations kind:source=destination,...`
with a new explicit selection to resolve supported naming conflicts.
Completed networks/containers
must match both their source identity and saved configuration plan. Container
retries retain the original selection set and dependency destinations. Older
source-only labels, changed configuration/dependencies, and incomplete or
altered container resource limits produce deterministic conflicts, not implicit
replacement or a false successful skip. Inspect such destinations before
explicitly removing or changing them yourself.

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

Capability reports are scoped to the requested resource and write mode. They
check current identity, ownership, stopped/running availability and read-only
boundaries without allocating an attachment. The dashboard pins attachment
requests and write consent to that identity; replacing a resource clears write
consent and makes its old attachment unavailable. A supported capability is not
itself evidence that a mount succeeded: the real host mount must become ready.

| Host / backend | Native integration | Requirements / limitations |
| --- | --- | --- |
| Linux / local containerd | Owned Linux bind mount; confined SSHFS for live container roots | Linux mount permission and kernel 5.12+ recursive safe attributes for binds; FUSE3 and SSHFS 3.7.6+ or equivalent containment fixes for running containers. No workload is started implicitly. |
| macOS / Porto Lima | Built-in macOS WebDAV filesystem over confined SFTP | No third-party filesystem driver. Private loopback authentication; images are read-only. Unix link creation and host chmod/chown are unavailable. |
| Linux / Porto Lima | Foreground SSHFS over confined SFTP/stdio | FUSE3 and SSHFS 3.7.6+ or equivalent `contain_symlinks` fixes; the scoped guest helper enforces the same root, identity and read-only boundaries as other native drivers. |
| Windows x64/ARM64 / Porto Lima | WinFsp host folder with direct confined SFTP | Install WinFsp 2.1+ once; the Porto desktop bundles its native mount helper. Running/stopped containers, images and local volumes use the identity-checked guest helper. |
| Standalone managed VM | Host-native bridge to its guest home | Running, Porto-owned standalone VM; macOS uses built-in WebDAV, Windows WinFsp, and Linux SSHFS. No implicit boot or access to unrelated VM resources. |

Standalone VM bridges use the scoped SFTP integration for their guest home.
Porto installs a checksum-verified, version-scoped guest helper
only after native-access consent, without replacing the VM's other tooling.
WinFsp is the maintained OS filesystem driver; installation requires your
administrator approval. Porto never installs a kernel driver silently. The
capability report distinguishes **driver missing** from **guest disconnected**.
Once installed, **Open files** exposes a real private folder usable by Explorer,
VS Code and other ordinary host editors.

macOS uses its built-in WebDAV filesystem and the same confined, direct SFTP
backend as Windows. No macFUSE installation or kernel-extension approval is
required. A private per-attachment password is passed to the OS through an
unlinked credential descriptor, never a URL, command argument, keychain entry or
diagnostic log. The server binds only to loopback. The native mount uses a
private host directory, not a synchronized copy; closing a successful save
commits it to the workload. macOS presents synthetic Unix ownership/mode bits;
existing guest permissions remain unchanged. WebDAV cannot represent Unix links
or chmod/chown, so links are not traversed through that native view and those
operations require guest tooling or a verified archive. File names retain backend
case semantics; refresh/reopen for concurrent changes and do not rely on guest
filesystem notifications.

Windows mounts use direct SFTP random-access I/O rather than a cached sync
folder: a successful write is persisted in the workload filesystem before the
host receives success. There is no delayed upload or automatic copy-back at
detach. Images are read-only both at WinFsp and the guest handler. All paths,
link targets and mutations are confined by `os.Root`; symlinks cannot expose
unrelated host/guest locations. Read-only bind mounts are enforced independently
of the GUI. Guest files retain their modes and logical UID/GID; the host ACL
grants access only to the mounting user. Host ACL ownership changes are
explicitly unsupported rather than translated from Windows SIDs into guest
UID/GIDs; use guest tooling or archive metadata for Unix ownership changes.
Case-sensitive guest names remain
case-sensitive. Filesystem notifications from arbitrary guest processes are
not guaranteed, so refresh or poll for external changes.

Rootless local kernels may forbid binding a running container's locked mount
subtree. In-app Files and native SFTP read its live `/proc` root without such a
bind; Linux native access uses SSHFS for that resource. Writes enter the actual
runtime user/mount namespaces so rootless ownership and overlay copy-up remain
correct. Local stopped/image bind mounts reject symlink traversal at the kernel
boundary. Confined SFTP permits contained relative links but rejects escapes;
SSHFS additionally rejects any link target containing `..`, including otherwise
contained parent-directory links. macOS WebDAV cannot represent Unix links and
does not traverse them.
Stopped/image snapshot mounts enter the actual
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
owned attachment records after an interrupted daemon. Windows readiness checks
the actual mounted volume's label/serial, not an empty directory; recovery
checks the bridge PID and creation time before stopping a process. Failed detach operations
remain visible; cleanup never removes source data. If a driver or backend is
unavailable, use the in-app inspector, verified archives, or VM copy instead.

SSHFS readiness and later opens verify an exact `porto-<attachment-id>` mount
source, not just a directory or a mount at the same path. Spaces and non-ASCII
paths are supported. A missing bridge, exited process or replaced mount is
reported disconnected; replaced mounts are never unmounted as Porto-owned.
Local Linux binds also persist and recheck their kernel mount ID. An unmounted
directory is not reported connected, and a replacement filesystem is never
detached as the old attachment. Deleted or recreated source volumes do not
prevent releasing the exact old owned bind.
Connecting records are persisted before launching the bridge, and failed
startup/backend cleanup retains a retryable recovery record. Completed backend
releases are checkpointed so a later host-path cleanup failure does not release
the backend again after restart. No recursive source deletion is used.
A legacy attachment without an ownership-marked mount
can require explicitly unmounting that exact host path with the OS tools before
retrying `porto docker files detach`; Porto does not guess ownership.

macOS verifies the exact owned loopback WebDAV mount source, including canonical
temporary-directory aliases. Shutdown unmounts while its server is still alive.
After a daemon/connection loss, recovery detaches only that identity-proven dead
mount; in-flight edits that did not successfully close are not claimed committed.
If the OS removes a dead bridge while an unmount is in progress, Porto verifies
the mount table again before completing cleanup. A still-mounted or replaced
filesystem retains a visible, retryable detach failure.
Stopped-container helpers recursively detach their own nested mounts inside
their private namespace. Managed VM file creation retains the guest SSH user's
ownership rather than attempting a privileged root chown.

### Real-runtime acceptance

`Runtime data` CI runs the real resource/backend matrix on Linux x64/ARM64
(local containerd and Lima), macOS Intel/Apple Silicon (Lima and built-in WebDAV),
and Windows x64/ARM64 (Lima and installed WinFsp). Each cell has independent
migration and native jobs that run in parallel with separate owned fixtures;
both phase checks must pass. This preserves the existing time caps and all
assertions instead of extending the deadline for a combined emulated run.
Each job creates a separate Porto-owned standalone source VM running an actual
Docker Engine, exposing its
API through the host's private Unix socket or named pipe and a real Docker
context. This exercises the Docker Desktop-style local socket/API contract,
not a mocked inventory or a claim about Docker Desktop's proprietary UI.

The suite migrates selected images, volumes, networks and stopped containers;
checks workload command/environment, health/restart/resource limits, labels,
mount relationships and published service access; and repeats the migration
without duplication. It exercises deterministic naming conflicts, unsupported
settings, sensitive consent, corrupt archives, cancellation, an actual 8 MiB
staging filesystem, killed-worker checkpoint recovery and ownership-proven
cleanup of never-started source helpers.

Native acceptance opens actual container/image/volume/VM host mounts, not
copies. It covers immutable images, read-only submounts, long/non-ASCII and
case-sensitive names, links, concurrent backend changes, guest permission
failures, live/stopped containers, protected deletion, recreated identities,
VM stop/reconnect and cleanup. Engine update/stop/restart is restricted to the
independently owned disposable CI engine; local runs never stop an existing
engine. Separate native-driver fixtures exercise an actually killed macOS
owner and a failed detach followed by owned recovery.

Unsupported semantics in the capability matrix remain explicit limitations,
not successful mounts. Driver installation on a user's machine is never
performed silently. In-app inspection and verified archives remain actionable
alternatives but never count as passing a native mount.
