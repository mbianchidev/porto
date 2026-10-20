# Continuous integration and releases

Porto's automation lives in `.github/workflows`. Every workflow declares the least privilege it needs and checks out the repository without persisted Git credentials.

## Workflows

| Workflow | File | Trigger | Purpose |
| --- | --- | --- | --- |
| CI | `ci.yml` | pull requests, pushes to `main`, manual | Formatting, vet, tests, cross-compilation, dashboard lint and build |
| CodeQL | `codeql.yml` | pull requests, pushes to `main`, weekly | Static analysis for `go` and `javascript-typescript` |
| Security audit | `security.yml` | pushes to `main`, weekly, manual | `govulncheck` and `npm audit` |
| Release | `release.yml` | tags matching `v*`, manual | Builds, packages, and publishes archives plus native macOS and Windows installers |

## CI details

The `go` job runs on `ubuntu-latest`, `macos-latest`, and `windows-latest` with the current stable toolchain, plus one extra `ubuntu-latest` job on `1.26.9` to guard the exact minimum version declared in `go.mod`. Linux runs `gofmt -l`, `go mod tidy -diff`, and `go test ./... -race`; the other platforms run the plain test suite.

The `cross-build` job compiles `./cmd/porto` with `CGO_ENABLED=0` for every release target (`linux`, `darwin`, and `windows` on `amd64` and `arm64`), so a broken platform build fails the pull request instead of the release.

The `ui` job installs with `npm ci` and runs `npm run lint`, `npm run build`,
and the desktop shell tests on Node 22.12, 24, and 26. Node 22.12 is the lowest
version Vite and oxlint accept. The Node 24 job audits the Electron lockfile
once, avoiding redundant registry requests and npm-version-specific tree checks.
Confirmed npm registry transport or server outages emit a warning instead of
failing the matrix; vulnerability reports and invalid lockfiles still fail.
The desktop test command initializes Electron before parallel test workers
start, preventing concurrent first-run extraction of its runtime.

The Node 24 job also runs the Playwright data-dashboard suite with Chromium.
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

The eight `Runtime data` jobs separately run actual Docker-source migration and
native resource mounts on Linux x64/ARM64 with both local containerd and Lima,
macOS Intel/Apple Silicon, and Windows x64/ARM64. They reuse the disposable
runtime bootstrap through `scripts/test-docker-clients.sh <local|lima> data`;
normal development-client jobs retain their existing scope. Linux installs
FUSE3/QEMU and builds checksum-pinned SSHFS 3.7.6 with the required symlink
containment fixes instead of the older distribution binary. It grants the runner
access to an available KVM device. A fresh KVM-group process retains access in both destination and source
QEMU children. Rootful jobs explicitly forward the CI identity, temporary directory,
architecture and toolchain environment across elevation, including Ubuntu 26's
sudo-rs. Windows checksum-verifies the explicitly selected WinFsp driver; macOS
uses its built-in WebDAV driver without macFUSE.

The production migration/native APIs operate on uniquely labelled fixture
images, volumes, networks and stopped/running containers. A separate
Porto-managed, no-host-mount standalone VM supplies a real Docker Engine source
and managed-VM native-file acceptance. Real Unix/named-pipe forwarding and
Docker context discovery are exercised without changing the active context
or credentials. Original source configuration and data are retained.
An actual 8 MiB tmpfs checks staged low-space failure; a killed migration worker
checks durable per-object outcomes and never-started helper recovery. Native
tests check actual workload-visible host edits, permissions, links, case,
concurrent changes, deletion/reused identities, connection loss and explicit
reconnect. Only disposable CI engines are stopped for update/shutdown tests.
The macOS native-driver suite also kills an actual mount-owner process and
checks exact owned recovery and retryable detach failure.

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
branches. A downloader override makes `app-builder-lib` use the same fetch-based
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

Native installers are retained for three days as the
`desktop-installers-darwin` and `desktop-installers-windows` CI artifacts.
They use the application version with `+ci.<run-number>` build metadata, so a
test build is not immediately considered older than the same stable version.
These are unsigned PR/test builds, not published releases.

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
On software-emulated hosts, the host prepares Lima's pinned default containerd
archive before boot: it verifies the downloaded gzip, decompresses it natively,
and pins the resulting tar with its own SHA-256. The guest retains the same
runtime version, rootless services, architecture, and 20-minute boot deadline,
without repeatedly decompressing the large payload under QEMU. Failed boots
print bounded host and guest cloud-init logs before the owned fixture is removed.
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
an isolated Electron process and verifies that the unmodified Electron 44.7.1
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
security fixes. Related standard-library fixes require Go `1.26.9` or
`1.27.2`; use a security-patched toolchain for local builds and vulnerability
scans, not an older patch release on a newer Go minor line.

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

For Windows Lima 2.2.0, the bundler checksum-verifies the stable source archive,
applies `scripts/patches/lima-2.2.0-windows-pid.patch`,
`scripts/patches/lima-2.2.0-windows-stop.patch` and
`scripts/patches/lima-2.2.0-windows-graceful-stop.patch` and
`scripts/patches/lima-2.2.0-windows-arm64-cpu.patch`, and rebuilds only
`limactl.exe` as `v2.2.0+porto.6`. The PID change is an upstream backport; the
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
