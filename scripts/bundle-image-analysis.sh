#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 3 ]; then
  echo "usage: bundle-image-analysis.sh <goos> <goarch> <runtime-root>" >&2
  exit 2
fi

goos="$1"
goarch="$2"
destination="$3"
script_directory="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
temporary="$(mktemp -d)"
trap 'rm -rf "$temporary"' EXIT

case "$goos/$goarch" in
  darwin/amd64)
    syft_checksum="e2e5831cbabaec9a36fe65c34755c1113982c842a685edce3ce0b54a0ded8003"
    grype_checksum="27eef17e42f0340af6582cdbc33ad18faf9f601c613ab81d2e1658a94cbc357f" ;;
  darwin/arm64)
    syft_checksum="1217c258bd5eb91ef3189cf22a82e02f9cac07e4ce934350f428e93aff674625"
    grype_checksum="7be39d3a0428c664ce0183e4aba31ed9accffce9eda7e7f6fa84e9c512285f27" ;;
  linux/amd64)
    syft_checksum="bb65249542db5fedeeb4d3f9b018b75e7264dbfbbb10486f044581a92f90490d"
    grype_checksum="64e07133d3486115de647a66581461dccf5c732748a0a3119077395565f15519" ;;
  linux/arm64)
    syft_checksum="929bd163db2d7cd2255d3bcb8967208079813d7b46b0c952f7b794d8e2c7d304"
    grype_checksum="86f570f75ca947ea3dd6486309232bcff6d78e44ba081ae75ab14eab72be3dae" ;;
  windows/amd64)
    syft_checksum="ada4c9b93af575f479ce80fce6629897272a2d41f300a14bc49bace020767ca2"
    grype_checksum="f867bbdc14bd8b0d99b3fc47d839af07480a540072b16896373a8ba32b76f588" ;;
  windows/arm64)
    syft_checksum="f52f7796e41e4d1ae511c228c99239fd09887077336dcb7a8c2aa7046ed7c816"
    grype_checksum="" ;;
  *) echo "unsupported image-analysis target: $goos/$goarch" >&2; exit 2 ;;
esac

mkdir -p "$destination/bin" "$destination/licenses"
: > "$destination/licenses/image-analysis-VERSIONS.txt"
bundle() {
  local tool="$1" version="$2" checksum="$3" extension="tar.gz" suffix=""
  if [ "$goos" = "windows" ]; then extension="zip"; suffix=".exe"; fi
  local asset="${tool}_${version}_${goos}_${goarch}.${extension}"
  bash "$script_directory/download-file.sh" \
    "https://github.com/anchore/${tool}/releases/download/v${version}/${asset}" "$temporary/$asset" "$checksum"
  node "$script_directory/verify-runtime-checksum.cjs" --verify "$checksum" "$temporary/$asset"
  mkdir "$temporary/$tool"
  if [ "$extension" = "zip" ]; then
    unzip -q "$temporary/$asset" "${tool}${suffix}" LICENSE -d "$temporary/$tool"
  else
    tar -xzf "$temporary/$asset" -C "$temporary/$tool" "$tool" LICENSE
  fi
  mv "$temporary/$tool/${tool}${suffix}" "$destination/bin/${tool}${suffix}"
  cp "$temporary/$tool/LICENSE" "$destination/licenses/${tool}.txt"
  if [ "$goos" != "windows" ]; then chmod 0755 "$destination/bin/$tool"; fi
  printf '%s v%s (%s, sha256:%s)\n' "$tool" "$version" "$asset" "$checksum" >> "$destination/licenses/image-analysis-VERSIONS.txt"
}

bundle syft 1.55.0 "$syft_checksum"
if [ -n "$grype_checksum" ]; then
  bundle grype 0.121.0 "$grype_checksum"
else
  printf '%s\n' 'grype not available for windows/arm64: upstream does not publish a native binary; no substituted x64 executable' >> "$destination/licenses/image-analysis-VERSIONS.txt"
fi
