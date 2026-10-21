#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 2 ] && [ "$#" -ne 3 ]; then
  echo "usage: download-file.sh <url> <output> [sha256]" >&2
  exit 2
fi

url="$1"
output="$2"
checksum="${3:-}"
retries="${PORTO_DOWNLOAD_RETRIES:-10}"
retry_max_time="${PORTO_DOWNLOAD_RETRY_MAX_TIME:-300}"
script_directory="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
download_output="$output"
cache_file=""

if [ -n "$checksum" ]; then
  if [[ ! "$checksum" =~ ^[[:xdigit:]]{64}$ ]]; then
    echo "Invalid expected SHA-256 checksum." >&2
    exit 2
  fi
  checksum="$(printf '%s' "$checksum" | tr '[:upper:]' '[:lower:]')"
  if [ -n "${PORTO_DOWNLOAD_CACHE:-}" ]; then
    mkdir -p "$PORTO_DOWNLOAD_CACHE"
    cache_file="$PORTO_DOWNLOAD_CACHE/$checksum"
    if [ -f "$cache_file" ]; then
      node "$script_directory/verify-runtime-checksum.cjs" --verify "$checksum" "$cache_file"
      printf 'Using checksum-verified download cache for %s\n' "${url%%\?*}" >&2
      cp "$cache_file" "$output"
      exit 0
    fi
    download_output="$(mktemp "$PORTO_DOWNLOAD_CACHE/.download.XXXXXX")"
    trap 'rm -f "$download_output"' EXIT
    trap 'exit 130' INT
    trap 'exit 143' HUP TERM
  fi
fi

printf 'Downloading %s\n' "${url%%\?*}" >&2
curl \
  --fail \
  --location \
  --retry "$retries" \
  --retry-all-errors \
  --retry-max-time "$retry_max_time" \
  --connect-timeout 30 \
  --silent \
  --show-error \
  "$url" \
  --output "$download_output"

if [ -n "$checksum" ]; then
  node "$script_directory/verify-runtime-checksum.cjs" --verify "$checksum" "$download_output"
fi
if [ -n "$cache_file" ]; then
  cp "$download_output" "$output"
  mv "$download_output" "$cache_file"
fi
