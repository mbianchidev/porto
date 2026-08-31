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
  Darwin) goos="darwin"; extension="tar.gz" ;;
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
trap 'rm -rf "$temporary"' EXIT HUP INT TERM

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

tar -xzf "$temporary/$asset" -C "$temporary"
source_dir="$temporary/porto-desktop_${version}_${goos}_${goarch}"
if [ ! -d "$source_dir" ]; then
  echo "The Porto desktop archive has an unexpected layout." >&2
  exit 1
fi

bin_dir="${PORTO_BIN_DIR:-$HOME/.local/bin}"
mkdir -p "$bin_dir"

run_as_root() {
  if [ "$(id -u)" -eq 0 ]; then
    "$@"
  elif command -v sudo >/dev/null 2>&1; then
    sudo "$@"
  else
    return 1
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
  ditto "$source_dir/Porto.app" "$install_root/Porto.app"
  ln -sf "$install_root/Porto.app/Contents/Resources/porto" "$bin_dir/porto"
  echo "Installed Porto at $install_root/Porto.app"
  echo "Until releases are signed, macOS may require: xattr -drs com.apple.quarantine \"$install_root/Porto.app\""
  if [ "${PORTO_NO_LAUNCH:-0}" != "1" ]; then
    open "$install_root/Porto.app"
  fi
else
  install_root="${PORTO_INSTALL_DIR:-$HOME/.local/opt/porto}"
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
  mkdir -p "$HOME/.local/share/applications"
  cat > "$HOME/.local/share/applications/porto.desktop" <<EOF
[Desktop Entry]
Type=Application
Name=Porto
Comment=Local apps, containers, Kubernetes, and virtual machines
Exec=$install_root/Porto
Icon=$install_root/resources/porto.png
Terminal=false
Categories=Development;
EOF

  if [ "${PORTO_SKIP_PREREQS:-0}" != "1" ] && ! command -v qemu-system-x86_64 >/dev/null 2>&1 && ! command -v qemu-system-aarch64 >/dev/null 2>&1; then
    if command -v apt-get >/dev/null 2>&1; then
      run_as_root apt-get update && run_as_root apt-get install -y qemu-system qemu-utils || echo "Install QEMU/KVM to use Porto virtual machines." >&2
    elif command -v dnf >/dev/null 2>&1; then
      run_as_root dnf install -y qemu-kvm qemu-img || echo "Install QEMU/KVM to use Porto virtual machines." >&2
    elif command -v pacman >/dev/null 2>&1; then
      run_as_root pacman -S --needed qemu-desktop || echo "Install QEMU/KVM to use Porto virtual machines." >&2
    elif command -v zypper >/dev/null 2>&1; then
      run_as_root zypper --non-interactive install qemu || echo "Install QEMU/KVM to use Porto virtual machines." >&2
    else
      echo "Install QEMU/KVM to use Porto virtual machines on Linux." >&2
    fi
  fi

  echo "Installed Porto at $install_root"
  if [ "${PORTO_NO_LAUNCH:-0}" != "1" ]; then
    nohup "$install_root/Porto" >/dev/null 2>&1 &
  fi
fi

case ":$PATH:" in
  *":$bin_dir:"*) ;;
  *) echo "Add $bin_dir to PATH to use the porto CLI." ;;
esac
