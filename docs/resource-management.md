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
