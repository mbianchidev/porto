#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 3 ]; then
  echo "usage: bundle-helm.sh <goos> <goarch> <runtime-root>" >&2
  exit 2
fi

goos="$1"
goarch="$2"
destination="$3"
script_directory="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
version="4.3.1"
case "$goos/$goarch" in
  darwin/amd64) checksum="34bec4eba761ba5c16d1965ee6b8f5b0abe5993974a802274e2a4f0418893d89" ;;
  darwin/arm64) checksum="6b4e61c8cd27d99e5a801e63ff25dd5c70b36875c2cd659d3e9ce6b76fc23198" ;;
  linux/amd64) checksum="0403a88ae20913d11b10a2467b50087721fea7581736da6b9d3992f5357fe54b" ;;
  linux/arm64) checksum="8cf8f66ecbc252ac8f7f06cca68aba88508ddfde3a6a34feb1678ad1694b4f88" ;;
  windows/amd64) checksum="fcfd7b796dcf0f225abc40b5636a3b01f55f3de2fb1dfb18475df94e62c14112" ;;
  windows/arm64) checksum="373edaae86db70342ef39b014e2e7764ea8f9cff08714843daa5e55b5ee492f2" ;;
  *) echo "unsupported Helm target: $goos/$goarch" >&2; exit 2 ;;
esac
temporary="$(mktemp -d)"
trap 'rm -rf "$temporary"' EXIT
extension="tar.gz"
suffix=""
if [ "$goos" = "windows" ]; then extension="zip"; suffix=".exe"; fi
asset="helm-v${version}-${goos}-${goarch}.${extension}"
bash "$script_directory/download-file.sh" "https://get.helm.sh/$asset" "$temporary/$asset" "$checksum"
node "$script_directory/verify-runtime-checksum.cjs" --verify "$checksum" "$temporary/$asset"
if [ "$extension" = "zip" ]; then
  unzip -q "$temporary/$asset" -d "$temporary"
else
  tar -xzf "$temporary/$asset" -C "$temporary"
fi
mkdir -p "$destination/bin" "$destination/licenses"
mv "$temporary/$goos-$goarch/helm$suffix" "$destination/bin/helm$suffix"
cp "$temporary/$goos-$goarch/LICENSE" "$destination/licenses/helm.txt"
if [ "$goos" != "windows" ]; then chmod 0755 "$destination/bin/helm"; fi
printf 'helm v%s (%s, sha256:%s)\n' "$version" "$asset" "$checksum" > "$destination/licenses/helm-VERSIONS.txt"
