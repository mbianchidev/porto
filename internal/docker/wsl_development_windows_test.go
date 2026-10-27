//go:build windows

package docker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/process"
	"github.com/mbianchidev/porto/internal/wsl"
)

func testWSLDevelopmentWorkflow(t *testing.T, ctx context.Context, manager *Manager, image, suite string) {
	t.Helper()
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		t.Fatal("distribution registration is restricted to the disposable CI fixture")
	}
	rootfs := os.Getenv("PORTO_TEST_WSL_ROOTFS")
	sourceBinary := os.Getenv("PORTO_TEST_PORTO_BINARY")
	if rootfs == "" || sourceBinary == "" {
		t.Fatal("WSL acceptance requires a verified rootfs and current Porto binary")
	}
	root := t.TempDir()
	binary := filepath.Join(root, "Porto's caf\u00e9", "porto.exe")
	if err := os.MkdirAll(filepath.Dir(binary), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(sourceBinary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, data, 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(commandContext context.Context, executable string, arguments ...string) ([]byte, error) {
		command := process.NewCommand(commandContext, "", executable, arguments...)
		command.Env = process.WithEnvironment(os.Environ(), "PORTO_WSL_RUNTIME_DIR="+os.Getenv("PORTO_TEST_WSL_RUNTIME"))
		return command.CombinedOutput()
	}
	mustRun := func(executable string, arguments ...string) []byte {
		t.Helper()
		output, err := run(ctx, executable, arguments...)
		if err != nil {
			t.Fatalf("%s %v: %v: %s", executable, arguments, err, output)
		}
		return output
	}
	selected, untouched := suite+"-selected-wsl", suite+"-untouched-wsl"
	for _, name := range []string{selected, untouched} {
		location := filepath.Join(root, name)
		mustRun("wsl.exe", "--import", name, location, rootfs, "--version", "2")
		t.Cleanup(func() {
			cleanupContext, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			output, err := run(cleanupContext, "wsl.exe", "--unregister", name)
			if err != nil {
				t.Errorf("remove only owned WSL fixture %s: %v: %s", name, err, output)
			}
		})
	}
	shell := func(name, script string, arguments ...string) []byte {
		t.Helper()
		args := append([]string{"--distribution", name, "--exec", "sh", "-lc", script, "porto-native-wsl"}, arguments...)
		return mustRun("wsl.exe", args...)
	}
	otherBefore := shell(untouched, `set -eu
printf '%s\n' synthetic-non-opted-in-data > "$HOME/porto-unrelated-fixture"
for file in "$HOME/.profile" "$HOME/.bashrc" "$HOME/porto-unrelated-fixture"; do
  if [ -f "$file" ]; then sha256sum "$file"; fi
done
`)
	configurationRoot, err := config.Dir()
	if err != nil {
		t.Fatal(err)
	}
	integration := wsl.New(configurationRoot, nil)
	manager.SetWSLIntegration(integration)
	mustRun(binary, "wsl", "enable", selected)
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		output, err := run(cleanupContext, binary, "wsl", "disable", selected)
		if err != nil {
			t.Errorf("remove selected WSL tooling: %v: %s", err, output)
		}
		if err := manager.ApplyWSLMounts(cleanupContext, true); err != nil {
			t.Errorf("release only owned WSL shares before unregistering fixtures: %v", err)
		}
	})
	if err := manager.ApplyWSLMounts(ctx, true); err != nil {
		t.Fatal(err)
	}
	wait := time.Now().Add(time.Minute)
	for !manager.ContainerSnapshot().Available || manager.ContainerSnapshot().Stale {
		if time.Now().After(wait) {
			t.Fatalf("owned engine inventory did not recover: %s", manager.ContainerSnapshot().Message)
		}
		time.Sleep(100 * time.Millisecond)
	}
	endpoint, err := config.DockerSocketPath()
	if err != nil {
		t.Fatal(err)
	}
	server := NewAPIServer(endpoint, NewAPI(manager, endpoint))
	if err := server.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Close(closeContext); err != nil {
			t.Errorf("close private WSL pipe: %v", err)
		}
	})
	containerName := suite + "-wsl"
	builtImage := suite + "-wsl:latest"
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := manager.ContainerAction(cleanupContext, containerName, "remove-force"); err != nil && !errorsIsNotFound(err) {
			t.Errorf("remove owned WSL container: %v", err)
		}
		if err := manager.RemoveImage(cleanupContext, builtImage, true); err != nil && !errorsIsNotFound(err) {
			t.Errorf("remove owned WSL image: %v", err)
		}
	})
	output := shell(selected, `set -eu
docker version
docker compose version
docker buildx version
workspace="$HOME/Porto project quote's caf`+"\u00e9"+`"
mkdir -p "$workspace/source"
printf '%s\n' synthetic-initial-file > "$workspace/source/message"
cat > "$workspace/compose.yaml" <<EOF
services:
  app:
    image: $1
    container_name: $2
    command: ["sh", "-c", "while :; do cat /workspace/message; sleep 1; done"]
    volumes:
      - ./source:/workspace
    labels:
      porto.test.suite: $3
EOF
docker compose -f "$workspace/compose.yaml" -p "$3" up --detach
docker exec "$2" sh -c 'printf "%s\n" synthetic-container-write > /workspace/from-container'
test "$(cat "$workspace/source/from-container")" = synthetic-container-write
printf '%s\n' synthetic-edited-file > "$workspace/source/message"
for attempt in $(seq 1 30); do
  if docker logs "$2" 2>&1 | grep -Fq synthetic-edited-file; then break; fi
  [ "$attempt" -lt 30 ] || { echo 'WSL file changes did not reach the polling workload' >&2; exit 1; }
  sleep 1
done
printf 'FROM %s\nCOPY message /wsl-build-message\n' "$1" > "$workspace/source/Dockerfile"
docker buildx build --no-cache --load --tag "$4" "$workspace/source"
test "$(docker run --rm --network none "$4" cat /wsl-build-message)" = synthetic-edited-file
docker inspect --format '{{json .Config.Env}}' "$2"
`, image, containerName, suite, builtImage)
	if !strings.Contains(string(output), "CHOKIDAR_USEPOLLING=true") || !strings.Contains(string(output), "WATCHPACK_POLLING=true") {
		t.Fatalf("selected WSL workload did not receive the documented watch policy: %s", output)
	}
	credential := shell(selected, `set -eu
printf '%s' '{"ServerURL":"registry.synthetic.invalid","Username":"synthetic","Secret":"mock-password"}' | docker-credential-porto store
printf '%s\n' registry.synthetic.invalid | docker-credential-porto get
printf '%s\n' registry.synthetic.invalid | docker-credential-porto erase
`)
	var decoded struct{ Username, Secret string }
	if err := json.Unmarshal(credential, &decoded); err != nil || decoded.Username != "synthetic" || decoded.Secret != "mock-password" {
		t.Fatal("WSL credential helper did not round-trip through the native Windows vault")
	}
	// Active UNC readers can legitimately wake their WSL file provider again.
	// Quiesce only this fixture before verifying the read-only stopped status.
	if err := manager.ContainerAction(ctx, containerName, "pause"); err != nil {
		t.Fatal(err)
	}
	mustRun("wsl.exe", "--terminate", selected)
	stoppedDeadline := time.Now().Add(30 * time.Second)
	for {
		status, err := integration.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		found, stopped := false, false
		for _, distribution := range status.Distributions {
			if distribution.Name != selected {
				continue
			}
			found = true
			if !distribution.Enabled {
				t.Fatalf("shutdown changed the selected registration identity: %+v", distribution)
			}
			stopped = !distribution.Running
		}
		if !found {
			t.Fatal("shutdown removed the selected registration")
		}
		if stopped {
			break
		}
		if time.Now().After(stoppedDeadline) {
			t.Fatal("the quiesced selected WSL fixture did not stop; status must not start it")
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	shell(selected, `set -eu; docker inspect "$1" >/dev/null`, containerName)
	if err := manager.ContainerAction(ctx, containerName, "unpause"); err != nil {
		t.Fatal(err)
	}
	shell(selected, `set -eu
docker exec "$1" sh -c 'if ! test -f /workspace/from-container; then
  echo "The selected WSL bind did not reconnect to its original data" >&2
  ls -la /workspace >&2
  cat /proc/self/mountinfo >&2
  exit 1
fi'`, containerName)
	mustRun(binary, "wsl", "disable", selected)
	otherAfter := shell(untouched, `set -eu
for file in "$HOME/.profile" "$HOME/.bashrc" "$HOME/porto-unrelated-fixture"; do
  if [ -f "$file" ]; then sha256sum "$file"; fi
done
test ! -e "$HOME/.local/share/porto/wsl"
`)
	if string(otherBefore) != string(otherAfter) {
		t.Fatalf("non-opted-in distribution changed: before=%s after=%s", otherBefore, otherAfter)
	}
	if _, err := manager.InspectContainer(ctx, containerName); err != nil {
		t.Fatalf("WSL removal damaged existing Linux workloads: %v", err)
	}
	t.Logf("real WSL 2 Compose, Buildx, bidirectional binds, polling watches, vault, shutdown and removal passed for %s", selected)
}
