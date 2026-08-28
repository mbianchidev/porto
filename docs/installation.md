# Installation

Porto needs both the `porto` binary and the compiled dashboard assets. Release archives include both.

## Install a release

Download the archive for your platform and `SHA256SUMS` from the [releases page](https://github.com/mbianchidev/porto/releases). Verify the download, then unpack it:

```sh
sha256sum --check --ignore-missing SHA256SUMS
tar -xzf porto_<version>_<os>_<arch>.tar.gz
```

macOS ships `shasum` instead of GNU `sha256sum`:

```sh
shasum -a 256 --check --ignore-missing SHA256SUMS
```

Windows releases use `.zip` archives.

Each archive has this layout:

```text
porto_<version>_<os>_<arch>/
  porto            # porto.exe on Windows
  ui/dist/         # dashboard assets
  README.md
  LICENSE
```

Keep `ui/dist` beside the binary when moving the installation, or set `PORTO_UI_DIR` to the dashboard directory.

## Build from source

Requirements:

- Go 1.25 or newer
- Node.js 22.12 or newer (Node.js 20.19 is also supported) and npm

From the repository root:

```sh
npm --prefix ui ci
npm --prefix ui run build
go build -o porto ./cmd/porto
```

The resulting `porto` binary contains the daemon and CLI. It looks for dashboard assets in this order:

1. the directory set by `PORTO_UI_DIR`;
2. `ui/dist` in the working directory;
3. `ui/dist` next to the executable;
4. `dist` next to the executable.

For dashboard development, run `npm --prefix ui run dev`. See [continuous integration and releases](ci-cd.md) for all local validation commands and release details.

To place Porto on your `PATH` and keep the daemon running across daily sessions, continue with the [daily-use guide](daily-use.md).
