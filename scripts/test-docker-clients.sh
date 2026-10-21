#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 1 ] || { [ "$1" != local ] && [ "$1" != lima ]; }; then
  echo "usage: test-docker-clients.sh <local|lima>" >&2
  exit 2
fi
if [ "${GITHUB_ACTIONS:-}" != true ]; then
  echo "This script provisions disposable CI runtimes only. Use the opt-in Go test for an existing Porto engine." >&2
  exit 2
fi

backend="$1"
root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$root"
temporary="$(mktemp -d "${RUNNER_TEMP:?}/porto-docker-clients.XXXXXX")"
runtime="$temporary/runtime"
containerd_pid=""
buildkit_pid=""
created_lima=false
porto="$temporary/porto"
client_tests="$temporary/porto-client-tests"
dump_lima_boot_logs() {
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
    const result = spawnSync("limactl", [
      "shell", "--workdir=/", "porto-engine", "--", "sudo", "-n",
      "tail", "-n", "200", "/var/log/cloud-init-output.log",
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
case "$(uname -m)" in
  x86_64) goarch=amd64 ;;
  arm64|aarch64) goarch=arm64 ;;
  *) echo "Unsupported CI architecture." >&2; exit 2 ;;
esac
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

if [ "$backend" = local ]; then
  if [ "$goos/$goarch" != linux/amd64 ]; then
    echo "The disposable local CI runtime requires Linux amd64." >&2
    exit 2
  fi
  full="$temporary/nerdctl-full.tar.gz"
  run_as_runner bash scripts/download-file.sh \
    https://github.com/containerd/nerdctl/releases/download/v2.4.1/nerdctl-full-2.4.1-linux-amd64.tar.gz "$full" \
    ff018f11ac2f1354bf7b3aaaca6e0b52d81139c746a89134744d0b7be3a0e62f
  node scripts/verify-runtime-checksum.cjs --verify \
    ff018f11ac2f1354bf7b3aaaca6e0b52d81139c746a89134744d0b7be3a0e62f "$full"
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
  fi
  lima_options=(start --tty=false --timeout=20m --name=porto-engine --vm-type=qemu
    --cpus=2 --memory=3 --disk=20 --mount-none --containerd=user)
  if [ "$PORTO_DOCKER_CLIENT_SOFTWARE_EMULATION" = 1 ]; then
    archive_override="$(node scripts/lima-containerd-archive.cjs "$goarch" "$temporary/containerd-bootstrap")"
    lima_options+=("--set=$archive_override")
  fi
  created_lima=true
  limactl "${lima_options[@]}" template:ubuntu-26.04
  export PORTO_DOCKER_CLIENT_OWN_FRESH_LIMA=1
fi

run_as_runner go test -c -o "$client_tests" ./internal/docker
(
  cd "$root/internal/docker"
  "$client_tests" -test.run '^TestDockerDevelopmentClientsIntegration$' -test.count=1 -test.failfast -test.timeout=25m -test.v
)
