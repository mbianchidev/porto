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
docker_git_commit="a7dcaa6fdb6ed04aacbfdc76357fdae01605609e"
docker_source_checksum="225b7ab2a15f5230b482df8461069cd4bce38891266fb9898d4188d0a3cbf54a"
compose_version="v5.5.1"
buildx_version="v0.37.1"
dive_version="v0.13.1"
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
mkdir -p "$destination/bin" "$destination/docker/cli-plugins" "$destination/lima" "$destination/licenses"
destination="$(cd "$destination" && pwd -P)"

download() {
  local url="$1"
  local output="$2"
  bash "$script_directory/download-file.sh" "$url" "$output"
}

verify() {
  local expected
  expected="$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]' | tr -d '[:space:]')"
  local file="$2"
  node "$script_directory/verify-runtime-checksum.cjs" --verify "$expected" "$file"
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

docker_source_asset="docker-cli-v${docker_version}.tar.gz"
download \
  "https://github.com/docker/cli/archive/refs/tags/v${docker_version}.tar.gz" \
  "$temporary/$docker_source_asset"
verify "$docker_source_checksum" "$temporary/$docker_source_asset"
tar -xzf "$temporary/$docker_source_asset" -C "$temporary"
docker_source_directory="$temporary/cli-${docker_version}"
docker_plugin_patch="$script_directory/patches/docker-cli-29.7.2-bundled-plugins.patch"
(
  cd "$docker_source_directory"
  git apply --check "$docker_plugin_patch"
  git apply "$docker_plugin_patch"
  cp vendor.mod go.mod
  cp vendor.sum go.sum
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" GOWORK=off GO111MODULE=on \
    go build -mod=vendor -buildvcs=false -trimpath \
      -ldflags "-s -w -X github.com/docker/cli/cli/version.PlatformName=Porto -X github.com/docker/cli/cli/version.Version=${docker_version} -X github.com/docker/cli/cli/version.GitCommit=${docker_git_commit} -X github.com/docker/cli/cli/version.BuildTime=2026-08-05T17:34:15Z" \
      -o "$destination/bin/docker${binary_suffix}" ./cmd/docker
)
docker_bundled=true
docker_asset="${docker_source_asset}+porto-bundled-plugins.patch"

docker_plugins_bundled=false
compose_checksum=""
buildx_checksum=""
if [ "$docker_bundled" = "true" ]; then
  case "$goos/$goarch" in
    darwin/arm64)
      compose_arch="aarch64"
      compose_checksum="998735c9b6fe68a4f05895e6ea73d71ad06f9fc7046383ad89e47346781b6af5"
      buildx_checksum="c3cbbc820d578b0aa8158dd62ef1af25a0c8a75ef53331dbe4e219471e1dbe8c"
      ;;
    darwin/amd64)
      compose_arch="x86_64"
      compose_checksum="a264d61e824bf08a78867e59cdf32eb09f0aee9ecdf9f6ebfa43f76dc52880f1"
      buildx_checksum="7003a7bae20e7741283db1e23dafdcb957776a8be85de3f459630b1dd4c19db0"
      ;;
    linux/arm64)
      compose_arch="aarch64"
      compose_checksum="732e3a84c1a0f67256ce80bc2598a24546b10ca05f9faa97efceb1171ece2ef7"
      buildx_checksum="e5cc9fe3bbff5cbc91230981f7860e06076110730a2db997082652199042a1f2"
      ;;
    linux/amd64)
      compose_arch="x86_64"
      compose_checksum="db1889184726840f75c4f9c001048430d4f25b3be3cb084d3ddd762bc0aed576"
      buildx_checksum="9447199cdb435f25880548343c128a4b6650e8891ee598905d8d29d39a8e359b"
      ;;
    windows/amd64)
      compose_arch="x86_64"
      compose_checksum="a3c0c73033eaede90210345d0cc2233edf4fab8fe0282a91dad8fd8436809d2f"
      buildx_checksum="3904abb2802f9bd83a2bf483b35bba81c57a4e0baff981e6886564c461f908b3"
      ;;
    windows/arm64)
      compose_arch="aarch64"
      compose_checksum="4bbb5d1ecc75bde1a9ca4afac43f5907c0d3bd0f88c7f00bf481ee7c8c1737be"
      buildx_checksum="bdf356a5f2c8f3efd8a357b1f044860216e15e654a9a7105ca4e5dd5b5697ee4"
      ;;
  esac
  compose_asset="docker-compose-${goos}-${compose_arch}${binary_suffix}"
  buildx_asset="buildx-${buildx_version}.${goos}-${goarch}${binary_suffix}"
  download \
    "https://github.com/docker/compose/releases/download/${compose_version}/${compose_asset}" \
    "$temporary/$compose_asset"
  download \
    "https://github.com/docker/buildx/releases/download/${buildx_version}/${buildx_asset}" \
    "$temporary/$buildx_asset"
  verify "$compose_checksum" "$temporary/$compose_asset"
  verify "$buildx_checksum" "$temporary/$buildx_asset"
  mv "$temporary/$compose_asset" "$destination/docker/cli-plugins/docker-compose${binary_suffix}"
  mv "$temporary/$buildx_asset" "$destination/docker/cli-plugins/docker-buildx${binary_suffix}"
  docker_plugins_bundled=true
fi

case "$goos/$goarch" in
  darwin/amd64) dive_checksum="04e4c1bac21be3aef99799cf0e470149a072ea4786be50718aa846cd13746523" ;;
  darwin/arm64) dive_checksum="38b7fa95a13e7f4d0b3060c875fe7427c2a0613ecff674bb45156eb34bca0b09" ;;
  linux/amd64) dive_checksum="0970549eb4a306f8825a84145a2534153badb4d7dcf3febd1967c706367c3d0e" ;;
  linux/arm64) dive_checksum="2fcd2cf20f634ccdb41efac44048b204bfc867c115641f37a7420693ed480a18" ;;
  windows/amd64) dive_checksum="3e764ff28c7b89f4da679deac80483249fbbae3a1d512c103d54609eec09086a" ;;
  windows/arm64) dive_checksum="de298f2edeffeac3e4c715eb516eab8075b50a7c8fce28194f730f2a384d4dc9" ;;
esac
dive_extension="tar.gz"
if [ "$goos" = "windows" ]; then
  dive_extension="zip"
fi
dive_asset="dive_${dive_version#v}_${goos}_${goarch}.${dive_extension}"
download "https://github.com/wagoodman/dive/releases/download/${dive_version}/${dive_asset}" "$temporary/$dive_asset"
verify "$dive_checksum" "$temporary/$dive_asset"
mkdir -p "$temporary/dive"
if [ "$dive_extension" = "zip" ]; then
  unzip -q "$temporary/$dive_asset" "dive.exe" "LICENSE" -d "$temporary/dive"
else
  tar -xzf "$temporary/$dive_asset" -C "$temporary/dive" dive LICENSE
fi
mv "$temporary/dive/dive${binary_suffix}" "$destination/bin/dive${binary_suffix}"
cp "$temporary/dive/LICENSE" "$destination/licenses/dive.txt"

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
  lima_runtime_version="v2.2.0+porto.3"
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
  cp "$docker_source_directory/LICENSE" "$destination/licenses/docker-cli.txt"
  cp "$docker_source_directory/NOTICE" "$destination/licenses/docker-cli-NOTICE.txt"
  cp "$docker_plugin_patch" "$destination/licenses/docker-cli-bundled-plugins.patch"
fi
if [ "$docker_plugins_bundled" = "true" ]; then
  download "https://raw.githubusercontent.com/docker/compose/${compose_version}/LICENSE" "$destination/licenses/docker-compose.txt"
  download "https://raw.githubusercontent.com/docker/compose/${compose_version}/NOTICE" "$destination/licenses/docker-compose-NOTICE.txt"
  download "https://raw.githubusercontent.com/docker/buildx/${buildx_version}/LICENSE" "$destination/licenses/docker-buildx.txt"
  download "https://raw.githubusercontent.com/docker/buildx/${buildx_version}/AUTHORS" "$destination/licenses/docker-buildx-AUTHORS.txt"
fi
if [ "$qemu_bundled" = "true" ]; then
  download "https://raw.githubusercontent.com/qemu/qemu/v${qemu_version}/COPYING" "$destination/licenses/qemu.txt"
fi

CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" \
  go build -trimpath -ldflags '-s -w' -o "$destination/bin/porto-runtime-helper" ./cmd/porto-runtime-helper

if [ "$goos" != "windows" ]; then
  chmod 0755 "$destination/bin/kubectl"
  chmod 0755 "$destination/bin/porto-runtime-helper"
  chmod 0755 "$destination/bin/dive"
  if [ "$docker_bundled" = "true" ]; then
    chmod 0755 "$destination/bin/docker"
    chmod 0755 "$destination/docker/cli-plugins/docker-compose"
    chmod 0755 "$destination/docker/cli-plugins/docker-buildx"
  fi
  [ "$kind_bundled" = "true" ] && chmod 0755 "$destination/bin/kind"
  chmod 0755 "$destination/bin/k9s"
  chmod 0755 "$destination/lima/bin/limactl"
fi

cat > "$destination/VERSIONS" <<EOF
kubectl ${kubectl_version}
docker $([ "$docker_bundled" = "true" ] && printf '%s (%s, sha256:%s, commit %s)' "$docker_version" "$docker_asset" "$docker_source_checksum" "$docker_git_commit" || printf 'not available for %s/%s' "$goos" "$goarch")
docker-compose $([ "$docker_plugins_bundled" = "true" ] && printf '%s (%s, sha256:%s)' "$compose_version" "$compose_asset" "$compose_checksum" || printf 'not available because Docker CLI is not bundled for %s/%s' "$goos" "$goarch")
docker-buildx $([ "$docker_plugins_bundled" = "true" ] && printf '%s (%s, sha256:%s)' "$buildx_version" "$buildx_asset" "$buildx_checksum" || printf 'not available because Docker CLI is not bundled for %s/%s' "$goos" "$goarch")
dive ${dive_version} (${dive_asset}, sha256:${dive_checksum})
kind $([ "$kind_bundled" = "true" ] && printf '%s' "$kind_version" || printf 'not available for %s/%s' "$goos" "$goarch")
k9s ${k9s_version}
lima ${lima_runtime_version}
qemu $([ "$qemu_bundled" = "true" ] && printf '%s (Windows build %s)' "$qemu_version" "$qemu_build" || printf 'not bundled for %s/%s' "$goos" "$goarch")
porto-runtime-helper 1
EOF

if [ "$docker_bundled" = "true" ]; then
  test -f "$destination/bin/docker${binary_suffix}"
  test -f "$destination/docker/cli-plugins/docker-compose${binary_suffix}"
  test -f "$destination/docker/cli-plugins/docker-buildx${binary_suffix}"
  test -f "$destination/licenses/docker-cli-bundled-plugins.patch"
  test -f "$destination/licenses/docker-compose.txt"
  test -f "$destination/licenses/docker-compose-NOTICE.txt"
  test -f "$destination/licenses/docker-buildx.txt"
  test -f "$destination/licenses/docker-buildx-AUTHORS.txt"
else
  test ! -e "$destination/bin/docker${binary_suffix}"
  test ! -e "$destination/docker/cli-plugins/docker-compose${binary_suffix}"
  test ! -e "$destination/docker/cli-plugins/docker-buildx${binary_suffix}"
fi
test -f "$destination/bin/dive${binary_suffix}"
test -f "$destination/licenses/dive.txt"

node "$(dirname "$0")/desktop-runtime-symlinks.cjs" --validate "$destination"
if [ "$goos/$goarch" = "$(go env GOHOSTOS)/$(go env GOHOSTARCH)" ]; then
  node "$script_directory/docker-toolchain-smoke.cjs" "$destination"
fi
