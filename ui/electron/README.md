# Porto desktop shell

A minimal Electron wrapper around the Porto daemon's web UI. It does not
implement any Porto functionality itself — it only opens a window pointed at
`http://127.0.0.1:37623` (the Porto daemon's default address) and starts
`porto daemon start` when that address is unreachable.

## Security

- `contextIsolation: true` and `nodeIntegration: false` (plus `sandbox: true`)
  on the `BrowserWindow`, so the loaded page has no access to Node or Electron
  APIs.
- `preload.js` is intentionally empty; it exposes no bridged API.
- The daemon is started detached and un-ref'd. Closing the window never stops
  it — Porto keeps managing projects, containers, clusters, and VMs in the
  background exactly as it does when driven from a browser tab.

## Run

```sh
npm install   # downloads Electron; only needed once
npm start
```

The daemon binary (`porto`) must be on `PATH` for the shell to be able to
start it automatically when it is not already running.

## Package

From the repository root, build the dashboard and a matching Porto binary,
then pass the target to Electron Packager:

```sh
npm --prefix ui run build
go build -o ui/electron/porto ./cmd/porto
npm --prefix ui run desktop:package -- \
  --platform=darwin \
  --arch=arm64 \
  --extra-resource=porto \
  --extra-resource=../dist
```

Release automation performs this for every supported operating system and
architecture. Packaged apps resolve the bundled binary from Electron's
resources directory before falling back to `PORTO_BINARY` or `PATH`.
