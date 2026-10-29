# Windows development integration

Porto keeps **Linux containerd and BuildKit in its owned Lima/QEMU VM** as the
default Windows-hosted engine. Selected WSL distributions remain development
clients of that engine. Native Windows containers use a separately owned
containerd/runhcs backend, selected explicitly through the `porto-windows`
Docker context or the dashboard's **Container backend view**.

These are separate endpoints and stores, not an image conversion or a switch to
Lima's WSL driver. Both API endpoints report their actual backend OS.

## Bundled QEMU and Unicode paths

The Windows bundle adds a per-process UTF-8 manifest to `qemu-img.exe` and its
architecture-matched `qemu-system-*.exe` before packaging or signing. This lets
their narrow Windows file APIs open firmware and disks in paths containing
non-ASCII characters on Windows 10 version 1903 or later, without changing the
host's locale/code-page settings or substituting an ASCII installation path.
See Microsoft's [UTF-8 process code-page documentation](https://learn.microsoft.com/en-us/windows/apps/design/globalizing/use-utf8-code-page).

The downloaded vendor archive is checksum-verified and left unchanged; only
isolated build copies are edited. Native code, image addressing, version/icon
resources and unrelated data are verified unchanged. Editing removes the
vendor Authenticode signature, which would otherwise be invalid; any signing
of these modified copies must happen after bundling. `runtime/VERSIONS` records
the change, and its manifest/build helper are retained under `runtime/licenses`.
A matching native Windows build also creates a Unicode-path image and opens
Unicode-path firmware in a paused, network-free QEMU process, then requires a
clean QMP exit. This quick probe does not replace full installed-runtime CI.

The bundled Lima also recognizes the native OS client's
`OpenSSH_for_Windows_` version banner. Otherwise it treats that client as version
zero and never applies its existing CPU-aware cipher preferences. The patch
retains the upstream OpenSSH 8.1 minimum for cipher reordering and the actual
Windows processor feature check; it does not force AES on unsupported CPUs.

The bundle also backports
[Lima's opening-session guard](https://github.com/lima-vm/lima/commit/396604e38baf2645a4fa23b4bb4f012ef41ddb85)
for the Ubuntu 24.04/systemd v255 first-boot `terminate-user` race documented in
[lima-vm/lima#5570](https://github.com/lima-vm/lima/issues/5570). It waits within
the existing boot budget before refreshing the guest environment. Unavailable
session inspection or sessions still opening after 30 seconds fail explicitly,
without an unsafe reset or an SSH-ready success marker.
Sessions that finish between enumeration and inspection are accepted only after
a fresh list confirms removal. Unknown state for a still-listed session remains
an error. Source-patch regression tests run on the build host, including when
cross-packaging a Windows target.

This upstream mitigation is not atomic: a new login after the last snapshot
can still race. The definitive fixes are in newer guest systemd versions
([0ae9073f](https://github.com/systemd/systemd/commit/0ae9073f15209b6e0acfbd23ec146c4a29b7abbe),
[b2a41090](https://github.com/systemd/systemd/commit/b2a4109031c1bd79c498f8642df150deeebe1708)).
The existing Windows Ubuntu 24.04 image selection and existing guest disks are
unchanged; no systemd binary, kernel, PAM policy or host configuration is patched.

## Native Windows setup

In **Settings > Native Windows containers**, inspect host capabilities and
explicitly install the native runtime. Setup requests administrator permission
for its protected service, binaries and named pipe. The ordinary desktop/project
daemon remains unprivileged. Missing Windows Containers or Hyper-V features
require explicit OS setup and any requested reboot; Porto does not enable
features or reboot automatically.

```powershell
porto docker windows status
porto docker windows install
porto docker windows context --confirm
docker --context porto-windows info
docker --context porto-windows pull mcr.microsoft.com/windows/nanoserver:ltsc2025
docker --context porto-windows run --name synthetic-windows --isolation process mcr.microsoft.com/windows/nanoserver:ltsc2025 cmd.exe /c echo synthetic-windows
docker --context porto-windows logs synthetic-windows
docker --context porto-windows inspect synthetic-windows
docker --context porto-windows rm synthetic-windows
```

Choose an image compatible with the inspected host, not merely one labelled
`windows/amd64`. Process isolation requires a matching supported Windows build;
different patch revisions are not different builds. Hyper-V isolation requires
the actual feature and usable hardware/nested virtualization. Unsupported
architectures, editions, image versions, host-process isolation and unverified
virtualization fail before native workload mutation.

Docker image configuration IDs and native runtime manifest digests are distinct.
Porto resolves full or short image IDs before native creation, tagging and
removal, and uses the validated immutable manifest for creation. Image inspection
retains the actual Windows `os.version` from the matching OCI configuration.

Native creation prepares the Windows snapshot and container metadata through the
maintained containerd/runhcs SDK, without executing the workload. Only an
explicit start allocates its owned network endpoint and starts the task. Native
logs, exec and TTY streams use protected Windows named pipes and the runhcs
console, not the experimental Windows `nerdctl create` implementation.
The dashboard WebSocket terminal uses the same maintained SDK exec path,
including console resize, actual exec exit codes and disconnect reaping.
Closing it does not stop container init or a different interactive exec.

`porto docker windows context` explicitly selects the native CLI context.
Changing **Container backend view** only changes the dashboard view: open
inspectors/terminals disconnect, but neither engine nor its running workloads
are stopped. Opted-in WSL wrappers continue using the original Linux endpoint.
Native inventory, request caches and live streams are separated from Linux;
native terminals select Windows shells such as `cmd.exe`, not a Linux toolbox.

Native bind mounts use explicit Windows host/container paths. Spaces and
non-ASCII paths remain argument values. Native networking uses its supported
Windows NAT/HCS integration; it does not substitute Linux CNI or create a public
Docker TCP listener. Treat access to the native control pipe as privileged host
access and grant it only to the owning user and administrators.

Workload mounts currently require existing directory bind sources. Managed or
anonymous volume mounts, pause/resume and automatic restart policies are rejected
explicitly rather than translated into different behavior.

Stop/remove the owned integration explicitly:

```powershell
porto docker windows stop --confirm
porto docker windows remove --confirm
```

Active native workloads prevent unsafe stop/update/removal. Removal keeps
native images, containers and volume data, and does not delete or interrupt the
Linux engine, WSL distributions or unrelated services/networks. A conflicting
service identity or ownership marker is an error, not permission to adopt it.
The disposable Windows development-client gate keeps an actual Linux workload
and binary volume dataset alive during native installation, an independently
built changed-binary update, repair, context changes and removal. It checks
unchanged Linux PID/start identity and new workload reads/writes, not only an
engine metadata sentinel.

Native registry proxy and CA settings cross the owned pipe from the owning
user and are DPAPI-encrypted at rest. See
[native Windows registry networking](networking.md#native-windows-registry-networking)
for explicit apply and repair commands.

The Windows AMD64 package includes checksum-pinned source builds of containerd,
the maintained runhcs shim, nerdctl and Windows CNI plugins. They use a patched
Go compiler and official crypto/network dependencies rather than shipping old
upstream prebuilt executables with known advisories. Build metadata and licenses
are retained in the runtime bundle. Native ARM64 Windows-container payloads are
not substituted with AMD64 binaries.

Linux BuildKit, CRIU, cgroup metrics, Linux file helpers, and the existing
Linux backup/migration workflows are not advertised as native Windows features.
Switch to the Linux view for those workflows instead of applying them to a
Windows store.

## Select a distribution

Install WSL and the distributions you want explicitly through Windows.
`wsl --list --verbose` shows their current versions. Porto requires WSL 2 and
never converts WSL 1 distributions, changes the default distribution, edits
`.wslconfig`/`wsl.conf`, or enables Windows virtualization features for you.
The selected user needs Windows executable interoperability, `wslpath`,
`sh`, `tar`, `realpath` and ordinary POSIX user-profile tooling.

In **Settings > Selected WSL distributions**, enable exactly the distribution
you want. Equivalent CLI commands are:

```powershell
porto wsl list
porto wsl enable "Ubuntu-24.04"
porto wsl status
```

Porto installs owned native Linux Docker, Compose, Buildx and `porto-wsl`
binaries only in that distribution user's `~/.local/share/porto/wsl`.
Its owned shell hooks update `.profile` and existing `.bashrc`/`.zshrc` files;
regular profile files are required. Symbolic links and unrelated tooling
directories are rejected before replacement. Open a new shell afterward.
Neither the distribution's existing Docker binaries nor `~/.docker/config.json`
are overwritten.

Linux clients use a mode-`0600` Unix socket in an owned mode-`0700` directory
under `/tmp`. A supervised-on-use user bridge relays Docker HTTP, upgrades and
Buildx streams through the installed Windows `porto.exe` to the private
`\\.\pipe\porto_docker_engine`. It opens **no TCP Docker or control API**.
The integration is tied to the distribution's registration ID and a generated
ownership token. Replaced registrations, missing tools and stopped
distributions are errors, not reasons to mutate or start other distributions.
Windows' UNC file provider can itself restart a distribution while existing
shared-file consumers keep reading it. Pause/stop those consumers or release
the selected shares before expecting the distribution to remain stopped.
Status inspection only lists registrations and running state; it does not
execute a command inside the distribution.

## Shares and file changes

The selected Linux home and the Windows user home exposed inside that
distribution become explicit share roots. Additional roots are optional:

```powershell
porto wsl enable --mount-root /workspace "Ubuntu-24.04"
porto wsl apply-mounts --restart-engine
```

Linux filesystem roots use the selected distribution's `\\wsl.localhost` share.
Windows-drive roots use their native Windows paths instead of a UNC round trip
through WSL, which can deny access to mounted Windows drives. Each selected root
gets a distinct guest mount point; existing Lima host-home mounts and their
permissions/options remain unchanged.

The second command is a separate, explicit maintenance action. It verifies
the existing engine's ownership, preserves unrelated Lima mount options and
restarts only Porto's owned Linux engine. Save running work first. It does not
replace the VM, delete images/containers/volumes, or change standalone VMs.
New engine creation includes selected roots; an existing engine is never
restarted implicitly by WSL enablement.

Bind paths are translated from selected absolute Linux roots, including
Windows-drive locations, into their owned guest mount points. Spaces, quotes
and non-ASCII characters stay in argument arrays rather than shell command
interpolation. Unknown roots and missing guest mounts fail with setup guidance.
Named volumes are not treated as host paths. Device passthrough from a different
WSL kernel is unsupported.

These are direct shared files, not a cached copy-back workspace. Native inotify
does not cross the Windows/Lima shared-filesystem boundary reliably. WSL bind
containers receive `CHOKIDAR_USEPOLLING=true` and `WATCHPACK_POLLING=true` defaults;
explicit application values are preserved. Other watch libraries must select
their polling mode explicitly. Rootless guest ownership and the source
filesystem still determine which writes are permitted.

Inside the opted-in shell:

```sh
docker info
docker compose up --detach
docker buildx build --no-cache --load --tag synthetic-wsl-app .
```

The wrapper defaults to Porto, clears inherited Docker connection overrides and
uses its own persistent `~/.config/porto/wsl-docker` configuration. Explicit Docker
connection flags retain Docker's normal meaning. Build contexts originate in
the selected Linux filesystem and travel through the private Buildx session.

## Credentials, repair and removal

The scoped `docker-credential-porto` helper stores Docker login secrets in the
current Windows user's native credential vault. Only an opted-in, currently
registered distribution can call it. Linux configuration contains the helper
name, not the secret. Vault errors are visible and never fall back to plaintext.
Existing unrelated Docker credentials and contexts are not imported or changed.

Repeat `porto wsl enable` to repair tools after changing the installed Windows
executable location or upgrading Porto. Repair retains the integration identity,
existing additional roots when none are supplied, and persistent CLI/Buildx
configuration. Partial provisioning restores previous tools and owned hooks.

```powershell
porto wsl disable "Ubuntu-24.04"
porto wsl apply-mounts --restart-engine
```

Removal stops only the owned distribution bridge and removes only its tools and
hooks. Windows vault credentials, persistent scoped CLI configuration and Linux
engine data remain. Apply mount changes separately to release old shares.
**Disable and release shares before externally deleting, renaming or reimporting
a distribution.** Registration-identity mismatches are refused rather than
automatically adopting a replacement distribution.

## Embedded terminals and supported boundaries

Windows 10 1809 and newer provide ConPTY. Porto uses the maintained Charm
ConPTY implementation for Linux container, pod, VM, k9s and Dive terminals. Resize,
disconnect, cancellation and exit codes use the same WebSocket contract as
Unix PTYs. The terminal waits for and reaps its own command; it does not
terminate unrelated host agents or the shared engine to close a console.
Background tools continue using consoleless Windows subprocesses.
Native Windows-container terminals use the runhcs console and protected named-pipe
I/O, with the same resize, disconnect and exit-code contract.

| Windows capability | x64 validation | ARM64 validation |
| --- | --- | --- |
| Private Docker named pipe and existing Lima/QEMU Linux engine | Native client/runtime acceptance | Cross-build plus native platform/unit coverage |
| Linux Docker, Compose, Buildx and WSL helper artifacts | Native Linux ELF checked and exercised in WSL 2 | Native ARM64 Linux ELF checked, never replaced with x64 artifacts |
| Selected-distribution Compose/build/bind/watch/credential workflow | Real WSL 2 acceptance with an independent owned QEMU engine and an untouched control distribution | Requires a real WSL 2-capable ARM64 host; hosted ARM64 runners do not provide that kernel capability |
| Native trusted portless HTTPS and embedded ConPTY | Dedicated native service/browser/console acceptance | Dedicated native service/browser/console acceptance |
| Native Windows containers | Separately selected containerd/runhcs; native acceptance targets supported Windows Server 2025 process-isolated workloads; other host/isolation combinations remain capability-gated | Native WCOW image/runtime support is unavailable; Linux/WSL ARM64 tooling remains separate and supported |
| KinD binary | Bundled x64 binary | Upstream native binary unavailable; reported separately |

The acceptance rows are executable CI contracts, not a claim of universal VPN,
browser or Windows-edition compatibility. See [networking](networking.md) for
HTTPS setup, native trust, proxy settings and backend-specific direct-IP limits.
