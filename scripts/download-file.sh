#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: download-file.sh <url> <output>" >&2
  exit 2
fi

url="$1"
output="$2"
retries="${PORTO_DOWNLOAD_RETRIES:-10}"
retry_max_time="${PORTO_DOWNLOAD_RETRY_MAX_TIME:-300}"

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
  --output "$output"
