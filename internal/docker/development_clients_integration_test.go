package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/process"
	"github.com/moby/moby/api/types/container"
	dockerclient "github.com/moby/moby/client"
)

func TestDockerDevelopmentClientsIntegration(t *testing.T) {
	if os.Getenv("PORTO_DOCKER_CLIENT_INTEGRATION") != "1" {
		t.Skip("set PORTO_DOCKER_CLIENT_INTEGRATION=1 to test development clients against an existing Porto backend")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	t.Cleanup(cancel)
	manager := New(nil)
	backend := os.Getenv("PORTO_DOCKER_CLIENT_BACKEND")
	if os.Getenv("PORTO_DOCKER_CLIENT_OWN_FRESH_LIMA") == "1" {
		limaHome := filepath.Clean(os.Getenv("LIMA_HOME"))
		runnerTemporary := filepath.Clean(os.Getenv("RUNNER_TEMP"))
		relativeLimaHome, err := filepath.Rel(runnerTemporary, limaHome)
		if os.Getenv("GITHUB_ACTIONS") != "true" || !filepath.IsAbs(limaHome) ||
			!filepath.IsAbs(runnerTemporary) || err != nil || relativeLimaHome == "." ||
			relativeLimaHome == ".." || strings.HasPrefix(relativeLimaHome, ".."+string(os.PathSeparator)) {
			t.Fatal("fresh Lima fixture ownership is restricted to an isolated CI runtime")
		}
		if _, err := os.Stat(manager.engineStatePath()); !os.IsNotExist(err) {
			t.Fatal("refusing to replace an existing engine ownership record")
		}
		owner := newInventoryInstanceID()
		if err := manager.writeLimaOwnership(ctx, owner); err != nil {
			t.Fatal(err)
		}
		if err := manager.writeEngineState(engineState{Mode: "lima", Instance: engineInstanceName, OwnerID: owner, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		if err := manager.installLimaRuntimeHelper(ctx, engineInstanceName); err != nil {
			t.Fatal(err)
		}
	}
	switch backend {
	case "local":
		if runtime.GOOS != "linux" {
			t.Fatal("the local backend requires Linux containerd, not desktop-host CPU or a proprietary engine")
		}
		manager = NewWithStateDir(nil, t.TempDir())
		if err := manager.writeEngineState(engineState{Mode: "direct", CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	case "", "lima":
		state, err := manager.readEngineState()
		if err != nil || state.Mode != "lima" || state.Instance != engineInstanceName {
			t.Fatalf("the Lima suite requires an existing Porto-owned engine: %v", err)
		}
		ownership := manager.EngineOwnershipStatus(ctx)
		if !ownership.Owned || !ownership.Verified {
			t.Fatalf("refusing an unverified engine: %s", ownership.Message)
		}
	default:
		t.Fatalf("unknown client-test backend %q", backend)
	}
	if err := manager.StartContainerInventory(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeContext, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		if err := manager.StopContainerInventory(closeContext); err != nil {
			t.Errorf("stop test inventory: %v", err)
		}
	})
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	for !manager.ContainerSnapshot().Available {
		select {
		case <-deadline.C:
			t.Fatalf("test inventory unavailable: %s", manager.ContainerSnapshot().Message)
		case <-time.After(50 * time.Millisecond):
		}
	}
	host := startDockerTestAPI(t, manager)
	client := dockerSDKClient(t, host)
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := os.MkdirTemp(root, ".porto-client-fixture-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(workspace); err != nil {
			t.Errorf("remove synthetic client workspace: %v", err)
		}
	})
	suite := "porto-clients-" + newInventoryInstanceID()
	image := suite + ":latest"
	registryName := suite + "-registry"
	registryTag := ""
	environment := process.WithEnvironment(os.Environ(),
		"DOCKER_HOST="+host, "DOCKER_CONTEXT=", "DOCKER_CONFIG="+t.TempDir(),
		"DOCKER_AUTH_CONFIG=", "BUILDX_BUILDER=default", "BUILDX_CONFIG="+t.TempDir(),
		"PORTO_CLIENT_TEST_IMAGE="+image, "PORTO_CLIENT_TEST_WORKSPACE="+workspace,
		"PORTO_CLIENT_TEST_SUITE="+suite, "TESTCONTAINERS_RYUK_DISABLED=true",
		"DEBUG=testcontainers*",
	)
	dockerCommand := func(commandContext context.Context, arguments ...string) ([]byte, error) {
		command := process.NewCommand(commandContext, root, "docker", arguments...)
		command.Env = environment
		return command.CombinedOutput()
	}
	runDocker := func(arguments ...string) []byte {
		t.Helper()
		output, err := dockerCommand(ctx, arguments...)
		if err != nil {
			t.Fatalf("docker %s: %v: %s", strings.Join(arguments, " "), err, output)
		}
		return output
	}
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		runtimeClient, err := manager.runtimeConnector(cleanupContext)
		if err != nil {
			t.Errorf("connect fixture cleanup: %v", err)
			return
		}
		containers, snapshotErr := runtimeClient.Snapshot(cleanupContext)
		closeErr := runtimeClient.Close()
		if snapshotErr != nil || closeErr != nil {
			t.Errorf("read fixture cleanup inventory: snapshot=%v, close=%v", snapshotErr, closeErr)
		}
		for _, item := range containers {
			if item.Labels["porto.test.suite"] == suite || item.Name == registryName {
				if err := manager.ContainerAction(cleanupContext, item.ID, "remove-force"); err != nil {
					t.Errorf("remove synthetic fixture %s: %v", item.ID, err)
				}
			}
		}
		for _, reference := range []string{image, registryTag} {
			if reference != "" {
				if err := manager.RemoveImage(cleanupContext, reference, true); err != nil && !errorsIsNotFound(err) {
					t.Errorf("remove synthetic image %s: %v", reference, err)
				}
			}
		}
		if err := manager.RemoveVolume(cleanupContext, suite+"-workspace", false); err != nil && !errorsIsNotFound(err) {
			t.Errorf("remove synthetic workspace volume: %v", err)
		}
	})

	archive := syntheticClientBuildArchive(t)
	build, err := client.ImageBuild(ctx, bytes.NewReader(archive), dockerclient.ImageBuildOptions{
		Tags: []string{image}, NoCache: true, Remove: true,
	})
	if err != nil {
		t.Fatalf("Docker SDK build: %v", err)
	}
	readClientProgress(t, build.Body)
	busy, err := client.ContainerCreate(ctx, dockerclient.ContainerCreateOptions{
		Name: suite + "-cpu",
		Config: &container.Config{
			Image: image, Cmd: []string{"sh", "-c", "while :; do :; done"},
			Labels: map[string]string{"porto.test.suite": suite},
		},
		HostConfig: &container.HostConfig{NetworkMode: "none"},
	})
	if err != nil {
		t.Fatalf("Docker SDK create: %v", err)
	}
	if _, err := client.ContainerStart(ctx, busy.ID, dockerclient.ContainerStartOptions{}); err != nil {
		t.Fatalf("Docker SDK start: %v", err)
	}
	for _, options := range []dockerclient.ContainerStatsOptions{
		{IncludePreviousSample: true}, {Stream: true}, {},
	} {
		sample, baseline := readDevelopmentClientStats(t, ctx, client, busy.ID, options)
		if options.IncludePreviousSample || options.Stream {
			hostDelta := sample.CPUStats.SystemUsage - baseline.SystemUsage
			containerDelta := sample.CPUStats.CPUUsage.TotalUsage - baseline.CPUUsage.TotalUsage
			percentage := float64(containerDelta) / float64(hostDelta) * float64(sample.CPUStats.OnlineCPUs) * 100
			// Average five fixed intervals to smooth emulated tick accounting,
			// retaining the same 5% quantization allowance and raw counters.
			if !(percentage > 1 && percentage <= float64(sample.CPUStats.OnlineCPUs)*105) {
				t.Fatalf("busy-workload CPU percentage is invalid: %f, stats=%+v, baseline=%+v", percentage, sample.CPUStats, baseline)
			}
			t.Logf("real backend SDK CPU over five intervals: %.2f%% with %d online CPUs", percentage, sample.CPUStats.OnlineCPUs)
		} else if sample.PreCPUStats.SystemUsage != 0 || sample.CPUStats.SystemUsage == 0 {
			t.Fatal("one-shot invented previous counters or omitted backend CPU totals")
		}
	}
	cpuOutput := strings.TrimSpace(string(runDocker("stats", "--no-stream", "--format", "{{.CPUPerc}}", busy.ID)))
	cpuPercentage, err := strconv.ParseFloat(strings.TrimSuffix(cpuOutput, "%"), 64)
	if err != nil || cpuPercentage <= 1 {
		t.Fatalf("ordinary Docker CLI CPU percentage = %q, error=%v", cpuOutput, err)
	}
	execution, err := client.ExecCreate(ctx, busy.ID, dockerclient.ExecCreateOptions{
		Cmd: []string{"sh", "-c", "cat /fixture"}, AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	attached, err := client.ExecAttach(ctx, execution.ID, dockerclient.ExecAttachOptions{})
	if err != nil {
		t.Fatal(err)
	}
	execOutput, err := io.ReadAll(attached.Reader)
	attached.Close()
	if err != nil || !bytes.Contains(execOutput, []byte("porto-synthetic-client-fixture")) {
		t.Fatalf("SDK exec output = %q, error=%v", execOutput, err)
	}
	runDocker("rm", "--force", busy.ID)

	imageArchive := filepath.Join(workspace, "synthetic-image.tar")
	runDocker("save", "--output", imageArchive, image)
	runDocker("image", "rm", "--force", image)
	runDocker("load", "--input", imageArchive)
	corruptArchive := filepath.Join(workspace, "corrupt-image.tar")
	if err := os.WriteFile(corruptArchive, []byte("synthetic invalid tar header"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := dockerCommand(ctx, "load", "--input", corruptArchive); err == nil {
		t.Fatalf("ordinary Docker load accepted a corrupt archive: %s", output)
	}
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	registryPort := reservation.Addr().(*net.TCPAddr).Port
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	runDocker("run", "--detach", "--name", registryName, "--label", "porto.test.suite="+suite,
		"--publish", "127.0.0.1:"+strconv.Itoa(registryPort)+":5000", "registry:2")
	port := strings.TrimSpace(string(runDocker("inspect", "--format", `{{(index (index .NetworkSettings.Ports "5000/tcp") 0).HostPort}}`, registryName)))
	if parsed, err := strconv.Atoi(port); err != nil || parsed <= 0 {
		t.Fatalf("disposable registry port = %q, error=%v", port, err)
	}
	registryClient := &http.Client{Timeout: 2 * time.Second}
	registryDeadline := time.Now().Add(time.Minute)
	var registryError error
	for {
		response, err := registryClient.Get("http://127.0.0.1:" + port + "/v2/")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
			registryError = errors.New("disposable registry returned " + response.Status)
		} else {
			registryError = err
		}
		if time.Now().After(registryDeadline) {
			t.Fatalf("disposable registry did not become ready: %v", registryError)
		}
		time.Sleep(200 * time.Millisecond)
	}
	registryTag = "127.0.0.1:" + port + "/" + suite + ":v1"
	runDocker("tag", image, registryTag)
	runDocker("push", registryTag)
	runDocker("image", "inspect", registryTag)

	workflowContext, cancelWorkflow := context.WithTimeout(ctx, 5*time.Minute)
	defer cancelWorkflow()
	command := process.NewCommand(workflowContext, root, "npm", "--prefix", "scripts/docker-clients", "test")
	command.Env = environment
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	if err := command.Run(); err != nil {
		t.Fatalf("Dev Containers/Testcontainers workflows: %v", err)
	}
	t.Log("real Testcontainers and Dev Containers workflows completed")
}

func readDevelopmentClientStats(
	t *testing.T,
	ctx context.Context,
	client *dockerclient.Client,
	id string,
	options dockerclient.ContainerStatsOptions,
) (container.StatsResponse, container.CPUStats) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	stats, err := client.ContainerStats(ctx, id, options)
	if err != nil {
		t.Fatalf("live backend CPU stats: %v", err)
	}
	body := stats.Body
	defer func() {
		if body != nil {
			if err := body.Close(); err != nil {
				t.Errorf("close live backend CPU stats: %v", err)
			}
		}
	}()
	decoder := json.NewDecoder(body)
	var sample container.StatsResponse
	if err := decoder.Decode(&sample); err != nil {
		t.Fatalf("decode live backend CPU stats: %v", err)
	}
	baseline := sample.PreCPUStats
	if !options.IncludePreviousSample && !options.Stream {
		return sample, baseline
	}
	if options.Stream {
		if sample.PreCPUStats.SystemUsage != 0 {
			t.Fatal("first streaming sample had invented previous counters")
		}
		baseline = sample.CPUStats
	}
	if baseline.SystemUsage == 0 || baseline.OnlineCPUs == 0 {
		t.Fatalf("backend CPU baseline is incomplete: %+v", baseline)
	}
	previous := baseline
	for interval := 0; interval < 5; interval++ {
		if interval > 0 || options.Stream {
			if !options.Stream {
				closeErr := body.Close()
				body = nil
				if closeErr != nil {
					t.Fatalf("close live backend CPU stats: %v", closeErr)
				}
				stats, err := client.ContainerStats(ctx, id, options)
				if err != nil {
					t.Fatalf("live backend CPU stats: %v", err)
				}
				body = stats.Body
				decoder = json.NewDecoder(body)
			}
			sample = container.StatsResponse{}
			if err := decoder.Decode(&sample); err != nil {
				t.Fatalf("decode live backend CPU stats: %v", err)
			}
		}
		if sample.CPUStats.OnlineCPUs != baseline.OnlineCPUs ||
			sample.PreCPUStats.SystemUsage == 0 ||
			sample.CPUStats.SystemUsage <= sample.PreCPUStats.SystemUsage ||
			sample.CPUStats.CPUUsage.TotalUsage < sample.PreCPUStats.CPUUsage.TotalUsage ||
			sample.CPUStats.SystemUsage <= previous.SystemUsage ||
			sample.CPUStats.CPUUsage.TotalUsage < previous.CPUUsage.TotalUsage {
			t.Fatalf("backend CPU counters are incomplete or non-monotonic: current=%+v, previous=%+v, last=%+v",
				sample.CPUStats, sample.PreCPUStats, previous)
		}
		previous = sample.CPUStats
	}
	return sample, baseline
}

func syntheticClientBuildArchive(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for name, contents := range map[string]string{
		"Dockerfile": "FROM alpine:latest\nCOPY fixture /fixture\nCMD [\"sleep\", \"300\"]\n",
		"fixture":    "porto-synthetic-client-fixture\n",
	} {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(contents))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(writer, contents); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func readClientProgress(t *testing.T, body io.ReadCloser) {
	t.Helper()
	defer func() {
		if err := body.Close(); err != nil {
			t.Errorf("close client progress: %v", err)
		}
	}()
	decoder := json.NewDecoder(body)
	for {
		var frame map[string]any
		err := decoder.Decode(&frame)
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("decode client progress: %v", err)
		}
		if message, ok := frame["error"].(string); ok && message != "" {
			t.Fatalf("image transfer failed: %s", message)
		}
	}
}

func errorsIsNotFound(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "not found") || strings.Contains(strings.ToLower(err.Error()), "no such")
}
