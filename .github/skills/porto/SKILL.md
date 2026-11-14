---
name: porto
description: Install and drive the Porto CLI (`porto`) to run local projects, containers, Compose stacks, Kubernetes clusters and Linux VMs on a developer machine. Use this skill whenever the user mentions Porto or `porto`, asks to install or upgrade Porto, wants to start, stop or tail logs of a local app or Compose stack through Porto, run Docker against the `porto` context, create a local k3s/k0s/kind cluster, create or exec into a Porto VM, or diagnose a runtime that Porto manages, even if they don't name the CLI explicitly.
---

# Porto CLI

Porto is a local-first development control plane. A Go daemon and the `porto`
CLI share state with an Electron desktop app and a browser dashboard at
`http://127.0.0.1:37623`. It manages native and Compose projects, its own
containerd + BuildKit Docker engine, k3s/k0s/kind clusters and Lima VMs.

Porto is for local development, not production. Drive it through the CLI
instead of editing its state directory, sockets or Lima instances directly: the
daemon records ownership so it never touches resources it didn't create, and
bypassing it breaks that bookkeeping.

## 1. Check before installing

```sh
command -v porto && porto version && porto daemon status
```

If `porto version` works, Porto is installed; go to step 3. Don't reinstall
"just to be safe": the installer stops running Porto daemons (and the projects
they manage) and replaces the app. Rerun it only when the user asks to install
or upgrade.

## 2. Install (only when missing or an upgrade is requested)

Unless the user already asked you to install Porto, tell them what will happen
and get a go-ahead first. The installer downloads a release, may need
administrator rights on Linux, and launches the desktop app.

macOS and Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/mbianchidev/porto/main/scripts/install-desktop.sh | sh
export PATH="$HOME/.local/bin:$PATH"
```

Windows PowerShell (open a new terminal afterwards so `Path` refreshes):

```powershell
irm https://raw.githubusercontent.com/mbianchidev/porto/main/scripts/install-desktop.ps1 | iex
```

The installers pick the package for the OS and architecture, verify it against
the release's `SHA256SUMS`, install for the current user, link `porto` into
`~/.local/bin`, add `docker` and `dive` links only when those commands aren't
already installed, and launch the app. Set these environment variables before
the command when needed:

| Variable | Effect |
| --- | --- |
| `PORTO_VERSION=v1.9.0` | Install a specific release instead of the latest |
| `PORTO_NO_LAUNCH=1` | Install without opening the app |
| `PORTO_BIN_DIR` | CLI link directory (default `~/.local/bin`) |
| `PORTO_INSTALL_DIR` | App location (macOS `~/Applications`, Linux `~/.local/opt/porto`) |
| `PORTO_SKIP_PREREQS=1` | Linux only: skip QEMU and sandbox setup when the user manages them |

Platform notes:

- **macOS**: builds aren't notarized, so Gatekeeper may block the first launch.
  Tell the user and point to the
  [quarantine guide](https://github.com/mbianchidev/porto/blob/main/docs/installation.md#macos-gatekeeper-quarantine).
  Never disable Gatekeeper globally. `brew install qemu` is only needed for
  snapshot-capable VMs.
- **Linux**: the installer installs or verifies QEMU and may configure an
  AppArmor sandbox profile through `sudo`. If you can't answer a sudo prompt,
  hand the command to the user.
- **Headless machines** (no desktop session): use the CLI and web archive from
  the [releases page](https://github.com/mbianchidev/porto/releases) as in
  [manual archive installation](https://github.com/mbianchidev/porto/blob/main/docs/installation.md#manual-archive-installation).
  Keep `porto` and `ui/dist` together, then run `porto docker engine-install`
  once the daemon is up.

Verify with `porto version`.

## 3. Make sure the daemon is running

```sh
porto daemon status   # prints "running"
```

- **Desktop install**: the app runs the daemon. If it isn't running, open the
  app (macOS: `open "$HOME/Applications/Porto.app"`). Don't start a second
  daemon next to the app.
- **CLI-only install**: `porto daemon start` stays in the foreground, so launch
  it as a background or detached process and poll `porto daemon status` until it
  prints `running`. Stopping that daemon gracefully stops every project it
  manages.

Use the same OS user and `PORTO_HOME` for the daemon and every CLI call;
otherwise the CLI reads a different state database.

## 4. Projects (native apps and Compose stacks)

```sh
porto scan --depth 3 ~/code          # flags go BEFORE the roots
porto list                           # JSON array of projects
porto start --no-pull <project>
porto logs <project> --stream all -n 200
porto restart <project>
porto stop <project>
```

- `porto list`, `porto runtime status`, `porto vm list` and
  `porto kubernetes clusters` print JSON. Parse it rather than scraping text:
  `porto list | jq -r '.[] | [.name, .status, .port, .strategy] | @tsv'`.
- `porto start` runs `git pull --ff-only` on the active branch first. Pass
  `--no-pull` when the user has local work so Porto doesn't move their branch.
  Dependency setup still runs.
- A project stays `starting` through dependency setup until
  `http://127.0.0.1:<port>/` returns HTTP 200, and becomes `crashed` if setup or
  the app fails. Check `porto logs` before reporting success.
- Ports are auto-assigned from `41000`; pin one with `porto port <project> 8080`.
  A running project is also routed at `http://<project>.porto.localhost:37680`.
- `stop` is graceful. `kill` is forceful, and for Compose projects it runs
  `docker compose down --remove-orphans`, removing the stack's containers.
- Discovery picks the first runnable strategy per subtree: Make, Compose,
  Node.js, Python, Go, then Rust. If a project is missing, rescan its root or
  read the [discovery rules](https://github.com/mbianchidev/porto/blob/main/docs/project-management.md#discovery).
- `porto branch <project> <branch>` switches the worktree and restarts a running
  project. It refuses dirty worktrees.

## 5. Containers through Porto's Docker engine

Prefer forms that leave the user's global Docker defaults alone:

```sh
porto docker run --rm alpine:latest echo hello   # Docker flags, Porto endpoint
docker --context porto ps --all
porto docker compose up --detach                 # from a Compose project dir
porto docker status                              # engine state
```

- `porto ps`, `porto exec`, `porto compose`, `porto images` and similar are
  shortcuts for `porto docker <cmd>`.
- **`porto start|stop|logs` act on projects, not containers.** For containers
  use `porto docker stop|logs <container>` or `docker --context porto ...`.
- `docker context use porto` changes the default context for every terminal.
  Run it only when the user wants Porto as their default engine. Unset
  `DOCKER_HOST` or `DOCKER_CONTEXT` if they override the selected context.
- Docker API 1.41 to 1.47 is supported; Swarm and engine plugins aren't. Check
  the [compatibility matrix](https://github.com/mbianchidev/porto/blob/main/docs/docker-engine.md#docker-api-compatibility-matrix)
  before relying on an unusual feature.

## 6. Kubernetes clusters

```sh
porto runtime status                                   # is kubernetes enabled?
porto runtime enable kubernetes
porto kubernetes cluster create dev --provider k3s --workers 1
porto kubernetes clusters
porto kubernetes pods --namespace all --context <ctx>
porto kubernetes logs <namespace> <pod> [--container name]
porto kubernetes exec <namespace> <pod> -- <command...>
porto kubernetes cluster stop dev                      # keeps disks and data
porto kubernetes cluster start dev
```

Providers are `k3s` (default), `k0s`, `kind`, and `apple` on Apple silicon.
Completed clusters are registered in `~/.kube/config`, so plain `kubectl` works
after selecting their context. Creation boots VMs and can take several minutes.
`porto kubernetes cluster delete <name>` removes the node VMs and kubeconfig
immediately, without a prompt.

## 7. Linux VMs

```sh
porto runtime enable vms
porto vm images
porto vm create test-ubuntu --image ubuntu-24.04 --vm-type qemu --cpus 2 --memory 2048 --disk 20
porto vm exec test-ubuntu uname -a
porto vm copy ./file.txt test-ubuntu:/tmp/file.txt
porto vm snapshot test-ubuntu before-change     # QEMU VMs only
porto vm stop test-ubuntu
```

`vm create` starts the VM unless `--stopped` is passed; `--memory` is MiB and
`--disk` is GiB. `porto vm list` hides Kubernetes node VMs and Lima instances
Porto doesn't own. `porto vm delete <name>` force-deletes immediately, without
a prompt.

## 8. Diagnose problems

```sh
porto diagnose --json
```

Exit codes: `0` healthy or degraded, `1` a required component is unavailable,
`2` an unsafe condition. Report the `checks` whose `state` isn't `healthy` or
`neutral`. `porto diagnose bundle --preview` shows what a support bundle would
contain. Repairs need consent: `porto diagnose repair <action> --confirm`. The
daemon log is `logs/porto.log` in the state directory: macOS
`~/Library/Application Support/porto`, Linux `~/.config/porto`, Windows
`%APPDATA%\porto`, or `PORTO_HOME` when set.

## Ground rules

- **Ask before anything destructive or global.** Deleting clusters, VMs or
  volumes, `kill` on Compose projects, storage prunes, anything taking
  `--confirm`, `docker activate`, `https install` and default-context changes
  affect the user's data or machine-wide settings. Some act immediately with no
  prompt, so explain the effect and let the user decide.
- **Don't kill Porto's processes by hand.** The daemon owns and tracks its Lima
  host agents, QEMU processes and engine. Use the matching `porto ... stop`
  command so its state stays consistent.
- **Keep the control ports on loopback** (`37623`, `37680`, `37681`). They're
  unauthenticated by design, so never bind or forward them to a network.
- **Trust the installed version over this file.** `porto help` lists the exact
  commands. The [docs index](https://github.com/mbianchidev/porto/blob/main/docs/README.md)
  covers what this skill leaves out: remote environments, extensions, data
  backups and migration, WSL, native Windows containers, networking and HTTPS.
