#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 3 ]; then
  echo "usage: bundle-synchronization-runtime.sh <goos> <goarch> <destination>" >&2
  exit 2
fi
goos="$1"
goarch="$2"
destination="$3"
case "$goos/$goarch" in
  darwin/amd64|darwin/arm64|linux/amd64|linux/arm64) ;;
  *) echo "Optimized shares require a supported POSIX host; ordinary bindings remain available." >&2; exit 2 ;;
esac
scripts="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
mkdir -p "$destination/bin" "$destination/licenses"
destination="$(cd "$destination" && pwd -P)"
temporary="$(mktemp -d)"
trap 'rm -rf "$temporary"' EXIT
source_checksum="8f36905875203f909c0edc89f938d6108ee5bfae64c7e8a011ba51e1fcac262e"
bash "$scripts/download-file.sh" \
  https://github.com/mutagen-io/mutagen/archive/refs/tags/v0.18.1.tar.gz \
  "$temporary/source.tar.gz" "$source_checksum"
tar -xzf "$temporary/source.tar.gz" -C "$temporary"
source="$temporary/mutagen-0.18.1"
(
  cd "$source"
  bash "$scripts/apply-runtime-patch.sh" "$PWD" "$scripts/patches/mutagen-0.18.1-version.patch"
  GOWORK=off go get \
    github.com/klauspost/compress@v1.20.1 \
    golang.org/x/net@v0.60.0 \
    golang.org/x/sys@v0.49.0 \
    golang.org/x/term@v0.47.0 \
    golang.org/x/text@v0.43.0 \
    google.golang.org/grpc@v1.84.0 \
    google.golang.org/protobuf@v1.36.12
  mkdir -p "$temporary/agents"
  for arch in amd64 arm64; do
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" GOWORK=off \
      go build -mod=readonly -buildvcs=false -trimpath -tags=mutagenagent -ldflags='-s -w' \
      -o "$temporary/agents/linux_$arch" ./cmd/mutagen-agent
  done
  cgo=0
  if [ "$goos" = darwin ]; then
    if [ "$(go env GOHOSTOS)" != darwin ]; then
      echo "macOS filesystem notifications require building the synchronization CLI on macOS." >&2
      exit 1
    fi
    cgo=1
  fi
  CGO_ENABLED="$cgo" GOOS="$goos" GOARCH="$goarch" GOWORK=off \
    go build -mod=readonly -buildvcs=false -trimpath -tags=mutagencli -ldflags='-s -w' \
    -o "$destination/bin/mutagen" ./cmd/mutagen
  cp LICENSE "$destination/licenses/mutagen.txt"
  cp go.mod "$destination/licenses/mutagen.go.mod"
  cp go.sum "$destination/licenses/mutagen.go.sum"
)
COPYFILE_DISABLE=1 tar -czf "$destination/bin/mutagen-agents.tar.gz" \
  -C "$temporary/agents" linux_amd64 linux_arm64
cp "$scripts/patches/mutagen-0.18.1-version.patch" "$destination/licenses/mutagen-version.patch"
printf '%s\n' \
  'Mutagen 0.18.1-porto.1; scoped two-way-safe sessions; Linux amd64/arm64 agents' \
  "Source SHA-256 $source_checksum; compiler $(go env GOVERSION)" \
  > "$destination/licenses/mutagen-build.txt"
