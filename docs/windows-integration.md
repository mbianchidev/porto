# Windows development integration

Porto's Windows container execution backend remains **Linux containerd and
BuildKit in its owned Lima/QEMU VM**. Selected WSL distributions are development
clients of that engine, not replacement engines. This slice does not introduce
a Windows-container runtime, change the Docker API's Linux OS reporting or
switch Lima to an experimental WSL driver. Native Windows containers remain
tracked separately in [#104](https://github.com/mbianchidev/porto/issues/104).

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
ConPTY implementation for container, pod, VM, k9s and Dive terminals. Resize,
disconnect, cancellation and exit codes use the same WebSocket contract as
Unix PTYs. The terminal waits for and reaps its own command; it does not
terminate unrelated host agents or the shared engine to close a console.
Background tools continue using consoleless Windows subprocesses.

| Windows capability | x64 validation | ARM64 validation |
| --- | --- | --- |
| Private Docker named pipe and existing Lima/QEMU Linux engine | Native client/runtime acceptance | Cross-build plus native platform/unit coverage |
| Linux Docker, Compose, Buildx and WSL helper artifacts | Native Linux ELF checked and exercised in WSL 2 | Native ARM64 Linux ELF checked, never replaced with x64 artifacts |
| Selected-distribution Compose/build/bind/watch/credential workflow | Real WSL 2 acceptance with an independent owned QEMU engine and an untouched control distribution | Requires a real WSL 2-capable ARM64 host; hosted ARM64 runners do not provide that kernel capability |
| Native trusted portless HTTPS and embedded ConPTY | Dedicated native service/browser/console acceptance | Dedicated native service/browser/console acceptance |
| Native Windows containers | Not implemented in this slice | Not implemented in this slice |
| KinD binary | Bundled x64 binary | Upstream native binary unavailable; reported separately |

The acceptance rows are executable CI contracts, not a claim of universal VPN,
browser or Windows-edition compatibility. See [networking](networking.md) for
HTTPS setup, native trust, proxy settings and backend-specific direct-IP limits.
