#!/bin/sh
set -eu

repository="${PORTO_REPOSITORY:-mbianchidev/porto}"
api_url="${PORTO_RELEASE_API_URL:-https://api.github.com/repos/${repository}/releases/latest}"
tag="${PORTO_VERSION:-}"

if [ -z "$tag" ]; then
  tag="$(curl -fsSL "$api_url" | sed -n 's/^[[:space:]]*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
fi
case "$tag" in
  v*) ;;
  *)
    echo "Unable to resolve a Porto release tag." >&2
    exit 1
    ;;
esac

case "$(uname -s)" in
  Darwin) goos="darwin"; extension="dmg" ;;
  Linux) goos="linux"; extension="tar.gz" ;;
  *)
    echo "This installer supports macOS and Linux. Use install-desktop.ps1 on Windows." >&2
    exit 1
    ;;
esac

case "$(uname -m)" in
  x86_64|amd64) goarch="amd64" ;;
  arm64|aarch64) goarch="arm64" ;;
  *)
    echo "Unsupported architecture: $(uname -m)" >&2
    exit 1
    ;;
esac

version="${tag#v}"
asset="porto-desktop_${version}_${goos}_${goarch}.${extension}"
base_url="${PORTO_RELEASE_BASE_URL:-https://github.com/${repository}/releases/download/${tag}}"
temporary="$(mktemp -d)"
mounted_volume=""
cleanup() {
  if [ -n "$mounted_volume" ]; then
    hdiutil detach "$mounted_volume" -quiet >/dev/null 2>&1 || true
  fi
  rm -rf "$temporary"
}
trap cleanup EXIT HUP INT TERM

curl -fL --retry 4 --retry-all-errors -o "$temporary/$asset" "$base_url/$asset"
curl -fL --retry 4 --retry-all-errors -o "$temporary/SHA256SUMS" "$base_url/SHA256SUMS"
expected="$(awk -v asset="$asset" '$2 == asset || $2 == "*" asset { print $1; exit }' "$temporary/SHA256SUMS")"
if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "$temporary/$asset" | awk '{print $1}')"
else
  actual="$(shasum -a 256 "$temporary/$asset" | awk '{print $1}')"
fi
if [ -z "$expected" ] || [ "$expected" != "$actual" ]; then
  echo "Checksum verification failed for $asset." >&2
  exit 1
fi

if [ "$goos" = "darwin" ]; then
  mounted_volume="$temporary/Porto"
  mkdir -p "$mounted_volume"
  hdiutil attach -quiet -nobrowse -readonly -mountpoint "$mounted_volume" "$temporary/$asset"
  source_app="$mounted_volume/Porto.app"
  if [ ! -d "$source_app" ]; then
    echo "The Porto DMG does not contain Porto.app." >&2
    exit 1
  fi
else
  tar -xzf "$temporary/$asset" -C "$temporary"
  source_dir="$temporary/porto-desktop_${version}_${goos}_${goarch}"
  if [ ! -d "$source_dir" ]; then
    echo "The Porto desktop archive has an unexpected layout." >&2
    exit 1
  fi
fi

bin_dir="${PORTO_BIN_DIR:-$HOME/.local/bin}"
mkdir -p "$bin_dir"

install_optional_link() {
  target="$1"
  link="$2"
  name="$3"
  [ -x "$target" ] || return 0
  if [ -e "$link" ] && [ ! -L "$link" ]; then
    echo "Preserving existing $name at $link; use the Porto CLI for its bundled tool." >&2
    return 0
  fi
  if [ -L "$link" ]; then
    existing="$(readlink "$link" 2>/dev/null || true)"
    if [ "$existing" != "$target" ]; then
      case "$existing" in
        */Porto.app/Contents/Resources/runtime/bin/docker|*/porto/resources/runtime/bin/docker)
          ;;
        *)
          echo "Preserving existing $name symlink at $link; use the Porto CLI for its bundled tool." >&2
          return 0
          ;;
      esac
    fi
  fi
  ln -sfn "$target" "$link"
}

install_bundled_tool_link() {
  command_name="$1"
  target="$2"
  display_name="$3"
  link="$bin_dir/$command_name"
  existing_command="$(command -v "$command_name" 2>/dev/null || true)"
  if [ -n "$existing_command" ] && [ "$existing_command" != "$link" ]; then
    echo "Preserving existing $display_name at $existing_command; use the Porto CLI for its bundled tool." >&2
    return 0
  fi
  install_optional_link "$target" "$link" "$display_name"
}

install_bundled_docker_link() {
  target="$1"
  install_bundled_tool_link "docker" "$target" "Docker CLI"
}

run_as_root() {
  if [ "$(id -u)" -eq 0 ]; then
    "$@"
  elif command -v sudo >/dev/null 2>&1; then
    sudo "$@"
  else
    return 1
  fi
}

verify_linux_qemu() {
  for qemu_executable in "$qemu_command" qemu-img; do
    if ! command -v "$qemu_executable" >/dev/null 2>&1 || ! "$qemu_executable" --version >/dev/null; then
      echo "Porto setup incomplete: $qemu_executable is missing or cannot execute. QEMU is required for the managed container engine and virtual machines. Repair it and retry; existing Porto files have not been replaced." >&2
      exit 1
    fi
  done
}

install_linux_prerequisites() {
  if [ "${PORTO_SKIP_PREREQS:-0}" = "1" ]; then
    echo "Linux runtime and desktop sandbox prerequisite checks explicitly skipped with PORTO_SKIP_PREREQS=1. Readiness has not been verified." >&2
    return
  fi

  case "$goarch" in
    amd64) qemu_command="qemu-system-x86_64" ;;
    arm64) qemu_command="qemu-system-aarch64" ;;
  esac
  if command -v "$qemu_command" >/dev/null 2>&1; then
    verify_linux_qemu
    return
  fi

  echo "Installing QEMU for Porto's managed container engine and virtual machines."
  if command -v apt-get >/dev/null 2>&1; then
    if run_as_root apt-get update && run_as_root apt-get install -y qemu-system qemu-utils; then
      verify_linux_qemu
      return
    fi
  elif command -v dnf >/dev/null 2>&1; then
    if run_as_root dnf install -y qemu-kvm qemu-img; then
      verify_linux_qemu
      return
    fi
  elif command -v pacman >/dev/null 2>&1; then
    if run_as_root pacman -S --needed qemu-desktop; then
      verify_linux_qemu
      return
    fi
  elif command -v zypper >/dev/null 2>&1; then
    if run_as_root zypper --non-interactive install qemu; then
      verify_linux_qemu
      return
    fi
  fi
  echo "Porto setup incomplete: QEMU is required for the managed container engine and virtual machines. Install $qemu_command and qemu-img, then retry. Existing Porto files have not been replaced." >&2
  exit 1
}

configure_linux_sandbox() {
  [ "${PORTO_SKIP_PREREQS:-0}" != "1" ] || return 0
  sandbox_key="kernel.apparmor_restrict_unprivileged_userns"
  if ! command -v sysctl >/dev/null 2>&1; then
    if [ ! -e /proc/sys/kernel/apparmor_restrict_unprivileged_userns ]; then return 0; fi
    echo "Porto setup incomplete: install sysctl to inspect the Linux desktop sandbox policy. Existing Porto files have not been replaced." >&2
    exit 1
  fi
  if ! sandbox_restriction="$(sysctl -n "$sandbox_key" 2>"$temporary/sandbox-probe.log")"; then
    if [ ! -e /proc/sys/kernel/apparmor_restrict_unprivileged_userns ]; then return 0; fi
    cat "$temporary/sandbox-probe.log" >&2
    echo "Porto setup incomplete: unable to inspect the Linux desktop sandbox policy. Existing Porto files have not been replaced." >&2
    exit 1
  fi
  case "$sandbox_restriction" in
    0) return 0 ;;
    1) ;;
    *)
      echo "Porto setup incomplete: unrecognized Linux desktop sandbox policy. Existing Porto files have not been replaced." >&2
      exit 1
      ;;
  esac
  mkdir -p "$(dirname "$install_root")"
  sandbox_parent="$(cd -P "$(dirname "$install_root")" && pwd -P)"
  sandbox_executable="$sandbox_parent/$(basename "$install_root")/Porto"
  echo "Configuring Porto's Chromium user-namespace sandbox; administrator approval may be required."
  if ! run_as_root /bin/sh -s -- "$sandbox_executable" "$(id -u)" <<'PORTO_APPARMOR_SETUP'
set -eu
PATH=/usr/sbin:/usr/bin:/sbin:/bin
export PATH
if ! command -v apparmor_parser >/dev/null 2>&1; then
  if command -v apt-get >/dev/null 2>&1; then
    apt-get update && apt-get install -y apparmor
  else
    echo "Install the distribution's apparmor_parser utility, then retry Porto setup." >&2
    exit 1
  fi
fi

install_linux_sandbox_profile() {
  profile_directory="$1"
  profile_executable="$2"
  profile_uid="$3"
  case "$profile_executable" in
    /*) ;;
    *) echo "The Porto sandbox needs an absolute executable path." >&2; return 1 ;;
  esac
  if [ "$(printf '%s' "$profile_executable" | LC_ALL=C tr -d '\000-\037\177')" != "$profile_executable" ]; then
    echo "The Porto installation path contains unsupported AppArmor control characters." >&2
    return 1
  fi
  case "$profile_executable" in
    *'"'*|*'\'*|*'*'*|*'?'*|*'['*|*']'*|*'{'*|*'}'*)
      echo "The Porto installation path contains unsupported AppArmor policy characters; choose a literal path without control characters or pattern syntax." >&2
      return 1
      ;;
  esac
  case "$profile_uid" in
    ''|*[!0-9]*) echo "The Porto sandbox requires a numeric account ID." >&2; return 1 ;;
  esac
  profile_hash="$(printf '%s' "$profile_executable" | sha256sum | awk '{print $1}')"
  profile_name="porto-sandbox-$profile_uid-$profile_hash"
  profile_path="$profile_directory/$profile_name"
  profile_temporary="$(mktemp "$profile_directory/.porto-sandbox.XXXXXX")"
  trap 'rm -f -- "$profile_temporary"' EXIT HUP INT TERM
  printf '%s\n' '# Managed by the Porto installer; grants only Chromium user namespaces.' \
    'abi <abi/4.0>,' 'include <tunables/global>' \
    "profile $profile_name \"$profile_executable\" flags=(unconfined) {" \
    '  userns,' '}' > "$profile_temporary"
  chmod 644 "$profile_temporary"
  apparmor_parser -Q "$profile_temporary"
  profile_operation="-a"
  if [ -e "$profile_path" ] || [ -L "$profile_path" ]; then
    if [ -L "$profile_path" ] || [ ! -f "$profile_path" ] || ! cmp -s "$profile_temporary" "$profile_path"; then
      echo "Refusing to overwrite an existing administrator AppArmor policy: $profile_path" >&2
      return 1
    fi
    profile_operation="-r"
  else
    ln "$profile_temporary" "$profile_path"
  fi
  if ! apparmor_parser "$profile_operation" "$profile_path"; then
    if [ "$profile_operation" = "-a" ]; then rm -f -- "$profile_path"; fi
    echo "Unable to load Porto's AppArmor sandbox permission." >&2
    return 1
  fi
  echo "Porto sandbox permission loaded: $profile_path"
}

install_linux_sandbox_profile /etc/apparmor.d "$1" "$2"
PORTO_APPARMOR_SETUP
  then
    echo "Porto setup incomplete: the desktop sandbox permission could not be configured. Existing Porto files have not been replaced; resolve the administrator or AppArmor error and retry." >&2
    exit 1
  fi
}

daemon_pids() {
  if [ "$goos" = "darwin" ]; then
    processes="$(ps -ax -o pid=,command=)" || return 1
    printf '%s\n' "$processes" | awk '
      {
        pid = $1
        sub(/^[[:space:]]*[0-9]+[[:space:]]+/, "", $0)
        if ($0 ~ /(^|\/)porto daemon start$/) print pid
      }
    '
    return
  fi

  for process_cmdline in /proc/[0-9]*/cmdline; do
    [ -r "$process_cmdline" ] || continue
    command="$(tr '\000' ' ' < "$process_cmdline" 2>/dev/null || true)"
    case "$command" in
      porto\ daemon\ start\ | */porto\ daemon\ start\ )
        pid="${process_cmdline#/proc/}"
        printf '%s\n' "${pid%/cmdline}"
        ;;
    esac
  done
}

stop_daemons() {
  if ! pids="$(daemon_pids)"; then
    echo "Unable to inspect running Porto daemons." >&2
    exit 1
  fi
  for pid in $pids; do
    kill "$pid" 2>/dev/null || true
  done
}

stop_linux_app() {
  app_executable="$1"
  for process_executable in /proc/[0-9]*/exe; do
    target="$(readlink "$process_executable" 2>/dev/null || true)"
    if [ "$target" = "$app_executable" ]; then
      pid="${process_executable#/proc/}"
      pid="${pid%/exe}"
      kill "$pid" 2>/dev/null || true
    fi
  done
}

if [ "$goos" = "darwin" ]; then
  install_root="${PORTO_INSTALL_DIR:-$HOME/Applications}"
  mkdir -p "$install_root"
  osascript -e 'tell application "Porto" to quit' >/dev/null 2>&1 || true
  stop_daemons
  sleep 1
  rm -rf "$install_root/Porto.app"
  ditto "$source_app" "$install_root/Porto.app"
  hdiutil detach "$mounted_volume" -quiet
  mounted_volume=""
  ln -sf "$install_root/Porto.app/Contents/Resources/porto" "$bin_dir/porto"
  install_bundled_docker_link "$install_root/Porto.app/Contents/Resources/runtime/bin/docker"
  install_bundled_tool_link \
    "dive" \
    "$install_root/Porto.app/Contents/Resources/runtime/bin/dive" \
    "Dive image inspector"
  echo "Installed Porto at $install_root/Porto.app"
  echo "Until releases are signed, macOS may require: xattr -drs com.apple.quarantine \"$install_root/Porto.app\""
  if [ "${PORTO_NO_LAUNCH:-0}" != "1" ]; then
    open "$install_root/Porto.app"
  fi
else
  install_root="${PORTO_INSTALL_DIR:-$HOME/.local/opt/porto}"
  install_linux_prerequisites
  configure_linux_sandbox
  stop_daemons
  if [ -d "$install_root" ]; then
    stop_linux_app "$install_root/Porto"
    sleep 1
  fi
  rm -rf "$install_root"
  mkdir -p "$(dirname "$install_root")"
  cp -a "$source_dir" "$install_root"
  ln -sf "$install_root/Porto" "$bin_dir/porto-desktop"
  ln -sf "$install_root/resources/porto" "$bin_dir/porto"
  install_bundled_docker_link "$install_root/resources/runtime/bin/docker"
  install_bundled_tool_link "dive" "$install_root/resources/runtime/bin/dive" "Dive image inspector"
  mkdir -p "$HOME/.local/share/applications"
  cat > "$HOME/.local/share/applications/porto.desktop" <<EOF
[Desktop Entry]
Type=Application
Name=Porto
Comment=Local apps, containers, Kubernetes, and virtual machines
Exec="$install_root/Porto"
Icon=$install_root/resources/porto.png
Terminal=false
Categories=Development;
EOF

  if [ "${PORTO_SKIP_PREREQS:-0}" = "1" ]; then
    echo "Installed Porto files at $install_root; Linux runtime prerequisite verification was explicitly skipped."
  else
    echo "Installed Porto at $install_root"
  fi
  if [ "${PORTO_NO_LAUNCH:-0}" != "1" ]; then
    nohup "$install_root/Porto" >/dev/null 2>&1 &
  fi
fi

case ":$PATH:" in
  *":$bin_dir:"*) ;;
  *) echo "Add $bin_dir to PATH to use the Porto and bundled Docker CLIs." ;;
esac
