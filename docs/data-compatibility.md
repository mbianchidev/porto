# Runtime data API and persisted-state compatibility

This reference specifies the shipped data-operation, local volume-backup and
migration-recovery contracts behind the [roadmap's compatibility work](https://github.com/mbianchidev/porto/issues/75).
It complements [runtime data management](data-management.md), which explains
the user workflows. Native attachment records, remote environments, extensions
and platform-specific runtime stores are separate contracts.

The source definitions are [data-operation types](../internal/dataops/types.go),
[filesystem types](../internal/datafiles/types.go),
[HTTP handlers](../internal/daemon/data_operations.go) and
[SQLite persistence](../internal/store/data_operations.go).
These describe existing behavior, not a proposed schema or an assigned future
release version.

## Compatibility boundaries

| Surface | Current contract | Compatibility consequence |
| --- | --- | --- |
| Local HTTP API | Unversioned `/api/data/` and `/api/docker/backups` routes; camelCase JSON fields | Keep the daemon, CLI and dashboard from the same release. Request decoders reject unknown fields, so even an optional request-field addition is not automatically compatible with an older daemon. |
| SQLite data tables | Additive, transactional `CREATE TABLE/INDEX IF NOT EXISTS` initialization; no data-schema version column or migration-version ledger | Reopening an existing compatible database does not rewrite terminal operation JSON. There is no general down migration or guarantee that arbitrary older binaries can operate on newer state. |
| Volume archives | Explicit manifest `version: 1` | Readers reject other versions, unknown manifest fields and unsupported ownership modes. Do not silently add manifest fields or change checksum serialization while claiming format-1 compatibility. |
| Resource identity | SHA-256 of the compact, ordered `Resource` JSON representation | JSON field names, order, omission rules and values affect previews, schedule keys and recovery ownership. A resource name is not an identity. |
| Operation recovery | Persisted outcomes plus fresh runtime ownership/integrity checks | An interrupted operation is not automatically replayed or rolled back. Recorded metadata is not proof that an unrecorded runtime mutation did not happen. |
| Optional Etcqlite | Additive `etcqlite_enabled` setting (default false), separate format-1 native instance metadata and verified recovery manifests | Existing clusters retain their datastore defaults. New k3s bindings pin decimal-string cluster IDs and endpoint/CA fingerprints. Native images use their own SQLite/SHA-256-trailer contract, not volume archive 1. See [Etcqlite recovery](integrations.md#native-recovery-and-reversible-removal). |

New changes must identify which of these boundaries they cross before assigning
a release version. A compatible change needs reader/writer fixtures for both
representations; an incompatible change needs an explicit conversion, failure
and recovery design. Do not infer downgrade support from a successful reopen
with the current executable.

## Resource and archive JSON

### Resource identity

`Resource` has these fields in this serialization order:

| Field | JSON type | Serialization |
| --- | --- | --- |
| `kind` | string | Always emitted; scheduled backups require `volume`. |
| `name` | string | Always emitted; the display/runtime name, not a generation identifier. |
| `id` | string | Always emitted; the backend's resource-generation identity. |
| `createdAt` | string | Omitted when empty; preserve the supplied representation. |
| `backend` | string | Omitted when empty; preserve the supplied backend identity. |
| `readOnly` | boolean | Always emitted, including `false`. |

For example, the bytes hashed for a resource with no optional identity fields
are exactly:

```json
{"kind":"volume","name":"synthetic-volume","id":"synthetic-generation","readOnly":false}
```

The hash is lowercase hexadecimal, with no trailing newline in its input.
Do not rebuild it by sorting a map or dropping `readOnly: false`.
The persisted resource key is this fingerprint when `resource.id` is present.
Without that ID, `prune` and `migration` use `runtime-data`; other actions use
`action:destination`. The daemon also serializes data jobs across resources.

Native volume generations use the guest `volume.json` identity rather than
the `_data` directory. Replacing volume contents does not retarget a schedule;
deleting and recreating the named volume invalidates its old identity.

### Archive format 1

An archive is a tar stream of `data/` members followed by exactly one regular
`porto-manifest.json` member. No tar member may follow that manifest.
The [archive reader/writer](../internal/datafiles/archive.go) defines:

| Manifest field | JSON type | Contract |
| --- | --- | --- |
| `version` | integer | Exactly `1`. |
| `resource` | Resource object | Nonempty `id`; volume import/download additionally requires `kind: "volume"`. |
| `createdAt` | timestamp string | Go `time.Time` JSON, written in UTC with RFC 3339 fractional seconds when needed. |
| `consistency` | string | Exactly `crash-consistent`; not an application-consistency claim. |
| `ownership` | string, optional | `filesystem`, `namespace`, or omitted/empty for legacy archives. Runtime exports use logical namespace ownership and translate supported rootless UID/GID mappings on restore. |
| `bytes` | integer | Sum of regular-file content bytes, not total tar size. |
| `entries` | array | Ordered manifest entries matching the tar members; an empty archive uses `[]`. |
| `sha256` | string | SHA-256 of the compact Go JSON encoding of this manifest with `sha256` set to `""`; all other fields and their omission/order rules remain unchanged. |

Each entry has `path`, `type`, `size`, `mode`, `uid`, `gid` and `modifiedAt`.
`linkTarget` and `sha256` are omitted when empty. Types are `file`, `directory`,
`symlink` and `hardlink`. Paths are relative to `data/`; hardlinks reference a
preceding regular file, and symlinks must stay contained. Mode is an integer
permission mask from `0` through `511` (`0777`), not an octal JSON string.
File SHA-256 values cover content bytes.

The reader checks member/manifest agreement, checksums, ordering, duplicate
paths and link containment. Devices, sockets, FIFOs and special permission
bits are unsupported. Limits are 64 GiB of logical content, 100,000 entries
and 32 MiB for the manifest; paths are limited to 4,096 bytes. The daemon
additionally bounds the whole archive to 64 GiB plus 32 MiB.

The operation's archive object is different from the manifest:
`{path, sha256, manifest, bytes, resource, consistency}`.
Here `sha256` covers the **whole tar file**, `manifest` is the manifest's
checksum, and `bytes` is the tar file size. All six fields are emitted.
Reformatting JSON or repacking a tar can therefore change an integrity value
even when file contents appear equivalent.

## Local HTTP contract

The default base is `http://127.0.0.1:37623`. This is the local control API,
not a remotely exposed backup service: it has no bearer-token authentication.
The daemon requires a loopback request host and, when supplied, a loopback
HTTP/HTTPS `Origin`. Non-local environment IDs require their explicit
authenticated environment path; they cannot fall back to these local routes.
See [remote environments](remote-environments.md) for that separate boundary.

JSON request bodies use `Content-Type: application/json`, contain one JSON
value, and are limited to 2 MiB. Unknown fields, including nested fields, fail
decoding. Missing fields initially take Go zero values; the action-specific
validation below still applies. These routes have no pagination cursor or
declared rate-limit protocol.

### Operations and archive transport

| Method and path | Request | Success response |
| --- | --- | --- |
| `POST /api/docker/storage/preview` | Data `Request`, with `action` selecting volume transfer, cleanup or migration | `200`, action-specific preview. A volume preview contains `request`, `resource`, `owners`, `destination`, `token`, `consistency`, `consequences`, and optional `archive`/`files`. |
| `POST /api/data/operations` | Confirmed `Request` from the reviewed preview | `202`, `Operation`. Acceptance starts asynchronous work; it does not mean the operation succeeded. |
| `GET /api/data/operations` | None | `200`, newest 100 operations by descending ID; `[]` when empty. |
| `GET /api/data/operations/{id}` | Positive integer ID | `200`, `Operation`. |
| `DELETE /api/data/operations/{id}` | Positive integer ID | `200`, `{"status":"cancellation-requested"}` for an active worker. Poll history for the final outcome; this does not delete history. |
| `PUT /api/data/archives` | Raw tar body, not JSON or multipart | `201`, verified archive object with a managed upload path. Invalid/incomplete uploads are removed. |
| `GET /api/data/operations/{id}/archive` | Positive integer ID | `200`, `application/x-tar`, after manifest and whole-file checksum revalidation. A failed/interrupted job can still have a verified recovery archive. |

`Request` fields follow the [source type](../internal/dataops/types.go):

| Field | JSON type | Meaning and validation |
| --- | --- | --- |
| `action` | string | `volume-export`, `volume-clone`, `volume-import`, `volume-restore`, `volume-empty`, `volume-remove`, `prune` or `migration`. |
| `resource` | Resource object | Target resource; previews resolve and pin its current identity where applicable. |
| `identity` | string | Fingerprint returned by the preview, not a name-only lookup. |
| `destination` | string, optional | New volume name for clone/import, or a confined managed export path. HTTP callers cannot choose arbitrary host files. |
| `archive` | string, optional | Managed uploaded archive path for import/restore; for `volume-empty`, the preview's content digest. |
| `directory` | string, optional | Reserved for internally constructed backup jobs; rejected when nonempty on manual operation creation. |
| `categories` | string array, optional | Explicit cleanup categories. |
| `selections` | Selection array, optional | Exact cleanup/migration selections, not a wildcard over future inventory. Each has string `kind`, `name`, `id`, and optional `destination`. |
| `context` | string, optional | Explicit local source Docker context for migration. |
| `includeSensitive` | boolean, optional | Separate consent for sensitive migration configuration; defaults to `false`. |
| `allowSourceHelper` | boolean, optional | Separate consent for stopped read-only source helpers; defaults to `false`. |
| `confirm` | boolean | Must be `true` when creating a manual operation. |
| `preview` | string, optional | Must be nonempty on creation and match the freshly validated preview during execution. Stale previews fail rather than selecting new resources. |
| `scheduleId`, `retention` | integers, optional | Internal backup metadata; rejected when nonzero on manual operation creation. |
| `trigger` | string, optional | Manual creation accepts absent/empty or `manual`; the worker defaults it to `manual`. Internal backup triggers are `scheduled` and `manual-backup`. |

The serializer always emits `action`, `resource`, `identity` and `confirm`;
other request fields have `omitempty`. Do not send internal scheduler fields
by copying a scheduled job's request into the manual-create endpoint.

An `Operation` always emits integer `id` and `bytes`, object `request` and
`result`, and string `status`, `phase` and `startedAt`. `completedAt` and
`error` are omitted when empty. Timestamp writers use UTC RFC 3339 with
nanosecond precision when needed. `bytes` reports operation progress, not
necessarily archive size or reclaimed space. `phase` is progress text, not a
closed enum.

Status is `running`, or terminal `succeeded`, `failed`, `cancelled`,
`interrupted` or `skipped`. A result has optional `archive`, `steps`,
`bytesReclaimed` and `message`. Missing `bytesReclaimed` means unavailable;
an explicit `0` means measured zero. Each step always emits string `kind`,
`source`, `destination` and `status`, and optionally `message` and `id`.
Keep per-step outcomes on partial failure rather than interpreting the
top-level failure as a rollback.

For example, read history without triggering a mutation:

```sh
curl --fail-with-body --silent --show-error \
  http://127.0.0.1:37623/api/data/operations
```

### Backup schedules

| Method and path | Request | Success response |
| --- | --- | --- |
| `GET /api/docker/backups` | None | `200`, schedules in ascending ID order; `[]` when empty. |
| `POST /api/docker/backups` | Complete `Schedule` object, for creation or update | `200`, saved `Schedule`, including its ID and recomputed deadline. |
| `DELETE /api/docker/backups/{id}?confirm=true` | Positive integer ID and explicit confirmation | `204`, no body. Existing archives and operation history remain. |
| `POST /api/docker/backups/{id}/run` | Positive integer ID; no body required | `202`, `Operation` with trigger `manual-backup`. A paused schedule may be run manually; its regular deadline is unchanged. |

All schedule response fields are emitted:

| Field | JSON type | Contract |
| --- | --- | --- |
| `id` | integer | `0` for creation; positive existing ID for update; negative is invalid. One schedule exists per resource fingerprint. |
| `resource` | Resource object | Identity-bearing, writable local volume. Create/resume resolves the current descriptor; an existing identity cannot be retargeted to a recreated volume. |
| `enabled` | boolean | Explicit policy, not inferred from whether Docker is currently available. An existing schedule can be paused while its source is unavailable. |
| `intervalHours` | integer | Inclusive range `1..8760`. |
| `retention` | integer | Inclusive range `1..365` verified recovery archives. |
| `directory` | string | Empty selects `backups/<first-16-fingerprint-characters>` beneath Porto's state root. Relative choices stay under `backups/`; absolute saved paths must also remain in that managed root. Responses/persistence contain an absolute path. |
| `nextRunAt` | timestamp string | Server-owned; every save recalculates it as current UTC time plus the interval. Supplying an old deadline does not preserve it. |

Automatic claiming requires Docker enabled, an enabled due schedule and no
running data job. Claiming the operation and moving its deadline happen in
one SQLite transaction. Missed intervals run once, then advance from the
current time; they are not replayed as a backlog.

### Errors and client handling

Runtime errors normally return JSON `{"message":"..."}`. Malformed JSON,
request-origin guards and some explicit not-found responses instead use
plain-text HTTP errors. There is no common machine-readable error-code enum.
Preserve the HTTP status and body; do not assume every error is JSON.

| Status | Current uses |
| --- | --- |
| `400` | Invalid/oversized JSON, unknown fields, invalid IDs/limits, missing confirmation, reserved scheduler fields, or refused input. |
| `403` | Non-loopback request host or supplied origin. |
| `404` | Explicit missing schedule responses and unavailable/expired archive download. |
| `409` | Changed resource/preview, competing runtime operation, runtime disabled by a route guard, or cancellation without an active worker. |
| `501` | Explicit unsupported filesystem capability. |
| `503` | Unavailable backend/worker. Backup schedule creation/resume also uses this status when Docker is disabled, unlike the `409` route guard. |
| `500` | Other persistence/execution failures. A direct missing operation-row lookup currently reaches this generic path, not a guaranteed `404`. |

Once a job has been accepted, execution errors are recorded in its operation
rather than retroactively changing the original `202` response. Canceling a
watcher or closing a view does not cancel the job.

## SQLite layout and upgrade behavior

The database is `porto.db` beneath `PORTO_HOME`, or Porto's OS-specific user
configuration directory when that override is absent. Transfers and backups
live beneath the same state root in `transfers/` and `backups/`. Their absolute
paths and resource identities are persisted; moving files alone is not a
state migration.

The data initializer creates the following schema in one transaction. This
is a reference, **not an instruction to run SQL against an installation**:

```sql
CREATE TABLE data_operations (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 resource_key TEXT NOT NULL,
 request TEXT NOT NULL,
 status TEXT NOT NULL,
 phase TEXT NOT NULL DEFAULT 'queued',
 bytes INTEGER NOT NULL DEFAULT 0,
 started_at TEXT NOT NULL,
 completed_at TEXT NOT NULL DEFAULT '',
 result TEXT NOT NULL DEFAULT '{}',
 error TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX idx_data_operation_active
 ON data_operations(resource_key) WHERE status='running';
CREATE INDEX idx_data_operation_history ON data_operations(started_at);
CREATE TABLE volume_backup_schedules (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 resource_key TEXT NOT NULL UNIQUE,
 resource TEXT NOT NULL,
 enabled INTEGER NOT NULL,
 interval_hours INTEGER NOT NULL CHECK(interval_hours BETWEEN 1 AND 8760),
 retention INTEGER NOT NULL CHECK(retention BETWEEN 1 AND 365),
 directory TEXT NOT NULL,
 next_run_at TEXT NOT NULL
);
CREATE INDEX idx_volume_backup_due
 ON volume_backup_schedules(enabled,next_run_at);
CREATE TABLE migration_volume_ledger (
 source_context TEXT NOT NULL,
 source_identity TEXT NOT NULL,
 destination TEXT NOT NULL,
 destination_identity TEXT NOT NULL,
 verified_at TEXT NOT NULL,
 PRIMARY KEY(source_context,source_identity,destination)
);
CREATE TABLE migration_source_temporaries (
 owner TEXT NOT NULL,
 kind TEXT NOT NULL,
 resource TEXT NOT NULL,
 reserved_at TEXT NOT NULL,
 PRIMARY KEY(owner,kind)
);
CREATE TABLE volume_transfer_temporaries (
 owner TEXT PRIMARY KEY,
 destination TEXT NOT NULL UNIQUE
);
```

The implementation adds `IF NOT EXISTS` to these statements. An error rolls
back this **data-table transaction**, including newly created indexes/tables;
it does not roll back every other initializer in `Store.Open`. Existing
incompatible tables are not repaired by `IF NOT EXISTS`. Preserve them and
diagnose the failure instead of deleting the database to force startup.

`request`, `result` and schedule `resource` are JSON text using the types above.
SQL `enabled` is written as `0` or `1`; readers treat exactly `1` as enabled.
Operation request and checkpoint writers each cap their JSON at 1 MiB.
Persisted JSON decoding ignores unknown fields and supplies Go zero values for
omitted fields, unlike the strict HTTP/manifest decoders. Schedule readers
subsequently validate the required volume identity, interval, retention and
absolute directory. This is **not** a lossless future-schema round trip:
typed rewrites, including running-job recovery, can discard unknown fields.
Malformed or wrongly typed JSON returns a decode error instead of an empty
successful result.

The migration volume ledger proves the exact source-context/source-identity/
destination mapping and its verified destination identity. It is not a
name-only permission to overwrite an existing destination.
Source-helper reservations contain JSON string fields `context`, `endpoint`,
`kind`, `name`, `owner` and optional `id`; kinds are `container` or `image`.
Destination transfer reservations contain `owner` and `destination`, with a
32-byte owner token required by the store.
Reservations precede runtime mutation, and failed ownership-proven cleanup
retains them. Do not remove these rows to hide a cleanup failure.

## Recovery and user-visible upgrade notes

The shipped data features retain these upgrade boundaries:

- Schedules and operation history survive restart. Startup marks running jobs
  `interrupted`, preserving verified archives and recorded successful steps.
  Running steps become `interrupted`; pending steps are not promoted to success.
  Corrupt operation JSON makes recovery fail without partially rewriting history.
- A backup is durably recorded and revalidated before retention deletes older
  identity/checksum-proven archives in the same directory. A retention failure
  does not make the verified replacement disappear. Expiration clears the old
  archive path and records phase `archive-expired`, retaining its history.
- Pausing or removing a schedule does not remove archives. A recreated volume
  needs an explicitly reviewed schedule for its new identity.
- Migration retry requires a fresh dry run with the saved source identities and
  destinations. Regrant sensitive/helper consent. Completed destination objects
  remain after partial failure; source resources are not removed as rollback.
- Restore validates in staging and checks ownership/integrity again before
  atomic publication. Failure before publication preserves the original
  destination. Restoring a volume is an explicit data operation, not a side
  effect of reinstalling or downgrading Porto.

Before an upgrade that changes one of these contracts, inspect operation
history, finish/cancel active data jobs, resolve retained helper reservations
through normal ownership-checked recovery, and preserve verified archives.
Quiesce application writers yourself when application-consistent recovery is
required.

For a control-plane recovery copy, plan downtime and stop the daemon before
copying its SQLite database and associated archive directories. Stopping the
daemon can stop managed native projects; closing only the desktop window does
not stop the daemon. The database uses WAL: **never copy a live `porto.db`
alone** and assume it contains all committed state. Preserve a consistent
database/sidecar set, permissions, archive paths and the original state-root
location. This copy does not back up guest disks or replace application data
backups.

There is no supported arbitrary in-place downgrade or automatic down migration.
Before using an earlier release, review its actual readers/runtime compatibility
and retain an untouched copy of the newer state. Restore a verified coherent
recovery set only while stopped and only for a compatible target; do not
drop new tables, rewrite fingerprints, change archive versions or clear
reservations by hand. Restoring old SQLite metadata does not rewind containers,
volumes, source runtimes or already-published data. Refresh inventory and review
new previews after recovery. If target compatibility is unknown, prefer a
forward fix over letting the old executable rewrite newer state.

## Regression evidence and remaining limits

Run the focused synthetic-fixture suite with:

```sh
go test ./internal/datafiles ./internal/dataops ./internal/store ./internal/daemon
```

| Boundary | Executable coverage |
| --- | --- |
| Fingerprint serialization and archive-1 readers | `TestResourceFingerprintCompatibility`, `TestVersionOneManifestCompatibility` in [datafiles tests](../internal/datafiles/). Legacy omitted ownership remains readable; unsupported versions/fields/ownership are rejected. |
| Additive initialization and partial-schema rollback | `TestDataOperationMigrationIsAdditiveAndIdempotent`, `TestDataOperationMigrationRollsBackPartialSchemaOnFailure` in [store tests](../internal/store/data_operations_test.go). |
| Legacy operation JSON and corrupt-record recovery | `TestDataOperationReadsLegacyJSONWithoutRewritingIt`, `TestDataRecoveryRejectsCorruptJSONWithoutChangingHistory` in the same store suite. |
| HTTP response shape and strict request decoding | `TestDataOperationHistoryHTTPContract`, `TestDataOperationRejectsIncompatibleJSON` in [daemon tests](../internal/daemon/data_operations_test.go). |
| Missed deadlines, verified recovery points and exact temporary ownership | Existing restart/checkpoint tests in the store suite and [source-helper reservation tests](../internal/store/migration_helpers_test.go). |
| Retention and failed restore safety | Existing [daemon retention tests](../internal/daemon/data_operations_test.go) and [archive restore tests](../internal/datafiles/archive_test.go). |

These are current-reader and synthetic recovery checks, not execution evidence
for every historical Porto binary, a complete downgrade matrix, real backend
acceptance, or native filesystem-driver qualification. Record those separately
when a future persisted-state change needs them. This slice does not assign
milestones, new feature versions, or complete the roadmap-wide compatibility
policy for other subsystems.
