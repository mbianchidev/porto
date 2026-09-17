package docker

import (
	"context"
	"time"
)

const limaBinfmtInstallCommand = `set -eu
binfmt_dir=/proc/sys/fs/binfmt_misc
definitions_dir=/usr/lib/binfmt.d
packaged_definitions_dir=/usr/share/qemu/binfmt.d
legacy_definitions_dir=/usr/share/doc/qemu-user-static
overrides_dir=/etc/binfmt.d

static_qemu_installed() {
  if dpkg-query -W -f='${Status}\n' qemu-user-static 2>/dev/null | grep -qx 'install ok installed'; then
    return 0
  fi
  dpkg-query -W -f='${Status} ${Provides}\n' qemu-user-binfmt 2>/dev/null |
    grep -Eq '^install ok installed .*qemu-user-static( |,|$)'
}

binfmt_ready() {
  [ -r "$binfmt_dir/status" ] && grep -qx enabled "$binfmt_dir/status" || return 1
  found=false
  for definition in "$definitions_dir"/qemu*.conf "$overrides_dir"/porto-qemu*.conf; do
    [ -f "$definition" ] || continue
    while IFS=: read -r marker name kind offset magic mask interpreter flags; do
      case "$marker:$name" in
        :qemu-*) ;;
        *) continue ;;
      esac
      [ -x "$interpreter" ] || return 1
      [ -r "$binfmt_dir/$name" ] || return 1
      grep -qx enabled "$binfmt_dir/$name" || return 1
      grep -q '^flags: .*F' "$binfmt_dir/$name" || return 1
      found=true
    done < "$definition"
  done
  [ "$found" = true ]
}

if ! static_qemu_installed; then
  if ! command -v apt-get >/dev/null 2>&1; then
    echo "Porto multi-platform setup requires an Ubuntu/Debian Lima guest with apt-get" >&2
    exit 1
  fi
  apt-get -o Acquire::Retries=3 update
  if apt-cache show qemu-user-static 2>/dev/null | grep -qx 'Package: qemu-user-static'; then
    package=qemu-user-static
  else
    package=qemu-user-binfmt
  fi
  DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=l \
    apt-get -o DPkg::Lock::Timeout=120 install --yes --no-install-recommends "$package"
fi

# Distro packages may assume native 32-bit support that Apple Silicon lacks.
for name in qemu-arm qemu-i386; do
  destination="$overrides_dir/porto-$name.conf"
  if [ -f "$definitions_dir/$name.conf" ]; then
    if [ -L "$destination" ]; then
      rm -f "$destination"
    fi
    continue
  fi
  definition_found=false
  for definition in "$packaged_definitions_dir/$name.conf" "$legacy_definitions_dir/$name.conf"; do
    [ -f "$definition" ] || continue
    mkdir -p "$overrides_dir"
    if [ -e "$destination" ] && [ ! -L "$destination" ]; then
      echo "Porto refuses to replace an existing non-symlink binfmt configuration: $destination" >&2
      exit 1
    fi
    if [ ! -L "$destination" ] || [ "$(readlink "$destination")" != "$definition" ]; then
      ln -sfn "$definition" "$destination"
    fi
    definition_found=true
    break
  done
  if [ "$definition_found" != true ]; then
    echo "Porto could not find a packaged binfmt definition for $name" >&2
    exit 1
  fi
done

if ! binfmt_ready; then
  systemctl restart systemd-binfmt.service
fi
if ! binfmt_ready; then
  echo "Porto QEMU binfmt handlers are missing, disabled, or lack the F flag required for container builds" >&2
  exit 1
fi
`

func (m *Manager) installLimaBinfmt(ctx context.Context) error {
	_, err := m.runCommand(
		ctx,
		5*time.Minute,
		"configure Porto multi-platform builds",
		nil,
		"limactl",
		"shell",
		"--workdir=/",
		engineInstanceName,
		"--",
		"sudo",
		"-n",
		"sh",
		"-c",
		limaBinfmtInstallCommand,
	)
	if err != nil {
		return err
	}
	return m.refreshLimaBuildKitPlatforms(ctx)
}
