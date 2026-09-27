#!/usr/bin/env bash
set -euo pipefail

# Temporary opt-in companion to macos-power-notification-compat.c; remove together.
if [ "$#" -gt 1 ]; then
  printf 'Usage: bash hacks/open-porto-macos-27.sh [path/to/Porto.app]\n' >&2
  exit 2
fi
if [ "$(uname -s)" != Darwin ]; then
  printf 'The Porto power-notification workaround only supports macOS 27.\n' >&2
  exit 1
fi
case "$(sw_vers -productVersion)" in
  27|27.*) ;;
  *)
    printf 'The Porto power-notification workaround only supports macOS 27.\n' >&2
    exit 1
    ;;
esac

app="${1:-/Applications/Porto.app}"
if [ ! -x "$app/Contents/MacOS/Porto" ]; then
  printf 'Porto executable not found in %s.\n' "$app" >&2
  exit 1
fi
if pgrep -u "$(id -u)" -x Porto >/dev/null; then
  printf 'Quit the running Porto desktop before using the recovery launcher. Its daemon can stay running.\n' >&2
  exit 1
else
  status="$?"
  if [ "$status" -ne 1 ]; then
    printf 'Unable to inspect running Porto processes (pgrep exited %s).\n' "$status" >&2
    exit "$status"
  fi
fi

directory="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
source="$directory/macos-power-notification-compat.c"
digest="$(shasum -a 256 "$source" | awk '{print $1}')"
cache="$HOME/Library/Caches/Porto/macos-power-notification"
library="$cache/$digest.dylib"
case "$library" in
  *:*)
    printf 'The compatibility library path cannot contain a colon.\n' >&2
    exit 1
    ;;
esac
if [ ! -f "$library" ]; then
  mkdir -p "$cache"
  temporary="$(mktemp -d "$cache/build.XXXXXX")"
  cleanup() {
    rm -f "$temporary/guard.dylib"
    rmdir "$temporary"
  }
  trap cleanup EXIT
  xcrun clang -Wall -Wextra -Werror -dynamiclib -arch arm64 -arch x86_64 \
    -framework IOKit -framework CoreFoundation "$source" -o "$temporary/guard.dylib"
  mv "$temporary/guard.dylib" "$library"
fi

printf 'Opening Porto with a macOS-only power-notification guard; no system security settings are changed.\n'
open -a "$app" --env "DYLD_INSERT_LIBRARIES=$library"
