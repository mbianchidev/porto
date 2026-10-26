package daemon

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	portodocker "github.com/mbianchidev/porto/internal/docker"
	"github.com/mbianchidev/porto/internal/process"
	"github.com/mbianchidev/porto/internal/resourcesaver"
	"github.com/mbianchidev/porto/internal/store"
)

func TestResourceSaverProcessWorker(t *testing.T) {
	database := os.Getenv("PORTO_TEST_SAVER_PROCESS_DATABASE")
	if database == "" {
		t.Skip("subprocess-only real ownership/recovery fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	st, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	server := New(st, nil)
	if err := server.docker.StartContainerInventory(ctx); err != nil {
		t.Fatal(err)
	}
	defer server.docker.StopContainerInventory(context.Background())
	if err := server.initializeResourceSaver(ctx); err != nil {
		t.Fatal(err)
	}
	defer server.resourceSaver.Close(context.Background())
	waitPolicy(t, ctx, func() bool {
		snapshot := server.docker.ContainerSnapshot()
		return snapshot.Available && !snapshot.Stale
	}, "worker fresh runtime inventory")
	go func() {
		if err := server.resourceSaver.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, err)
		}
	}()
	mux := http.NewServeMux()
	server.runtimeRoutes(mux)
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/api/") {
			mux.ServeHTTP(w, request)
			return
		}
		routes, _ := containerHTTPRoutes(server.docker.ContainerSnapshot())
		routes, _ = server.resourceSaverRoutes(routes)
		server.networkRoutesMu.Lock()
		server.networkRoutes = routes
		server.networkRoutesMu.Unlock()
		if !server.proxyContainerRoute(w, request, request.Host) {
			http.Error(w, "owned fixture route unavailable", http.StatusServiceUnavailable)
		}
	}))
	defer listener.Close()
	if err := json.NewEncoder(os.Stdout).Encode(map[string]string{"url": listener.URL}); err != nil {
		t.Fatal(err)
	}
	<-ctx.Done()
}

type saverProcess struct {
	command *exec.Cmd
	url     string
	stderr  *os.File
}

func startSaverProcess(t *testing.T, ctx context.Context, database string) *saverProcess {
	t.Helper()
	log, err := os.CreateTemp(filepath.Dir(database), "worker-stderr-*.log")
	if err != nil {
		t.Fatal(err)
	}
	command := process.NewCommand(ctx, "", os.Args[0], "-test.run=^TestResourceSaverProcessWorker$")
	command.Env = process.WithEnvironment(os.Environ(), "PORTO_TEST_SAVER_PROCESS_DATABASE="+database)
	command.Stderr = log
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	worker := &saverProcess{command: command, stderr: log}
	t.Cleanup(func() {
		worker.kill(t)
		if t.Failed() {
			data, err := os.ReadFile(log.Name())
			if err != nil {
				t.Logf("read ownership-worker diagnostics: %v", err)
			} else {
				t.Logf("ownership-worker diagnostics: %s", data[max(0, len(data)-16384):])
			}
		}
	})
	ready := make(chan error, 1)
	go func() {
		var message struct {
			URL string `json:"url"`
		}
		err := json.NewDecoder(stdout).Decode(&message)
		worker.url = message.URL
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil || worker.url == "" {
			data, _ := os.ReadFile(log.Name())
			t.Fatalf("resource worker did not publish its isolated listener: %v: %s", err, data)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	return worker
}

func (worker *saverProcess) kill(t *testing.T) {
	t.Helper()
	if worker.command == nil {
		return
	}
	if err := worker.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Error(err)
	}
	if err := worker.command.Wait(); err == nil {
		t.Error("the ownership worker unexpectedly exited cleanly instead of exercising process loss")
	}
	if err := worker.stderr.Close(); err != nil {
		t.Error(err)
	}
	worker.command = nil
}

func (worker *saverProcess) state(t *testing.T, ctx context.Context) resourcesaver.State {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, worker.url+"/api/runtime/resource-saver", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var status struct {
		State resourcesaver.State `json:"state"`
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("worker status failed: %s", response.Status)
	}
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	return status.State
}

func (worker *saverProcess) snapshot(t *testing.T, ctx context.Context) portodocker.ContainerSnapshot {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, worker.url+"/api/docker/containers/snapshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var snapshot portodocker.ContainerSnapshot
	if response.StatusCode != http.StatusOK {
		t.Fatalf("worker inventory failed: %s", response.Status)
	}
	if err := json.NewDecoder(response.Body).Decode(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func testComposeResourceSaverProcessRecovery(t *testing.T, ctx context.Context, manager *portodocker.Manager, image, target string, measure bool) {
	t.Helper()
	root := t.TempDir()
	api := httptest.NewServer(portodocker.NewAPI(manager, ""))
	defer api.Close()
	project := fmt.Sprintf("porto-policy-compose-%d", time.Now().UnixNano())
	hostname := project + ".docker"
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := reservation.Addr().(*net.TCPAddr).Port
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`services:
  front:
    image: %q
    platform: linux/%s
    pull_policy: never
    ports: ["127.0.0.1:%d:8080"]
    environment:
      PORTO_TEST_DEPENDENCY: http://dependency:8080/
      PORTO_TEST_IDLE_TIMER: "1"
    depends_on:
      dependency:
        condition: service_healthy
    labels:
      dev.porto.managed: "true"
      dev.porto.resource-saver: stop
      dev.porto.hostname: %q
      dev.porto.http-port: "8080"
  dependency:
    image: %q
    platform: linux/%s
    pull_policy: never
    healthcheck:
      test: ["CMD", "/fixture", "--selftest"]
      interval: 10s
      timeout: 30s
    labels:
      dev.porto.managed: "true"
      dev.porto.resource-saver: stop
`, image, target, port, project, image, target)
	file := filepath.Join(root, "compose.yaml")
	if err := os.WriteFile(file, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	compose := func(commandContext context.Context, args ...string) ([]byte, error) {
		arguments := append([]string{"compose", "--project-name", project, "--file", file}, args...)
		command := process.NewCommand(commandContext, root, "docker", arguments...)
		command.Env = process.WithEnvironment(os.Environ(), "DOCKER_HOST=tcp://"+strings.TrimPrefix(api.URL, "http://"),
			"DOCKER_CONTEXT=", "DOCKER_CONFIG="+filepath.Join(root, "client"), "DOCKER_AUTH_CONFIG=")
		return command.CombinedOutput()
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if output, err := compose(cleanup, "down", "--volumes", "--remove-orphans"); err != nil {
			t.Errorf("remove only the synthetic Compose group: %v: %s", err, output)
		}
	}()
	if output, err := compose(ctx, "up", "--detach", "--no-build"); err != nil {
		for _, container := range manager.ContainerSnapshot().Containers {
			if container.ComposeProject == project {
				document, inspectErr := manager.InspectContainer(ctx, container.ID)
				t.Logf("synthetic Compose member %s labels=%v inspect-error=%v inspect=%s", container.Name, container.Labels, inspectErr, document)
			}
		}
		t.Fatalf("create the actual synthetic Compose dependency group: %v: %s", err, output)
	}
	members := map[string]string{}
	waitPolicy(t, ctx, func() bool {
		for _, container := range manager.ContainerSnapshot().Containers {
			if container.ComposeProject == project {
				members[container.Labels["com.docker.compose.service"]] = container.ID
			}
		}
		return len(members) == 2
	}, "both real Compose members in the owned inventory")
	direct := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	waitPolicy(t, ctx, func() bool {
		response, err := client.Get(direct)
		if err != nil {
			return false
		}
		defer response.Body.Close()
		return response.StatusCode == http.StatusOK && response.Header.Get("X-Synthetic-Dependency") == "ready"
	}, "front's actual dependent HTTP readiness")
	if err := manager.CheckEngineIdle(ctx); err == nil {
		t.Fatal("real active Compose tasks did not block engine suspension")
	}
	if measure {
		first, err := manager.ContainerMetric(ctx, members["front"])
		if err != nil || first.CPU == nil || first.Memory == nil || first.Memory.Usage == nil {
			t.Fatalf("real idle workload counters unavailable: %+v, %v", first, err)
		}
		started := time.Now()
		time.Sleep(15 * time.Second)
		second, err := manager.ContainerMetric(ctx, members["front"])
		if err != nil || second.CPU == nil || second.CPU.TotalUsage <= first.CPU.TotalUsage {
			t.Fatalf("the idle timer workload did not provide a positive measured CPU baseline: %+v, %v", second, err)
		}
		delta := second.CPU.TotalUsage - first.CPU.TotalUsage
		t.Logf("idle synthetic service with 100ms housekeeping timer: CPU=%dns over %s (%.6f%% of one core), memory=%d bytes; exact same task is removed by scale-to-zero",
			delta, time.Since(started), 100*float64(delta)/float64(time.Since(started)), *first.Memory.Usage)
	}
	database := filepath.Join(root, "worker.db")
	if err := manager.StopContainerInventory(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := manager.StartContainerInventory(ctx); err != nil {
			t.Error(err)
			return
		}
		waitPolicy(t, ctx, func() bool {
			snapshot := manager.ContainerSnapshot()
			return snapshot.Available && !snapshot.Stale
		}, "parent inventory recovers before synthetic Compose cleanup")
	}()
	worker := startSaverProcess(t, ctx, database)
	defer func() { worker.kill(t) }()
	configure := func(configuration resourcesaver.Config) {
		t.Helper()
		data, err := json.Marshal(configuration)
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPut, worker.url+"/api/runtime/resource-saver", bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("configure worker: %s: %s", response.Status, body)
		}
	}
	configuration := resourcesaver.Defaults()
	configuration.Mode, configuration.IdleSeconds, configuration.WakeSeconds = resourcesaver.ScaleZero, 1, 30
	execContext, stopExec := context.WithCancel(ctx)
	defer stopExec()
	hold, err := manager.StartExec(execContext, portodocker.ExecRequest{
		ContainerID: members["front"], Command: []string{"/fixture", "--hold"}, AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	held := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(hold.Stdout()).ReadString('\n')
		if err == nil && line != "synthetic-exec-active\n" {
			err = fmt.Errorf("unexpected active-exec fixture output %q", line)
		}
		held <- err
	}()
	select {
	case err := <-held:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	configure(configuration)
	application := "compose:" + project
	waitPolicy(t, ctx, func() bool {
		state := worker.state(t, ctx)
		for _, event := range state.Events {
			if event.Action == "suspend-failed" && strings.Contains(event.Message, "active execs") {
				if _, owned := state.Entries[application]; owned {
					t.Fatal("a real active exec was claimed by a failed group suspension")
				}
				return true
			}
		}
		return false
	}, "actual active exec prevents group suspension before mutation")
	if err := hold.Kill(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, hold.Stderr()); err != nil {
		t.Fatal(err)
	}
	if err := hold.Wait(); err == nil {
		t.Fatal("the long-running exec did not observe its explicit termination")
	}
	stopExec()
	sleep := func() resourcesaver.Entry {
		t.Helper()
		var entry resourcesaver.Entry
		waitPolicy(t, ctx, func() bool {
			entry = worker.state(t, ctx).Entries[application]
			return entry.Phase == "sleeping"
		}, "real two-member Compose group idle suspension")
		if len(entry.Application.Members) != 2 {
			t.Fatalf("Compose wake ownership omitted a dependency: %+v", entry)
		}
		return entry
	}
	entry := sleep()
	for _, member := range entry.Application.Members {
		waitPolicy(t, ctx, func() bool {
			for _, container := range worker.snapshot(t, ctx).Containers {
				if container.ID == member.ID {
					return container.State == "exited" || !container.TaskPresent
				}
			}
			return false
		}, "every owned group task releases runnable CPU and process memory")
	}
	worker.kill(t)
	worker = startSaverProcess(t, ctx, database)
	recovered := worker.state(t, ctx)
	if recovered.Config.Mode != configuration.Mode || recovered.Entries[application].Token != entry.Token {
		t.Fatal("abrupt process restart lost policy or stopped-group ownership")
	}
	payload := []byte("synthetic\x00group request\xff")
	post := func(requestContext context.Context) (*http.Response, error) {
		request, err := http.NewRequestWithContext(requestContext, http.MethodPost, worker.url+"/", bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		request.Host = hostname
		return client.Do(request)
	}
	checkPost := func() {
		t.Helper()
		response, err := post(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != http.StatusOK || string(data) != fmt.Sprintf("%x", sha256.Sum256(payload)) ||
			response.Header.Get("X-Synthetic-Dependency") != "ready" {
			t.Fatalf("process-recovered group released traffic before dependency readiness: %s: %s: %v", response.Status, data, err)
		}
	}
	checkPost()
	for _, id := range members {
		waitPolicy(t, ctx, func() bool {
			for _, container := range worker.snapshot(t, ctx).Containers {
				if container.ID == id {
					return container.State == "running"
				}
			}
			return false
		}, "both group tasks wake after a real process crash")
	}
	controlReadiness := func(action string) {
		t.Helper()
		response, err := client.Post(direct+"/"+action, "text/plain", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("synthetic readiness %s failed: %s", action, response.Status)
		}
	}
	controlReadiness("hold")
	sleep()
	canceled, cancelUpload := context.WithCancel(ctx)
	canceledRequest := make(chan error, 1)
	go func() {
		response, err := post(canceled)
		if response != nil {
			_ = response.Body.Close()
		}
		canceledRequest <- err
	}()
	waitPolicy(t, ctx, func() bool {
		if worker.state(t, ctx).Pending != 1 {
			return false
		}
		resumed := 0
		for _, container := range worker.snapshot(t, ctx).Containers {
			if container.ComposeProject != project || container.State != "running" ||
				container.Healthcheck != nil && container.Health.Status != "healthy" {
				continue
			}
			var owner struct {
				Phase string `json:"phase"`
			}
			if err := json.Unmarshal([]byte(container.Labels["io.porto.resource-saver.owner"]), &owner); err == nil && owner.Phase == "resumed" {
				resumed++
			}
		}
		return resumed == 2
	}, "initiating request waits for actual application readiness")
	cancelStarted := time.Now()
	cancelUpload()
	if err := <-canceledRequest; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled initiating body lost client cancellation: %v", err)
	}
	waitPolicy(t, ctx, func() bool {
		state := worker.state(t, ctx)
		if time.Since(cancelStarted) > 5*time.Second {
			t.Fatalf("client cancellation left its real wake queued until the readiness timeout: %+v", state)
		}
		return state.Pending == 0 && state.Entries[application].Phase == "failed"
	}, "canceling a real pending wake reaps reservations and retains failure state")
	controlReadiness("release")
	checkPost()
	controlReadiness("hold")
	configuration.WakeSeconds = 15
	configure(configuration)
	sleep()
	response, err := post(ctx)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if response.StatusCode != http.StatusGatewayTimeout || readErr != nil || closeErr != nil || !bytes.Contains(body, []byte("readiness")) {
		t.Fatalf("actual readiness timeout did not preserve gateway diagnostics: %s: %s: %v: %v", response.Status, body, readErr, closeErr)
	}
	if worker.state(t, ctx).Pending != 0 {
		t.Fatal("a real readiness timeout leaked its queued request")
	}
	controlReadiness("release")
	configuration.WakeSeconds = 30
	configure(configuration)
	checkPost()
	for _, id := range members {
		if err := manager.SetResourceSaverPolicy(ctx, id, "pause", false); err != nil {
			t.Fatal(err)
		}
	}
	configuration.Mode = resourcesaver.Pause
	configure(configuration)
	paused := sleep()
	worker.kill(t)
	if err := manager.ContainerAction(ctx, members["front"], "unpause"); err != nil {
		t.Fatal(err)
	}
	if err := manager.ContainerAction(ctx, members["front"], "pause"); err != nil {
		t.Fatal(err)
	}
	worker = startSaverProcess(t, ctx, database)
	state := worker.state(t, ctx)
	if state.Entries[application].Phase != "recovery-required" {
		t.Fatal("a real unpause/re-pause during process downtime was adopted")
	}
	response, err = post(ctx)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(response.Body)
	closeErr = response.Body.Close()
	if response.StatusCode != http.StatusBadGateway || readErr != nil || closeErr != nil || !strings.Contains(string(data), "manually") {
		t.Fatalf("ambiguous paused ownership did not retain actionable gateway diagnostics: %s: %s: %v: %v", response.Status, data, readErr, closeErr)
	}
	for _, member := range paused.Application.Members {
		found := false
		for _, container := range worker.snapshot(t, ctx).Containers {
			if container.ID != member.ID {
				continue
			}
			found = true
			if container.State != "paused" || container.PID != member.PID {
				t.Fatal("process recovery silently resumed or replaced a paused group member")
			}
		}
		if !found {
			t.Fatal("process recovery lost a protected paused group member")
		}
	}
	t.Log("real Compose group wake, active-exec exclusion, canceled initiating body, readiness timeout, SIGKILL recovery, persisted policy, and manual unpause/re-pause quarantine passed")
}
