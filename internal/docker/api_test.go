package docker

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
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
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
	controlapi "github.com/moby/buildkit/api/services/control"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

func TestDockerAPIHandlesVersionedCoreRoutes(t *testing.T) {
	manager := New(&fakeRunner{
		outputs: map[string][]byte{
			"nerdctl version": []byte("nerdctl version 2.1.0\n"),
			"nerdctl ps -a --no-trunc --format {{json .}}":            nil,
			"nerdctl images --digests --no-trunc --format {{json .}}": nil,
		},
		errors: map[string]error{},
	})
	handler := NewAPI(manager, "/tmp/porto.sock")

	ping := httptest.NewRecorder()
	handler.ServeHTTP(ping, httptest.NewRequest(http.MethodGet, "/v1.47/_ping", nil))
	if ping.Code != http.StatusOK || ping.Body.String() != "OK" || ping.Header().Get("Builder-Version") != "2" {
		t.Fatalf("ping = %d %q", ping.Code, ping.Body.String())
	}

	info := httptest.NewRecorder()
	handler.ServeHTTP(info, httptest.NewRequest(http.MethodGet, "/v1.47/info", nil))
	if info.Code != http.StatusOK {
		t.Fatalf("info = %d: %s", info.Code, info.Body.String())
	}
	var document map[string]any
	if err := json.NewDecoder(info.Body).Decode(&document); err != nil {
		t.Fatalf("decode info: %v", err)
	}
	if document["ID"] != "porto" || document["ServerVersion"] == "" {
		t.Fatalf("unexpected info: %+v", document)
	}
}

func TestDockerAPICreatesContainerThroughNativeBackend(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"nerdctl create --name demo --network bridge --env MODE=test --label app=demo --publish 127.0.0.1:8080:80/tcp alpine:latest sleep 30": []byte("container-id\n"),
		},
		errors: map[string]error{},
	}

	handler := NewAPI(New(runner), "/tmp/porto.sock")
	body := bytes.NewBufferString(`{
		"Image":"alpine:latest",
		"Cmd":["sleep","30"],
		"Env":["MODE=test"],
		"Labels":{"app":"demo"},
		"HostConfig":{
			"NetworkMode":"bridge",
			"PortBindings":{"80/tcp":[{"HostIp":"127.0.0.1","HostPort":"8080"}]}
		}
	}`)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1.47/containers/create?name=demo", body))
	if response.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "container-id") {
		t.Fatalf("unexpected create response: %s", response.Body.String())
	}
}

func TestDockerAPICreatesPrivilegedKindContainer(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		if len(command.Args) == 0 || command.Args[0] != "create" {
			return nil, fmt.Errorf("unexpected command: %+v", command)
		}
		required := [][]string{
			{"--privileged"},
			{"--security-opt", "seccomp=unconfined"},
			{"--security-opt", "apparmor=unconfined"},
			{"--tmpfs", "/tmp"},
			{"--tmpfs", "/run"},
			{"--volume", "/var"},
			{"--volume", "/lib/modules:/lib/modules:ro"},
			{"--cgroupns", "private"},
			{"--userns", "host"},
			{"--device", "/dev/fuse:/dev/fuse:rwm"},
			{"--sysctl", "net.ipv6.conf.all.forwarding=1"},
		}
		for _, sequence := range required {
			if !containsArgumentSequence(command.Args, sequence) {
				return nil, fmt.Errorf("missing arguments %v in %v", sequence, command.Args)
			}
		}
		return []byte("kind-node-id\n"), nil
	}
	body := bytes.NewBufferString(`{
		"Image":"kindest/node:v1.36.0",
		"Hostname":"porto-kind-control-plane",
		"Tty":true,
		"Volumes":{"/var":{}},
		"HostConfig":{
			"Privileged":true,
			"SecurityOpt":["seccomp=unconfined","apparmor=unconfined"],
			"Tmpfs":{"/tmp":"","/run":""},
			"Binds":["/lib/modules:/lib/modules:ro"],
			"CgroupnsMode":"private",
			"UsernsMode":"host",
			"Devices":[{"PathOnHost":"/dev/fuse","PathInContainer":"/dev/fuse","CgroupPermissions":"rwm"}],
			"Sysctls":{"net.ipv6.conf.all.forwarding":"1"},
			"NetworkMode":"kind",
			"RestartPolicy":{"Name":"on-failure","MaximumRetryCount":1},
			"Init":false
		}
	}`)
	response := httptest.NewRecorder()
	NewAPI(New(runner), "/tmp/porto.sock").ServeHTTP(
		response,
		httptest.NewRequest(http.MethodPost, "/v1.47/containers/create?name=porto-kind-control-plane", body),
	)
	if response.Code != http.StatusCreated {
		t.Fatalf("create privileged container = %d: %s", response.Code, response.Body.String())
	}
}

func TestDockerAPIRejectsUnsupportedOperationsExplicitly(t *testing.T) {
	handler := NewAPI(New(&fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}), "/tmp/porto.sock")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1.47/events", nil))
	if response.Code != http.StatusNotImplemented || !strings.Contains(response.Body.String(), "does not support") {
		t.Fatalf("unsupported = %d: %s", response.Code, response.Body.String())
	}
}

func TestDockerAPIPullsDigestFromDockerTagParameter(t *testing.T) {
	const digest = "sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5"
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"nerdctl pull docker.io/kindest/node@" + digest: nil,
		},
		errors: map[string]error{},
	}
	response := httptest.NewRecorder()
	NewAPI(New(runner), "/tmp/porto.sock").ServeHTTP(
		response,
		httptest.NewRequest(http.MethodPost, "/v1.47/images/create?fromImage=docker.io%2Fkindest%2Fnode&tag="+digest, nil),
	)
	if response.Code != http.StatusOK {
		t.Fatalf("pull digest = %d: %s", response.Code, response.Body.String())
	}
}

func TestDockerAPIBuildsMultiPlatformImage(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.streamer = func(command runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
		if command.Name != "nerdctl" {
			return nil, fmt.Errorf("unexpected command: %s", command.Name)
		}
		args := strings.Join(command.Args, " ")
		if !strings.Contains(args, "build --progress plain") ||
			!strings.Contains(args, "--platform linux/amd64,linux/arm64") ||
			!strings.Contains(args, "--tag example/app:latest") ||
			!strings.Contains(args, "--build-arg VERSION=1") {
			return nil, fmt.Errorf("unexpected build arguments: %s", args)
		}
		if command.Args[len(command.Args)-1] != "-" || command.StdinReader == nil {
			return nil, fmt.Errorf("build context was not streamed from an archive: %+v", command)
		}
		archive := tar.NewReader(command.StdinReader)
		header, err := archive.Next()
		if err != nil {
			return nil, fmt.Errorf("read stored build context: %w", err)
		}
		content, err := io.ReadAll(archive)
		if err != nil {
			return nil, fmt.Errorf("read stored Dockerfile: %w", err)
		}
		if header.Name != "Dockerfile" {
			return nil, fmt.Errorf("unexpected build context entry: %q", header.Name)
		}
		if string(content) != "FROM scratch\n" {
			return nil, fmt.Errorf("unexpected Dockerfile: %q", content)
		}
		chunk := runtimes.OutputChunk{Stream: "stdout", Data: []byte("build complete\n")}
		if err := emit(chunk); err != nil {
			return nil, err
		}
		return chunk.Data, nil
	}

	var contextArchive bytes.Buffer
	archive := tar.NewWriter(&contextArchive)
	if err := archive.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0o600, Size: int64(len("FROM scratch\n"))}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write([]byte("FROM scratch\n")); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(
		http.MethodPost,
		`/v1.47/build?t=example%2Fapp%3Alatest&platform=linux%2Famd64%2Clinux%2Farm64&buildargs=%7B%22VERSION%22%3A%221%22%7D`,
		&contextArchive,
	)
	request.Header.Set("Content-Type", "application/x-tar")
	response := httptest.NewRecorder()
	NewAPI(New(runner), "/tmp/porto.sock").ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("build response = %d: %s", response.Code, response.Body.String())
	}
	var message struct {
		Stream string `json:"stream"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(response.Body.Bytes()), &message); err != nil {
		t.Fatalf("decode build stream: %v: %s", err, response.Body.String())
	}
	if message.Stream != "build complete\n" {
		t.Fatalf("unexpected build stream: %+v", message)
	}
}

func TestDockerAPIBuildRejectsEmptyContextArchive(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1.47/build", http.NoBody)
	request.Header.Set("Content-Type", "application/x-tar")
	response := httptest.NewRecorder()
	NewAPI(New(&fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}), "/tmp/porto.sock").
		ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "empty") {
		t.Fatalf("empty build context = %d: %s", response.Code, response.Body.String())
	}
}

func TestDockerCLIBuildsImageThroughPortoContext(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket compatibility test")
	}

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker CLI is not installed")
	}
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.streamer = func(command runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
		args := strings.Join(command.Args, " ")
		if !strings.Contains(args, "--platform linux/amd64") {
			return nil, fmt.Errorf("missing build platform argument: %s", args)
		}
		chunk := runtimes.OutputChunk{Stream: "stdout", Data: []byte("Successfully built sha256:porto\n")}
		if err := emit(chunk); err != nil {
			return nil, err
		}
		return chunk.Data, nil
	}

	socketDir, err := os.MkdirTemp("/tmp", "porto-build-api-*")
	if err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "docker.sock")
	ctx, cancel := context.WithCancel(context.Background())
	server := NewAPIServer(socketPath, NewAPI(New(runner), socketPath))
	if err := server.Start(ctx); err != nil {
		cancel()
		t.Fatalf("start API server: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		closeContext, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer closeCancel()
		_ = server.Close(closeContext)
	})

	configDir := t.TempDir()
	createContext := exec.Command("docker", "context", "create", "porto", "--docker", "host=unix://"+socketPath)
	createContext.Env = append(os.Environ(), "DOCKER_CONFIG="+configDir)
	if output, err := createContext.CombinedOutput(); err != nil {
		t.Fatalf("create Docker context: %v: %s", err, output)
	}
	buildContext := t.TempDir()
	if err := os.WriteFile(filepath.Join(buildContext, "Dockerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	build := exec.Command(
		"docker", "--context", "porto", "build",
		"--platform", "linux/amd64",
		"--tag", "example/app:latest",
		buildContext,
	)
	build.Env = append(os.Environ(), "DOCKER_CONFIG="+configDir, "DOCKER_BUILDKIT=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("docker build: %v: %s", err, output)
	}
}

func TestDockerCLIExecStreamsStdinAndMultiplexedOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket compatibility test")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker CLI is not installed")
	}
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		if strings.Join(command.Args, " ") == "container inspect demo" {
			return []byte(`{"Id":"demo","State":{"Running":true},"HostConfig":{"Privileged":true},"Config":{"Tty":false}}`), nil
		}
		if strings.Join(command.Args, " ") == "wait demo" {
			return []byte("0\n"), nil
		}
		return nil, fmt.Errorf("unexpected command: %+v", command)
	}
	runner.starter = func(command runtimes.Command) (runtimes.Process, error) {
		for _, sequence := range [][]string{
			{"exec", "--privileged"},
			{"--interactive"},
			{"demo", "cat"},
		} {
			if !containsArgumentSequence(command.Args, sequence) {
				return nil, fmt.Errorf("missing exec arguments %v in %v", sequence, command.Args)
			}
		}
		return newFakeProcess(func(stdin []byte) ([]byte, []byte, error) {
			return stdin, nil, nil
		}), nil
	}

	socketDir, err := os.MkdirTemp("/tmp", "porto-exec-api-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "docker.sock")
	ctx, cancel := context.WithCancel(context.Background())
	server := NewAPIServer(socketPath, NewAPI(New(runner), socketPath))
	if err := server.Start(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		closeContext, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer closeCancel()
		_ = server.Close(closeContext)
	})

	configDir := t.TempDir()
	createContext := exec.Command("docker", "context", "create", "porto", "--docker", "host=unix://"+socketPath)
	createContext.Env = append(os.Environ(), "DOCKER_CONFIG="+configDir)
	if output, err := createContext.CombinedOutput(); err != nil {
		t.Fatalf("create Docker context: %v: %s", err, output)
	}
	command := exec.Command("docker", "--context", "porto", "exec", "--privileged", "-i", "demo", "cat")
	command.Env = append(os.Environ(), "DOCKER_CONFIG="+configDir)
	command.Stdin = strings.NewReader("kind payload")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("docker exec: %v: %s", err, output)
	}
	if string(output) != "kind payload" {
		t.Fatalf("docker exec output = %q", output)
	}
}

func TestDockerCLIAttachUsesHijackedStdio(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket compatibility test")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker CLI is not installed")
	}
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		if strings.Join(command.Args, " ") == "container inspect demo" {
			return []byte(`{"Id":"demo","State":{"Running":true},"Config":{"Tty":false}}`), nil
		}
		if strings.Join(command.Args, " ") == "wait demo" {
			return []byte("0\n"), nil
		}
		return nil, fmt.Errorf("unexpected command: %+v", command)
	}
	runner.starter = func(command runtimes.Command) (runtimes.Process, error) {
		if len(command.Args) == 0 || command.Args[0] != "attach" || command.Args[len(command.Args)-1] != "demo" {
			return nil, fmt.Errorf("unexpected attach command: %v", command.Args)
		}
		return newFakeProcess(func([]byte) ([]byte, []byte, error) {
			return []byte("attached\n"), nil, nil
		}), nil
	}

	socketDir, err := os.MkdirTemp("/tmp", "porto-attach-api-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "docker.sock")
	ctx, cancel := context.WithCancel(context.Background())
	server := NewAPIServer(socketPath, NewAPI(New(runner), socketPath))
	if err := server.Start(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		closeContext, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer closeCancel()
		_ = server.Close(closeContext)
	})

	configDir := t.TempDir()
	createContext := exec.Command("docker", "context", "create", "porto", "--docker", "host=unix://"+socketPath)
	createContext.Env = append(os.Environ(), "DOCKER_CONFIG="+configDir)
	if output, err := createContext.CombinedOutput(); err != nil {
		t.Fatalf("create Docker context: %v: %s", err, output)
	}
	command := exec.Command("docker", "--context", "porto", "attach", "demo")
	command.Env = append(os.Environ(), "DOCKER_CONFIG="+configDir)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("docker attach: %v: %s", err, output)
	}
	if string(output) != "attached\n" {
		t.Fatalf("docker attach output = %q", output)
	}
}

func TestDockerCLIContainerArchiveUploadAndDownload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket compatibility test")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker CLI is not installed")
	}
	var uploadedName string
	var uploadedContent string
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		args := command.Args
		if len(args) >= 7 && args[0] == "exec" && args[1] == "demo" && args[2] == "stat" {
			target := args[len(args)-1]
			switch target {
			case "/tmp":
				return []byte("0\x0041ed\x001700000000\x00"), nil
			case "/tmp/download.txt":
				return []byte("15\x0081a4\x001700000000\x00"), nil
			}
		}
		return nil, fmt.Errorf("unexpected command: %+v", command)
	}
	runner.starter = func(command runtimes.Command) (runtimes.Process, error) {
		args := strings.Join(command.Args, " ")
		switch {
		case strings.Contains(args, "tar -xpf -") && strings.Contains(args, "-C /tmp"):
			return newFakeProcess(func(stdin []byte) ([]byte, []byte, error) {
				archive := tar.NewReader(bytes.NewReader(stdin))
				header, err := archive.Next()
				if err != nil {
					return nil, nil, err
				}
				content, err := io.ReadAll(archive)
				if err != nil {
					return nil, nil, err
				}
				uploadedName = header.Name
				uploadedContent = string(content)
				return nil, nil, nil
			}), nil
		case strings.Contains(args, "tar -cpf - -C /tmp download.txt"):
			return newFakeProcess(func([]byte) ([]byte, []byte, error) {
				var output bytes.Buffer
				archive := tar.NewWriter(&output)
				content := []byte("downloaded data")
				if err := archive.WriteHeader(&tar.Header{Name: "download.txt", Mode: 0o644, Size: int64(len(content))}); err != nil {
					return nil, nil, err
				}
				if _, err := archive.Write(content); err != nil {
					return nil, nil, err
				}
				if err := archive.Close(); err != nil {
					return nil, nil, err
				}
				return output.Bytes(), nil, nil
			}), nil
		default:
			return nil, fmt.Errorf("unexpected archive command: %v", command.Args)
		}
	}

	socketDir, err := os.MkdirTemp("/tmp", "porto-archive-api-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "docker.sock")
	ctx, cancel := context.WithCancel(context.Background())
	server := NewAPIServer(socketPath, NewAPI(New(runner), socketPath))
	if err := server.Start(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		closeContext, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer closeCancel()
		_ = server.Close(closeContext)
	})
	configDir := t.TempDir()
	createContext := exec.Command("docker", "context", "create", "porto", "--docker", "host=unix://"+socketPath)
	createContext.Env = append(os.Environ(), "DOCKER_CONFIG="+configDir)
	if output, err := createContext.CombinedOutput(); err != nil {
		t.Fatalf("create Docker context: %v: %s", err, output)
	}

	source := filepath.Join(t.TempDir(), "upload.txt")
	if err := os.WriteFile(source, []byte("uploaded data"), 0o600); err != nil {
		t.Fatal(err)
	}
	upload := exec.Command("docker", "--context", "porto", "cp", source, "demo:/tmp")
	upload.Env = append(os.Environ(), "DOCKER_CONFIG="+configDir)
	if output, err := upload.CombinedOutput(); err != nil {
		t.Fatalf("docker cp upload: %v: %s", err, output)
	}
	if uploadedName != "upload.txt" || uploadedContent != "uploaded data" {
		t.Fatalf("uploaded archive = %q %q", uploadedName, uploadedContent)
	}

	destination := filepath.Join(t.TempDir(), "download.txt")
	download := exec.Command("docker", "--context", "porto", "cp", "demo:/tmp/download.txt", destination)
	download.Env = append(os.Environ(), "DOCKER_CONFIG="+configDir)
	if output, err := download.CombinedOutput(); err != nil {
		t.Fatalf("docker cp download: %v: %s", err, output)
	}
	content, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "downloaded data" {
		t.Fatalf("downloaded content = %q", content)
	}
}

func TestDockerCLIImageSaveStreamsArchive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket compatibility test")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker CLI is not installed")
	}
	var imageArchive bytes.Buffer
	tarWriter := tar.NewWriter(&imageArchive)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o644, Size: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write([]byte("[]")); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.starter = func(command runtimes.Command) (runtimes.Process, error) {
		if strings.Join(command.Args, " ") != "save --platform linux/"+runtime.GOARCH+" alpine:latest" {
			return nil, fmt.Errorf("unexpected image save command: %v", command.Args)
		}
		return newFakeProcess(func([]byte) ([]byte, []byte, error) {
			return imageArchive.Bytes(), nil, nil
		}), nil
	}

	socketDir, err := os.MkdirTemp("/tmp", "porto-image-save-api-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "docker.sock")
	ctx, cancel := context.WithCancel(context.Background())
	server := NewAPIServer(socketPath, NewAPI(New(runner), socketPath))
	if err := server.Start(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		closeContext, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer closeCancel()
		_ = server.Close(closeContext)
	})
	configDir := t.TempDir()
	createContext := exec.Command("docker", "context", "create", "porto", "--docker", "host=unix://"+socketPath)
	createContext.Env = append(os.Environ(), "DOCKER_CONFIG="+configDir)
	if output, err := createContext.CombinedOutput(); err != nil {
		t.Fatalf("create Docker context: %v: %s", err, output)
	}
	destination := filepath.Join(t.TempDir(), "image.tar")
	save := exec.Command("docker", "--context", "porto", "image", "save", "--output", destination, "alpine:latest")
	save.Env = append(os.Environ(), "DOCKER_CONFIG="+configDir)
	if output, err := save.CombinedOutput(); err != nil {
		t.Fatalf("docker image save: %v: %s", err, output)
	}
	saved, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(saved, imageArchive.Bytes()) {
		t.Fatal("saved image archive did not match runtime output")
	}
}

func TestDockerCLIUpdatesContainerResources(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket compatibility test")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker CLI is not installed")
	}
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		if !containsArgumentSequence(command.Args, []string{"update", "--cpus", "2"}) ||
			!containsArgumentSequence(command.Args, []string{"--memory", "2147483648"}) ||
			!containsArgumentSequence(command.Args, []string{"--memory-swap", "2147483648"}) ||
			command.Args[len(command.Args)-1] != "demo" {
			return nil, fmt.Errorf("unexpected update command: %v", command.Args)
		}
		return nil, nil
	}

	socketDir, err := os.MkdirTemp("/tmp", "porto-update-api-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "docker.sock")
	ctx, cancel := context.WithCancel(context.Background())
	var updateBody []byte
	api := NewAPI(New(runner), socketPath)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/update") {
			updateBody, _ = io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(updateBody))
		}
		api.ServeHTTP(w, r)
	})
	server := NewAPIServer(socketPath, handler)
	if err := server.Start(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		closeContext, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer closeCancel()
		_ = server.Close(closeContext)
	})
	configDir := t.TempDir()
	createContext := exec.Command("docker", "context", "create", "porto", "--docker", "host=unix://"+socketPath)
	createContext.Env = append(os.Environ(), "DOCKER_CONFIG="+configDir)
	if output, err := createContext.CombinedOutput(); err != nil {
		t.Fatalf("create Docker context: %v: %s", err, output)
	}
	update := exec.Command(
		"docker", "--context", "porto", "update",
		"--cpus", "2", "--memory", "2048m", "--memory-swap", "2048m", "demo",
	)
	update.Env = append(os.Environ(), "DOCKER_CONFIG="+configDir)
	if output, err := update.CombinedOutput(); err != nil {
		t.Fatalf("docker update: %v: %s; body=%s", err, output, updateBody)
	}
}

func TestDockerAPIRejectsUnsupportedMountSemanticsAndFollowsLogs(t *testing.T) {
	handler := NewAPI(New(&fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}), "/tmp/porto.sock")
	create := httptest.NewRecorder()
	handler.ServeHTTP(create, httptest.NewRequest(
		http.MethodPost,
		"/v1.47/containers/create",
		bytes.NewBufferString(`{"Image":"alpine","HostConfig":{"Mounts":[{"Type":"bind","Source":"/tmp","Target":"/data"}]}}`),
	))
	if create.Code != http.StatusNotImplemented || !strings.Contains(create.Body.String(), "Mounts") {
		t.Fatalf("mount response = %d: %s", create.Code, create.Body.String())
	}
	logRunner := &fakeRunner{
		outputs: map[string][]byte{
			"nerdctl container inspect demo": []byte(`{"Config":{"Tty":true}}`),
		},
		errors: map[string]error{},
		ordered: map[string][]runtimes.OutputChunk{
			"nerdctl logs --follow --tail all demo": {
				{Stream: "stdout", Data: []byte("ready\n")},
			},
		},
	}
	logs := httptest.NewRecorder()
	NewAPI(New(logRunner), "/tmp/porto.sock").ServeHTTP(
		logs,
		httptest.NewRequest(http.MethodGet, "/v1.47/containers/demo/logs?follow=1&stdout=1&stderr=1", nil),
	)
	if logs.Code != http.StatusOK || logs.Body.String() != "ready\n" {
		t.Fatalf("follow response = %d: %s", logs.Code, logs.Body.String())
	}
}

func TestDockerAPIMultiplexesContainerLogStreams(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"nerdctl container inspect demo": []byte(`[{"Config":{"Tty":false}}]`),
		},
		errors: map[string]error{},
		ordered: map[string][]runtimes.OutputChunk{
			"nerdctl logs --tail all demo": {
				{Stream: "stdout", Data: []byte("out\n")},
				{Stream: "stderr", Data: []byte("err\n")},
				{Stream: "stdout", Data: []byte("out2\n")},
			},
		},
	}
	handler := NewAPI(New(runner), "/tmp/porto.sock")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(
		http.MethodGet,
		"/v1.47/containers/demo/logs?stdout=1&stderr=1",
		nil,
	))
	if response.Code != http.StatusOK {
		t.Fatalf("logs response = %d: %s", response.Code, response.Body.String())
	}
	data := response.Body.Bytes()
	if len(data) != 37 ||
		data[0] != 1 || string(data[8:12]) != "out\n" ||
		data[12] != 2 || string(data[20:24]) != "err\n" ||
		data[24] != 1 || string(data[32:37]) != "out2\n" {
		t.Fatalf("unexpected multiplexed log frames: %v", data)
	}
}

func TestDockerCLIContextInfoCompatibility(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket compatibility test")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker CLI is not installed")
	}
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"nerdctl version": []byte("nerdctl version 2.1.0\n"),
			"nerdctl ps -a --no-trunc --format {{json .}}":            nil,
			"nerdctl images --digests --no-trunc --format {{json .}}": nil,
			"nerdctl network ls --format {{json .}}":                  nil,
			"nerdctl volume ls --format {{json .}}":                   nil,
			"nerdctl create --name compatibility alpine:latest true":  []byte("compatibility-id\n"),
		},
		errors: map[string]error{},
	}
	socketDir, err := os.MkdirTemp("/tmp", "porto-api-*")
	if err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "docker.sock")
	ctx, cancel := context.WithCancel(context.Background())
	server := NewAPIServer(socketPath, NewAPI(New(runner), socketPath))
	if err := server.Start(ctx); err != nil {
		cancel()
		t.Fatalf("start API server: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		closeContext, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer closeCancel()
		_ = server.Close(closeContext)
	})

	configDir := t.TempDir()
	create := exec.Command("docker", "context", "create", "porto", "--docker", "host=unix://"+socketPath)
	create.Env = append(os.Environ(), "DOCKER_CONFIG="+configDir)
	if output, err := create.CombinedOutput(); err != nil {
		t.Fatalf("create Docker context: %v: %s", err, output)
	}
	info := exec.Command("docker", "--context", "porto", "info", "--format", "{{.ServerVersion}}")
	info.Env = append(os.Environ(), "DOCKER_CONFIG="+configDir)
	output, err := info.CombinedOutput()
	if err != nil {
		t.Fatalf("docker context info: %v: %s", err, output)
	}
	if strings.TrimSpace(string(output)) == "" {
		t.Fatal("docker context info returned an empty server version")
	}
	createContainer := exec.Command("docker", "--context", "porto", "create", "--name", "compatibility", "alpine:latest", "true")
	createContainer.Env = append(os.Environ(), "DOCKER_CONFIG="+configDir)
	output, err = createContainer.CombinedOutput()
	if err != nil {
		t.Fatalf("docker context create: %v: %s; backend commands: %+v", err, output, runner.commands)
	}
	if strings.TrimSpace(string(output)) != "compatibility-id" {
		t.Fatalf("unexpected Docker CLI create output: %s", output)
	}
	for _, args := range [][]string{
		{"--context", "porto", "ps", "-a"},
		{"--context", "porto", "images"},
		{"--context", "porto", "network", "ls"},
		{"--context", "porto", "volume", "ls"},
		{"--context", "porto", "start", "compatibility-id"},
		{"--context", "porto", "rm", "--force", "compatibility-id"},
	} {
		command := exec.Command("docker", args...)
		command.Env = append(os.Environ(), "DOCKER_CONFIG="+configDir)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("docker %s: %v: %s", strings.Join(args, " "), err, output)
		}
	}
}

func TestDockerComposeUpUsesPortoNativeSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket compatibility test")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker CLI is not installed")
	}
	version := exec.Command("docker", "compose", "version", "--short")
	version.Env = append(os.Environ(), "DOCKER_HOST=", "DOCKER_CONTEXT=")
	if output, err := version.CombinedOutput(); err != nil {
		t.Skipf("Docker Compose is not available: %v: %s", err, output)
	}

	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	containerExists := false
	running := false
	networks := map[string]string{}
	volumeExists := false
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		args := strings.Join(command.Args, " ")
		switch {
		case args == "version":
			return []byte("nerdctl version 2.1.0\n"), nil
		case strings.HasPrefix(args, "ps "):
			if containerExists {
				state, status := "created", "Created"
				if running {
					state, status = "running", "Up"
				}
				return []byte(fmt.Sprintf(`{"ID":"container-id","Names":"compose-test-app-1","Image":"alpine:latest","ImageID":"sha256:alpine","State":%q,"Status":%q,"Networks":"compose-test_default","Labels":"com.docker.compose.project=compose-test,com.docker.compose.service=app,com.docker.compose.oneoff=False,com.docker.compose.config-hash=16e3d369"}`+"\n", state, status)), nil
			}
			return nil, nil
		case strings.HasPrefix(args, "network ls "):
			var output strings.Builder
			for _, name := range []string{"compose-test_default", "compose-test_backend"} {
				if id := networks[name]; id != "" {
					networkName := strings.TrimPrefix(name, "compose-test_")
					fmt.Fprintf(&output, `{"ID":%q,"Name":%q,"Driver":"bridge","Scope":"local","Labels":%q}`+"\n",
						id, name, "com.docker.compose.project=compose-test,com.docker.compose.network="+networkName)
				}
			}
			return []byte(output.String()), nil
		case strings.HasPrefix(args, "network inspect "):
			target := strings.TrimPrefix(args, "network inspect ")
			for name, id := range networks {
				if target == name || target == id {
					networkName := strings.TrimPrefix(name, "compose-test_")
					return []byte(fmt.Sprintf(`[{"Name":%q,"Id":%q,"Driver":"bridge","Scope":"local","Labels":{"com.docker.compose.project":"compose-test","com.docker.compose.network":%q}}]`, name, id, networkName)), nil
				}
			}
			return nil, errors.New("no such network")
		case strings.HasPrefix(args, "volume ls "):
			if volumeExists {
				return []byte(`{"Name":"compose-test_data","Driver":"local","Scope":"local","Labels":"com.docker.compose.project=compose-test,com.docker.compose.volume=data"}` + "\n"), nil
			}
			return nil, nil
		case args == "volume inspect compose-test_data":
			if volumeExists {
				return []byte(`[{"Name":"compose-test_data","Driver":"local","Scope":"local","Labels":{"com.docker.compose.project":"compose-test","com.docker.compose.volume":"data"}}]`), nil
			}
			return nil, errors.New("no such volume")
		case strings.HasPrefix(args, "volume create "):
			volumeExists = true
			return []byte("compose-test_data\n"), nil
		case strings.HasPrefix(args, "image inspect alpine:latest"):
			return []byte(`[{"Id":"sha256:alpine","RepoTags":["alpine:latest"],"RepoDigests":[],"Config":{},"Architecture":"arm64","Os":"linux","Size":1}]`), nil
		case strings.HasPrefix(args, "network create "):
			fields := strings.Fields(args)
			name := fields[len(fields)-1]
			id := name + "-id"
			networks[name] = id
			return []byte(id + "\n"), nil
		case strings.HasPrefix(args, "create "):
			containerExists = true
			return []byte("container-id\n"), nil
		case args == "container inspect container-id":
			if !containerExists {
				return nil, errors.New("no such container")
			}
			return []byte(`[{"Id":"container-id","Name":"/compose-test-app-1","Config":{"Image":"alpine:latest","Labels":{"com.docker.compose.project":"compose-test","com.docker.compose.service":"app"},"Tty":false},"State":{"Status":"created","Running":false,"ExitCode":0},"HostConfig":{"NetworkMode":"compose-test_default"},"NetworkSettings":{"Networks":{"compose-test_default":{}}}}]`), nil
		case args == "start container-id":
			running = true
			return nil, nil
		case strings.HasPrefix(args, "stop "):
			running = false
			return nil, nil
		case strings.HasPrefix(args, "rm "):
			containerExists = false
			return nil, nil
		case strings.HasPrefix(args, "network rm "):
			target := strings.TrimPrefix(args, "network rm ")
			for name, id := range networks {
				if target == name || target == id {
					delete(networks, name)
				}
			}
			return nil, nil
		case strings.HasPrefix(args, "volume rm "):
			volumeExists = false
			return nil, nil
		default:
			return nil, fmt.Errorf("unexpected backend command: %s", args)
		}
	}
	var requestMu sync.Mutex
	requests := make([]string, 0)
	api := NewAPI(New(runner), "")
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestMu.Lock()
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		requestMu.Unlock()
		api.ServeHTTP(w, r)
	})

	socketDir, err := os.MkdirTemp("/tmp", "porto-compose-api-*")
	if err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "docker.sock")
	ctx, cancel := context.WithCancel(context.Background())
	server := NewAPIServer(socketPath, handler)
	if err := server.Start(ctx); err != nil {
		cancel()
		t.Fatalf("start API server: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		closeContext, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer closeCancel()
		_ = server.Close(closeContext)
	})

	composeFile := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(composeFile, []byte("services:\n  app:\n    image: alpine:latest\n    command: [\"sleep\", \"30\"]\n    stop_grace_period: 5s\n    volumes:\n      - data:/data\n    networks:\n      - default\n      - backend\nvolumes:\n  data:\nnetworks:\n  backend:\n"), 0o600); err != nil {
		t.Fatalf("write Compose file: %v", err)
	}
	compose := exec.Command(
		"docker", "compose",
		"--project-name", "compose-test",
		"--file", composeFile,
		"up", "--detach", "--no-build", "--pull", "never",
	)
	compose.Env = append(os.Environ(), "DOCKER_HOST=unix://"+socketPath, "DOCKER_CONTEXT=")
	if output, err := compose.CombinedOutput(); err != nil {
		requestMu.Lock()
		gotRequests := append([]string(nil), requests...)
		requestMu.Unlock()
		runner.mu.Lock()
		gotCommands := append([]runtimes.Command(nil), runner.commands...)
		runner.mu.Unlock()
		t.Fatalf("docker compose up: %v: %s\nrequests: %v\ncommands: %+v", err, output, gotRequests, gotCommands)
	}
	runner.mu.Lock()
	createdWithAlias := false
	for _, command := range runner.commands {
		arguments := strings.Join(command.Args, " ")
		if strings.HasPrefix(arguments, "create ") && strings.Contains(arguments, "--network-alias app") {
			createdWithAlias = true
		}
	}
	runner.mu.Unlock()
	if !createdWithAlias {
		t.Fatalf("Compose service alias was not passed to the runtime: %+v", runner.commands)
	}
	down := exec.Command(
		"docker", "compose",
		"--project-name", "compose-test",
		"--file", composeFile,
		"down", "--timeout", "5", "--volumes", "--remove-orphans",
	)
	down.Env = append(os.Environ(), "DOCKER_HOST=unix://"+socketPath, "DOCKER_CONTEXT=")
	if output, err := down.CombinedOutput(); err != nil {
		requestMu.Lock()
		gotRequests := append([]string(nil), requests...)
		requestMu.Unlock()
		runner.mu.Lock()
		gotCommands := append([]runtimes.Command(nil), runner.commands...)
		runner.mu.Unlock()
		t.Fatalf("docker compose down: %v: %s\nrequests: %v\ncommands: %+v", err, output, gotRequests, gotCommands)
	}
	if containerExists || len(networks) > 0 || volumeExists {
		t.Fatalf("Compose resources remain: container=%t networks=%v volume=%t", containerExists, networks, volumeExists)
	}
}

func TestAPIServerRemovesSocketOnClose(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket cleanup test")
	}
	socketDir, err := os.MkdirTemp("/tmp", "porto-api-cleanup-*")
	if err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "docker.sock")
	server := NewAPIServer(socketPath, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	if err := server.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}}
	response, err := client.Get("http://docker/_ping")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = response.Body.Close()
	cancel()
	closeContext, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer closeCancel()
	if err := server.Close(closeContext); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket still exists: %v", err)
	}
}

func TestAPIServerRefusesToReplaceActiveSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket protection test")
	}
	socketDir, err := os.MkdirTemp("/tmp", "porto-api-active-*")
	if err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "docker.sock")
	ctx, cancel := context.WithCancel(context.Background())
	first := NewAPIServer(socketPath, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("OK"))
	}))
	if err := first.Start(ctx); err != nil {
		cancel()
		t.Fatalf("start first server: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		closeContext, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer closeCancel()
		_ = first.Close(closeContext)
	})
	second := NewAPIServer(socketPath, http.NotFoundHandler())
	if err := second.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "active") {
		t.Fatalf("expected active socket refusal, got %v", err)
	}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}}
	response, err := client.Get("http://docker/_ping")
	if err != nil {
		t.Fatalf("first server became unreachable: %v", err)
	}
	_ = response.Body.Close()
}

func TestDockerAPIBridgesBuildKitControlUpgrade(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket upgrade test")
	}
	manager := New(&fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}})
	manager.dialBuildKit = func(context.Context) (net.Conn, error) {
		client, backend := net.Pipe()
		go func() {
			defer backend.Close()
			buffer := make([]byte, len("control"))
			if _, err := io.ReadFull(backend, buffer); err == nil {
				_, _ = backend.Write([]byte("ready"))
			}
		}()
		return client, nil
	}
	if err := exerciseBuildKitUpgrade(t, manager, "/grpc", nil, "control", "ready"); err != nil {
		t.Fatal(err)
	}
}

func TestBuildKitSessionAdapterForwardsMetadata(t *testing.T) {
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	control := &echoBuildKitControl{
		metadata: make(chan metadata.MD, 1),
		errors:   make(chan error, 1),
	}
	controlapi.RegisterControlServer(server, control)
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := dialBuildKitSession(ctx, listener.DialContext, http.Header{
		"Upgrade":                             []string{"h2c"},
		"X-Docker-Expose-Session-Uuid":        []string{"session-id"},
		"X-Docker-Expose-Session-Grpc-Method": []string{"/filesync.FileSync/DiffCopy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.Write([]byte("session")); err != nil {
		t.Fatal(err)
	}
	result := make([]byte, len("session"))
	if _, err := io.ReadFull(session, result); err != nil {
		t.Fatal(err)
	}
	if string(result) != "session" {
		t.Fatalf("session response = %q", result)
	}
	select {
	case received := <-control.metadata:
		if values := received.Get("x-docker-expose-session-uuid"); len(values) != 1 || values[0] != "session-id" {
			t.Fatalf("session metadata = %v", received)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("BuildKit session metadata was not forwarded")
	}
}

func TestDockerAPIBridgesBuildKitSessionUpgrade(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket upgrade test")
	}
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	control := &echoBuildKitControl{
		metadata: make(chan metadata.MD, 1),
		errors:   make(chan error, 1),
	}
	controlapi.RegisterControlServer(server, control)
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})

	manager := New(&fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}})
	manager.dialBuildKit = listener.DialContext
	if err := exerciseBuildKitUpgrade(t, manager, "/session", map[string]string{
		"X-Docker-Expose-Session-Uuid":        "session-id",
		"X-Docker-Expose-Session-Grpc-Method": "/filesync.FileSync/DiffCopy",
	}, "session", "session"); err != nil {
		select {
		case sessionErr := <-control.errors:
			t.Fatalf("session tunnel: %v; BuildKit session: %v", err, sessionErr)
		case <-time.After(time.Second):
			t.Fatalf("session tunnel: %v; BuildKit session returned no error", err)
		}
	}
	select {
	case received := <-control.metadata:
		if values := received.Get("x-docker-expose-session-uuid"); len(values) != 1 || values[0] != "session-id" {
			t.Fatalf("session metadata = %v", received)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("BuildKit session metadata was not forwarded")
	}
}

type echoBuildKitControl struct {
	controlapi.UnimplementedControlServer
	metadata chan metadata.MD
	errors   chan error
}

func (s *echoBuildKitControl) Session(stream grpc.BidiStreamingServer[controlapi.BytesMessage, controlapi.BytesMessage]) error {
	if received, ok := metadata.FromIncomingContext(stream.Context()); ok {
		if s.metadata != nil {
			s.metadata <- received
		}
	}
	for {
		message, err := stream.Recv()
		if err != nil {
			if s.errors != nil {
				s.errors <- err
			}
			return err
		}
		if err := stream.Send(message); err != nil {
			if s.errors != nil {
				s.errors <- err
			}
			return err
		}
	}
}

func exerciseBuildKitUpgrade(
	t *testing.T,
	manager *Manager,
	endpoint string,
	headers map[string]string,
	payload string,
	expected string,
) error {
	t.Helper()
	socketDir, err := os.MkdirTemp("/tmp", "porto-buildkit-api-*")
	if err != nil {
		return err
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "docker.sock")
	ctx, cancel := context.WithCancel(context.Background())
	apiServer := NewAPIServer(socketPath, NewAPI(manager, socketPath))
	if err := apiServer.Start(ctx); err != nil {
		cancel()
		return err
	}
	t.Cleanup(func() {
		cancel()
		closeContext, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer closeCancel()
		_ = apiServer.Close(closeContext)
	})

	connection, err := net.Dial("unix", socketPath)
	if err != nil {
		return err
	}
	defer connection.Close()
	var request strings.Builder
	fmt.Fprintf(&request, "POST %s HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: h2c\r\nContent-Length: 0\r\n", endpoint)
	for key, value := range headers {
		fmt.Fprintf(&request, "%s: %s\r\n", key, value)
	}
	request.WriteString("\r\n")
	if _, err := io.WriteString(connection, request.String()); err != nil {
		return fmt.Errorf("write upgrade request: %w", err)
	}
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodPost})
	if err != nil {
		return fmt.Errorf("read upgrade response: %w", err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		return fmt.Errorf("upgrade status = %d", response.StatusCode)
	}
	if _, err := io.WriteString(connection, payload); err != nil {
		return fmt.Errorf("write tunnel payload: %w", err)
	}
	result := make([]byte, len(expected))
	if _, err := io.ReadFull(reader, result); err != nil {
		return fmt.Errorf("read tunnel response: %w", err)
	}
	if string(result) != expected {
		return fmt.Errorf("tunnel response = %q, want %q", result, expected)
	}
	return nil
}

func containsArgumentSequence(arguments, sequence []string) bool {
	for index := 0; index+len(sequence) <= len(arguments); index++ {
		matches := true
		for offset := range sequence {
			if arguments[index+offset] != sequence[offset] {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

type fakeProcess struct {
	stdin  *io.PipeWriter
	stdout *io.PipeReader
	stderr *io.PipeReader
	done   chan error
	kill   func()
}

func newFakeProcess(run func([]byte) ([]byte, []byte, error)) *fakeProcess {
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	stderrReader, stderrWriter := io.Pipe()
	done := make(chan error, 1)
	var finish sync.Once
	complete := func(err error) {
		finish.Do(func() {
			_ = stdinReader.Close()
			_ = stdoutWriter.Close()
			_ = stderrWriter.Close()
			done <- err
		})
	}
	go func() {
		input, err := io.ReadAll(stdinReader)
		if err != nil {
			complete(err)
			return
		}
		stdout, stderr, runErr := run(input)
		if len(stdout) > 0 {
			_, _ = stdoutWriter.Write(stdout)
		}
		if len(stderr) > 0 {
			_, _ = stderrWriter.Write(stderr)
		}
		complete(runErr)
	}()
	return &fakeProcess{
		stdin: stdinWriter, stdout: stdoutReader, stderr: stderrReader, done: done,
		kill: func() { complete(context.Canceled) },
	}
}

func (p *fakeProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *fakeProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *fakeProcess) Stderr() io.ReadCloser { return p.stderr }
func (p *fakeProcess) Wait() error           { return <-p.done }
func (p *fakeProcess) Kill() error {
	p.kill()
	return nil
}
func (p *fakeProcess) PID() int { return 1234 }
