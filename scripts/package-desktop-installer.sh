#!/usr/bin/env bash

set -euo pipefail

if [ "$#" -ne 4 ]; then
  echo "usage: package-desktop-installer.sh <darwin|windows> <amd64|arm64> <version> <output-directory>" >&2
  exit 1
fi

goos="$1"
goarch="$2"
version="$3"
output_directory="$4"

case "$goos" in
  darwin)
    electron_platform="darwin"
    extension="dmg"
    binary="porto"
    builder_target=(--mac dmg)
    ;;
  windows)
    electron_platform="win32"
    extension="exe"
    binary="porto.exe"
    builder_target=(--win nsis)
    ;;
  *)
    echo "unsupported installer platform: $goos" >&2
    exit 1
    ;;
esac

case "$goarch" in
  amd64)
    electron_arch="x64"
    ;;
  arm64)
    electron_arch="arm64"
    ;;
  *)
    echo "unsupported installer architecture: $goarch" >&2
    exit 1
    ;;
esac

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$repo_root"

output_directory="$(mkdir -p "$output_directory" && cd "$output_directory" && pwd -P)"
temporary="$(mktemp -d)"
trap 'rm -rf "$temporary"' EXIT HUP INT TERM

desktop_version="${version%%[-+]*}"
app_root="$temporary/app"
runtime_directory="$temporary/runtime"
packager_output="$temporary/package"
builder_output="$temporary/installer"
mkdir -p "$app_root" "$packager_output" "$builder_output"

CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
  go build -trimpath -ldflags '-s -w' -o "$app_root/$binary" ./cmd/porto
bash scripts/bundle-desktop-runtime.sh "$goos" "$goarch" "$runtime_directory"
test -f "$runtime_directory/VERSIONS"

(
  cd ui/electron
  node package.cjs \
    --platform="$electron_platform" \
    --arch="$electron_arch" \
    --out="$packager_output" \
    --app-version="$desktop_version" \
    --build-version="${GITHUB_RUN_NUMBER:-0}" \
    --extra-resource="$app_root/$binary" \
    --extra-resource="$repo_root/ui/dist" \
    --extra-resource="$runtime_directory"
)

packaged_app="$(find "$packager_output" -mindepth 1 -maxdepth 1 -type d -print -quit)"
if [ -z "$packaged_app" ]; then
  echo "Electron Packager produced no Porto application." >&2
  exit 1
fi
test -n "$(find "$packaged_app" -path '*/runtime/VERSIONS' -print -quit)"
node scripts/desktop-runtime-symlinks.cjs --validate "$packaged_app"

builder_input="$packaged_app"
if [ "$goos" = "darwin" ]; then
  builder_input="$(find "$packaged_app" -mindepth 1 -maxdepth 1 -type d -name '*.app' -print -quit)"
  if [ -z "$builder_input" ]; then
    echo "Electron Packager produced no macOS application bundle." >&2
    exit 1
  fi
fi

if [ "$goos" = "darwin" ]; then
  packaged_binary="$builder_input/Contents/Resources/$binary"
  if [ ! -x "$packaged_binary" ]; then
    echo "Packaged Porto.app is missing its executable daemon: $packaged_binary" >&2
    exit 1
  fi
else
  packaged_binary="$packaged_app/resources/$binary"
  if [ ! -f "$packaged_binary" ]; then
    echo "Packaged Porto application is missing its daemon: $packaged_binary" >&2
    exit 1
  fi
fi

(
  cd ui/electron
  CSC_IDENTITY_AUTO_DISCOVERY=false npx --no-install electron-builder \
    --prepackaged "$builder_input" \
    "${builder_target[@]}" \
    "--$electron_arch" \
    --publish never \
    --config.directories.output="$builder_output"
)

installer="$(find "$builder_output" -maxdepth 1 -type f -name "*.$extension" -print -quit)"
if [ -z "$installer" ]; then
  echo "electron-builder produced no .$extension installer." >&2
  exit 1
fi

asset="porto-desktop_${version}_${goos}_${goarch}.${extension}"
cp "$installer" "$output_directory/$asset"
echo "$output_directory/$asset"
