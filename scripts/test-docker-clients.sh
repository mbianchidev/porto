#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ] || { [ "$1" != local ] && [ "$1" != lima ]; }; then
  echo "usage: test-docker-clients.sh <local|lima> [clients|data]" >&2
  exit 2
fi
suite="${2:-clients}"
if [ "$suite" != clients ] && [ "$suite" != data ]; then
  echo "Unknown runtime suite: $suite" >&2
  exit 2
fi
phase="${PORTO_RUNTIME_DATA_PHASE:-all}"
if [ "$suite" = data ] && [ "$phase" != all ] && [ "$phase" != migration ] && [ "$phase" != native ] && [ "$phase" != engine ]; then
  echo "Unknown runtime data phase: $phase" >&2
  exit 2
fi
if [ "$suite" = data ] && [ "$phase" = engine ] && [ "$1" != lima ]; then
  echo "The engine acceptance phase requires an independently owned Lima runtime." >&2
  exit 2
fi
export PORTO_RUNTIME_DATA_PHASE="$phase"
if [ "${GITHUB_ACTIONS:-}" != true ]; then
  echo "This script provisions disposable CI runtimes only. Use the opt-in Go test for an existing Porto engine." >&2
  exit 2
fi

backend="$1"
root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$root"
temporary="$(mktemp -d "${RUNNER_TEMP:?}/porto-ci.XXXXXX")"
runtime="$temporary/runtime"
containerd_pid=""
buildkit_pid=""
resource_monitor_pid=""
created_lima=false
porto="$temporary/porto"
client_tests="$temporary/porto-client-tests"
dump_lima_boot_logs() {
  if [ "$(uname -s)" = Linux ] && [ -c /dev/kvm ]; then
    printf '\nCI KVM access diagnostics\n' >&2
    if ! { id && ls -l /dev/kvm && getfacl /dev/kvm; } >&2; then
      echo "Some read-only KVM access diagnostics were unavailable." >&2
    fi
  fi
  if ! node scripts/lima-boot-diagnostics.cjs "$LIMA_HOME/porto-engine"; then
    echo "Some read-only boot diagnostics were unavailable; preserving the original failure." >&2
  fi
  for name in serial.log ha.stderr.log ha.stdout.log; do
    file="$LIMA_HOME/porto-engine/$name"
    if [ -f "$file" ]; then
      printf '\nCI Lima diagnostics: %s\n' "$name" >&2
      if ! tail -n 120 "$file" >&2; then
        echo "Unable to read CI Lima diagnostics from $file." >&2
      fi
    fi
  done
  if ! node -e '
    const { spawnSync } = require("node:child_process")
    const script = String.raw`
status=0
report() {
  printf "\nCI guest diagnostic: %s\n" "$*"
  "$@" || { code=$?; printf "Diagnostic command failed (%s): %s\n" "$code" "$*" >&2; status=1; }
}
report uname -a
report sudo -n cloud-init analyze blame
report systemd-analyze critical-chain --no-pager
report cat /proc/meminfo
report cat /proc/pressure/cpu /proc/pressure/io /proc/pressure/memory
report ps -eo pid,stat,comm,wchan:32
report cat /etc/fstab
report lsblk -o NAME,TYPE,FSTYPE,LABEL,PARTLABEL,UUID,PARTUUID,MOUNTPOINTS
report ls -l /dev/disk/by-label
report sudo -n blkid -p /dev/vda13 /dev/vda15
report findmnt --mountpoint /boot
report findmnt --mountpoint /boot/efi
report udevadm info --query=property --name=/dev/vda13
report udevadm info --query=property --name=/dev/vda15
report systemctl show boot.mount boot-efi.mount local-fs.target systemd-udevd.service systemd-udev-trigger.service systemd-hwdb-update.service systemd-sysusers.service systemd-modules-load.service systemd-tmpfiles-setup-dev-early.service cloud-init-local.service cloud-init-network.service cloud-config.service cloud-final.service ssh.service systemd-networkd-wait-online.service -p Id -p ActiveState -p Result -p ActiveEnterTimestampMonotonic -p InactiveExitTimestampMonotonic -p ExecMainStartTimestampMonotonic -p ExecMainExitTimestampMonotonic -p CPUUsageNSec
report sudo -n journalctl -b -u systemd-udevd.service -u systemd-udev-trigger.service -u systemd-hwdb-update.service -u systemd-sysusers.service -u systemd-modules-load.service -u boot.mount -u boot-efi.mount --no-pager -n 160
report sudo -n tail -n 200 /var/log/cloud-init-output.log
exit "$status"
`
    const result = spawnSync("limactl", [
      "shell", "--workdir=/", "porto-engine", "--", "sh", "-c", script,
    ], { stdio: ["ignore", 2, 2], timeout: 60000, killSignal: "SIGKILL" })
    if (result.error) console.error("Unable to read CI guest boot logs: " + result.error.message)
    process.exitCode = result.error ? 1 : (result.status ?? 1)
  '; then
    echo "CI guest boot diagnostics were unavailable; preserving the original failure." >&2
  fi
}
cleanup() {
  result=$?
  trap - EXIT HUP INT TERM
  if [ -n "$resource_monitor_pid" ]; then
    if jobs -pr | grep -Fx "$resource_monitor_pid" >/dev/null; then
      if ! kill "$resource_monitor_pid"; then
        echo "Unable to stop the owned macOS CI resource monitor." >&2
        result=1
      fi
    fi
    if ! wait "$resource_monitor_pid"; then
      echo "macOS CI resource monitoring failed." >&2
      result=1
    fi
  fi
  if [ "$created_lima" = true ]; then
    if [ "$result" -ne 0 ]; then
      dump_lima_boot_logs
    fi
    if ! "$porto" docker engine-remove --confirm; then
      if ! limactl delete --force porto-engine; then
        echo "Failed to remove the isolated CI Porto engine." >&2
        result=1
      fi
    fi
  fi
  for pid in "$buildkit_pid" "$containerd_pid"; do
    if [ -n "$pid" ]; then
      if kill -0 "$pid" 2>/dev/null; then
        kill "$pid"
        wait "$pid" || true
      fi
    fi
  done
  rm -rf "$temporary"
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' HUP TERM

case "$(uname -s)" in
  Linux) goos=linux ;;
  Darwin) goos=darwin ;;
  MINGW*|MSYS*) goos=windows; porto="$temporary/porto.exe"; client_tests="$temporary/porto-client-tests.exe" ;;
  *) echo "Unsupported CI host." >&2; exit 2 ;;
esac
case "${RUNNER_ARCH:-$(go env GOARCH)}" in
  X64|amd64|x86_64) goarch=amd64 ;;
  ARM64|arm64|aarch64) goarch=arm64 ;;
  *) echo "Unsupported CI architecture." >&2; exit 2 ;;
esac
if [ "$suite/$goos" = data/darwin ]; then
  node - "$temporary" <<'NODE' &
const fs = require('node:fs')
const os = require('node:os')
const { execFileSync } = require('node:child_process')
function sample() {
  const disk = fs.statfsSync(process.argv[2])
  // Walking every process's arguments can stall on a heavily loaded runner.
  const qemu = execFileSync('ps', ['-axo', 'pid=,rss=,pcpu=,comm='], { encoding: 'utf8', timeout: 5000 })
    .split('\n').filter((line) => line.includes('qemu-system')).map((line) => line.trim())
  const swap = execFileSync('sysctl', ['-n', 'vm.swapusage'], { encoding: 'utf8', timeout: 5000 }).trim()
  console.log('::notice title=macOS CI resources::' + JSON.stringify({
    architecture: os.arch(),
    cpus: os.availableParallelism(),
    loadAverage: os.loadavg(),
    freeDiskMiB: Math.floor(disk.bavail * disk.bsize / 1048576),
    totalMemoryMiB: Math.floor(os.totalmem() / 1048576),
    freeMemoryMiB: Math.floor(os.freemem() / 1048576),
    swap, qemuPidRssKiBAndCpuPctAndCommand: qemu,
  }))
}
process.once('SIGTERM', () => process.exit(0))
sample()
setInterval(sample, 120000)
NODE
  resource_monitor_pid=$!
fi
run_as_runner() {
  if [ "$backend" = local ] && [ "$(id -u)" = 0 ] && [ -n "${SUDO_USER:-}" ]; then
    sudo -u "$SUDO_USER" env "PATH=$PATH" "PORTO_DOWNLOAD_CACHE=${PORTO_DOWNLOAD_CACHE:-}" "$@"
  else
    "$@"
  fi
}
if [ "$backend" = local ] && [ "$(id -u)" = 0 ] && [ -n "${SUDO_USER:-}" ]; then
  chown "$SUDO_UID:$SUDO_GID" "$temporary"
fi
run_as_runner bash scripts/bundle-desktop-runtime.sh "$goos" "$goarch" "$runtime"
export PATH="$runtime/bin:$runtime/lima/bin:$runtime/qemu:$PATH"
export PORTO_HOME="$temporary/porto-home"
export PORTO_DOCKER_CLIENT_INTEGRATION=1
export PORTO_DOCKER_CLIENT_BACKEND="$backend"
export PORTO_DOCKER_CLIENT_SOFTWARE_EMULATION=0
PORTO_DOCKER_CLIENT_LIMA_DEPENDENCIES="$(node scripts/lima-containerd-archive.cjs --dependencies)"
export PORTO_DOCKER_CLIENT_LIMA_DEPENDENCIES

if [ "$goos/$goarch" = windows/arm64 ]; then
  export QEMU_SYSTEM_AARCH64="'$(cygpath -m "$runtime/qemu/qemu-system-aarch64.exe")' -machine virt -accel tcg,thread=multi,tb-size=512 -cpu neoverse-n1"
  export PORTO_DOCKER_CLIENT_SOFTWARE_EMULATION=1
  echo "::notice::Windows ARM64 hosted runners cannot provide nested WHPX; using QEMU TCG for the isolated Linux guests."
fi
if [ "$goos" = linux ] && [ ! -c /dev/kvm ]; then
  export PORTO_DOCKER_CLIENT_SOFTWARE_EMULATION=1
  echo "::notice::This hosted Linux runner has no KVM device; preparing the rootless runtime archive on the host for QEMU TCG."
fi

if [ "$backend" = local ]; then
  if [ "$goos" != linux ]; then
    echo "The disposable local CI runtime requires Linux." >&2
    exit 2
  fi
  case "$goarch" in
    amd64) full_checksum=ff018f11ac2f1354bf7b3aaaca6e0b52d81139c746a89134744d0b7be3a0e62f ;;
    arm64) full_checksum=2deffe14625f93866f90a8f265d0f59d6fa476c7dc54b98adb327b1b4613efd4 ;;
    *) echo "Unsupported local runtime architecture." >&2; exit 2 ;;
  esac
  full="$temporary/nerdctl-full.tar.gz"
  run_as_runner bash scripts/download-file.sh \
    "https://github.com/containerd/nerdctl/releases/download/v2.4.1/nerdctl-full-2.4.1-linux-$goarch.tar.gz" "$full" \
    "$full_checksum"
  node scripts/verify-runtime-checksum.cjs --verify \
    "$full_checksum" "$full"
  mkdir -p "$temporary/tools" "$temporary/network"
  tar -xzf "$full" -C "$temporary/tools"
  export PATH="$temporary/tools/bin:$PATH"
  export CONTAINERD_ADDRESS="$temporary/containerd.sock"
  export CONTAINERD_NAMESPACE=default
  export CONTAINERD_SNAPSHOTTER=overlayfs
  export BUILDKIT_HOST="unix://$temporary/buildkit.sock"
  export CNI_PATH="$temporary/tools/libexec/cni"
  export NETCONFPATH="$temporary/network"
  export NERDCTL_TOML="$temporary/nerdctl.toml"
  printf 'data_root = "%s"\ncgroup_manager = "cgroupfs"\ninsecure_registry = true\n' \
    "$temporary/nerdctl-data" > "$NERDCTL_TOML"
  printf 'version = 3\nroot = "%s"\nstate = "%s"\ndisabled_plugins = ["io.containerd.cri.v1.images", "io.containerd.cri.v1.runtime"]\n[grpc]\naddress = "%s"\n' \
    "$temporary/containerd-root" "$temporary/containerd-state" "$CONTAINERD_ADDRESS" > "$temporary/containerd.toml"
  containerd --config "$temporary/containerd.toml" > "$temporary/containerd.log" 2>&1 &
  containerd_pid=$!
  for attempt in {1..60}; do
    if ctr --address "$CONTAINERD_ADDRESS" version >/dev/null 2>&1; then break; fi
    if ! kill -0 "$containerd_pid" 2>/dev/null || [ "$attempt" = 60 ]; then
      cat "$temporary/containerd.log" >&2
      exit 1
    fi
    sleep 1
  done
  buildkitd --addr "$BUILDKIT_HOST" --root "$temporary/buildkit-root" \
    --oci-worker=false --containerd-worker=true \
    --containerd-worker-addr "$CONTAINERD_ADDRESS" --containerd-worker-namespace default \
    --containerd-worker-snapshotter overlayfs > "$temporary/buildkit.log" 2>&1 &
  buildkit_pid=$!
  for attempt in {1..60}; do
    if buildctl --addr "$BUILDKIT_HOST" debug workers >/dev/null 2>&1; then break; fi
    if ! kill -0 "$buildkit_pid" 2>/dev/null || [ "$attempt" = 60 ]; then
      cat "$temporary/buildkit.log" >&2
      exit 1
    fi
    sleep 1
  done
else
  export LIMA_HOME="$temporary/lima"
  if [ "$goos" = windows ]; then
    export CGO_ENABLED=0
  fi
  go build -o "$porto" ./cmd/porto
  if [ "$goos" = darwin ]; then
    case "$(sysctl -n kern.hv_support)" in
      0)
        export PORTO_DOCKER_CLIENT_SOFTWARE_EMULATION=1
        echo "::warning::Software-emulated macOS CPU ticks cannot enforce a physical CPU ceiling; raw counter, finite activity, and native-backend ceiling checks remain enabled."
        ;;
      1) ;;
      *) echo "Cannot determine macOS hardware virtualization support." >&2; exit 1 ;;
    esac
    if [ "$suite/$goarch" = data/amd64 ]; then
      export QEMU_SYSTEM_X86_64="'$(command -v qemu-system-x86_64)' -machine q35,vmport=off -accel tcg,thread=multi,tb-size=512 -global ICH9-LPC.disable_s3=1 -global ICH9-LPC.disable_s4=1 -cpu max,-avx512vl,-pdpe1gb"
      export PORTO_DOCKER_CLIENT_SOFTWARE_EMULATION=1
      PORTO_DOCKER_CLIENT_LIMA_IMAGES="$(node scripts/lima-containerd-archive.cjs --intel-tcg-images)"
      export PORTO_DOCKER_CLIENT_LIMA_IMAGES
      echo "::warning::Intel macOS TCG uses Lima's CI-only no_timer_check workaround with matching pinned kernel/initrd; Porto assertions and production defaults are unchanged."
      echo "::notice::Intel macOS data fixtures explicitly use bounded QEMU TCG; a hardware capability bit is not support for nested HVF on hosted runners."
    fi
  fi
  memory=3
  if [ "$suite" = data ]; then memory=2; fi
  if [ "$suite/$goos/$goarch" = data/darwin/amd64 ]; then memory=1.5; fi
  lima_options=(start --tty=false --timeout=20m --name=porto-engine --vm-type=qemu
    --cpus=2 "--memory=$memory" --disk=20 --mount-none --containerd=user
    "--set=$PORTO_DOCKER_CLIENT_LIMA_DEPENDENCIES")
  if [ -n "${PORTO_DOCKER_CLIENT_LIMA_IMAGES:-}" ]; then
    lima_options+=("--set=$PORTO_DOCKER_CLIENT_LIMA_IMAGES")
  fi
  if [ "$PORTO_DOCKER_CLIENT_SOFTWARE_EMULATION" = 1 ]; then
    if [ "$goarch" = arm64 ]; then
      lima_options+=('--set=.vmOpts.qemu.cpuType.aarch64 = "neoverse-n1"')
    fi
    archive_override="$(node scripts/lima-containerd-archive.cjs "$goarch" "$temporary/containerd-bootstrap")"
    lima_options+=("--set=$archive_override")
  fi
  created_lima=true
  limactl "${lima_options[@]}" template:ubuntu-26.04
  if [ "$goos/$goarch" = windows/amd64 ]; then
    echo "::notice::Disabling unsupported WHPX user shadow stacks only in the disposable x64 guest; native acceleration and acceptance checks remain enabled."
    MSYS2_ARG_CONV_EXCL='*' limactl shell --workdir=/ porto-engine -- sudo -n sh -eu <<'GUEST'
printf '%s\n' 'GRUB_CMDLINE_LINUX="${GRUB_CMDLINE_LINUX} nousershstk"' \
  > /etc/default/grub.d/99-porto-ci-whpx.cfg
update-grub
GUEST
    limactl stop --tty=false porto-engine
    limactl start --tty=false --timeout=20m porto-engine
    MSYS2_ARG_CONV_EXCL='*' limactl shell --workdir=/ porto-engine -- sh -eu -c '
      grep -qw nousershstk /proc/cmdline
      if grep -qw user_shstk /proc/cpuinfo; then
        echo "The disposable WHPX guest still exposes unsafe user shadow stacks" >&2
        exit 1
      fi
      printf "CI WHPX guest kernel arguments: "
      cat /proc/cmdline
    '
  fi
  export PORTO_DOCKER_CLIENT_OWN_FRESH_LIMA=1
fi

if [ "$suite/$goos/$goarch" = clients/windows/amd64 ]; then
  wsl_rootfs="$temporary/ubuntu-24.04.5-wsl-amd64.wsl"
  wsl_checksum="bb415d824822c4b878125729af451a5d18fb13d1cf5cbed9a7393ad64ac6039e"
  run_as_runner bash scripts/download-file.sh \
    "https://releases.ubuntu.com/noble/ubuntu-24.04.5-wsl-amd64.wsl" \
    "$wsl_rootfs" "$wsl_checksum"
  node scripts/verify-runtime-checksum.cjs --verify "$wsl_checksum" "$wsl_rootfs"
  export PORTO_TEST_WSL_ROOTFS="$(cygpath -w "$wsl_rootfs")"
  export PORTO_TEST_PORTO_BINARY="$(cygpath -w "$porto")"
  export PORTO_TEST_WSL_RUNTIME="$(cygpath -w "$runtime/wsl")"
  export PORTO_TEST_WINDOWS_COEXISTENCE=1
  export PORTO_WINDOWS_RUNTIME_DIR="$(cygpath -w "$runtime/windows-containers/amd64/bin")"
fi

run_as_runner go test -c -o "$client_tests" ./internal/docker
if [ "$suite/$goos/$goarch" = data/darwin/amd64 ]; then
  go clean -cache
fi
(
  cd "$root/internal/docker"
  if [ "$suite" = data ]; then
    export PORTO_RUNTIME_DATA_ACCEPTANCE=1
    export PORTO_RUNTIME_DATA_VM_TYPE=qemu
    "$client_tests" -test.run '^TestRuntimeDataAcceptance$' -test.count=1 -test.failfast -test.timeout=55m -test.v
  else
    "$client_tests" -test.run '^TestDockerDevelopmentClientsIntegration$' -test.count=1 -test.failfast -test.timeout=25m -test.v
  fi
)
if [ "$suite" = clients ]; then
  policy_tests="$temporary/porto-policy-tests"
  if [ "$goos" = windows ]; then policy_tests="$policy_tests.exe"; fi
  run_as_runner go test -c -o "$policy_tests" ./internal/daemon
  (
    cd "$root/internal/daemon"
    "$policy_tests" -test.run '^TestResourceSaverOwnedRuntimeAcceptance$' -test.count=1 -test.failfast -test.timeout=20m -test.v
  )
  if [ "$goos" != windows ]; then
    run_as_runner env PORTO_TEST_SYNC_BINARY="$runtime/bin/mutagen" \
      go test ./internal/syncshares -run '^TestMaintainedSynchronizationPreservesLargeSyntheticRepository$' -count=1 -failfast -timeout=5m -v
  fi
fi
if [ "$suite/$goos" = data/darwin ] && { [ "$phase" = all ] || [ "$phase" = native ]; }; then
  PORTO_TEST_WEBDAV_NATIVE=1 go test ./internal/nativefiles -run '^TestMacOSNativeWebDAV' -count=1 -failfast -timeout=5m -v
fi
