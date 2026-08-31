#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 3 ]; then
  echo "usage: bundle-desktop-runtime.sh <goos> <goarch> <destination>" >&2
  exit 2
fi

goos="$1"
goarch="$2"
destination="$3"

kubectl_version="${KUBECTL_VERSION:-v1.36.1}"
kind_version="${KIND_VERSION:-v0.33.0}"
k9s_version="${K9S_VERSION:-v0.50.18}"
lima_version="${LIMA_VERSION:-v2.2.0}"

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

download "https://raw.githubusercontent.com/kubernetes/kubernetes/${kubectl_version}/LICENSE" "$destination/licenses/kubernetes.txt"
download "https://raw.githubusercontent.com/kubernetes-sigs/kind/${kind_version}/LICENSE" "$destination/licenses/kind.txt"
download "https://raw.githubusercontent.com/derailed/k9s/${k9s_version}/LICENSE" "$destination/licenses/k9s.txt"

if [ "$goos" != "windows" ]; then
  chmod 0755 "$destination/bin/kubectl"
  [ "$kind_bundled" = "true" ] && chmod 0755 "$destination/bin/kind"
  chmod 0755 "$destination/bin/k9s"
  chmod 0755 "$destination/lima/bin/limactl"
fi

cat > "$destination/VERSIONS" <<EOF
kubectl ${kubectl_version}
kind $([ "$kind_bundled" = "true" ] && printf '%s' "$kind_version" || printf 'not available for %s/%s' "$goos" "$goarch")
k9s ${k9s_version}
lima ${lima_version}
EOF

node "$(dirname "$0")/desktop-runtime-symlinks.cjs" --validate "$destination"
