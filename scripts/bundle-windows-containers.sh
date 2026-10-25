#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 2 ] || [ "$1" != amd64 ]; then
  echo "usage: bundle-windows-containers.sh amd64 <destination>; no native ARM64 runtime substitution is supported" >&2
  exit 2
fi

destination="$2"
script_directory="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
containerd_version="2.4.2"
containerd_checksum="02cbbecd0ef78a521d124afa791b843ba5458ef206e1f7f017999ff45c80c0b4"
nerdctl_version="2.4.1"
nerdctl_checksum="ddae9bac35fe8ad35a631d64a179d17589bed3055756f94ef59cf2e0b2a95ee7"
hcsshim_version="0.15.0-rc.4"
hcsshim_checksum="8428476775ee95f469a5049144e90e02f9fb340e49b73088bc29e6958b5136a0"
cni_version="0.3.3"
cni_checksum="a9d50637b5361cb9cd3374a6f7f024dad5f09e47b2a6dac8ed99da523c8cc0b6"
temporary="$(mktemp -d)"
trap 'rm -rf -- "$temporary"' EXIT
mkdir -p "$destination/bin" "$destination/licenses"
destination="$(cd "$destination" && pwd -P)"

compiler="$(go env GOVERSION)"
node -e 'const match=/^go(\d+)\.(\d+)\.(\d+)$/.exec(process.argv[1]); if(!match || Number(match[1])<1 || (Number(match[1])===1 && (Number(match[2])<27 || (Number(match[2])===27 && Number(match[3])<3)))) throw new Error("Patched Go 1.27.3 or newer is required for native Windows runtime builds")' "$compiler"
export GOTOOLCHAIN="$compiler"

download() {
  bash "$script_directory/download-file.sh" "$1" "$2" "$3"
  node "$script_directory/verify-runtime-checksum.cjs" --verify "$3" "$2"
}

download "https://github.com/containerd/containerd/archive/refs/tags/v${containerd_version}.tar.gz" \
  "$temporary/containerd.tar.gz" "$containerd_checksum"
download "https://github.com/containerd/nerdctl/archive/refs/tags/v${nerdctl_version}.tar.gz" \
  "$temporary/nerdctl.tar.gz" "$nerdctl_checksum"
download "https://github.com/microsoft/hcsshim/archive/refs/tags/v${hcsshim_version}.tar.gz" \
  "$temporary/hcsshim.tar.gz" "$hcsshim_checksum"
download "https://github.com/microsoft/windows-container-networking/archive/refs/tags/v${cni_version}.tar.gz" \
  "$temporary/cni.tar.gz" "$cni_checksum"
for name in containerd nerdctl hcsshim cni; do
  tar -xzf "$temporary/$name.tar.gz" -C "$temporary"
done

build_source() {
  local directory="$1"
  local output="$2"
  local package="$3"
  local flags="$4"
  (
    cd "$directory"
    # Upstream archives contain old vendored crypto/network packages. Keep the
    # source tag while building against patched official dependencies.
    GOWORK=off go mod edit -require=golang.org/x/crypto@v0.58.0 \
      -require=golang.org/x/net@v0.60.0 -require=golang.org/x/sys@v0.49.0 \
      -require=github.com/sirupsen/logrus@v1.10.2 -require=google.golang.org/grpc@v1.83.2 \
      -dropreplace=golang.org/x/sys
    imports="$(CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOWORK=off go list -mod=mod -deps -f '{{.ImportPath}}' "$package")"
    if printf '%s\n' "$imports" | grep -E '^golang\.org/x/crypto/openpgp($|/)' >&2; then
      echo "GO-2026-5932: a native runtime source imports deprecated OpenPGP" >&2
      exit 1
    fi
    CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOWORK=off \
      go build -mod=mod -buildvcs=false -trimpath -ldflags "-s -w $flags" -o "$output" "$package"
    binary_name="$(basename "$output" .exe)"
    printf '%s\n' "$imports" > "$destination/licenses/$binary_name.imports.txt"
    go version -m "$output" | tail -n +2 > "$destination/licenses/$binary_name.buildinfo.txt"
  )
}

build_source "$temporary/containerd-${containerd_version}" "$destination/bin/containerd.exe" ./cmd/containerd \
  "-X github.com/containerd/containerd/v2/version.Version=v${containerd_version}+porto.1"
build_source "$temporary/hcsshim-${hcsshim_version}" "$destination/bin/containerd-shim-runhcs-v1.exe" ./cmd/containerd-shim-runhcs-v1 ""
build_source "$temporary/nerdctl-${nerdctl_version}" "$destination/bin/nerdctl.exe" ./cmd/nerdctl \
  "-X github.com/containerd/nerdctl/v2/pkg/version.Version=v${nerdctl_version}+porto.1"
for name in nat sdnbridge sdnoverlay; do
  build_source "$temporary/windows-container-networking-${cni_version}" "$destination/bin/$name.exe" "./plugins/$name" \
    "-X main.version=v${cni_version}+porto.1"
done
cp "$temporary/containerd-${containerd_version}/LICENSE" "$destination/licenses/containerd.txt"
cp "$temporary/nerdctl-${nerdctl_version}/LICENSE" "$destination/licenses/nerdctl.txt"
cp "$temporary/hcsshim-${hcsshim_version}/LICENSE" "$destination/licenses/hcsshim.txt"
cp "$temporary/windows-container-networking-${cni_version}/LICENSE" "$destination/licenses/windows-cni.txt"

cat > "$destination/VERSIONS" <<EOF
windows-containerd v${containerd_version}+porto.1 (source sha256:${containerd_checksum})
windows-runhcs v${hcsshim_version} (source sha256:${hcsshim_checksum})
windows-nerdctl v${nerdctl_version}+porto.1 (source sha256:${nerdctl_checksum})
windows-cni v${cni_version}+porto.1 (source sha256:${cni_checksum})
windows-runtime-compiler ${compiler}
windows-runtime-patched-dependencies x/crypto v0.58.0, x/net v0.60.0, x/sys v0.49.0, logrus v1.10.2, grpc v1.83.2; no stale x/sys replacement
windows-runtime-import-policy exact production import graphs and resolved build information in licenses/
EOF
node "$script_directory/windows-container-toolchain.cjs" "$destination/bin" amd64
