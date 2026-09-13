#!/usr/bin/env bash

set -euo pipefail
umask 077

if [ "$#" -ne 7 ]; then
  echo "usage: apply-update.sh <darwin|linux> <parent-pid> <package> <destination> <executable> <error-file> <archive-root>" >&2
  exit 2
fi

platform="$1"
parent_pid="$2"
package_path="$3"
destination="$4"
executable_path="$5"
error_file="$6"
archive_root="$7"
log_file="${package_path}.install.log"
candidate="${destination}.porto-update-${parent_pid}"
backup="${destination}.porto-backup-${parent_pid}"
mount_directory=""
mounted=0
extract_directory=""
completed=0

exec >>"$log_file" 2>&1

safe_update_path() {
  case "$1" in
    "${destination}.porto-update-"*|"${destination}.porto-backup-"*) return 0 ;;
    *) return 1 ;;
  esac
}

cleanup() {
  status="$?"
  if [ "$completed" -ne 1 ] && [ "$status" -eq 0 ]; then
    status=1
  fi
  if [ "$mounted" -eq 1 ]; then
    hdiutil detach "$mount_directory" -quiet || true
  fi
  if [ -n "$mount_directory" ] && [ -d "$mount_directory" ]; then
    rmdir "$mount_directory" 2>/dev/null || true
  fi
  if [ -n "$extract_directory" ] && [ -d "$extract_directory" ]; then
    rm -rf "$extract_directory"
  fi
  if safe_update_path "$candidate" && [ -e "$candidate" ]; then
    rm -rf "$candidate"
  fi
  if [ "$completed" -ne 1 ]; then
    if safe_update_path "$backup" && [ -e "$backup" ] && [ ! -e "$destination" ]; then
      mv "$backup" "$destination" || true
    fi
    printf 'Porto could not install the downloaded update. See %s for details.\n' "$log_file" >"$error_file"
    if [ -x "$executable_path" ]; then
      if [ "$platform" = "darwin" ]; then
        open "$destination" || true
      else
        nohup "$executable_path" >/dev/null 2>&1 &
      fi
    fi
  fi
  exit "$status"
}
trap cleanup EXIT HUP INT TERM

case "$platform" in
  darwin)
    case "$destination" in
      /*.app) ;;
      *) echo "Refusing to replace an invalid macOS destination: $destination" >&2; exit 1 ;;
    esac
    ;;
  linux)
    if [ "$destination" = "/" ] || [ "$archive_root" = "" ]; then
      echo "Refusing to replace an invalid Linux destination." >&2
      exit 1
    fi
    ;;
  *)
    echo "Unsupported update platform: $platform" >&2
    exit 1
    ;;
esac

if [ ! -f "$package_path" ]; then
  echo "Downloaded Porto update is missing: $package_path" >&2
  exit 1
fi

for _ in $(seq 1 120); do
  if ! kill -0 "$parent_pid" 2>/dev/null; then
    break
  fi
  sleep 1
done
if kill -0 "$parent_pid" 2>/dev/null; then
  echo "Porto did not exit before the update timeout." >&2
  exit 1
fi

if [ "$platform" = "darwin" ]; then
  mount_directory="$(mktemp -d "$(dirname "$package_path")/porto-update-mount.XXXXXX")"
  hdiutil attach -quiet -nobrowse -readonly -mountpoint "$mount_directory" "$package_path"
  mounted=1
  source_app="$mount_directory/Porto.app"
  if [ ! -x "$source_app/Contents/MacOS/Porto" ]; then
    echo "The downloaded DMG does not contain a valid Porto.app." >&2
    exit 1
  fi
  ditto "$source_app" "$candidate"
  if [ ! -x "$candidate/Contents/MacOS/Porto" ]; then
    echo "The staged Porto.app is incomplete." >&2
    exit 1
  fi
else
  extract_directory="$(mktemp -d "$(dirname "$package_path")/porto-update-extract.XXXXXX")"
  while IFS= read -r entry; do
    case "/$entry/" in
      *"/../"*|*"/./"*) echo "The update archive contains an unsafe path: $entry" >&2; exit 1 ;;
    esac
    case "$entry" in
      /*) echo "The update archive contains an absolute path: $entry" >&2; exit 1 ;;
    esac
  done < <(tar -tzf "$package_path")
  tar -xzf "$package_path" -C "$extract_directory"
  source_app="$extract_directory/$archive_root"
  if [ ! -x "$source_app/Porto" ]; then
    echo "The downloaded archive does not contain a valid Porto application." >&2
    exit 1
  fi
  cp -a "$source_app" "$candidate"
fi

if [ -e "$destination" ]; then
  mv "$destination" "$backup"
fi
if ! mv "$candidate" "$destination"; then
  echo "Unable to activate the downloaded Porto update." >&2
  exit 1
fi
if safe_update_path "$backup" && [ -e "$backup" ]; then
  rm -rf "$backup"
fi
rm -f "$error_file" "$package_path"
completed=1

if [ "$platform" = "darwin" ]; then
  if ! open "$destination"; then
    echo "Porto was updated but could not be relaunched automatically." >&2
  fi
else
  nohup "$destination/Porto" >/dev/null 2>&1 &
fi
