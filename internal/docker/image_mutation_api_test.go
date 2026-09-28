package docker

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestDockerAPIImageTag(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		if strings.Join(command.Args, " ") != "tag alpine:latest example/app:v1" {
			return nil, errors.New("unexpected tag command: " + strings.Join(command.Args, " "))
		}
		return nil, nil
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1.47/images/alpine:latest/tag?repo=example%2Fapp&tag=v1",
		nil,
	)

	NewAPI(New(runner), "/tmp/porto.sock").ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("tag = %d: %s", response.Code, response.Body.String())
	}
}

func TestDockerAPIImageLoadStreamsArchiveAndProgress(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.streamer = func(command runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
		if strings.Join(command.Args, " ") != "load" {
			return nil, errors.New("unexpected load command: " + strings.Join(command.Args, " "))
		}
		data, err := io.ReadAll(command.StdinReader)
		if err != nil {
			return nil, err
		}
		if string(data) != "synthetic-image-archive" {
			return nil, errors.New("image archive was not streamed to the runtime")
		}
		if err := emit(runtimes.OutputChunk{Stream: "stdout", Data: []byte("unpacking example/app:v1\n")}); err != nil {
			return nil, err
		}
		return nil, nil
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1.47/images/load",
		bytes.NewBufferString("synthetic-image-archive"),
	)

	NewAPI(New(runner), "/tmp/porto.sock").ServeHTTP(response, request)

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "unpacking example/app:v1") {
		t.Fatalf("load = %d: %s", response.Code, response.Body.String())
	}
}

func TestDockerAPIImagePushUsesRegistryAuthAndCleansTemporaryConfig(t *testing.T) {
	var configDirectory string
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.streamer = func(command runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
		if strings.Join(command.Args, " ") != "push registry.example.com/team/app:v1" {
			return nil, errors.New("unexpected push command: " + strings.Join(command.Args, " "))
		}
		for _, entry := range command.Env {
			if value, ok := strings.CutPrefix(entry, "DOCKER_CONFIG="); ok {
				configDirectory = value
			}
		}
		if configDirectory == "" {
			return nil, errors.New("authenticated push did not set DOCKER_CONFIG")
		}
		data, err := os.ReadFile(filepath.Join(configDirectory, "config.json"))
		if err != nil {
			return nil, err
		}
		if !bytes.Contains(data, []byte("registry.example.com")) ||
			!bytes.Contains(data, []byte(base64.StdEncoding.EncodeToString([]byte("user:token")))) {
			return nil, errors.New("registry credentials missing from temporary config")
		}
		if err := emit(runtimes.OutputChunk{Stream: "stdout", Data: []byte("pushed manifest sha256:abc\n")}); err != nil {
			return nil, err
		}
		return nil, nil
	}
	auth, err := json.Marshal(map[string]string{
		"username":      "user",
		"password":      "token",
		"serveraddress": "registry.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1.47/images/registry.example.com/team/app/push?tag=v1",
		nil,
	)
	request.Header.Set("X-Registry-Auth", base64.RawURLEncoding.EncodeToString(auth))

	NewAPI(New(runner), "/tmp/porto.sock").ServeHTTP(response, request)

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "pushed manifest") {
		t.Fatalf("push = %d: %s", response.Code, response.Body.String())
	}
	if configDirectory == "" {
		t.Fatal("temporary registry config path was not observed")
	}
	if _, err := os.Stat(configDirectory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary registry config was not removed: %v", err)
	}
}

func TestDockerAPIImageLoadReportsMalformedArchiveFailure(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.streamer = func(runtimes.Command, func(runtimes.OutputChunk) error) ([]byte, error) {
		return []byte("invalid tar header"), errors.New("exit status 1")
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1.47/images/load", bytes.NewBufferString("broken"))

	NewAPI(New(runner), "/tmp/porto.sock").ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), "invalid tar header") {
		t.Fatalf("load failure = %d: %s", response.Code, response.Body.String())
	}
}

func TestDockerAPIImageImportStreamsRootFilesystem(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.streamer = func(command runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
		if strings.Join(command.Args, " ") != "image import --platform linux/amd64 --message seed - example/rootfs:v1" {
			return nil, errors.New("unexpected import command: " + strings.Join(command.Args, " "))
		}
		data, err := io.ReadAll(command.StdinReader)
		if err != nil || string(data) != "synthetic-rootfs" {
			return nil, errors.New("rootfs archive was not streamed")
		}
		return nil, emit(runtimes.OutputChunk{Stream: "stdout", Data: []byte("sha256:imported\n")})
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1.47/images/create?fromSrc=-&repo=example%2Frootfs&tag=v1&platform=linux%2Famd64&message=seed",
		bytes.NewBufferString("synthetic-rootfs"),
	)

	NewAPI(New(runner), "/tmp/porto.sock").ServeHTTP(response, request)

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "sha256:imported") {
		t.Fatalf("import = %d: %s", response.Code, response.Body.String())
	}
}
