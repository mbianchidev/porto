# Continuous integration and releases

Porto's automation lives in `.github/workflows`. Every workflow declares the least privilege it needs and checks out the repository without persisted Git credentials.

## Workflows

| Workflow | File | Trigger | Purpose |
| --- | --- | --- | --- |
| CI | `ci.yml` | pull requests, pushes to `main`, manual | Formatting, vet, tests, cross-compilation, dashboard lint and build |
| CodeQL | `codeql.yml` | pull requests, pushes to `main`, weekly | Static analysis for `go` and `javascript-typescript` |
| Security audit | `security.yml` | pushes to `main`, weekly, manual | `govulncheck` and `npm audit` |
| Emulated runtime acceptance | `emulated-runtime.yml` | manual | Full software-emulated Lima platform/phase matrix |
| Release | `release.yml` | tags matching `v*`, manual | Builds, packages, and publishes archives plus native macOS and Windows installers |

CodeQL retains both complete language analyses on every existing event, including
draft pull requests; there are no changed-file or language-selection exclusions.
The security audit runs `govulncheck@latest` and audits the complete dashboard
lockfile with `npm audit --package-lock-only --audit-level=high`. The audit-only
job does not install packages, run dependency lifecycle scripts, or transfer an
unused npm download cache. Locked `npm ci` installations, builds, and tests remain
part of CI and release validation; registry errors and high-severity findings
still fail the security audit.

The maintained official `golang.org/x/crypto` module is required for SSH.
GO-2026-5932 covers its retired OpenPGP packages in every module version and has
no upstream fixed version; Porto imports none of those packages. The crypto
dependency policy checks production and test dependency graphs for all six
release targets and rejects any future deprecated OpenPGP import. It does not
suppress vulnerability findings, replace official cryptography with a fork, or
claim that the upstream module-only advisory has disappeared.

## CI details

The `go` job runs on `ubuntu-latest`, `macos-latest`, and `windows-latest` with the current stable toolchain, plus one extra `ubuntu-latest` job on `1.27.3` to guard the exact minimum version declared in `go.mod`. Linux runs `gofmt -l`, `go mod tidy -diff`, and `go test ./... -race`; the other platforms run the plain test suite.

The native platform-networking job runs on Ubuntu, Windows x64 and Windows
ARM64. Disposable hosts install the real loopback HTTPS integration, check
occupied IPv4/IPv6 port rollback, import two synthetic CAs, and verify a dotted
portless route in a real browser without certificate-error bypass flags. It
checks native trust, certificate renewal, least-privilege helper identity,
crash recovery, reinstall and fingerprint-scoped removal. Ubuntu includes
Chromium's real NSS database; Windows uses the current user's native store.
The Windows fixture answers the real protected-root confirmation only in its
own CLI child process and only when the dialog displays the complete synthetic
CA fingerprint. This consent helper exists only in the disposable CI test; it
does not change Windows trust policy or automate a developer's desktop.
ConPTY resize, subprocess exit and disconnect/reaping contracts run natively
on both Windows architectures. The privileged Linux fixture is a synthetic TLS
server, not a privileged Porto daemon or workload.

Native Windows-container acceptance has a separate Windows Server 2025 AMD64
row. It builds only the owned native payload and current Porto CLI, using an
immutable Nano Server image reference. Its native fixture covers lifecycle,
networking, bind writes, logs/exec, host/image/isolation rejection and
ownership-safe mode/data preservation. Docker create must leave the task Created,
without a PID or fixture side effects; start is tested separately. Native image
configuration IDs remain distinct from manifest targets across listing,
inspection, tagging and creation. Unsupported hosts/architectures have
unit and capability gates, not an x64 binary fallback.

The native fixture also exercises the actual dashboard WebSocket against SDK
execs, including exact nonzero exit, resize, process/exec-metadata reaping and an
unrelated interactive exec that must survive. The Windows development-client
row enables `PORTO_TEST_WINDOWS_COEXISTENCE=1` against its already running owned
Linux engine, preserving live PID/start identity and exact binary volume data
through native install, changed-binary update, repair and context changes.

Networking acceptance uses loopback-only synthetic TLS registries and real
HTTP, HTTPS and SOCKS proxies on every existing development-client backend.
The matrix includes rootful local Linux and KVM-backed Linux Lima, plus Lima
on macOS and Windows. The Linux Lima row fails if native KVM is unavailable;
software emulation is not substituted for that required acceptance.
The full Linux Lima fixture runs in a fresh `kvm`-group process, as the runtime
data fixtures do, so a later engine wake does not depend on the initial device
ACL remaining unchanged. KVM access diagnostics are retained on boot failure.
It verifies custom CA trust and its removal, proxy transitions and local
bypass; rootful local Linux additionally requests a real container private IP.
Unit transport gates run on Linux, macOS and Windows and exercise changing
resolver answers without modifying host DNS. Expired/near-expiry leaf fixtures
verify recovery without replacing the trusted CA.

Image-transfer tests use a real HTTP/1 server and flush backend progress before
reading either a content-length or chunked binary upload. This catches a
request-body draining race; in-memory response recorders alone cannot exercise
that HTTP server behavior.

The reported Lima save/load flake also came from its older nerdctl 2.3.5:
`save` could exit before its asynchronous export writer finished, returning an
empty or truncated archive. The preceding passing run used the same version.
Lima 2.2.1 now supplies checksum-pinned nerdctl 2.4.1, including the
[upstream save fix](https://github.com/containerd/nerdctl/commit/80d3ab8a4dac11f4c87f0ea09d07492807c458a8).
Saved archives are checked through the end before Porto serves them, so a
failed export cannot masquerade as a successful backup.

Development-client acceptance also runs Resource Saver against the same
disposable backends: original binary POST preservation, shared concurrent wakes,
real WebSocket activity, manual-pause exclusion, and owned-engine shutdown and
traffic-triggered readiness. POSIX Lima rows additionally synchronize a synthetic
10,000-file project through the owned SSH endpoint, verify strict host-key
pinning with an unrelated synthetic key, mount the verified guest copy in an
actual container, and check bidirectional writes and inotify notifications.
The maintained-tool test independently covers ignores, executable permissions,
relative symlinks, renames and retaining both sides of a conflict. These are
additional gates, not replacements for the existing clients or native mounts.

An opt-in native Apple Silicon fixture creates only its own 2 GiB VZ engine with
no host-folder mounts. It verifies Rosetta, QEMU-compatible policy transitions,
native-only rejections and native arm64 builds, as well as the resource and
sharing cases above. It requires installed Rosetta and must not be substituted
with a software-emulated result. Reproduction commands and measured boundaries
are in [resource management](resource-management.md).

Native HTTPS acceptance includes a real Porto daemon restart and live renewal,
not only a substitute TLS server. Only its helper installation is elevated;
the daemon runs under the original Unix user or a restricted Windows token.
The approved macOS workflow uses the existing launchd/login-keychain integration
and installed Chrome, with noninteractive sudo only for the existing root
install/uninstall entry points. Each trust fixture runs in a fresh process so
Go's cached system root pool cannot retain the preceding fixture's removed CA.
Native cases fail on unavailable trust/authorization instead of disabling TLS
checks or silently passing.

### Blocked hosted macOS trust verification

The hosted macOS attempt reached the existing `cert trust` login-keychain
operation but did not complete within its 120-second acceptance deadline.
[The recorded run](https://github.com/mbianchidev/porto/actions/runs/40232377758/job/121286650808)
does not establish native macOS HTTPS acceptance. Porto will not bypass user
trust authorization or replace login-keychain trust with machine-wide trust to
make hosted CI pass.

Full launchd/login-keychain/browser/restart tests remain executable in
`Approved macOS native HTTPS`, a manual workflow requiring a disposable
`[self-hosted, macOS, porto-native-trust]` runner and a logged-in operator
available to approve only its synthetic certificate. Select the revision being
reviewed and confirm `approve_user_trust`. No success is claimed until that
workflow actually passes. Ordinary macOS Go, desktop startup, filesystem,
installer and development-client gates remain automatic.
No self-hosted runner carrying the required approval label was registered when
this blocker was recorded, so the manual workflow has not been dispatched.

The native payload is built from checksum-pinned official source archives with
Go 1.27.3 or newer and patched crypto/network/sys/logging/gRPC dependencies.
This avoids newly shipping old upstream executable toolchains or the CNI
release's pinned vulnerable `x/sys` replacement. Each built entry point rejects
deprecated OpenPGP imports; the ordinary vulnerability audit remains enabled.
Download caches contain verified inputs, not trusted final native executables.

The Windows x64 development-client row also imports two fresh, named WSL 2
distributions from a checksum-pinned Ubuntu 24.04.5 rootfs. Only one opts in.
The existing independently owned QEMU engine serves real native Linux Docker,
Compose and Buildx clients through the private Windows pipe. Acceptance checks
quoted/non-ASCII paths, bidirectional bind writes, polling file changes, the
native Windows credential vault, distro shutdown/reconnect and removal without
changing the control distribution. No WSL driver replaces QEMU, and there is no
WSL 1 fallback or expected-failure path. The fixture releases owned shares
before unregistering only its own distributions.

The Windows Lima source patch also classifies only `ERROR_INVALID_FUNCTION`
from the WSL UNC provider as a temporarily unavailable mount, like Lima's
existing missing-directory case. A stopped/reconnecting distribution must not
make the running engine's configuration unreadable. Ordinary UNC paths,
access-denied errors and non-directory paths still fail validation; actual
mount/read failures remain visible. The full native shutdown/reconnect workflow
and a source-level error-classification test exercise this boundary.
Explicit requests from the selected running distribution wait for its UNC
directory provider to support real reads before reaching Docker. This bounded
readiness gate does not restart a stopped distribution, probe an unselected
one, or retry permission errors; read-only status never touches its filesystem.

Hosted Windows ARM64 does not provide the nested virtualization needed for WSL
2. That row verifies native HTTPS/ConPTY and architecture-matched Linux ELF
artifacts rather than claiming a real ARM64 WSL workflow. The complete platform
boundaries are in [Windows development integration](windows-integration.md).

The `cross-build` matrix compiles `./cmd/porto` with `CGO_ENABLED=0` in six
independent jobs, one for each release target (`linux`, `darwin`, and `windows`
on `amd64` and `arm64`). All targets remain mandatory; a broken platform build
fails the pull request instead of the release.

Generic Linux x64 jobs use `ubuntu-latest`, including native containerd and
KVM-backed Lima acceptance. ARM macOS jobs use `macos-latest`, including the
native-filesystem and optional emulated-runtime rows. These rolling aliases track
GitHub's stable images, which can lag the OS vendor's newest release during a
rollout. Acceptance explicitly rejects the wrong architecture on rolling-image
rows; a future alias change must not silently replace x64 Linux or ARM macOS
coverage.

Linux ARM64 remains `ubuntu-26.04-arm` and Intel macOS remains
`macos-26-intel`: GitHub provides neither `ubuntu-latest-arm` nor an Intel
`macos-latest` alias. These pins preserve architecture coverage, not a blanket
host-version requirement. Windows runner labels, guest/container images and
toolchain pins are unchanged. Linux still builds SSHFS against the host's FUSE3
libraries, checks the actual FUSE device and requires actual `/dev/kvm` for the
VM-backed PR gate; a rolling image is not permission to fall back to emulation.
The Ubuntu 26.04 **guest** requirement for rootless startup and the macOS 26/Xcode
26 minimum for Icon Composer packaging are separate from host-runner aliases.
See GitHub's [supported runner labels](https://docs.github.com/en/actions/reference/runners/github-hosted-runners).

CI shares Go module downloads separately from compiler objects. Build caches
are scoped by runner OS/architecture, resolved Go version, workload, target,
and dependency/runtime inputs, so a host-only cache cannot displace a
cross-build or native-packaging cache. Go still validates compiler cache
entries against current source and flags; test executions retain `-count=1`.
Cache paths are exported through the job environment so post-job saves can
resolve them even when setup actions are nested inside runtime acceptance.
The same rule keeps nested Electron/builder download caches available at cleanup.
On privileged Linux client fixtures, downloading and compilation run as the
runner user so restored caches remain usable and readable by cache upload.
Only the integration executable and its isolated runtime run as root.

Desktop jobs cache Electron/builder downloads and checksum-addressed runtime
inputs, not built applications, container layers, VM disks, or runtime state.
Every runtime cache read recomputes SHA-256 against the caller's expected
digest; corrupt bytes fail the job. A miss retains the existing bounded
download retries, then verifies and atomically publishes the complete cache
entry. Manifests and licenses without expected checksums are always fetched.
Source builds of the patched Docker CLI and Windows Lima still run against
verified sources and patches, reusing Go's compiler cache rather than skipping
builds or executable smoke checks. Docker integration image builds retain
their no-cache setting and use fresh disposable engines.

POSIX bundles also compile Porto's patched synchronization CLI and both Linux
agent architectures from checksum-pinned Mutagen source. Module and compiler
cache keys include that builder's pinned dependencies; no final synchronization
binary, daemon state or selected-project data is cached. macOS compilation
retains native FSEvents; the upstream deprecated FSEvents scheduling API produces
a compiler warning, not a suppressed diagnostic or a polling-only replacement.

The same optional download cache is available locally by setting
`PORTO_DOWNLOAD_CACHE` to a dedicated directory when running packaging scripts.
Leaving it unset preserves uncached downloads. `scripts/download-file.sh`
accepts an optional third SHA-256 argument; only downloads with an expected
checksum can enter the cache.

Build, packaging, runtime-acceptance, and security jobs use the latest Node 26.x
release. This host/build toolchain selection does not change the Node runtime
embedded in Electron.

The `ui` job installs with `npm ci` and runs `npm run lint`, `npm run build`,
and the desktop shell tests on Node 22.12, 24, and 26. Node 22.12 is the lowest
version Vite and oxlint accept. The Node 26 job audits the Electron lockfile
once, avoiding redundant registry requests and npm-version-specific tree checks.
Confirmed npm registry transport or server outages emit a warning instead of
failing the matrix; vulnerability reports and invalid lockfiles still fail.
The desktop test command initializes Electron before parallel test workers
start, preventing concurrent first-run extraction of its runtime.

The Node 26 job also runs the Playwright data-dashboard suite with Chromium.
Its browser downloads are cached by dashboard lockfile; the browser/system
dependency installation step still runs on every build. The suite selects the
full `chromium` channel, so `--no-shell` omits the unused Chrome Headless Shell.
The browser cache uses a separate layout version instead of restoring the old
archive containing both browsers. npm caches include
both dashboard and Electron lockfiles where both are installed. Neither
`npm ci`, dependency audits, nor desktop/browser tests are skipped on cache hits.
Synthetic container fixtures cover bounded stdout/stderr streams, cancellation,
copy/filter controls, running/stopped/read-only files, confirmed mutations and
downloads, recent resource counters, and sensitive metadata. These browser
tests do not connect to a user's daemon or require a workload shell.
Storage fixtures also cover exact category/object confirmation, referenced
volume protection, visible mount ownership, backup-policy edits, cancellation
and importing before a volume exists. Linux unit tests verify authoritative
metadata allocation and stable native volume identity across data replacement.
Migration fixtures additionally verify source-bound retry selections, reset
sensitive/helper consent, exact image dependencies, IPv6/exposed-port fidelity,
stale metadata, conflicting destinations and durable per-object interruption
outcomes. Native-access fixtures cover resource-bound write consent, unavailable
or replaced bridges, explicit failed-detach states, escaped host paths, recovery,
confined long/non-ASCII paths and connection loss. Unit/SFTP/browser fixtures do
not count as successful OS-native mounts for every platform/backend row; see
the [real-runtime matrix](data-management.md#real-runtime-acceptance).

### Required acceptance and optional platform integration

PR CI separates backend correctness, native filesystem behavior, and full
desktop-VM integration instead of making every signal depend on two emulated
VMs booting and remaining responsive.

| Coverage | Default PR gate | Execution |
| --- | --- | --- |
| Real runtime data | Yes, 7 jobs | Native Linux x64/ARM64 containerd; KVM-backed x64 Lima; migration/native phases plus the accelerated Lima engine lifecycle |
| Native filesystem drivers | Yes, 4 jobs | Actual macOS Intel/Apple Silicon WebDAV and Windows x64/ARM64 WinFsp mounts, with confined synthetic SFTP sources and no guest VM |
| Software-emulated Lima integration | No, 15 opt-in cells | Linux ARM64, macOS Intel/Apple Silicon and Windows x64/ARM64; migration, native, and engine phases |

The `Native filesystem` jobs exercise real host-mounted files, not an in-memory
filesystem mock. Windows builds the current native helper and installs the
checksum-pinned WinFsp driver. macOS uses its built-in WebDAV driver. These
checks retain direct edits, read-only behavior, confinement, and owned cleanup;
the macOS suite also exercises delayed readiness, owner-process death, and detach
recovery. They run independently of guest provisioning. Existing packaged
Windows-helper checks in `Native installers` remain required as well.

Required VM-backed acceptance checks for `/dev/kvm` before provisioning; it
must not silently fall back to a software-emulated destination. Linux ARM64's
native containerd migration and native-resource tests remain required. Those
local-backend fixtures may use an isolated emulated Docker source VM, but do
not qualify the host's Lima destination/restart behavior.

The Linux ARM64 and macOS/Windows Lima cells live in the manually dispatched
`Emulated runtime acceptance` workflow. This is a virtualization boundary,
not just an OS-name distinction: the hosted Linux ARM64 runner also lacks KVM,
and its restart case exhausted the 85-minute job budget. Hosted-runner TCG
timing, guest kernel timer panics, and repeated SSH startup made those
VM-backed cells an unreliable PR gate.
Deferring this matrix is an explicit coverage tradeoff, not a fix for those upstream or
timing problems. Passing the default gate does **not** establish full
software-emulated Lima migration, native-resource integration, or engine-restart
acceptance. Those results require a separate successful run on the selected
revision and platform. Nothing uses `continue-on-error` to disguise failures.

Once the manual workflow is available on the repository's default branch, run
one platform/phase against an explicit branch or tag:

```sh
gh workflow run emulated-runtime.yml --ref <branch-or-tag> \
  -f platform=windows-11-arm -f phase=migration
```

Use `platform=all` and `phase=all` for all 15 cells, or select
`platform=ubuntu-26.04-arm` for the unaccelerated Linux Lima row. Manual failures remain
failures, and canceled or unrun cells never count as accepted.
Select `platform=macos-latest` for Apple Silicon or `platform=macos-26-intel`
for Intel macOS; the input choices and full matrix use the same runner labels.

Both runtime workflows share `.github/actions/runtime-data/action.yml`, retain
workload-scoped Go and verified-download caches, and use fail-fast within their
own matrices. No cache contains fixture runtime state. Runtime phases run in
parallel on separately owned runtimes through
`scripts/test-docker-clients.sh <local|lima> data`;
`PORTO_RUNTIME_DATA_PHASE=migration|native|engine` selects a phase, while an unset value
or `all` retains the combined opt-in local suite. The migration phase runs every
round-trip, failure and interruption assertion. The native phase creates its
real resources through the same verified migration round trip, then runs every
mount, workload and standalone-VM lifecycle assertion. The engine phase also
performs the verified migration round trip, then runs all owned-engine
update, restart, identity-preserving reattachment and shutdown assertions.
Separate restart phases avoid combining both restart scenarios in one
executable. Local-containerd cells have no Lima engine to restart; selecting the
dedicated engine phase without an independently owned CI Lima runtime is an error.
No job waits for another phase's artifacts. The 85-minute job, 55-minute
executable, 40-minute acceptance and 20-minute setup/boot caps are unchanged.
Normal development-client jobs retain their existing scope. Linux installs
FUSE3, `qemu-utils`, and only the host-architecture QEMU package:
`qemu-system-x86` on x64 or `qemu-system-arm` on ARM64. All source and destination
fixtures retain their host-architecture guests. Linux builds checksum-pinned
SSHFS 3.7.6 with the required symlink containment fixes instead of the older
distribution binary. It grants the runner
access to an available KVM device. A fresh KVM-group process retains access in both destination and source
QEMU children. Rootful jobs explicitly forward the CI identity, temporary directory,
architecture, toolchain, phase and download-cache environment across elevation, including Ubuntu 26's
sudo-rs. Windows checksum-verifies the explicitly selected WinFsp driver; macOS
uses its built-in WebDAV driver without macFUSE.
Disposable CI destinations start the normal container inventory and borrow its
healthy runtime connection, matching daemon behavior instead of opening a new
containerd SSH channel for each file or migration operation.
Container creation, exec and network SDK adapters borrow that same connection
without closing the inventory-owned client when an individual operation finishes.
An authoritative empty native inventory never invokes the compatibility CLI:
there are no container fields to enrich. Deleting the final container also
clears old compatibility metadata before any later container recreation.
Compatibility CLI calls can reuse that connection's exact engine origin, with
ownership checked inside the actual guest command before executing nerdctl.
They do not spawn separate Lima status and ownership processes for each
individual API operation. Streamed filesystem helper requests use the same
owned origin and check ownership inside the guest before consuming transfer
input. This avoids repeating discovery for each WebDAV request while retaining
fresh resource-identity checks for every request and the 15-second mount-readiness
budget. A changed owner, instance or disconnected inventory
requires normal discovery again. Cold compatibility starts/restarts have a
one-minute task/shim/network startup budget instead of the generic 20-second
metadata budget; stricter caller deadlines still win.
Already-bound remote volumes verify their pinned metadata generation through
the existing guest identity helper instead of repeating nerdctl volume discovery.
Every concurrent request still makes its own fresh identity query; changed
generations, invalid helper identities and helper failures remain errors.
Initial volume path, driver and option validation is unchanged.
If a software-emulated CI volume listing reaches that metadata deadline, a
separate read-only diagnostic measures an SSH no-op and the same guest listing
under one 30-second diagnostic budget. It retains the original ownership
wrapper, reports guest execution time, and still returns the original failure.
The original read also records when guest start and finish markers reach the
host, rather than relying only on timings from a later successful diagnostic.
Timing markers are removed from metadata output, and the original deadline and
exit status are preserved. Fixture cleanup logs each resource's elapsed time
and remaining aggregate budget; exhausting that two-minute budget is reported
separately from an individual metadata timeout.
Successful requests, mutations, arbitrary shell commands, and caller
cancellation are never replayed by the post-failure probes.

The production migration/native APIs operate on uniquely labelled fixture
images, volumes, networks and stopped/running containers. A separate
Porto-managed, no-host-mount standalone VM supplies a real Docker Engine source
and managed-VM native-file acceptance. Real Unix/named-pipe forwarding and
Docker context discovery are exercised without changing the active context
or credentials. Original source configuration and data are retained.
The Docker source proxy reuses HTTP connections over its owned SSH transport,
rather than starting a new SSH process for every API request. Two sequential
real Docker pings must share one tunnel, and cleanup closes both HTTP pools.
An actual 8 MiB tmpfs checks staged low-space failure; a killed migration worker
checks durable per-object outcomes and never-started helper recovery.
The worker initializes the same inventory-owned runtime as the daemon before
transfer and reports explicit readiness or failure JSON; test-runner failure
text must never be mistaken for an actual helper-backed transfer checkpoint.
Native tests check actual workload-visible host edits, permissions, links, case,
concurrent changes, deletion/reused identities, connection loss and explicit
reconnect. Only disposable CI engines are stopped for update/shutdown tests.
The macOS native-driver suite also kills an actual mount-owner process and
checks exact owned recovery and retryable detach failure. A real delayed mount
exceeds the former shared 30-second startup watchdog without exceeding either
phase's limit: SFTP initialization and mounting each have an independent
30-second budget, followed by 15-second readiness. Successful initialization
disarms its watchdog; stalled initialization or readiness still cancels the
owned transport, and caller cancellation remains active throughout setup.
Source provisioning has a separate 20-minute setup budget; the unchanged
40-minute acceptance budget starts only after its real Docker fixtures are ready.
The executable's overall 55-minute deadline still bounds setup and acceptance
within each phase job.
Before destination engine update/restart, the now-unused disposable source VM
is stopped rather than consuming emulated host resources. In the engine phase
this follows the verified migration; combined `all` runs first complete the
standalone-VM reconnect cases as well.
Fixture cleanup visits each exact destination once, removing containers before
volumes/networks and image aliases last. Repeated alias lookups must not consume
the unchanged two-minute cleanup budget on software-emulated runners.
Freshly inspected stopped containers use ordinary removal, avoiding unnecessary
forced teardown and its extra identity lookup and ten-second finalization loop.
Running, paused, or ambiguous states still require force removal. Both paths
keep the production native-attachment and managed-container guards.
Independent volume and network removals overlap with at most two in flight,
while containers and image aliases remain serial. Each removal still performs
fresh ownership checks through the production APIs, and all failures are retained.
This keeps slow Windows SSH startup from serializing unrelated cleanup work past
the same deadline.
Temporary-volume rollback and fixture volume cleanup also reuse their freshly
verified inspection through the normal removal guards. They do not cache
ownership or repeat the same SSH inspection within the operation; the production
rollback still has its 30-second budget.

For local acceptance against an already-running, ownership-verified Porto
Lima destination, compile this revision's Linux helper beside a compiled test
executable as `runtime/bin/porto-runtime-helper`, matching the guest architecture.
Run that test executable with `PORTO_RUNTIME_DATA_ACCEPTANCE=1`,
`PORTO_RUNTIME_DATA_USE_CURRENT_HELPER=1` and
`-test.run '^TestRuntimeDataAcceptance$' -test.timeout=45m -test.v`.
It creates/removes its own separately owned source VM and fixture resources;
the destination engine and its installed helper are never replaced or stopped.

For an optional synthetic round trip against an **already running, owned**
Lima engine, cross-build the current Linux helper and set
`PORTO_STORAGE_INTEGRATION=1` / `PORTO_STORAGE_TEST_HELPER=<absolute-helper-path>`
before running `go test ./internal/docker -run
'^TestStorageVolumeLifecycleIntegration$' -count=1`. Match the helper architecture
to the guest. The fixture uploads a private temporary helper, creates uniquely
named synthetic volumes and cleans only identity-proven fixture resources; it
never starts/stops the engine or replaces its installed helper. It uses the
production runtime connector, not a test-only transport override.
`PORTO_STORAGE_RUNTIME_TEST=<absolute-cross-built-runtimefiles-test-path>` also
runs the Linux-specific metadata/identity tests in the guest.

`PORTO_DOCKER_TUNNEL_INTEGRATION=1 go test ./internal/docker -run
'^TestLimaContainerdProductionConnectionIntegration$' -count=1` separately checks
the real production connector on an already-running owned Lima engine without
reading workload data. It connects twice, cancels completed dial contexts,
checks containerd's health, and proves that closing each connection leaves
Lima's shared shell usable. Unit fixtures reject independent SSH forwarding
and cover helper failure diagnostics, pending cancellation and subprocess reaping.

The log stress case renders all 5,000 retained rows after a 6,000-record burst,
checks the discard count and locale-formatted timestamps, and keeps the same
assertion deadlines. Log rendering shares one time formatter per render rather
than creating one for every record.

The desktop lockfile keeps patched `brace-expansion` releases in both dependency
branches, `fast-uri` 3.1.9, and node-gyp's `undici` 6.29.1. Electron stays on the
compatible 44.9.x line at 44.9.3; development-client fixtures use patched `undici`
8.12.3. Dependency audits remain enforced without advisory suppressions.
A downloader override makes `app-builder-lib` use the same fetch-based
`@electron/get` 5.x downloader as Electron and Electron Packager, removing its
legacy `got`/`http-cache-semantics` dependency chain without downgrading
electron-builder or changing Electron. The installer script enables that
downloader's proxy support so `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY` remain
effective. Remove the override when electron-builder adopts the maintained
downloader itself.

Native runtime packaging executes `scripts/lima-runtime-smoke.cjs` against the
actual bundled Lima executable. It uses an isolated `LIMA_HOME` with synthetic
VM files to check dead-PID recovery, live-PID preservation, malformed-PID
diagnostics, and disk/configuration preservation without booting a VM.
Windows CI repeats these checks against the installed package in a path with
spaces, rather than only checking that its binaries exist. It also starts
synthetic consoleless processes and verifies that a forced stop terminates the
host agent and its descendants, releases the log files, and preserves VM data.
A failed termination must return an error without removing the PID files.
The same job runs the Windows update helper against that installed package,
covering silent in-place replacement and NSIS installation paths containing
spaces without launching the updated desktop.
These checks do not boot a guest or prove guest-kernel compatibility.

Native installers build in four independent OS/architecture jobs rather than
two sequential architecture loops. Each job verifies exactly one installer;
each macOS job also verifies its matching desktop archive and DMG checksum.
The Windows AMD64 job retains the complete installed-package, WinFsp,
logging, blocked-update, in-place-update, and uninstall smoke checks; ARM64
remains a cross-built package, not a claimed native execution test.
CI and CodeQL matrices use fail-fast: a failed member cancels queued and
running siblings instead of spending more time on an already-failed matrix.
Successful runs still execute every platform, architecture, and validation
step. Go test invocations stop starting tests after a failure, and Playwright
stops after its first failed test. Existing cleanup, timeouts, and bounded
download retries remain in place.

Installers are retained for three days as
`desktop-installers-<darwin|windows>-<amd64|arm64>` CI artifacts. Artifact
compression is disabled because DMGs, EXEs, and desktop tarballs are already
compressed; file-integrity validation and retention remain unchanged.
They use the application version with `+ci.<run-number>` build metadata, so a
test build is not immediately considered older than the same stable version.
These are unsigned PR/test builds, not published releases.

Superseded pull-request CI and CodeQL runs are canceled independently.
Push, scheduled, and manually triggered runs retain their existing behavior.

The development-client matrix runs real Docker SDK/CLI, Dev Containers 0.89.0,
and Testcontainers 12.2.0 workflows against disposable local Linux containerd
and native macOS/Windows Lima backends. Windows exercises Porto's named pipe.
The Linux runtime is checksum-pinned nerdctl-full 2.4.1 with separate
containerd/BuildKit sockets and state; Lima jobs use isolated `LIMA_HOME` and
`PORTO_HOME` roots. Only their own processes and VM are removed.
Hosted macOS and Windows runners use small host-architecture Ubuntu 26.04 QEMU
guests with no host mounts and a bounded 20-minute startup budget. Hosted macOS
requires software emulation because nested Virtualization.framework support is
unavailable; matching the host architecture keeps its guest agent/runtime helper
aligned without cross-architecture x86 emulation. Windows ARM64 hosted runners
also use TCG through Lima's QEMU command override because nested WHPX cannot
provide the required physical address width. This affects only disposable CI
guests, not packaged runtime defaults or native WinFsp mounting. Windows also
avoids unused SSHFS home-folder mounts and their startup probes. Short temporary
directory names keep both guest SSH socket paths within macOS's Unix-socket
length limit. The test harness records
ownership only inside the isolated runner temporary directory, including native
Windows paths. Windows disables CGO for the daemon/test build, as the client
suite does not require WinFsp. The compiled client-test executable sits beside
the bundled `runtime` directory, matching desktop helper discovery instead of
asking Windows executable lookup to find a Linux ELF binary on `PATH`.
Before package installation, each disposable Ubuntu guest uses the
[official Ubuntu snapshot service](https://snapshot.ubuntu.com/) at midnight UTC
on the date its fixture configuration was generated. The snapshot ID is logged
and reused for the destination, source VM, and restarts. Only the Ubuntu source
URIs change; suites, components, archive signing keys, signature verification,
and package hashes remain intact. Direct immutable URIs avoid mixing indexes
from out-of-sync live mirrors; APT snapshot selectors alone still fetch live
indexes. APT retries transient download failures, including HTTP 503 responses,
up to ten times per file, with exponential backoff capped at 30 seconds and
30-second HTTP/HTTPS transport timeouts. Retry delays are logged for both index
and package downloads. An unavailable snapshot or failed index fetch remains
an error after the retry budget is exhausted, with no fallback to mutable
mirrors or relaxed checks. The 20-minute boot deadline is unchanged, and
installed Porto environments are not reconfigured.
On software-emulated hosts, the host prepares Lima's pinned default containerd
archive before boot: it verifies the downloaded gzip, decompresses it natively,
and pins the resulting tar with its own SHA-256. The guest retains the same
runtime version, rootless services, architecture, and 20-minute boot deadline,
without repeatedly decompressing the large payload under QEMU. Failed boots
print bounded kernel/device-discovery records, read-only QMP disk/CPU state,
and host logs before the owned fixture is removed. When SSH is reachable, the
same diagnostic attempt records the guest fstab, block labels, mounted devices,
udev properties and unit timings, plus bounded udev and cloud-init logs.
Cloud-init module durations and the systemd critical chain distinguish early
network/SSH setup from later package and rootless provisioning. Both macOS
architectures report host load, memory, swap, and owned-runner QEMU process names
and usage while acceptance runs; guest CPU pressure alone is not a host capacity
measurement. Periodic sampling reads fixed process fields, not every process's
argument memory, so diagnostics do not add that expensive host-wide scan.
Unavailable diagnostics are reported without replacing the original failure;
diagnostics never repair mounts, change guest state or create engine ownership.
Fresh Lima fixtures fail immediately if that helper is unavailable.
Linux runners without `/dev/kvm` use the same host-side archive preparation.
Software-emulated ARM64 guests select `neoverse-n1` for both the destination and
source VM, avoiding the EDK2 LPA2 boot hang reported in
[tianocore/edk2#11962](https://github.com/tianocore/edk2/issues/11962).
The 20-minute guest startup budget and resource assertions remain unchanged.
The isolated Ubuntu guests omit APT translation, AppStream and
command-not-found indexes and the post-install `needrestart` scanner; signed
package indexes and normal dependency resolution remain enabled. Source Docker
installation omits optional recommendations. This keeps software-emulated setup
from spending its boot budget on unrelated desktop/package-search metadata or
restart suggestions for a disposable guest.
macOS data jobs lower the fixture process priority, inherited by both QEMU
guests, to leave scheduling priority for the hosted runner's heartbeat.
Intel macOS data jobs use two vCPUs per fixture VM, 1.5 GiB for the destination
and 1 GiB for the source, keeping the two-guest allocation within hosted RAM.
They select Lima's complete TCG fallback configuration: multi-threaded execution,
a 512 MiB translation cache per VM, disabled unsupported S3/S4 power states and
a supported emulated CPU. Forcing just the accelerator while Lima selects its
HVF configuration bypasses these TCG defaults. Serializing both guest vCPUs and
reducing the translation cache slows the CPU-bound device discovery and rootless
provisioning. The guest retains its two vCPUs and normal clock handling, without
instruction counting. Raising Lima's outer timeout does not repair an
emergency-mode boot.
The Intel runtime-data phases verify that both destination and source retain
the image's `LABEL=BOOT` and `LABEL=UEFI` fstab entries, resolve those labels to
the actual mounted `/boot` and `/boot/efi` devices, and reach `local-fs.target`.
The image, required mounts, rootless runtime and all boot/acceptance caps remain
unchanged. See [QEMU's instruction-counting design](https://www.qemu.org/docs/master/devel/tcg-icount.html).
A reported `kern.hv_support=1` does not establish supported nested
HVF: GitHub does not support nested virtualization on macOS runner images
([actions/runner-images#13277](https://github.com/actions/runner-images/issues/13277)).
Both source and destination inherit this CI-only QEMU override; packaged runtime
defaults are unchanged.
They remove the Go build cache after compiling the fixture, leaving disk space
for both guest disks, and emit periodic free-disk, RAM/swap and QEMU RSS/CPU
samples as job notices to aid lost-runner diagnosis when normal logs are
unavailable. Software-emulated data fixtures also log runtime
command durations so slow guest work can be distinguished from host process
startup and repeated discovery. Failed compatibility starts also retain nerdctl
debug output and the exact synthetic workload's observed task state. Guest boot
and acceptance thresholds are unchanged.
Porto forwards its selected boot budget to Lima's own `--timeout` as well as
the parent process context: first boots and recorded standalone restarts retain
20 minutes, while Kubernetes-node restarts remain five minutes. Lima's internal
ten-minute default must not silently shorten the recorded budget.
Porto-owned Docker engine installation, explicit restart and ownership recovery
likewise use the same 20-minute VM boot budget, without changing shorter caller
deadlines or the separate Kubernetes-node restart policy.

Ubuntu 26.04 avoids a [systemd v255 logind deadlock](https://github.com/lima-vm/lima/issues/5570)
that affects Ubuntu 24.04: Lima's first-boot `loginctl terminate-user` can race an SSH login,
leaving rootless provisioning waiting indefinitely for the user systemd socket.
An `engine.json` missing during cleanup after a failed boot is secondary: the
client test records Porto ownership only after Lima finishes starting.

Linux bundles tools as the runner user and elevates only the isolated
containerd/BuildKit workload.
CPU counter assertions remain enabled and average a fixed five-interval window.
Every interval checks available, monotonic backend counters; each window must
report finite, positive busy-workload activity and has a two-minute deadline.
Linux, Windows, and hardware-accelerated macOS retain the existing CPU ceiling
with its 5% quantization allowance. When macOS reports `kern.hv_support=0`, CI
explicitly warns and gates only that physical ceiling for its software-emulated
fixture: cgroup nanoseconds can outpace guest `/proc/stat` ticks even across a
longer window. Linux documents these
[tick-based CPU-accounting limitations](https://docs.kernel.org/admin-guide/cpu-load.html).
Raw counters are never clamped or rescaled, and synthetic SDK/unit-conversion
tests still enforce exact percentages. A real accelerated Mac is required to
validate the physical ceiling on that platform.
The workload verifies busy-container CPU percentages, exec, valid/corrupt image
archives, ordinary tag/push to a disposable registry, and client-owned cleanup.
It uses a named Dev Containers workspace volume and disables Ryuk while the
harness owns cleanup; it does not grant an untrusted guest a host control socket.

Client dependencies live under `scripts/docker-clients`, separate from the
shipped dashboard/desktop. Their lockfile uses maintained glob 13 instead of
archiver's deprecated glob 10. Optional protobuf/native SSH installation scripts
are explicitly disabled; these tests use Unix sockets/named pipes, not SSH clients.
Dependency installation and audit are required before these jobs run.

The Windows installer job also checksum-verifies and installs the official
WinFsp 2.1 driver on its disposable runner, then tests the **installed**
`porto-files-mount.exe` against synthetic confined SFTP roots. Container,
image, volume and VM fixtures cover real host paths, direct editor
write/rename, concurrent guest changes, read-only image rejection, spaces/
non-ASCII paths and owned detach without source deletion. Normal desktop
installation never silently installs WinFsp; users explicitly install it
before enabling native host folders.

Desktop logging tests run natively on Windows and macOS. The Windows installer
check also starts the installed daemon against a deliberately invalid,
isolated test database to verify that early failures reach `logs/porto.log`
at the default debug level, without duplicate records when stderr already
points at that file. No user database or running daemon is used.

macOS CI and DMG releases also run
`scripts/macos-startup.test.cjs`. It forces a failed power-registration call in
an isolated Electron process and verifies that the unmodified Electron 44.9.3
runtime still loads a sandboxed renderer and exits cleanly. The former native
workaround and crash-expecting negative control have been removed.
It also runs the production packager against an isolated synthetic source tree
and replaces the resulting application archive with a synthetic fixture before
launching it. Previous `Porto-<platform>-<architecture>` build directories must
not enter the archive when the packaging destination changes; ordinary app
resources and both icon formats remain included. The packaged executable
must render and exit cleanly with failed power registration without an
external guard, preserve Electron Node mode and its executable path, and
repair closed standard streams. `PORTO_TEST_MACOS_APP=/path/to/Porto.app`
runs these checks on a temporary copy of an already-built app instead;
the original bundle and user data are never modified.
The same suite prepares the cached source-development app, forces failed power
registration, and checks rendering and reopening without a command-line app
path. `ui/electron/dev-launcher.test.cjs` checks normal argument-free reopen,
preserved development-mode behavior, unchanged npm runtime files, cache reuse,
and Windows/Linux source arguments.
Synthetic macOS launches pass `-ApplePersistenceIgnoreState YES` so saved state
from earlier abnormal exits cannot open AppKit's modal reopen-windows prompt. This
affects only those test processes, not user defaults or normal Porto launches.
The test-only failure injector clears its loader environment before child
processes start. No native guard is linked or injected. Startup deadlines stay
unchanged, and failures include captured stdout/stderr plus the fixture's current
startup phase. Successful probes verify rendering, the locked runtime version,
and the normal quit event instead of accepting a deliberate crash.
The native tests are skipped on Windows and Linux; no macOS library is built
or added to their packages.

Both macOS DMGs and desktop tar archives are built on macOS from the same
app, retaining Electron's original fuse-aware Node dispatch and stdio
initialization. Packaging uses an invocation-private temporary directory so
parallel builds cannot remove one another's staged icon catalog or application.
Resource changes must precede any future code signing/notarization. macOS tar
archives omit AppleDouble metadata and retain the existing release layout.

macOS packages include a native Icon Composer `.icon` asset made from Porto's
existing artwork, with glass effects disabled, alongside the unchanged `.icns`
fallback. Electron Packager compiles the modern icon into `Assets.car`; native
smoke tests check both icon resources and their `Info.plist` entries. Building
macOS packages requires macOS 26 or newer and Xcode 26 or newer with `actool`
available. Command Line Tools alone cannot compile the modern icon. Install full
Xcode and complete its first-launch setup before running the macOS desktop tests
or preparing a release. Source development still uses the `.icns` icon and
prepares its cached bundle without compiling a native entry point.

Packaging honors an explicit `DEVELOPER_DIR`, otherwise checks the active Xcode
and then `/Applications/Xcode.app/Contents/Developer`. It selects tools only for
the current command, without changing the global `xcode-select` setting. A custom
or versioned Xcode installation can be selected explicitly:

```sh
DEVELOPER_DIR=/Applications/Xcode.app/Contents/Developer ./release.sh 0.2.0 --dry-run
```

`node scripts/macos-toolchain.cjs` checks Xcode and `actool` and prints the selected
developer directory. On macOS, `release.sh` runs this preflight before fetching,
changing release versions, or running validation; the native installer script
also checks before building or downloading runtime inputs. Missing or unsupported
tools fail with setup instructions rather than a late packaging assertion.

Dependency updates arrive through `.github/dependabot.yml`, which groups Go modules, dashboard packages, and GitHub Actions into weekly pull requests.

The Go module currently uses gRPC `v1.86.0-dev`, which includes the upstream
fix for the missing-authority server panic,
[GO-2026-6443](https://pkg.go.dev/vuln/GO-2026-6443). Prefer a stable release
containing the fix when updating this dependency; keep the vulnerability
scan enabled.

The module graph keeps `golang.org/x/net` at `v0.60.0` or newer for the HTTP/2
security fixes. Standard-library fixes, including compromised intermediate
certificate distrust ([GO-2026-6809](https://pkg.go.dev/vuln/GO-2026-6809)),
are included in the minimum supported Go `1.27.3`; use that release or a newer
security-patched toolchain for local builds and vulnerability scans.

## Cutting a release

1. Make sure `main` is green, checked out, clean, and tracking `origin/main`.
2. Optionally run the complete release validation without changing tracked files or Git history:

   ```sh
   ./release.sh 0.2.0 --dry-run
   ```

3. Prepare the release locally:

   ```sh
   ./release.sh 0.2.0
   ```

   The script accepts `0.2.0` or `v0.2.0`, validates strict SemVer, updates the Go CLI version plus the dashboard and Electron package manifests and lockfiles, runs the local checks below, creates a `chore(release): v0.2.0` commit when release metadata changed, and creates an annotated tag. Nothing is pushed by default.

4. Inspect the local commit and tag, then publish them explicitly:

   ```sh
   ./release.sh 0.2.0 --push
   ```

   Pass `--push` on the initial invocation to prepare and publish in one step. The script fetches first and atomically pushes `main` and the tag, so the release workflow cannot start from a tag without its release commit.

5. The `Release` workflow runs the Go test suite, lints and builds the dashboard, packages CLI/web and standalone desktop archives for every target, creates macOS DMGs and Windows NSIS EXE installers on native runners, and publishes a GitHub release with generated notes.

Tags must contain a strict SemVer prefixed with `v`. A suffix such as
`v1.2.3-rc.1` is published as a pre-release. Release assets are assembled and
verified while the GitHub release is still a draft, then published together.
Re-running a failed job can refresh that draft safely. A published release is
immutable: reruns fail instead of replacing assets that update clients may
already be downloading. A manual run from the Actions tab still requires the
tag to exist.

Release tag resolution checks out the explicit tag reference, validates SemVer,
and pins all three packaging jobs to that tag's resolved commit. The archive/test
job and the macOS and Windows installer jobs then run concurrently, keeping their
existing native builds, architecture loops, dependency installations, audits and
startup checks. Installers can finish even if the independent archive/test job
fails, but publication requires every job to succeed. Per-tag release concurrency
never cancels an in-progress publication.

Each producer uploads its existing artifact with seven-day retention and no
additional compression of the already-compressed archives, DMGs and EXEs.
The publisher downloads the three exact artifact IDs from those jobs in parallel
into the existing `dist` layout, rather than selecting arbitrary artifacts by a
wildcard. The sixteen-asset gate, checksum verification, provenance attestations,
draft recovery and immutable published-release checks are unchanged. This reduces
release serialization and transfer overhead without reusing PR-built packages or
skipping cold-install/build checks.

## Release artifacts

Each target produces a CLI/web archive named `porto_<version>_<os>_<arch>`
(`.tar.gz`, or `.zip` for Windows) with this layout:

```text
porto_1.2.3_darwin_arm64/
  porto            # porto.exe on Windows
  ui/dist/         # dashboard assets
  README.md
  LICENSE
```

The daemon resolves the dashboard from `$PORTO_UI_DIR`, `ui/dist` in the working directory, then `ui/dist` or `dist` next to the executable, so keep `ui/dist` beside the binary when installing. Release binaries are built with `CGO_ENABLED=0 -trimpath`; linker flags strip debug data and inject the release SemVer into `porto --version`.

Each target also produces `porto-desktop_<version>_<os>_<arch>`. Desktop
archives bundle the matching Porto binary, dashboard, icon, `kubectl`, Lima,
`k9s`, Docker CLI, Compose, Buildx, Dive, and supported `kind` clients. The app prepends those bundled tools to
the daemon's `PATH`, so they do not need separate installation.

The Docker CLI is rebuilt from the checksum-pinned official `v29.7.2` source
with one packaged-path patch:
`scripts/patches/docker-cli-29.7.2-bundled-plugins.patch`. The patch adds the
adjacent `runtime/docker/cli-plugins` directory as the highest-priority plugin
location without changing the user's Docker config directory. Compose `v5.5.1`
and Buildx `v0.37.1` are downloaded from their upstream releases with exact
per-platform SHA-256 pins. Their licenses/notices, the Docker patch, asset
names, versions, and digests ship under `runtime/licenses` and
`runtime/VERSIONS`.

Dive `v0.13.1` is checksum-pinned for every supported desktop architecture.
Its MIT license and version metadata ship beside the other runtime tools, and
native packaging smoke tests execute `dive --version`.

For Windows Lima 2.2.1, the bundler checksum-verifies the stable source archive,
applies `scripts/patches/lima-2.2.0-windows-pid.patch`,
`scripts/patches/lima-2.2.0-windows-stop.patch` and
`scripts/patches/lima-2.2.0-windows-graceful-stop.patch` and
`scripts/patches/lima-2.2.0-windows-arm64-cpu.patch`, and
`scripts/patches/lima-2.2.1-windows-wsl-mount.patch`, and rebuilds only
`limactl.exe` as `v2.2.1+porto.2`. The first four patches were introduced for
2.2.0 and remain necessary on 2.2.1. The PID change is an upstream backport; the
force-stop change makes Windows SIGKILL terminate the selected process tree
with a bounded, hidden `taskkill.exe` call and waits for the target to exit.
It stops the host-agent tree before the driver can exit and orphan children.
If `taskkill.exe` reports an already-exited target, its process handle must
confirm termination before the stop succeeds; other failures block PID cleanup,
restart, and deletion. Graceful QEMU stops use the owned QMP socket's ACPI
powerdown command instead of an unavailable console event, then wait for normal
host-agent exit and actual stopped state. The real Windows resource suite
exercises VM loss/reconnect and engine update/shutdown through that path.
The ARM64 fix reads the Windows-populated `x/sys/cpu` feature bits even when
its `Initialized` flag is false, matching that library's platform behavior.
Native ARM64 packaging checks that AES detection agrees with the OS feature bit.
Native Windows packaging also verifies that only WSL UNC provider
`ERROR_INVALID_FUNCTION` is classified as a temporarily unavailable root.
Other signals keep upstream behavior. All patches and the Apache-2.0 license
ship in `runtime/licenses`, and `runtime/VERSIONS` records the patched version.
Other platforms keep their verified upstream binaries. Remove each patch
when moving to a stable Lima version containing its fix; keep the runtime
smoke checks as the release gate.

macOS releases additionally contain architecture-specific `.dmg` installers,
and Windows releases contain architecture-specific NSIS `.exe` installers.
The one-line install scripts select these native packages automatically;
portable archives remain available for manual and headless setups.
The desktop updater consumes these same exact asset names through the GitHub
Releases API; Linux updates use the portable desktop `.tar.gz` archive.
Every desktop package also embeds the complete release SemVer separately from
OS version metadata, so a release candidate can still detect the matching
stable release.

A `SHA256SUMS` file covers every archive and installer, and each asset gets a
signed build provenance attestation. The assets themselves are not
code-signed. Verify a download with:

```sh
sha256sum --check --ignore-missing SHA256SUMS  # shasum -a 256 --check on macOS
gh attestation verify porto_1.2.3_linux_amd64.tar.gz --repo mbianchidev/porto
```

## Running the checks locally

```sh
gofmt -l .
go mod tidy -diff
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
go test ./...
go build ./cmd/porto
npm --prefix ui ci
npm --prefix ui run lint
npm --prefix ui run build
```

An opt-in multi-platform integration test uses an already running, Porto-owned
Lima engine on macOS or Linux. It applies engine emulation provisioning, then
runs uncached Dockerfile commands for eight Linux targets through a temporary
Porto Docker API socket and checks the exported architecture for each target.
It uses synthetic build inputs and does not restart or recreate the VM:

```sh
PORTO_DOCKER_MULTIPLATFORM_INTEGRATION=1 \
  go test ./internal/docker -run '^TestDockerMultiPlatformBuildIntegration$' -count=1 -v
```

Workflow files can be validated before pushing with:

```sh
go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/*.yml
```
