# Porto desktop app

A native Porto host around the daemon's web UI. It does not implement any
Porto functionality itself — it opens a window pointed at
`http://127.0.0.1:37623` (the Porto daemon's default address) and starts
`porto daemon start` unless the address exposes a compatible API and confirms
that its dashboard assets are ready.

## Security

- `contextIsolation: true` and `nodeIntegration: false` (plus `sandbox: true`)
  on the desktop window, so the loaded page has no access to Node or desktop
  runtime APIs.
- `preload.js` exposes only narrow desktop-preference and application-update
  operations. The main process rejects calls from pages outside the local Porto
  dashboard origin.
- Update downloads come only from the stable release returned by GitHub's
  `mbianchidev/porto` Releases API. Porto selects the exact installer for the
  current OS and architecture and verifies it against the release's
  `SHA256SUMS` entry before offering a restart.
- The daemon is started detached and un-ref'd. Closing the window never stops
  it — Porto keeps managing projects, containers, clusters, and VMs in the
  background exactly as it does when driven from a browser tab.

## Run

```sh
npm install   # installs the desktop runtime; only needed once
npm start
```

Development runs require the daemon binary (`porto`) on `PATH`. Release
packages bundle the daemon and portable runtime clients, so users do not need a
separate Porto, Docker CLI, Lima, `kubectl`, or `kind` installation. Windows
packages also bundle QEMU for the managed container runtime and virtual machines.
Windows ARM64 excludes Docker CLI and KinD because upstream binaries are not
published for that target.

## Package

From the repository root, build the dashboard and a matching Porto binary,
then package the branded Porto application:

```sh
npm --prefix ui run build
go build -o ui/electron/porto ./cmd/porto
bash scripts/bundle-desktop-runtime.sh darwin arm64 runtime
npm --prefix ui run desktop:package -- \
  --platform=darwin \
  --arch=arm64 \
  --porto-release-version=1.0.0 \
  --extra-resource=porto \
  --extra-resource=../dist \
  --extra-resource=../../runtime
```

Release automation performs this for every supported operating system and
architecture. Native macOS and Windows hosts can build the final DMG or NSIS
EXE package with:

```sh
bash scripts/package-desktop-installer.sh darwin arm64 1.0.0 dist
# Use: windows amd64 1.0.0 dist on Windows.
```

The assisted Windows installer leaves the desktop-shortcut option unchecked by
default. Silent installs do not create a desktop shortcut.

Packaged apps compare the bundled daemon's SHA-256 identity with the identity
cached by the running process and replace it when they differ, including
same-version rebuilds installed at the same path. They automatically provision
the bundled Porto container runtime on first launch. Packaged apps always start
the daemon executable embedded in their resources; they never require
`PORTO_BINARY` or a separate `porto` executable on `PATH`.

`scripts/bundle-desktop-runtime.sh` creates the runtime directory used by
releases. Packaged apps resolve the bundled binary and tools from Porto's
resources directory before falling back to `PORTO_BINARY` or `PATH`. Packages
also contain a statically linked Linux `porto-runtime-helper`; Porto
installs it only inside the Porto-owned Lima instance and uses it for guest-local
CNI operations and runtime capability probes.

## Updates

Installed builds check GitHub Releases shortly after launch and every 12 hours.
When a newer stable version exists, Porto prompts before downloading it. The
Settings page can enable automatic downloads; installation still waits for the
user to choose **Restart and update**.

The restart replaces only the packaged desktop application and, when required,
its detached daemon. It does not delete or recreate VMs, Kubernetes clusters,
containers, images, or volumes. After the updated app launches, the existing
daemon is reused when its bundled binary identity still matches, or replaced so
the new dashboard can reconnect to the same managed resources.

Updates require Porto to run from a user-writable installation. The standard
installers use `~/Applications/Porto.app`, `%LOCALAPPDATA%\Programs\Porto`, and
`~/.local/opt/porto`, which satisfy that requirement. A read-only or
administrator-owned installation reports an error and leaves the downloaded
package available instead of quitting.
