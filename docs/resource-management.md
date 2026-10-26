# Resource management and architecture policies

Resource Saver is opt-in and disabled by default. Its host-side HTTP listeners
remain available while eligible applications are asleep. An initiating request
waits for the owned task and its readiness checks before the original method,
headers and body are forwarded.

## Resource Saver modes

| Mode | What changes | What remains |
| --- | --- | --- |
| `disabled` | No new automatic suspension | Existing owned sleepers retain their wake listeners |
| `engine-idle-suspend` | Stops only a verified, wholly idle Porto-owned Lima engine | VM disk, configuration, and host listeners |
| `container-pause` | Freezes eligible tasks, reducing their process CPU | Resident process memory and disk data |
| `container-scale-to-zero` | Stops eligible tasks, releasing task memory | Container metadata, writable layers, volumes and host listeners; process memory is lost |

Configure global policy in Settings or with:

```sh
porto resource-saver configure --mode container-scale-to-zero --idle 300 --wake 60
porto resource-saver container example-api --mode stop
porto resource-saver status
```

The container inspector also exposes pause, stop and exclusion choices. Mounted
or stateful workloads require separate explicit consent (`--stateful` in the
container command). Opting in does not make unsupported workloads eligible.

Equivalent container/Compose labels are `dev.porto.resource-saver=pause` or
`stop`, `dev.porto.resource-saver.stateful=true` for mounted data, and an optional
`dev.porto.resource-saver.idle-seconds`. The existing `dev.porto.http-port` and
`dev.porto.hostname` labels provide the owned HTTP route. Every Compose member
must opt in, and non-HTTP dependencies need health checks. The application is
resumed as one group.

Settings and `configure --exclude 'database-*,important-api'` support name
exclusions. Per-application overrides use IDs such as `compose:example` or
`container:<full-ID>` and JSON fields `mode` and `idleSeconds`:

```sh
porto resource-saver configure --overrides '{"compose:example":{"idleSeconds":900}}'
```

### Safety and request boundaries

Only HTTP/HTTPS requests through Porto's URL wake listeners are retained.
Published ports used directly, raw TCP/UDP and additional published protocols
do not provide first-connection wake. The bounded queue defaults to 64 requests,
16 MiB per body, and a 60-second wake/readiness deadline. Client cancellation
releases its reservation; concurrent requests share one resume. Active proxied
streams and WebSocket upgrades hold the application active until disconnect.

Interactive containers, running execs, unobservable multi-process applications,
established connections, unsupported networks, unmanaged dependencies, missing
metrics, incomplete inventory and ambiguous routes block automatic suspension.
Idle detection uses observed CPU, network and disk activity, not a VM's reserved
memory. Long-lived internal connections also conservatively count as activity.

Engine suspension additionally checks every containerd namespace and active
BuildKit work. Running or manually paused tasks, Kubernetes nodes, native file
attachments, data operations and incomplete observations prevent it. External
containerd installations and native Windows containers are not automatically
suspended.

Ownership is identity-bound and journaled before transitions, then verified
against backend metadata before resume. A manual lifecycle action releases only
that selected member from automatic ownership. Pause ownership is quarantined
after daemon downtime because an external manual unpause/re-pause cannot be
proven from a saved PID alone; resume it manually before opting in again.
Stopped-task ownership is checked again on wake. Failed wakes return a gateway
error with diagnostics, never a successful empty response. Backend pause,
suspension, resume and failure events appear in Activity.

## Engine budgets

CPU, memory, swap and disk settings apply only to Porto's owned Lima engine.
Use Settings or:

```sh
porto docker engine-resources configure --cpus 2 --memory 4096 --swap 0 --disk 100
porto docker engine-resources apply --restart-engine
porto docker engine-resources status
```

CPU, memory and disk zeroes preserve existing settings or the provider's defaults
for a new engine. Omitted swap leaves it unchanged; swap zero disables only
Porto-owned swap. Managed swap uses an owned Linux swap file and refuses
unmanaged swap or unsupported filesystems. Other guest provisions are preserved.

Existing engines require an explicit idle restart. Disk capacity may only grow;
shrinking or changing an unsupported driver is rejected before configuration
mutation. Failed edits restore the original configuration and preserve the
existing VM disk. Disk growth is not an undoable shrink operation.

## Apple Silicon and amd64

Architecture policy is separate from Resource Saver:

```sh
porto runtime acceleration status
porto runtime acceleration configure --policy rosetta-preferred --rosetta
porto runtime acceleration apply --restart-engine
```

`automatic` uses verified backend capabilities. `native-only` rejects foreign
platforms, including BuildKit solve constraints. `rosetta-preferred` uses a
configured Rosetta handler, with a verified QEMU fallback when unavailable.
`qemu-compatible` requires the QEMU path. Container inspector creation options,
the `dev.porto.architecture-policy` Compose/container label, and the VM
`--architecture-policy` option override global policy for that workload.

Porto probes Apple's Virtualization.framework directly, Lima's installed
drivers and guest agents, matching QEMU executables, and guest binfmt handlers.
Container handlers must be enabled, executable and carry the fix-binary flag.
Capabilities are refreshed rather than retained across provider upgrades.
Native arm64 workloads remain native; creating one does not install emulation
software. Existing owned-engine QEMU setup remains scoped to that guest.

Rosetta is opt-in, requires Apple Silicon, an arm64 VZ guest and an already
installed Apple runtime. If needed, explicitly run
`softwareupdate --install-rosetta` and accept Apple's license yourself. Porto
does not silently install it or register host-wide emulation handlers.
QEMU-backed engines cannot be converted to VZ or have their disks replaced.
After a VZ engine has a Rosetta filesystem device, disabling translation keeps
that existing device configuration and disables only its binfmt handler.
Selecting QEMU does not remove a guest device or change the VM's architecture.

For a native arm64 standalone VM with optional amd64 userspace:

```sh
porto vm create example --image ubuntu-24.04 --vm-type vz --arch aarch64 --rosetta
```

An amd64 **kernel** on Apple Silicon requires QEMU full-system software
emulation, not Rosetta or hardware-accelerated amd64 virtualization. The matching
QEMU executable and additional Lima Linux guest agent must be available.
Apple Silicon desktop bundles include the extra guest agent; source installs
may need `lima-additional-guestagents`. Snapshots retain their QEMU-only boundary.
The dashboard and runtime status distinguish native, Rosetta, QEMU and
unsupported/missing-capability states without claiming unmeasured speedups.

## Optimized synchronized project shares

macOS and Linux can opt selected, registered project roots into a maintained
Mutagen two-way-safe session. Windows/WSL retains ordinary bind mounts and the
existing selected-distribution integration; synchronized mode is capability
gated rather than substituting different path or ownership semantics.

```sh
porto shares create example-project --ignore node_modules,dist
porto shares status
porto shares mode <share-id> bind
porto shares mode <share-id> synchronized
porto shares detach <share-id> --confirm
```

Settings exposes the same project selector, startup/scan/staging state,
connectivity, conflicts and filesystem problems. Only the selected root is
synchronized into its identity-bound `/mnt/porto-sync/<share-id>` directory.
Whole home directories, filesystem roots, overlapping roots and symlink roots
are rejected. No existing host directory or guest copy is deleted on detach.

New container bind mounts inside a selected synchronized root use that guest
copy after a successful flush and conflict check. Existing containers are not
retargeted; recreate them after a binding-mode change. The
`dev.porto.share-mode=bind` label opts a container back into ordinary bindings.
Paused, disconnected, conflicting or replaced-engine sessions fail before a
new workload is created rather than falling back to an empty or stale copy.

Both endpoints can change files. Atomic renames, staging and notifications use
the maintained tool's implementation. Ignore patterns and VCS exclusions stay
local. Portable permissions synchronize executable bits but not host UID/GID,
ACLs or every platform-specific mode. Relative contained symlinks are supported;
unsafe links and case collisions are reported. Conflicts retain both versions;
inspect the host and guest copies and resolve them explicitly. Neither side
automatically wins a destructive conflict. A stopped engine disconnects its
session; reconnect uses the same ownership record and pinned guest host key,
not a replacement guest. Active synchronized sessions prevent automatic engine
suspension until their synchronization is paused.

SSH configuration, verified host keys and the Mutagen daemon are private to
Porto. Global SSH configuration and unrelated Mutagen sessions are not changed.
Multiplexing, persistence and agent forwarding are disabled for these private
connections.

Desktop runtimes include a source-built `0.18.1-porto.1` CLI and Linux amd64/arm64
agents, compiled with the current Go toolchain and pinned patched dependencies.
Upstream 0.18.1 prebuilt executables are not used: their older Go and network
dependencies have known vulnerabilities. Source development can prepare the
same scoped toolchain without installing it host-wide:

```sh
bash scripts/bundle-synchronization-runtime.sh darwin arm64 stage/synchronization-runtime
export PATH="$PWD/stage/synchronization-runtime/bin:$PATH"
```

Use the host's actual OS/architecture; macOS builds require macOS for native
filesystem notifications. Ordinary binding mode does not require this tool.

## Reproducing native measurements

On an Apple Silicon Mac with installed Rosetta and the documented development
toolchain, first build the runtime inputs:

```sh
bash scripts/bundle-desktop-runtime.sh darwin arm64 stage/runtime-darwin-arm64
```

The following command explicitly authorizes a disposable VZ acceptance VM.
It creates private `PORTO_HOME` and `LIMA_HOME` directories, uses two CPUs,
2 GiB RAM and a 20 GiB sparse disk, disables host-folder mounts, and removes
only that VM afterward. Existing Porto engines are not selected:

```sh
PATH="$PWD/stage/runtime-darwin-arm64/bin:$PWD/stage/runtime-darwin-arm64/lima/bin:$PATH" \
  PORTO_TEST_NATIVE_RUNTIME_POLICIES=1 \
  go test ./internal/daemon -run '^TestNativeAppleSiliconRuntimePolicies$' \
    -count=1 -timeout=25m -v
```

The workload is a synthetic amd64 Go HTTP server built into a scratch image,
with a no-cache BuildKit `RUN` step that executes the original Linux binary.
It echoes the SHA-256 of an 819,200-byte initiating binary POST, exercises
12 concurrent wakes, a real WebSocket, manual pause, complete engine shutdown,
Rosetta/QEMU policy changes, and a native arm64 build. Sharing uses 10,000
small synthetic files, an actual guest container bind, bidirectional edits and
inotify. The separate maintained-tool case covers ignores and retained conflicts:

```sh
PORTO_TEST_SYNC_BINARY="$PWD/stage/runtime-darwin-arm64/bin/mutagen" \
  go test ./internal/syncshares \
    -run '^TestMaintainedSynchronizationPreservesLargeSyntheticRepository$' \
    -count=1 -timeout=5m -v
```

An observed run on an Apple M5 Max, macOS 26.6, Go 1.27.3, Lima 2.2.1,
an arm64 VZ guest and `0.18.1-porto.1` synchronization toolchain recorded:

| Observation | Measured result |
| --- | --- |
| Fresh engine provisioning, possibly using Lima's cloud-image cache | 38.75 s |
| No-cache synthetic BuildKit build | 0.671 s |
| Measured host process subtree RSS before / immediately after / five seconds after build | 138,264,576 / 138,264,576 / 146,685,952 bytes |
| Running synthetic task memory before scale-to-zero | 38,563,840 bytes; task absent after stopping |
| Original binary POST after task scale-to-zero | 1.099 s; exact payload checksum preserved |
| Host process subtree RSS before complete engine suspension | 216,121,344 bytes; owned process absent afterward |
| Native VZ host-process CPU during a five-second idle sample | 0.00 CPU seconds at `ps` resolution; process absent after suspension |
| Original POST through complete engine and task wake/readiness | 10.699 s |
| Initial 10,000-file host-to-guest synchronization | 2.632 s |

These are reproducible observations, not latency guarantees or cross-product
benchmarks. RSS covers the measured owned process subtree, not every macOS
Virtualization.framework service or all guest RAM. CPU uses differences between
the native VZ process's cumulative `ps time` samples, not the VM's configured CPU
reservation. This already-quiet fixture did not show a measurable idle CPU
decrease, and its small build did not demonstrate automatic post-build RSS
reclamation. Complete engine suspension removed the measured process instead.
The daemon, host listeners and OS caches remain. Tiny-file synchronization
measures metadata-heavy initialization, not sequential byte throughput.
Battery savings and improvements over ordinary binds have not been measured
and are not claimed.
