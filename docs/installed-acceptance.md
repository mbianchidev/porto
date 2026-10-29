# Installed-package acceptance

These gates address the **test-definition gap** in #100; definitions and
cross-builds are not a claim that six installed platforms have passed. Each target still
needs a successful native CI execution and its `installed-acceptance_*.json`
artifact. Keep the existing development-client and runtime-data results as
separate evidence, not replacements for this gate.

The two installed-package matrices collect every platform's result even when
one sibling fails. A canceled build is not qualification evidence, and
rebuilding all canceled siblings repeatedly obscures independent defects.
This diagnostic/release need can consume more minutes in a failing run;
every row remains required and any failure still fails CI. Other CI matrices
retain their normal fail-fast policy.

## What the gate exercises

`scripts/installed-acceptance.cjs` uses the actual installed desktop,
`resources/porto` (`porto.exe` on Windows), Docker CLI, Compose and Buildx.
It does not build a source-served API, inject a fake backend, adopt an existing
engine, or skip a missing tool/transport.

The fixture:

1. Creates a fresh, checkout-local synthetic HOME, PORTO_HOME, LIMA_HOME,
   Electron profile, Docker configuration and Buildx state. It does not inherit
   credentials, SSH agents, native-loader variables, trust overrides or source
   dashboard/daemon overrides. A Git discovery ceiling prevents the synthetic
   project from inheriting the surrounding source checkout. It refuses existing
   Porto processes and listeners
   on the fixed daemon/router ports and Windows Porto named pipe.
2. Installs under a path containing both spaces and non-ASCII characters. It
   checks native executable headers, pinned metadata and reported Docker
   **29.7.2**, Compose **5.5.1** and Buildx **0.37.1** versions. Docker's reported
   plugin paths must resolve to the installed plugins, despite unrelated stale
   plugins and an unrelated current context in the synthetic configuration.
   CLI operations have only the installed runtime on PATH.
3. Starts the real installed Electron application without changing its
   resources, entry point or fuses. The desktop must launch the installed
   daemon, whose health identity must match that binary's hash, provision a
   real Porto-owned Lima engine, and render its packaged dashboard.
4. Uses **`porto docker cli buildx build --builder default --load`** to build a
   synthetic BusyBox-based image, then inspects its loaded ID and label.
   The installed Compose plugin starts two services on the same named network.
   The worker depends on the app's native HTTP health check, reaches its
   `app` service DNS name over HTTP, and reads its shared named-volume token
   through a read-only mount. Real container exec reads both
   the built image fixture and retained token. Missing downloads, tools, backend
   or named-pipe/socket support fail.
   The synthetic HTTP check uses Porto's default 30-second probe timeout,
   including exec/SSH setup on software-emulated hosted guests. It must still
   fetch the actual token successfully; stack command deadlines and production
   operation timeouts are unchanged.
   Network inspection must report distinct live endpoint addresses under the
   selected named network; the pinned CNI-based nerdctl inspection does not
   itself expose Docker's `NetworkID`. Porto resolves canonical IDs from the
   selected backend's network inspection for Docker container summaries and
   inspection, so Compose can recognize an unchanged network without recreating
   its services. The gate requires those exact IDs and actual service DNS/HTTP
   communication, not invented identities or metadata alone.
5. Connects to that installed dashboard's loopback DevTools endpoint and uses
   its ordinary renderer-origin container APIs to observe, stop and start the
   same Compose container. With the workload running, ordinary desktop update
   preparation must refuse with HTTP 409, leaving its PID, start time and
   volume token unchanged. No real user project or workload participates.
6. Updates the stack to remove its worker with `up --remove-orphans`, requiring
   the obsolete service to disappear while the app container and shared data
   remain unchanged. It then changes a synthetic environment revision and
   restores the previous revision: both operations must replace the selected
   service, expose the actual selected environment, remain HTTP-ready, and
   retain the same named-volume data. It runs real Compose down, retaining the synthetic
   volume. It stops only the
   owned fixture engine and recorded desktop/daemon processes, checking process
   identity again before signaling. The normal bounded state-convergence wait
   requires the event-backed inventory to contain no remaining synthetic
   containers; a successful remove is not confused with an asynchronously
   published, older inventory snapshot.
   The same bounded convergence wait is used after Compose up before requiring
   each selected service's running-only list to contain exactly one real
   container ID. After worker removal, the whole project's running-only list
   must contain exactly the unchanged app ID. Faster lifecycle metadata
   acknowledgment may precede that event snapshot; a nonempty malformed or
   multi-container response to a single-service query still fails immediately.
7. Reapplies the same already-built package, checks that owned executables were
   replaced with the shipped bytes, and reopens the installed desktop. The
   existing image, engine ownership and real volume token must survive. No
   second runtime or desktop build is needed.
8. Uninstalls and verifies the unchanged populated Porto database (including a
   registered, never-started synthetic project and any SQLite WAL), engine
   metadata and guest disk, plus unrelated Docker configuration, context,
   credential-helper, plugin, command-entry and Buildx sentinels. Windows uses
   the real NSIS uninstaller. macOS/Linux exercise manual removal of the owned
   application and owned command/desktop entries, not a fictional uninstaller.
   The guest baseline hashes Lima 2.2's canonical `disk` path, including a
   confined legacy-migration link when present, not the pre-2.1-only `diffdisk`
   filename.

The Unix package driver, `scripts/installed-package.cjs`, serves the built
artifact and its checksum on an ephemeral loopback HTTP endpoint to the
existing `install-desktop.sh`. Windows keeps the existing NSIS, QEMU-blocked
update, Lima process-recovery, installed logging and native-filesystem checks;
its update runs the helper shipped in the installed package. The old
Unix-socket fake-backend Compose test is retained as a Linux **API contract**
test, not mislabeled as Windows or installed-runtime acceptance.

## Native targets and guest execution

The relevant `ci.yml` jobs select matching hosts, rather than calling an x64
cross-build on an ARM host or an ARM cross-build on an x64 host "native":

| Target | Job / runner |
| --- | --- |
| Linux AMD64 | `linux-toolchain`, `ubuntu-latest` |
| Linux ARM64 | `linux-toolchain`, `ubuntu-24.04-arm` |
| macOS AMD64 | `native-installers`, `macos-26-intel` |
| macOS ARM64 | `native-installers`, `macos-latest` |
| Windows AMD64 | `native-installers`, `windows-2025` |
| Windows ARM64 | `native-installers`, `windows-11-arm` |

Hosted fixtures explicitly select **Lima/QEMU TCG**, using Lima's supported
per-user `override.yaml` configuration and QEMU executable selection. The
installed Porto binary still creates the guest and its ownership record; the
harness never fabricates that record or precreates an engine to claim. It
reuses the existing checksum-verified `lima-containerd-archive.cjs` bootstrap
and dependency-provision fixture. Linux/macOS install their documented QEMU
prerequisite; Windows must select its shipped architecture-matched QEMU.
The host-only Windows archive download uses verified Git for Windows Bash
without loading profiles, not the unrelated `System32\bash.exe` WSL launcher.
No WSL distribution is needed to prepare the installed Lima guest archive.
Linux renders the actual desktop through Xvfb. On AppArmor-restricted Linux
hosts, the real installer also requests the scoped Chromium user-namespace
permission described in [Linux desktop sandbox setup](installation.md#linux-desktop-sandbox).
The gate never uses `--no-sandbox` or globally disables the restriction.
Manual app removal retains this administrator-installed host prerequisite.

"Native" here means native installed **host executables**, not hardware-
accelerated guests. TCG results do **not** establish default macOS VZ/HVF,
Linux KVM or Windows WHPX acceptance. A disposable capable host can run the
same gate with the `default` backend to qualify the normal first-run driver.
Do not infer a capability blocker from a fail-fast cancellation. A concrete
unsupported host, unavailable runner/toolchain, missing prerequisite, failed
download or failed real workflow is a failed/unqualified row, never a skip,
host-CLI fallback, `continue-on-error` result or another platform's pass.

Build macOS DMGs/archives only on macOS with the existing documented
[Icon Composer toolchain](ci-cd.md#ci-details). The acceptance scripts never
modify package resources after signing or patch the installed Electron runtime.
They do not install native HTTPS trust or run the separate #101 trust gate.

## Running and reviewing

Run executable acceptance only as an unprivileged user on a fresh disposable CI
host, never on a developer machine. The scripts require the CI environment and
refuse existing
Porto endpoints/processes instead of borrowing or stopping them. All fixture
paths must remain below the checkout; use a short checkout path for native
Unix socket limits.

Process ownership uses kernel executable and creation metadata, not mutable
command-line titles. Linux reads the owned PID's procfs records; macOS builds
an invocation-private libproc inspector with the same native Xcode/clang
toolchain required for packaging. That inspector is test-only and is removed
on exit, not installed or shipped. Windows retains native executable-path and
creation-time checks. Unknown or changed ownership never permits signaling.
Failed startup retains bounded, sanitized Lima/QEMU diagnostics under the
synthetic fixture's `diagnostics` directory, even if failed-guest cleanup
removes the original log paths. The same bounded desktop, daemon and guest
diagnostics are printed for workflow or cleanup failures after startup.
Guest serial output is retained alongside the host-agent and QEMU logs. A
Windows startup that is still provisioning after five minutes also
collects one read-only SSH/session diagnostic: native client verbose output,
guest CPU/memory/pressure and process state, and bounded SSH/logind/cloud-init
journals. The diagnostic has its own three-minute cap so it can observe a
120-second PAM/session stall, redacts output, and does not
repair or restart the guest. Host CPU count and available memory are reported
separately from guest pressure. No provisioning or acceptance deadline changes.
Fixed stage labels identify the failing operation, and each original failure
category is reported before cleanup errors are combined. Raw exception
messages, arguments and environment values are not printed.
Renderer-origin container start requests use the existing one-minute
start-command allowance rather than the debugger's shorter 30-second default.
Other debugger requests and all production operation deadlines are unchanged.
Backend availability alone does not complete startup: the actual packaged
dashboard target must appear within the same existing 25-minute provisioning
wait. The fixture does not start a premature one-minute page timer while
Electron is still completing its normal provisioning sequence. Actual DOM
rendering remains required afterward.

Unix CI passes the package it just built:

```sh
node scripts/installed-package.cjs \
  "dist/porto-desktop_${VERSION}_darwin_arm64.dmg" \
  "$VERSION" darwin/arm64 .installed-macos qemu-tcg
```

For Linux use its native `.tar.gz` artifact and `linux/amd64` or `linux/arm64`
inside the job's Xvfb session. Windows CI surrounds its real installer/update/
uninstaller with `prepare`, `preflight`, `run`, `upgraded` and `removed` commands
from
`installed-acceptance.cjs`, applying the synthetic `environment` before
installation. `removed` writes the evidence artifact only after **all** gates
pass, then deletes the stopped synthetic fixture. Failed fixtures are retained
for diagnosis; no success report is emitted.

Fast, non-provisioning validation:

```sh
node --test scripts/installed-acceptance.test.cjs
node --check scripts/installed-fixture.cjs
node --check scripts/installed-acceptance.cjs
node --check scripts/installed-package.cjs
actionlint .github/workflows/ci.yml
```

Those focused unit tests check isolation and fail-closed assertions with
synthetic files and an ephemeral listener. They are **not** native installed
Compose/Buildx, first-launch, upgrade or uninstall execution evidence.
