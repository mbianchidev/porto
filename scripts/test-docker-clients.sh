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
  const qemu = execFileSync('ps', ['-axo', 'pid=,rss=,pcpu=,args='], { encoding: 'utf8', timeout: 5000 })
    .split('\n').filter((line) => line.includes('qemu-system')).map((line) => line.trim())
  const swap = execFileSync('sysctl', ['-n', 'vm.swapusage'], { encoding: 'utf8', timeout: 5000 }).trim()
  console.log('::notice title=macOS CI resources::' + JSON.stringify({
    architecture: os.arch(),
    cpus: os.availableParallelism(),
    loadAverage: os.loadavg(),
    freeDiskMiB: Math.floor(disk.bavail * disk.bsize / 1048576),
    totalMemoryMiB: Math.floor(os.totalmem() / 1048576),
    freeMemoryMiB: Math.floor(os.freemem() / 1048576),
    swap, qemuPidRssKiBAndCpuPctAndArguments: qemu,
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
  export QEMU_SYSTEM_AARCH64="'$(cygpath -m "$runtime/qemu/qemu-system-aarch64.exe")' -machine virt,accel=tcg -cpu neoverse-n1"
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
      echo "::notice::Intel macOS data fixtures explicitly use bounded QEMU TCG; a hardware capability bit is not support for nested HVF on hosted runners."
    fi
  fi
  memory=3
  if [ "$suite" = data ]; then memory=2; fi
  if [ "$suite/$goos/$goarch" = data/darwin/amd64 ]; then memory=1.5; fi
  lima_options=(start --tty=false --timeout=20m --name=porto-engine --vm-type=qemu
    --cpus=2 "--memory=$memory" --disk=20 --mount-none --containerd=user
    "--set=$PORTO_DOCKER_CLIENT_LIMA_DEPENDENCIES")
  if [ "$PORTO_DOCKER_CLIENT_SOFTWARE_EMULATION" = 1 ]; then
    if [ "$goarch" = arm64 ]; then
      lima_options+=('--set=.vmOpts.qemu.cpuType.aarch64 = "neoverse-n1"')
    fi
    archive_override="$(node scripts/lima-containerd-archive.cjs "$goarch" "$temporary/containerd-bootstrap")"
    lima_options+=("--set=$archive_override")
  fi
  created_lima=true
  limactl "${lima_options[@]}" template:ubuntu-26.04
  export PORTO_DOCKER_CLIENT_OWN_FRESH_LIMA=1
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
if [ "$suite/$goos" = data/darwin ] && { [ "$phase" = all ] || [ "$phase" = native ]; }; then
  PORTO_TEST_WEBDAV_NATIVE=1 go test ./internal/nativefiles -run '^TestMacOSNativeWebDAV' -count=1 -failfast -timeout=5m -v
fi
