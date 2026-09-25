#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 3 ]; then
  echo "usage: bundle-desktop-runtime.sh <goos> <goarch> <destination>" >&2
  exit 2
fi

goos="$1"
goarch="$2"
destination="$3"
script_directory="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"

kubectl_version="${KUBECTL_VERSION:-v1.36.1}"
kind_version="${KIND_VERSION:-v0.33.0}"
k9s_version="${K9S_VERSION:-v0.50.18}"
lima_version="${LIMA_VERSION:-v2.2.0}"
docker_version="29.7.2"
qemu_version="11.1.0"
qemu_build="20260811"

case "$goos/$goarch" in
  linux/amd64|linux/arm64|darwin/amd64|darwin/arm64|windows/amd64|windows/arm64) ;;
  *)
    echo "unsupported desktop runtime target: $goos/$goarch" >&2
    exit 2
    ;;
esac

temporary="$(mktemp -d)"
trap 'rm -rf "$temporary"' EXIT
rm -rf "$destination"
mkdir -p "$destination/bin" "$destination/lima" "$destination/licenses"
destination="$(cd "$destination" && pwd -P)"

download() {
  local url="$1"
  local output="$2"
  curl --fail --location --retry 4 --retry-all-errors --silent --show-error "$url" --output "$output"
}

sha256_file() {
  node -e 'const fs=require("node:fs");const crypto=require("node:crypto");const file=process.argv[1];const hash=crypto.createHash("sha256");hash.update(fs.readFileSync(file));process.stdout.write(hash.digest("hex"))' "$1"
}

verify() {
  local expected
  expected="$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]' | tr -d '[:space:]')"
  local file="$2"
  local actual
  actual="$(sha256_file "$file")"
  if [ -z "$expected" ] || [ "$expected" != "$actual" ]; then
    echo "checksum mismatch for $(basename "$file"): expected $expected, got $actual" >&2
    exit 1
  fi
}

manifest_checksum() {
  local manifest="$1"
  local asset="$2"
  awk -v asset="$asset" '$2 == asset || $2 == "*" asset { print $1; exit }' "$manifest"
}

binary_suffix=""
if [ "$goos" = "windows" ]; then
  binary_suffix=".exe"
fi

kubectl_asset="kubectl${binary_suffix}"
kubectl_url="https://dl.k8s.io/release/${kubectl_version}/bin/${goos}/${goarch}/${kubectl_asset}"
download "$kubectl_url" "$destination/bin/$kubectl_asset"
download "${kubectl_url}.sha256" "$temporary/kubectl.sha256"
verify "$(cat "$temporary/kubectl.sha256")" "$destination/bin/$kubectl_asset"

docker_bundled=false
docker_extension="tgz"
case "$goos/$goarch" in
  darwin/arm64)
    docker_os="mac"
    docker_arch="aarch64"
    docker_checksum="b8683ed19d1f06048a496f9b8429e2c71d0b088d475b7487c054ea3666c02a3c"
    ;;
  darwin/amd64)
    docker_os="mac"
    docker_arch="x86_64"
    docker_checksum="fb1f1aa7ac7af4364165b9eadfda92e96c8ced508fca74f53079719891367438"
    ;;
  linux/arm64)
    docker_os="linux"
    docker_arch="aarch64"
    docker_checksum="43d143448adf2c2787704e7d7704fd6d62d367a54c5edaef0a3f75509cb0938d"
    ;;
  linux/amd64)
    docker_os="linux"
    docker_arch="x86_64"
    docker_checksum="803d433f226db4776e1768fd319fc6c6e4935a456acf84fcc0080818b854bc8f"
    ;;
  windows/amd64)
    docker_os="win"
    docker_arch="x86_64"
    docker_extension="zip"
    docker_checksum="ed9222f478a5d143ac90e8e2fd3209b5076382cdb4b210321f97aa4b68bc6811"
    ;;
  windows/arm64)
    docker_os=""
    docker_arch=""
    docker_checksum=""
    ;;
esac
if [ -n "$docker_os" ]; then
  docker_asset="docker-${docker_version}.${docker_extension}"
  docker_url="https://download.docker.com/${docker_os}/static/stable/${docker_arch}/${docker_asset}"
  download "$docker_url" "$temporary/$docker_asset"
  verify "$docker_checksum" "$temporary/$docker_asset"
  mkdir -p "$temporary/docker"
  if [ "$docker_extension" = "zip" ]; then
    unzip -q "$temporary/$docker_asset" "docker/docker.exe" -d "$temporary/docker"
    mv "$temporary/docker/docker/docker.exe" "$destination/bin/docker.exe"
  else
    tar -xzf "$temporary/$docker_asset" -C "$temporary/docker" docker/docker
    mv "$temporary/docker/docker/docker" "$destination/bin/docker"
  fi
  docker_bundled=true
fi

kind_bundled=false
if [ "$goos/$goarch" != "windows/arm64" ]; then
  kind_asset="kind-${goos}-${goarch}"
  kind_url="https://github.com/kubernetes-sigs/kind/releases/download/${kind_version}/${kind_asset}"
  download "$kind_url" "$temporary/$kind_asset"
  download "${kind_url}.sha256sum" "$temporary/kind.sha256sum"
  verify "$(manifest_checksum "$temporary/kind.sha256sum" "$kind_asset")" "$temporary/$kind_asset"
  mv "$temporary/$kind_asset" "$destination/bin/kind${binary_suffix}"
  kind_bundled=true
fi

case "$goos" in
  darwin) k9s_os="Darwin"; k9s_extension="tar.gz" ;;
  linux) k9s_os="Linux"; k9s_extension="tar.gz" ;;
  windows) k9s_os="Windows"; k9s_extension="zip" ;;
esac
k9s_asset="k9s_${k9s_os}_${goarch}.${k9s_extension}"
download "https://github.com/derailed/k9s/releases/download/${k9s_version}/checksums.sha256" "$temporary/k9s-checksums.txt"
download "https://github.com/derailed/k9s/releases/download/${k9s_version}/${k9s_asset}" "$temporary/$k9s_asset"
verify "$(manifest_checksum "$temporary/k9s-checksums.txt" "$k9s_asset")" "$temporary/$k9s_asset"
mkdir -p "$temporary/k9s"
if [ "$k9s_extension" = "zip" ]; then
  unzip -q "$temporary/$k9s_asset" -d "$temporary/k9s"
else
  tar -xzf "$temporary/$k9s_asset" -C "$temporary/k9s"
fi
mv "$temporary/k9s/k9s${binary_suffix}" "$destination/bin/k9s${binary_suffix}"

case "$goos" in
  darwin)
    lima_os="Darwin"
    [ "$goarch" = "amd64" ] && lima_arch="x86_64" || lima_arch="arm64"
    lima_extension="tar.gz"
    ;;
  linux)
    lima_os="Linux"
    [ "$goarch" = "amd64" ] && lima_arch="x86_64" || lima_arch="aarch64"
    lima_extension="tar.gz"
    ;;
  windows)
    lima_os="Windows"
    [ "$goarch" = "amd64" ] && lima_arch="AMD64" || lima_arch="ARM64"
    lima_extension="zip"
    ;;
esac
lima_asset="lima-${lima_version#v}-${lima_os}-${lima_arch}.${lima_extension}"
download "https://github.com/lima-vm/lima/releases/download/${lima_version}/SHA256SUMS" "$temporary/lima-checksums.txt"
download "https://github.com/lima-vm/lima/releases/download/${lima_version}/${lima_asset}" "$temporary/$lima_asset"
verify "$(manifest_checksum "$temporary/lima-checksums.txt" "$lima_asset")" "$temporary/$lima_asset"
if [ "$lima_extension" = "zip" ]; then
  unzip -q "$temporary/$lima_asset" -d "$destination/lima"
else
  tar -xzf "$temporary/$lima_asset" -C "$destination/lima"
fi

lima_runtime_version="$lima_version"
if [ "$goos" = "windows" ] && [ "$lima_version" = "v2.2.0" ]; then
  lima_runtime_version="v2.2.0+porto.2"
  download "https://github.com/lima-vm/lima/archive/refs/tags/v2.2.0.tar.gz" "$temporary/lima-source.tar.gz"
  verify "cdba3804df7d8c00a2af674a3fe0b24c19673a0e846e5f75ac9badf227ce52f5" "$temporary/lima-source.tar.gz"
  # The release archive supplies templates; source-only aliases are not Go
  # build inputs and cannot be extracted reliably by Windows tar.
  tar --exclude='lima-2.2.0/templates/*' \
    --exclude='lima-2.2.0/pkg/limayaml/default.yaml' \
    --exclude='lima-2.2.0/pkg/cidata/cloud-config.yaml' \
    -xzf "$temporary/lima-source.tar.gz" -C "$temporary"
  lima_pid_patch="$script_directory/patches/lima-2.2.0-windows-pid.patch"
  lima_stop_patch="$script_directory/patches/lima-2.2.0-windows-stop.patch"
  (
    cd "$temporary/lima-2.2.0"
    git apply --check "$lima_pid_patch" "$lima_stop_patch"
    git apply "$lima_pid_patch" "$lima_stop_patch"
    CGO_ENABLED=0 GOOS=windows GOARCH="$goarch" GOWORK=off \
      go build -mod=readonly -buildvcs=false -trimpath \
        -ldflags "-s -w -X github.com/lima-vm/lima/v2/pkg/version.Version=$lima_runtime_version" \
        -o "$destination/lima/bin/limactl.exe" ./cmd/limactl
  )
  cp "$temporary/lima-2.2.0/LICENSE" "$destination/licenses/lima.txt"
  cp "$lima_pid_patch" "$destination/licenses/lima-windows-pid.patch"
  cp "$lima_stop_patch" "$destination/licenses/lima-windows-stop.patch"
fi

if [ "$goos/$goarch" = "$(go env GOHOSTOS)/$(go env GOHOSTARCH)" ]; then
  node "$script_directory/lima-runtime-smoke.cjs" "$destination/lima/bin/limactl${binary_suffix}"
fi

qemu_bundled=false
if [ "$goos" = "windows" ]; then
  case "$goarch" in
    amd64)
      qemu_asset="qemu-w64-setup-${qemu_build}.exe"
      qemu_url="https://qemu.weilnetz.de/w64/2026/${qemu_asset}"
      qemu_checksum="f98a8aeb5f7faea9765b6dee28316c266cd179d80354a2fed8e50176f9a2e59f"
      qemu_system="qemu-system-x86_64.exe"
      ;;
    arm64)
      qemu_asset="qemu-arm-setup-${qemu_build}.exe"
      qemu_url="https://qemu.weilnetz.de/aarch64/2026/${qemu_asset}"
      qemu_checksum="58bf65887e4d3af1eef705bdecdcb1fa25dead950fc970c20e6094662afc957f"
      qemu_system="qemu-system-aarch64.exe"
      ;;
  esac
  download "$qemu_url" "$temporary/$qemu_asset"
  verify "$qemu_checksum" "$temporary/$qemu_asset"

  if ! seven_zip="$(
    cd "$(dirname "$0")/../ui/electron"
    node -e 'process.stdout.write(require("7zip-bin-full").path7z)'
  )"; then
    echo "7zip-bin-full is required to bundle QEMU; run npm --prefix ui/electron ci first." >&2
    exit 1
  fi
  if [ ! -f "$seven_zip" ]; then
    echo "7zip-bin-full extractor is missing: $seven_zip" >&2
    exit 1
  fi

  qemu_extracted="$temporary/qemu-extracted"
  mkdir -p "$qemu_extracted"
  if ! "$seven_zip" x -bd -y "-o$qemu_extracted" "$temporary/$qemu_asset" >/dev/null; then
    echo "failed to extract $qemu_asset" >&2
    exit 1
  fi
  qemu_img="$(find "$qemu_extracted" -type f -iname 'qemu-img.exe' -print -quit)"
  if [ -z "$qemu_img" ]; then
    echo "$qemu_asset did not contain qemu-img.exe" >&2
    exit 1
  fi
  qemu_root="$(dirname "$qemu_img")"
  if [ ! -f "$qemu_root/$qemu_system" ]; then
    echo "$qemu_asset did not contain $qemu_system beside qemu-img.exe" >&2
    exit 1
  fi

  mkdir -p "$destination/qemu"
  cp -R "$qemu_root/." "$destination/qemu/"
  find "$destination/qemu" -maxdepth 1 -type f -iname 'qemu-system-*.exe' ! -iname "$qemu_system" -delete
  test -f "$destination/qemu/qemu-img.exe"
  test -f "$destination/qemu/$qemu_system"
  qemu_bundled=true
fi

download "https://raw.githubusercontent.com/kubernetes/kubernetes/${kubectl_version}/LICENSE" "$destination/licenses/kubernetes.txt"
download "https://raw.githubusercontent.com/kubernetes-sigs/kind/${kind_version}/LICENSE" "$destination/licenses/kind.txt"
download "https://raw.githubusercontent.com/derailed/k9s/${k9s_version}/LICENSE" "$destination/licenses/k9s.txt"
if [ "$docker_bundled" = "true" ]; then
  download "https://raw.githubusercontent.com/docker/cli/v${docker_version}/LICENSE" "$destination/licenses/docker-cli.txt"
  download "https://raw.githubusercontent.com/docker/cli/v${docker_version}/NOTICE" "$destination/licenses/docker-cli-NOTICE.txt"
fi
if [ "$qemu_bundled" = "true" ]; then
  download "https://raw.githubusercontent.com/qemu/qemu/v${qemu_version}/COPYING" "$destination/licenses/qemu.txt"
fi

CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" \
  go build -trimpath -ldflags '-s -w' -o "$destination/bin/porto-runtime-helper" ./cmd/porto-runtime-helper

if [ "$goos" != "windows" ]; then
  chmod 0755 "$destination/bin/kubectl"
  chmod 0755 "$destination/bin/porto-runtime-helper"
  [ "$docker_bundled" = "true" ] && chmod 0755 "$destination/bin/docker"
  [ "$kind_bundled" = "true" ] && chmod 0755 "$destination/bin/kind"
  chmod 0755 "$destination/bin/k9s"
  chmod 0755 "$destination/lima/bin/limactl"
fi

cat > "$destination/VERSIONS" <<EOF
kubectl ${kubectl_version}
docker $([ "$docker_bundled" = "true" ] && printf '%s' "$docker_version" || printf 'not available for %s/%s' "$goos" "$goarch")
kind $([ "$kind_bundled" = "true" ] && printf '%s' "$kind_version" || printf 'not available for %s/%s' "$goos" "$goarch")
k9s ${k9s_version}
lima ${lima_runtime_version}
qemu $([ "$qemu_bundled" = "true" ] && printf '%s (Windows build %s)' "$qemu_version" "$qemu_build" || printf 'not bundled for %s/%s' "$goos" "$goarch")
porto-runtime-helper 1
EOF

node "$(dirname "$0")/desktop-runtime-symlinks.cjs" --validate "$destination"
