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
porto docker volume remove restored-fixture --confirm
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

Image content is loaded and its config digest checked. Volume data uses the
source's read-only container archive API or an already accessible local
mountpoint. For an unattached Docker Desktop volume, select **Allow temporary
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

Each selected object's identity and destination are checkpointed as **pending**
before transfer, **running** before mutation, and **succeeded**, **failed** or
**cancelled** afterward. A daemon restart preserves recorded successes and marks
the active object's unrecorded outcome **interrupted**; it may already exist, so
its completion is never guessed. A failed checkpoint stops further transfers.
Source metadata is rechecked against the confirmed dry run immediately before
each object is handled.

Use **Review migration** in the operation history to restore exact selections
and destination names, then run a new dry run. Sensitive/helper consent is
cleared and must be granted again. Missing, unsupported or recreated source
identities are shown explicitly and are not silently replaced by matching names.
The CLI can repeat the original `migrate --context ... --objects ...` command;
it also obtains a new preview before `--confirm`. Completed networks/containers
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
| Linux / local containerd | Owned Linux bind mount | Linux mount permission, kernel 5.12+ recursive safe attributes; stopped snapshots do not start workloads |
| macOS / Porto Lima | Foreground SSHFS over Lima SSH | macFUSE and a maintained SSHFS build proving `contain_symlinks`; sudo SFTP access inside the Porto guest |
| Linux / Porto Lima | Foreground SSHFS over Lima SSH | FUSE3 and containment-capable SSHFS |
| Windows x64/ARM64 / Porto Lima | WinFsp host folder with direct confined SFTP | Install WinFsp 2.1+ once; the Porto desktop bundles its native mount helper. Running/stopped containers, images and local volumes use the identity-checked guest helper. |
| Standalone managed VM | SSHFS of its guest home | Running, Porto-owned standalone VM; no implicit boot or access to unrelated VM resources |

On Windows, the standalone VM row uses the same WinFsp/SFTP integration for
its guest home. Porto installs a checksum-verified, version-scoped guest helper
only after native-access consent, without replacing the VM's other tooling.
WinFsp is the maintained OS filesystem driver; installation requires your
administrator approval. Porto never installs a kernel driver silently. The
capability report distinguishes **driver missing** from **guest disconnected**.
Once installed, **Open files** exposes a real private folder usable by Explorer,
VS Code and other ordinary host editors.

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
owned attachment records after an interrupted daemon. Windows readiness checks
the actual mounted volume's label/serial, not an empty directory; recovery
checks the bridge PID and creation time before stopping a process. Failed detach operations
remain visible; cleanup never removes source data. If a driver or backend is
unavailable, use the in-app inspector, verified archives, or VM copy instead.

SSHFS readiness and later opens verify an exact `porto-<attachment-id>` mount
source, not just a directory or a mount at the same path. Spaces and non-ASCII
paths are supported. A missing bridge, exited process or replaced mount is
reported disconnected; replaced mounts are never unmounted as Porto-owned.
Connecting records are persisted before launching the bridge, and failed
startup/backend cleanup retains a retryable recovery record. Completed backend
releases are checkpointed so a later host-path cleanup failure does not release
the backend again after restart. No recursive source deletion is used.
A legacy attachment without an ownership-marked mount
can require explicitly unmounting that exact host path with the OS tools before
retrying `porto docker files detach`; Porto does not guess ownership.

### Acceptance coverage and limits

Migration fixtures cover read-only source requests, exact image dependencies,
unsupported settings, port/network fidelity, duplicate destinations, metadata
changes, partial failures, cancellation and durable per-object restart recovery.
The shared verified-archive tests separately cover corrupted members, low space,
links/metadata and original-preserving restore failures; these are reused transfer
guarantees, not a second migration implementation.

Native lifecycle fixtures cover owned/replaced mounts, escaped paths, failed
startup/cleanup and recovery. Confined direct-SFTP fixtures exercise long and
non-ASCII paths, permissions, links, concurrent changes, connection loss and
read-only access. The Windows installed-helper CI fixture exercises real WinFsp
host folders for synthetic container/image/volume/VM roots.

These fixtures do not establish every real resource/backend/platform combination.
Docker Desktop source-to-Porto round trips and native macOS/Linux mount acceptance
still require isolated real runtimes and the matrix's driver prerequisites.
Neither an unavailable driver nor an archive or in-app inspection fallback counts
as a successful native mount.
